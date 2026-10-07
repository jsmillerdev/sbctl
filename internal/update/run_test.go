package update

import (
	"context"
	"errors"
	"log/slog"
	"os"
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
	// blocker is what RebootBlocker answers; upgradeTakes is how far Upgrade moves the clock.
	blocker      string
	upgradeTakes time.Duration
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
			r.now = r.now.Add(r.upgradeTakes)
			if r.upgrades[i] < 0 {
				return -1, errors.New("exec failed")
			}
			return r.upgrades[i], nil
		},
		RebootRequired: func(context.Context) bool { return r.rebootOK },
		RebootBlocker:  func(context.Context) string { return r.blocker },
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
	r.latest = "v1.2.0" // a newer release: worth another try
	_ = r.run()
	if r.calls != 2 {
		t.Errorf("the next window tries a newer release, got %d calls", r.calls)
	}
}

// A release that rolled back is not tried again every week: the node waits for a newer one.
func TestRolledBackReleaseIsNotRetriedUntilANewerOneAppears(t *testing.T) {
	r := newRig(t, config.UpdateAuto)
	r.upgrades = []int{ExitRolledBack}
	r.at("2026-10-04 03:00")
	_ = r.run()
	if st, _ := r.d.Store.Load(); st.RolledBack != "v1.1.0" {
		t.Fatalf("the rolled-back release is remembered: %q", st.RolledBack)
	}
	for _, when := range []string{"2026-10-11 03:00", "2026-10-18 03:00", "2026-10-18 03:15"} {
		r.at(when)
		if err := r.run(); err != nil {
			t.Fatal(err)
		}
	}
	if r.calls != 1 {
		t.Fatalf("the same release was tried again: %d calls", r.calls)
	}
	if n := r.logs.count("unattended_upgrade_skipped"); n != 2 {
		t.Errorf("want one skip event per window (2), got %d", n)
	}
	// GitHub out of reach: the node cannot tell the release is new, so it keeps waiting.
	r.latestEr = errors.New("github is down")
	r.at("2026-10-25 03:00")
	_ = r.run()
	if r.calls != 1 {
		t.Errorf("tried without knowing that the release changed: %d calls", r.calls)
	}
	// resume lifts the skip for the same release.
	r.latestEr = nil
	was, err := Resume(r.d.Store)
	if err != nil || !strings.Contains(was, "v1.1.0") {
		t.Fatalf("Resume = %q, %v", was, err)
	}
	r.upgrades = []int{ExitOK}
	r.at("2026-11-01 03:00")
	_ = r.run()
	if r.calls != 2 {
		t.Errorf("after resume the window tries again: %d calls", r.calls)
	}
	if st, _ := r.d.Store.Load(); st.RolledBack != "" {
		t.Errorf("a successful upgrade forgets the rollback: %q", st.RolledBack)
	}
}

// The last stretch of the window starts no upgrade: it would run its outage past the close.
func TestNoUpgradeStartsInTheLastHourOfTheWindow(t *testing.T) {
	r := newRig(t, config.UpdateAuto) // Sun 03:00-05:00: the cutoff is 60 min
	r.at("2026-10-04 04:01")
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 || r.logs.count("unattended_upgrade_skipped") != 1 {
		t.Fatalf("started with 59 min left: %d calls", r.calls)
	}
	r.at("2026-10-04 04:00") // exactly the cutoff: still allowed
	_ = r.run()
	if r.calls != 1 {
		t.Errorf("an upgrade at the cutoff is allowed: %d calls", r.calls)
	}
	// The next window starts on time.
	r.at("2026-10-11 03:00")
	_ = r.run()
	if r.calls != 2 {
		t.Errorf("%d calls", r.calls)
	}
	// A short window still has a time to start in: half of it.
	r = newRig(t, config.UpdateAuto)
	r.d.Update.Window = "Sun 03:00-03:30"
	if got := StartCutoff(mustWindow(t, "Sun 03:00-03:30")); got != 15*time.Minute {
		t.Errorf("StartCutoff of 30 min = %s", got)
	}
	r.at("2026-10-04 03:10")
	_ = r.run()
	if r.calls != 1 {
		t.Errorf("a short window must not be shut for good: %d calls", r.calls)
	}
}

