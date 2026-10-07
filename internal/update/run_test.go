package update

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

// logBuf collects the messages of a run so that a test can ask for a stable key.
type logBuf struct {
	mu   sync.Mutex
	msgs []string
}

func (l *logBuf) Enabled(context.Context, slog.Level) bool { return true }
func (l *logBuf) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, r.Message)
	return nil
}
func (l *logBuf) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logBuf) WithGroup(string) slog.Handler      { return l }
func (l *logBuf) count(msg string) int {
	n := 0
	for _, m := range l.msgs {
		if m == msg {
			n++
		}
	}
	return n
}

type rig struct {
	t        *testing.T
	d        Deps
	logs     *logBuf
	now      time.Time
	upgrades []int // exit codes handed out in order; the last one repeats
	calls    int
	latest   string
	latestEr error
	reboots  int
	rebootOK bool
}

// sunday is 2026-10-04, a Sunday; the default test window is Sun 03:00-05:00 UTC.
func newRig(t *testing.T, mode string) *rig {
	r := &rig{t: t, logs: &logBuf{}, now: time.Date(2026, 10, 4, 3, 30, 0, 0, time.UTC), upgrades: []int{0}, latest: "v1.1.0"}
	u := config.DefaultUpdate()
	u.Mode, u.Window = mode, "Sun 03:00-05:00"
	r.d = Deps{
		Update:  u,
		Version: "v1.0.0",
		Store:   Store{Path: filepath.Join(t.TempDir(), "update", "state.json")},
		Log:     slog.New(r.logs),
		Now:     func() time.Time { return r.now },
		Latest:  func(context.Context) (string, error) { return r.latest, r.latestEr },
		Upgrade: func(context.Context) (int, error) {
			i := r.calls
			if i >= len(r.upgrades) {
				i = len(r.upgrades) - 1
			}
			r.calls++
			if r.upgrades[i] < 0 {
				return -1, errors.New("exec failed")
			}
			return r.upgrades[i], nil
		},
		RebootRequired: func(context.Context) bool { return r.rebootOK },
		Reboot:         func(context.Context) error { r.reboots++; return nil },
	}
	return r
}

func (r *rig) run() error {
	r.t.Helper()
	return Run(context.Background(), r.d)
}

func (r *rig) at(s string) {
	r.t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, time.UTC)
	if err != nil {
		r.t.Fatal(err)
	}
	r.now = v
}

func TestNotifyModeNeverUpgrades(t *testing.T) {
	r := newRig(t, config.UpdateNotify)
	for _, when := range []string{"2026-10-04 03:00", "2026-10-04 03:30", "2026-10-11 04:00"} {
		r.at(when)
		if err := r.run(); err != nil {
			t.Fatal(err)
		}
	}
	if r.calls != 0 {
		t.Errorf("notify mode ran the upgrade %d time(s)", r.calls)
	}
}

func TestAutoModeUpgradesOnlyInsideTheWindow(t *testing.T) {
	r := newRig(t, config.UpdateAuto)
	for _, when := range []string{"2026-10-04 02:59", "2026-10-04 05:00", "2026-10-05 03:30", "2026-10-03 03:30", "2026-10-04 12:00"} {
		r.at(when)
		if err := r.run(); err != nil {
			t.Fatal(err)
		}
	}
	if r.calls != 0 {
		t.Fatalf("the upgrade ran %d time(s) outside the window", r.calls)
	}
	r.at("2026-10-04 03:00")
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 || r.logs.count("unattended_upgrade_started") != 1 {
		t.Errorf("want one upgrade at the opening of the window, got %d", r.calls)
	}
}

func TestAutoModeRunsOncePerWindow(t *testing.T) {
	r := newRig(t, config.UpdateAuto)
	for _, when := range []string{"2026-10-04 03:00", "2026-10-04 03:15", "2026-10-04 04:45"} {
		r.at(when)
		if err := r.run(); err != nil {
			t.Fatal(err)
		}
	}
	if r.calls != 1 {
		t.Errorf("a window that completed an upgrade ran it %d times", r.calls)
	}
	r.at("2026-10-11 03:00") // the next Sunday: a new window
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if r.calls != 2 {
		t.Errorf("the next window should run once more, got %d calls in all", r.calls)
	}
}

