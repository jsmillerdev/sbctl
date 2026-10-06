package backup

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/config"
)

// putHistory stores a timeline history file the way PushWAL does (zstd).
func (e *testEnv) putHistory(t *testing.T, ref string, tli uint32, body string) {
	t.Helper()
	var buf bytes.Buffer
	enc, err := newEncoder(&buf, 1)
	if err != nil {
		t.Fatal(err)
	}
	enc.Write([]byte(body))
	enc.Close()
	if err := e.store.Put(context.Background(), walKey(ref, strings.ToUpper(hex8(tli))+".history"), &buf); err != nil {
		t.Fatal(err)
	}
}

// putManifest stores a manifest only (plus the WAL file it starts from); restore planning needs no data.
func (e *testEnv) putManifest(t *testing.T, ref string, tli int, start, stop string, stopTime time.Time) Manifest {
	t.Helper()
	m := Manifest{Version: manifestVersion, ID: backupID(stopTime.Add(-time.Minute)), Ref: ref, Reason: ReasonManual, Timeline: tli,
		StartLSN: start, StopLSN: stop, StartWAL: walName(uint32(tli), 1), StopWAL: walName(uint32(tli), 2),
		StartTime: stopTime.Add(-time.Minute), StopTime: stopTime, Data: dataName}
	if err := e.svc.writeManifest(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	e.fakeWAL(t, ref, m.StartWAL)
	return m
}

func TestParseLSN(t *testing.T) {
	for in, want := range map[string]uint64{"0/0": 0, "0/2000028": 0x2000028, "1/0": 1 << 32, "A/FF": 0xA<<32 | 0xFF} {
		if got, err := parseLSN(in); err != nil || got != want {
			t.Errorf("parseLSN(%q) = %x, %v, want %x", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "0/", "/1", "g/1"} {
		if _, err := parseLSN(in); err == nil {
			t.Errorf("parseLSN(%q) succeeded", in)
		}
	}
}

// After an in-place restore forks timeline 2 off timeline 1, the base backups of
// timeline 1 that were taken after the fork must not be chosen.
func TestPlanRestoreSkipsBackupsPastAFork(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	h := time.Hour
	early := e.putManifest(t, testRef, 1, "0/1000028", "0/1000100", e.now.Add(-30*h)) // before the fork
	late := e.putManifest(t, testRef, 1, "0/5000028", "0/5000100", e.now.Add(-10*h))  // after the fork
	other := e.putManifest(t, testRef, 2, "0/6000028", "0/6000100", e.now.Add(-2*h))  // on the new timeline
	e.putHistory(t, testRef, 2, "1\t0/3000000\tno recovery target specified\n")

	// Newest eligible backup before "now" is the one on timeline 2.
	p, err := e.svc.PlanRestore(ctx, testRef, e.now, "")
	if err != nil || p.Manifest.ID != other.ID {
		t.Fatalf("plan for now = %+v, %v; want %s", p, err, other.ID)
	}
	// A target between the fork-crossing backup and the new one must fall back to the
	// pre-fork backup, not pick the unusable timeline-1 backup.
	p, err = e.svc.PlanRestore(ctx, testRef, e.now.Add(-5*h), "")
	if err != nil || p.Manifest.ID != early.ID {
		t.Fatalf("plan = %+v, %v; want %s (not %s)", p, err, early.ID, late.ID)
	}
	// Pinning the unusable one is an error that says why.
	if _, err := e.svc.PlanRestore(ctx, testRef, e.now, late.ID); err == nil || !strings.Contains(err.Error(), "not on the history") {
		t.Fatalf("pinned backup past the fork = %v", err)
	}
	// A backup that ends before the fork stays usable when pinned.
	if p, err := e.svc.PlanRestore(ctx, testRef, e.now, early.ID); err != nil || p.Manifest.ID != early.ID {
		t.Fatalf("pinned early backup = %+v, %v", p, err)
	}
}

func TestPlanRestoreNoBackupOnHistory(t *testing.T) {
	e := newTestEnv(t)
	e.putManifest(t, testRef, 1, "0/5000028", "0/5000100", e.now.Add(-10*time.Hour))
	e.putHistory(t, testRef, 2, "1\t0/3000000\tx\n")
	if _, err := e.svc.PlanRestore(context.Background(), testRef, e.now, ""); err == nil || !strings.Contains(err.Error(), "take a new base backup") {
		t.Fatalf("err = %v", err)
	}
}

// Without any history file every backup of the project is on the one timeline.
func TestPlanRestoreWithoutHistoryAcceptsAll(t *testing.T) {
	e := newTestEnv(t)
	m := e.putManifest(t, testRef, 1, "0/5000028", "0/5000100", e.now.Add(-time.Hour))
	if p, err := e.svc.PlanRestore(context.Background(), testRef, e.now, ""); err != nil || p.Manifest.ID != m.ID {
		t.Fatalf("plan = %+v, %v", p, err)
	}
}

func TestPlanRestoreLatestAndBackupIgnoreTime(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	old := e.putManifest(t, testRef, 1, "0/1000028", "0/1000100", e.now.Add(-48*time.Hour))
	last := e.putManifest(t, testRef, 1, "0/2000028", "0/2000100", e.now.Add(-24*time.Hour))
	// A time target before every backup fails, but "latest" and "backup" have no time.
	zero := time.Time{}
	if _, err := e.svc.PlanRestore(ctx, testRef, zero, ""); err == nil {
		t.Fatal("zero time target must fail")
	}
	p, err := e.svc.PlanRestoreWith(ctx, testRef, zero, RestoreOptions{Latest: true})
	if err != nil || p.Mode != RestoreToLatest || p.Manifest.ID != last.ID {
		t.Fatalf("latest = %+v, %v", p, err)
	}
	p, err = e.svc.PlanRestoreWith(ctx, testRef, zero, RestoreOptions{ToBackup: true, BackupID: old.ID})
	if err != nil || p.Mode != RestoreToBackup || p.Manifest.ID != old.ID {
		t.Fatalf("backup = %+v, %v", p, err)
	}
	if _, err := e.svc.PlanRestoreWith(ctx, testRef, zero, RestoreOptions{Latest: true, ToBackup: true}); err == nil {
		t.Fatal("Latest and ToBackup together must fail")
	}
}

func TestRecoveryConfByMode(t *testing.T) {
	e := newTestEnv(t)
	m := Manifest{ID: "20261006T000000Z-aaaaaa"}
	target := e.now
	for _, tc := range []struct {
		mode      string
		has, lack []string
	}{
		{RestoreToTime, []string{"recovery_target_time = '2026-10-06 12:00:00+00'", "recovery_target_action = 'promote'", "recovery_target_timeline = 'latest'"}, []string{"recovery_target = "}},
		{RestoreToLatest, []string{"restore_command = ", "recovery_target_timeline = 'latest'", "end of the archive"}, []string{"recovery_target_time =", "recovery_target = ", "recovery_target_action"}},
		{RestoreToBackup, []string{"recovery_target = 'immediate'", "recovery_target_action = 'promote'"}, []string{"recovery_target_time ="}},
	} {
		conf := e.svc.recoveryConf(&RestorePlan{Source: testRef, TargetRef: testRef2, Mode: tc.mode, Target: target, Manifest: m})
		for _, w := range append(tc.has, "archive_command = ", "wal push --ref "+testRef2, "wal fetch --ref "+testRef) {
			if !strings.Contains(conf, w) {
				t.Errorf("%s: conf lacks %q:\n%s", tc.mode, w, conf)
			}
		}
		for _, w := range tc.lack {
			if strings.Contains(conf, w) {
				t.Errorf("%s: conf has %q:\n%s", tc.mode, w, conf)
			}
		}
	}
	for _, g := range []string{"restore_command", "recovery_target", "recovery_target_time", "recovery_target_action", "recovery_target_timeline"} {
		found := false
		for _, r := range recoveryGUCs {
			found = found || r == g
		}
		if !found {
			t.Errorf("%s is written by the seeder but not reset afterwards", g)
		}
	}
}

func TestRestoreRefusesSystemAndReusedRef(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.addProject(t, testRef)
	e.storeBase(t, testRef, fakeDataDir(t), e.now.Add(-time.Hour), nil)
	fm := &fakeManager{e: e, dataDir: filepath.Join(t.TempDir(), "restored")}
	e.svc.opt.Manager = fm

	for _, as := range []string{"", testRef2} {
		if _, err := e.svc.RestoreWith(ctx, config.SystemRef, e.now, as, RestoreOptions{Force: true}); err == nil || !strings.Contains(err.Error(), "system cluster") {
			t.Errorf("restore of system (as=%q) = %v", as, err)
		}
	}

	// testRef2 belonged to a deleted project: its archive is still there.
	e.fakeWAL(t, testRef2, walName(1, 7))
	_, err := e.svc.RestoreWith(ctx, testRef, e.now, testRef2, RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "already holds data") || !strings.Contains(err.Error(), "wal") {
		t.Fatalf("restore under a ref with a leftover archive = %v", err)
	}
	if len(fm.created) != 0 {
		t.Fatal("Create must not run when the ref has a leftover archive")
	}
}

func TestNewBackupIDIsUnique(t *testing.T) {
	now := time.Date(2026, 10, 6, 3, 0, 0, 0, time.UTC)
	a, b := newBackupID(now), newBackupID(now)
	if a == b {
		t.Fatalf("two ids in the same second collide: %s", a)
	}
	if !strings.HasPrefix(a, "20261006T030000Z-") {
		t.Errorf("id = %s", a)
	}
}

// The recovery settings must stay until the cluster has left recovery.
func TestFinishRecoveryWaitsForPromotion(t *testing.T) {
	e := newTestEnv(t)
	e.svc.opt.Access = &portAccess{ports: map[string]int{}}
	e.svc.opt.RecoveryPoll = time.Millisecond
	var probes, altered atomic.Int32
	e.svc.probe = func(context.Context, string) (bool, error) {
		n := probes.Add(1)
		switch {
		case n == 1:
			return true, errors.New("connection refused") // server still starting
		case n < 5:
			return true, nil
		}
		return false, nil
	}
	e.svc.alter = func(_ context.Context, _ string, gucs []string) error {
		if probes.Load() < 5 {
			t.Errorf("settings reset after only %d probes: recovery was still running", probes.Load())
		}
		altered.Add(1)
		if len(gucs) == 0 {
			t.Error("nothing to reset")
		}
		return nil
	}
	if err := e.svc.finishRecovery(context.Background(), testRef, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if altered.Load() != 1 {
		t.Fatalf("reset ran %d times", altered.Load())
	}
}

func TestResetRecoverySettingsLeavesThemWhenRecoveryDoesNotFinish(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.svc.opt.Access = &portAccess{ports: map[string]int{}}
	e.svc.opt.RecoveryPoll = time.Millisecond
	e.svc.opt.RecoveryTimeout = 50 * time.Millisecond
	e.svc.probe = func(context.Context, string) (bool, error) { return true, nil }
	e.svc.alter = func(context.Context, string, []string) error {
		t.Error("recovery settings were reset while the cluster was still in recovery")
		return nil
	}
	if e.svc.resetRecoverySettings(ctx, testRef) {
		t.Fatal("reported success")
	}
	evs, err := e.reg.ListEvents(ctx, testRef, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != "restore.cleanup_pending" {
		t.Fatalf("events = %+v", evs)
	}
	// FinishRestore uses the same wait; once recovery is over it clears the settings.
	var cleared bool
	e.svc.probe = func(context.Context, string) (bool, error) { return false, nil }
	e.svc.alter = func(context.Context, string, []string) error { cleared = true; return nil }
	if err := e.svc.FinishRestore(ctx, testRef); err != nil || !cleared {
		t.Fatalf("FinishRestore = %v, cleared %v", err, cleared)
	}
}

func TestResetRecoverySettingsWithoutAccessIsRecordedNotFatal(t *testing.T) {
	e := newTestEnv(t)
	if e.svc.resetRecoverySettings(context.Background(), testRef) {
		t.Fatal("no access cannot succeed")
	}
	evs, _ := e.reg.ListEvents(context.Background(), testRef, 10)
	if len(evs) != 1 || evs[0].Kind != "restore.cleanup_failed" {
		t.Fatalf("events = %+v", evs)
	}
}