func mustWindow(t *testing.T, s string) config.Window {
	t.Helper()
	w, err := config.ParseWindow(s)
	if err != nil {
		t.Fatal(err)
	}
	return w
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

// The daemon announces releases (internal/health, internal/alerts); a pass of `update run` never
// logs update_available, whatever Latest says.
func TestRunDoesNotAnnounceReleases(t *testing.T) {
	r := newRig(t, config.UpdateNotify)
	r.at("2026-10-06 12:00")
	r.d.Latest = func(context.Context) (string, error) {
		t.Fatal("asked for the latest release in notify mode")
		return "", nil
	}
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if r.logs.count("update_available") != 0 || r.logs.count("update_check_failed") != 0 {
		t.Error("update run must leave the release check to the daemon")
	}
}

// Latest is asked only to tell whether a rolled-back release has a successor, so a node whose
// checks are off upgrades without it.
func TestAutoModeUpgradesWithoutAskingForTheLatestRelease(t *testing.T) {
	r := newRig(t, config.UpdateAuto)
	r.d.Latest = func(context.Context) (string, error) { t.Fatal("asked for the latest release"); return "", nil }
	r.at("2026-10-04 03:00")
	if err := r.run(); err != nil || r.calls != 1 {
		t.Fatalf("%v, %d calls", err, r.calls)
	}
}

// An upgrade that began and never reported (the node lost power, the process was killed) is not
// started again by the next tick of the window: a person looks first.
func TestInterruptedUpgradePausesInsteadOfRunningAgain(t *testing.T) {
	r := newRig(t, config.UpdateAuto)
	r.at("2026-10-04 03:00")
	died := r.d.Upgrade
	r.d.Upgrade = func(ctx context.Context) (int, error) {
		// What the disk holds when the node dies in the middle of the upgrade.
		st, err := r.d.Store.Load()
		if err != nil || st.InProgress == nil || st.InProgress.Version != "v1.0.0" || !st.InProgress.Window.Equal(r.now) {
			t.Errorf("the record must be saved before the upgrade starts: %+v, %v", st.InProgress, err)
		}
		panic("power cut")
	}
	func() {
		defer func() { _ = recover() }()
		_ = r.run()
	}()
	r.d.Upgrade = died

	r.at("2026-10-04 03:15") // the next tick of the same window
	err := r.run()
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("Run = %v, want the interruption reported", err)
	}
	if r.calls != 0 || r.logs.count("unattended_upgrade_interrupted") != 1 {
		t.Fatalf("the upgrade ran again after a crash: %d calls", r.calls)
	}
	st, _ := r.d.Store.Load()
	if st.InProgress != nil || !strings.Contains(st.Blocked, "did not report how it ended") {
		t.Errorf("state: %+v", st)
	}
	// The pause holds into later windows, and no reboot happens meanwhile.
	r.d.Update.OSSecurityUpdates, r.rebootOK = true, true
	r.at("2026-10-11 03:00")
	if err := r.run(); err != nil || r.calls != 0 || r.reboots != 0 || r.logs.count("unattended_upgrade_skipped") != 1 {
		t.Fatalf("a paused node: %v, %d calls, %d reboots", err, r.calls, r.reboots)
	}
	if was, err := Resume(r.d.Store); err != nil || was == "" {
		t.Fatalf("Resume = %q, %v", was, err)
	}
	r.at("2026-10-18 03:00")
	if err := r.run(); err != nil || r.calls != 1 {
		t.Fatalf("after resume the window runs: %v, %d calls", err, r.calls)
	}
	if st, _ := r.d.Store.Load(); st.InProgress != nil || st.Result == nil {
		t.Errorf("a finished upgrade leaves no in-progress record: %+v", st)
	}
}

// A refused or failed upgrade clears the record too: only a run that never returned leaves it.
func TestEveryExitClearsTheInProgressRecord(t *testing.T) {
	for _, exit := range []int{ExitOK, ExitRefused, ExitRolledBack, ExitNeedsOperator, -1} {
		r := newRig(t, config.UpdateAuto)
		r.upgrades = []int{exit}
		r.at("2026-10-04 03:00")
		_ = r.run()
		if st, _ := r.d.Store.Load(); st.InProgress != nil {
			t.Errorf("exit %d left an in-progress record", exit)
		}
	}
}