func TestRefusedUpgradeIsRetriedInTheSameWindow(t *testing.T) {
	r := newRig(t, config.UpdateAuto)
	r.upgrades = []int{ExitRefused, ExitRefused, ExitOK}
	for _, when := range []string{"2026-10-04 03:00", "2026-10-04 03:15", "2026-10-04 03:30", "2026-10-04 03:45"} {
		r.at(when)
		if err := r.run(); err != nil {
			t.Fatalf("a refusal changed nothing and is not a failure of the unit: %v", err)
		}
	}
	if r.calls != 3 {
		t.Errorf("want 2 refusals and 1 success, then no more tries: %d calls", r.calls)
	}
	st, _ := r.d.Store.Load()
	if st.Result == nil || st.Result.Exit != ExitOK || st.Blocked != "" {
		t.Errorf("state after success: %+v", st)
	}
}

func TestRolledBackUpgradeFailsTheUnitAndWaitsForTheNextWindow(t *testing.T) {
	r := newRig(t, config.UpdateAuto)
	r.upgrades = []int{ExitRolledBack}
	r.at("2026-10-04 03:00")
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("Run = %v, want the rollback reported so that the unit shows failed", err)
	}
	r.at("2026-10-04 03:15")
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 {
		t.Errorf("a rolled-back window must not retry: %d calls", r.calls)
	}
	st, _ := r.d.Store.Load()
	if st.Blocked != "" {
		t.Errorf("a rollback leaves the node healthy and does not pause automatic upgrades: %q", st.Blocked)
	}
	r.at("2026-10-11 03:00")
	_ = r.run()
	if r.calls != 2 {
		t.Errorf("the next window tries again, got %d calls", r.calls)
	}
}

func TestOperatorFailureBlocksUntilResumed(t *testing.T) {
	for _, exit := range []int{ExitNeedsOperator, 1, -1} {
		r := newRig(t, config.UpdateAuto)
		r.upgrades = []int{exit}
		r.at("2026-10-04 03:00")
		if err := r.run(); err == nil {
			t.Fatalf("exit %d: want an error", exit)
		}
		r.at("2026-10-11 03:00")
		if err := r.run(); err != nil {
			t.Fatal(err)
		}
		if r.calls != 1 || r.logs.count("unattended_upgrade_skipped") != 1 {
			t.Fatalf("exit %d: a blocked node must skip the next window (calls %d)", exit, r.calls)
		}
		was, err := Resume(r.d.Store)
		if err != nil || was == "" {
			t.Fatalf("Resume = %q, %v", was, err)
		}
		r.upgrades = []int{ExitOK}
		r.calls = 0
		r.at("2026-10-18 03:00")
		if err := r.run(); err != nil || r.calls != 1 {
			t.Fatalf("after resume the window runs: %v, %d calls", err, r.calls)
		}
	}
}

func TestReleaseIsAnnouncedOnce(t *testing.T) {
	r := newRig(t, config.UpdateNotify)
	r.d.Update.CheckInterval = "1h"
	r.at("2026-10-06 12:00")
	for i := 0; i < 3; i++ {
		if err := r.run(); err != nil {
			t.Fatal(err)
		}
		r.now = r.now.Add(2 * time.Hour)
	}
	if n := r.logs.count("update_available"); n != 1 {
		t.Errorf("update_available logged %d times for one release", n)
	}
	r.latest = "v1.2.0"
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if n := r.logs.count("update_available"); n != 2 {
		t.Errorf("a newer release is announced again: %d", n)
	}
	r.latest = "v1.0.0" // nothing newer than the installed version
	r.d.Version = "v1.2.0"
	r.now = r.now.Add(2 * time.Hour)
	_ = r.run()
	if n := r.logs.count("update_available"); n != 2 {
		t.Errorf("a release that is not newer must not be announced: %d", n)
	}
}

func TestCheckIntervalAndFailures(t *testing.T) {
	r := newRig(t, config.UpdateNotify)
	checks := 0
	r.d.Latest = func(context.Context) (string, error) { checks++; return r.latest, r.latestEr }
	r.d.Update.CheckInterval = "6h"
	r.at("2026-10-06 12:00")
	_ = r.run()
	r.now = r.now.Add(time.Hour)
	_ = r.run()
	if checks != 1 {
		t.Errorf("a check one hour after the last one with a 6h interval: %d checks", checks)
	}
	r.now = r.now.Add(5 * time.Hour)
	_ = r.run()
	if checks != 2 {
		t.Errorf("after the interval the node asks again: %d checks", checks)
	}
	r.d.Update.CheckInterval = "off"
	r.now = r.now.Add(48 * time.Hour)
	_ = r.run()
	if checks != 2 {
		t.Errorf("checks are off: %d", checks)
	}

	// A failed check is logged and does not fail the unit.
	r = newRig(t, config.UpdateNotify)
	r.latestEr = errors.New("github is down")
	r.at("2026-10-06 12:00")
	if err := r.run(); err != nil || r.logs.count("update_check_failed") != 1 {
		t.Errorf("failed check: %v, logged %d", err, r.logs.count("update_check_failed"))
	}
}

func TestAutoModeUpgradesWithChecksOff(t *testing.T) {
	r := newRig(t, config.UpdateAuto)
	r.d.Update.CheckInterval = "off"
	r.d.Latest = func(context.Context) (string, error) { t.Fatal("asked GitHub with checks off"); return "", nil }
	r.at("2026-10-04 03:00")
	if err := r.run(); err != nil || r.calls != 1 {
		t.Fatalf("%v, %d calls", err, r.calls)
	}
}

func TestOSRebootOnlyInsideTheWindowAndOncePerWindow(t *testing.T) {
	r := newRig(t, config.UpdateNotify)
	r.d.Update.OSSecurityUpdates = true
	r.rebootOK = true

	r.at("2026-10-04 02:00") // before the window
	_ = r.run()
	r.at("2026-10-06 03:30") // a Tuesday
	_ = r.run()
	if r.reboots != 0 {
		t.Fatalf("rebooted outside the window")
	}
	r.at("2026-10-04 03:30")
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if r.reboots != 1 || r.logs.count("os_reboot") != 1 {
		t.Fatalf("want one reboot in the window, got %d", r.reboots)
	}
	// The node comes back, the marker is still there for some reason, and the window is still open.
	r.at("2026-10-04 03:50")
	_ = r.run()
	if r.reboots != 1 {
		t.Errorf("a second reboot in the same window (a reboot loop): %d", r.reboots)
	}
	r.at("2026-10-11 03:30")
	_ = r.run()
	if r.reboots != 2 {
		t.Errorf("the next window reboots again when one is due: %d", r.reboots)
	}
}

func TestOSRebootNeedsTheSettingsAndARequest(t *testing.T) {
	r := newRig(t, config.UpdateNotify)
	r.rebootOK = true
	r.at("2026-10-04 03:30")
	r.d.Update.OSSecurityUpdates = false // an existing node: not ours to reboot
	_ = r.run()
	r.d.Update.OSSecurityUpdates, r.d.Update.OSReboot = true, "never"
	_ = r.run()
	r.d.Update.OSReboot, r.rebootOK = "window", false
	_ = r.run()
	if r.reboots != 0 {
		t.Errorf("rebooted %d time(s) without being asked", r.reboots)
	}
}

func TestOSRebootWaitsWhenTheUpgradeFailed(t *testing.T) {
	r := newRig(t, config.UpdateAuto)
	r.d.Update.OSSecurityUpdates = true
	r.rebootOK = true
	r.upgrades = []int{ExitNeedsOperator}
	r.at("2026-10-04 03:00")
	_ = r.run()
	r.at("2026-10-04 03:15")
	_ = r.run()
	if r.reboots != 0 {
		t.Error("rebooted a node whose upgrade needs the operator")
	}
}

func TestUpgradeThenRebootInOneRun(t *testing.T) {
	r := newRig(t, config.UpdateAuto)
	r.d.Update.OSSecurityUpdates = true
	r.rebootOK = true
	r.at("2026-10-04 03:00")
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 || r.reboots != 1 {
		t.Errorf("upgrade then reboot: %d upgrade(s), %d reboot(s)", r.calls, r.reboots)
	}
	st, _ := r.d.Store.Load()
	if st.Window.IsZero() || st.RebootWindow.IsZero() {
		t.Errorf("both records must be saved before the machine goes down: %+v", st)
	}
}

func TestStateFile(t *testing.T) {
	dir := t.TempDir()
	s := Store{Path: filepath.Join(dir, "a", "b", "state.json")}
	if st, err := s.Load(); err != nil || st != (State{}) {
		t.Fatalf("missing file: %+v, %v", st, err)
	}
	want := State{Latest: "v1.2.3", Notified: "v1.2.3", Blocked: "x", Result: &Result{Exit: 4}}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || got.Latest != want.Latest || got.Blocked != "x" || got.Result.Exit != 4 {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if err := writeFile(s.Path, "{not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Error("a corrupt state file must be an error, not an empty state")
	}
}