func TestStoreLockKeepsTwoPassesApart(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "state.json")}
	release, err := s.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(); !errors.Is(err, ErrBusy) {
		t.Errorf("a second pass: %v, want ErrBusy", err)
	}
	release()
	again, err := s.Lock()
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
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

// An upgrade that outlasts the window must not be followed by a reboot after the close.
func TestNoRebootAfterTheWindowClosedDuringTheUpgrade(t *testing.T) {
	r := newRig(t, config.UpdateAuto)
	r.d.Update.OSSecurityUpdates = true
	r.rebootOK = true
	r.upgradeTakes = 2 * time.Hour // 03:30 + 2 h = 05:30, after the 05:00 close
	r.at("2026-10-04 03:30")
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 {
		t.Fatalf("the upgrade ran %d times", r.calls)
	}
	if r.reboots != 0 || r.logs.count("os_reboot_deferred") != 1 {
		t.Fatalf("rebooted %d time(s) after the window closed", r.reboots)
	}
	if st, _ := r.d.Store.Load(); !st.RebootWindow.IsZero() {
		t.Errorf("a deferred reboot must not use up the window's reboot: %+v", st)
	}
	// The next window reboots.
	r.upgradeTakes = 0
	r.at("2026-10-11 03:30")
	_ = r.run()
	if r.reboots != 1 {
		t.Errorf("the next window reboots: %d", r.reboots)
	}
}

// An upgrade that ends inside the window still allows the reboot, if enough of the window is left.
func TestRebootAfterAShortUpgradeInTheSameRun(t *testing.T) {
	r := newRig(t, config.UpdateAuto)
	r.d.Update.OSSecurityUpdates = true
	r.rebootOK = true
	r.upgradeTakes = 30 * time.Minute // 03:00 -> 03:30
	r.at("2026-10-04 03:00")
	_ = r.run()
	if r.reboots != 1 {
		t.Errorf("reboots: %d", r.reboots)
	}
}

func TestNoRebootInTheLastMinutesOfTheWindow(t *testing.T) {
	r := newRig(t, config.UpdateNotify)
	r.d.Update.OSSecurityUpdates = true
	r.rebootOK = true
	r.at("2026-10-04 04:46") // 14 min left, the cutoff is 15
	_ = r.run()
	if r.reboots != 0 || r.logs.count("os_reboot_deferred") != 1 {
		t.Fatalf("rebooted with 14 min left: %d", r.reboots)
	}
	r.at("2026-10-04 04:45")
	_ = r.run()
	if r.reboots != 1 {
		t.Errorf("a reboot at the cutoff is allowed: %d", r.reboots)
	}
}

func TestNoRebootAfterARefusedUpgrade(t *testing.T) {
	r := newRig(t, config.UpdateAuto)
	r.d.Update.OSSecurityUpdates = true
	r.rebootOK = true
	r.upgrades = []int{ExitRefused, ExitOK}
	r.at("2026-10-04 03:00")
	_ = r.run()
	if r.reboots != 0 || r.logs.count("os_reboot_deferred") != 1 {
		t.Fatalf("the node rebooted in the pass in which its upgrade was refused: %d", r.reboots)
	}
	// The next tick's upgrade goes through and the reboot follows in the same window.
	r.at("2026-10-04 03:15")
	_ = r.run()
	if r.calls != 2 || r.reboots != 1 {
		t.Errorf("calls %d, reboots %d", r.calls, r.reboots)
	}
}

func TestNoRebootWhileTheNodeIsUnhealthyOrBusy(t *testing.T) {
	r := newRig(t, config.UpdateNotify)
	r.d.Update.OSSecurityUpdates = true
	r.rebootOK = true
	r.blocker = "a base backup is running: supavise-basebackup@abc.service"
	r.at("2026-10-04 03:00")
	_ = r.run()
	r.at("2026-10-04 03:15")
	_ = r.run()
	if r.reboots != 0 || r.logs.count("os_reboot_deferred") != 2 {
		t.Fatalf("rebooted a busy node: %d reboot(s), %d deferrals", r.reboots, r.logs.count("os_reboot_deferred"))
	}
	r.blocker = ""
	r.at("2026-10-04 03:30")
	_ = r.run()
	if r.reboots != 1 {
		t.Errorf("the reboot follows once the node is quiet: %d", r.reboots)
	}
}

func TestNoGateNoReboot(t *testing.T) {
	r := newRig(t, config.UpdateNotify)
	r.d.Update.OSSecurityUpdates = true
	r.rebootOK = true
	r.d.RebootBlocker = nil
	r.at("2026-10-04 03:00")
	_ = r.run()
	if r.reboots != 0 {
		t.Error("a reboot without a health gate")
	}
}

func TestStateFile(t *testing.T) {
	dir := t.TempDir()
	s := Store{Path: filepath.Join(dir, "a", "b", "state.json")}
	if st, err := s.Load(); err != nil || st != (State{}) {
		t.Fatalf("missing file: %+v, %v", st, err)
	}
	want := State{Blocked: "x", Result: &Result{Exit: 4}, InProgress: &Progress{Version: "v1"}}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || got.InProgress == nil || got.InProgress.Version != "v1" || got.Blocked != "x" || got.Result.Exit != 4 {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if err := writeFile(s.Path, "{not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Error("a corrupt state file must be an error, not an empty state")
	}
}

func TestStatePathIsOutsideTheStateDirectory(t *testing.T) {
	if got := StatePath("/var/lib/supavise"); got != "/var/lib/supavise-upgrade/state.json" {
		t.Errorf("StatePath = %s", got)
	}
	if got := StatePath("/data/sv/"); got != "/data/sv-upgrade/state.json" {
		t.Errorf("StatePath = %s", got)
	}
}

// The directory is trusted only when it is a plain directory of the user running the update that
// others cannot write; a symlink or a lax mode is refused.
func TestStoreRefusesAnUntrustedDirectory(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := (Store{Path: filepath.Join(link, "state.json")}).Save(State{Blocked: "v1"}); err == nil || !strings.Contains(err.Error(), "not a plain directory") {
		t.Errorf("Save through a symlinked directory: %v", err)
	}
	lax := filepath.Join(base, "lax")
	if err := os.Mkdir(lax, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lax, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := (Store{Path: filepath.Join(lax, "state.json")}).Save(State{}); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Errorf("Save into a group-writable directory: %v", err)
	}
	good := Store{Path: filepath.Join(base, "good", "state.json")}
	if err := good.Save(State{Blocked: "v1"}); err != nil {
		t.Errorf("a directory the store made itself: %v", err)
	}
}

func TestProjectBlocker(t *testing.T) {
	cases := []struct{ name, json, want string }{
		{"healthy", `[{"ref":"a","status":"ACTIVE_HEALTHY"},{"ref":"b","status":"INACTIVE"},{"ref":"c","status":"REMOVED"}]`, ""},
		{"none", `[]`, ""},
		{"an operation in flight", `[{"ref":"a","status":"ACTIVE_HEALTHY"},{"ref":"b","status":"UPGRADING"}]`, "lifecycle operation is in flight: b UPGRADING"},
		{"a restore", `[{"ref":"b","status":"RESTORING"}]`, "in flight"},
		{"unhealthy", `[{"ref":"a","status":"ACTIVE_UNHEALTHY"}]`, "not healthy: a ACTIVE_UNHEALTHY"},
		{"busy beats unhealthy", `[{"ref":"a","status":"ACTIVE_UNHEALTHY"},{"ref":"b","status":"RESTARTING"}]`, "in flight"},
		{"a status nobody listed", `[{"ref":"a","status":"PAUSE_FAILED"},{"ref":"b","status":"RESIZING"}]`, "not healthy: a PAUSE_FAILED, b RESIZING"},
		{"garbage", `not json`, "cannot read the project list"},
	}
	for _, c := range cases {
		got := ProjectBlocker([]byte(c.json))
		if c.want == "" && got != "" || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestUnitListParsing(t *testing.T) {
	out := "● supavise-basebackup@abc.service loaded failed failed Base backup\n  supavise-upgrade.service loaded failed failed Supavise\n\nsupavise-gotrue@abc.service loaded failed failed GoTrue\n"
	if got := strings.Join(FailedUnits(out), ","); got != "supavise-basebackup@abc.service,supavise-gotrue@abc.service" {
		t.Errorf("FailedUnits = %s", got)
	}
	if got := UnitNames(""); len(got) != 0 {
		t.Errorf("UnitNames of nothing = %v", got)
	}
}
