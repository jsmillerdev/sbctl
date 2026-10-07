package backup

import (
	"bytes"
	"context"
	"errors"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"os"
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
	if done, err := e.svc.resetRecoverySettings(ctx, testRef); done || err != nil {
		t.Fatalf("slow recovery = %v, %v: not done and not an error", done, err)
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
	if done, err := e.svc.resetRecoverySettings(context.Background(), testRef); done || err != nil {
		t.Fatalf("no access = %v, %v", done, err)
	}
	evs, _ := e.reg.ListEvents(context.Background(), testRef, 10)
	if len(evs) != 1 || evs[0].Kind != "restore.cleanup_failed" {
		t.Fatalf("events = %+v", evs)
	}
}

// A cluster that was replaying WAL and then stops answering has died of a fatal recovery
// error (no commit after the target in the archive): the restore must fail, not succeed.
func TestFinishRecoveryFailsWhenClusterDiesDuringRecovery(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.svc.opt.Access = &portAccess{ports: map[string]int{}}
	e.svc.opt.RecoveryPoll = time.Millisecond
	e.svc.opt.RecoveryFailGrace = 20 * time.Millisecond
	var probes atomic.Int32
	e.svc.probe = func(context.Context, string) (bool, error) {
		if probes.Add(1) <= 3 {
			return true, nil // hot standby accepts connections while replaying
		}
		return true, errors.New("connection refused") // then FATAL and shutdown
	}
	e.svc.alter = func(context.Context, string, []string) error {
		t.Error("recovery settings reset on a dead cluster")
		return nil
	}
	done, err := e.svc.resetRecoverySettings(ctx, testRef)
	if done || !errors.Is(err, errRecoveryFailed) {
		t.Fatalf("dead cluster = %v, %v; want errRecoveryFailed", done, err)
	}
	evs, _ := e.reg.ListEvents(ctx, testRef, 10)
	if len(evs) != 1 || evs[0].Kind != "restore.recovery_failed" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestFinishRecoveryFailsWhenClusterNeverAnswers(t *testing.T) {
	e := newTestEnv(t)
	e.svc.opt.Access = &portAccess{ports: map[string]int{}}
	e.svc.opt.RecoveryPoll = time.Millisecond
	e.svc.opt.RecoveryTimeout = 30 * time.Millisecond
	e.svc.probe = func(context.Context, string) (bool, error) { return true, errors.New("connection refused") }
	if err := e.svc.finishRecovery(context.Background(), testRef, 30*time.Millisecond); !errors.Is(err, errRecoveryFailed) {
		t.Fatalf("never answering = %v; want errRecoveryFailed", err)
	}
}

// Restoring as a new project must report a failed recovery as an error.
func TestRestoreAsNewReportsFailedRecovery(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.addProject(t, testRef)
	e.storeBase(t, testRef, fakeDataDir(t), e.now.Add(-time.Hour), nil)
	e.svc.opt.Manager = &fakeManager{e: e, dataDir: filepath.Join(t.TempDir(), "restored")}
	e.svc.opt.Access = &portAccess{ports: map[string]int{}}
	e.svc.opt.RecoveryPoll = time.Millisecond
	e.svc.opt.RecoveryFailGrace = time.Millisecond
	var n atomic.Int32
	e.svc.probe = func(context.Context, string) (bool, error) {
		if n.Add(1) == 1 {
			return true, nil
		}
		return true, errors.New("connection refused")
	}
	if p, err := e.svc.RestoreWith(ctx, testRef, e.now, "bcdefghijklmnopqrstu", RestoreOptions{}); err == nil || !errors.Is(err, errRecoveryFailed) {
		t.Fatalf("restore with a dying cluster = %v, %v; want an error wrapping errRecoveryFailed", p, err)
	}
	evs, _ := e.reg.ListEvents(ctx, testRef, 10)
	var failed bool
	for _, ev := range evs {
		if ev.Kind == "restore.completed" {
			t.Fatal("a failed restore recorded restore.completed")
		}
		failed = failed || ev.Kind == "restore.failed"
	}
	if !failed {
		t.Fatalf("no restore.failed event: %+v", evs)
	}
}

// An in-place restore whose recovery dies puts the original data back.
func TestRestoreInPlaceRollsBackWhenRecoveryFails(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.addProject(t, testRef)
	e.storeBase(t, testRef, fakeDataDir(t), e.now.Add(-time.Hour), nil)
	dd := e.svc.opt.DataDir(testRef)
	writeFile(t, filepath.Join(dd, "PG_VERSION"), []byte("old"))
	fm := &fakeManager{e: e}
	e.svc.opt.Manager = fm
	e.svc.opt.Access = &portAccess{ports: map[string]int{}}
	e.svc.opt.RecoveryPoll = time.Millisecond
	e.svc.opt.RecoveryFailGrace = time.Millisecond
	var n atomic.Int32
	e.svc.probe = func(context.Context, string) (bool, error) {
		if n.Add(1) == 1 {
			return true, nil
		}
		return true, errors.New("connection refused")
	}
	if _, err := e.svc.RestoreWith(ctx, testRef, e.now, "", RestoreOptions{Force: true}); !errors.Is(err, errRecoveryFailed) {
		t.Fatalf("in-place restore with a dying cluster = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dd, "PG_VERSION")); string(b) != "old" {
		t.Fatalf("original data not back in place: %q", b)
	}
	if failed, _ := filepath.Glob(dd + ".failed-restore-*"); len(failed) != 1 {
		t.Fatalf("failed restore not kept for inspection: %v", failed)
	}
}

func TestRestoreInPlaceRefusesWhatIsNotADataDirectory(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.addProject(t, testRef)
	e.storeBase(t, testRef, fakeDataDir(t), e.now.Add(-time.Hour), nil)
	dd := e.svc.opt.DataDir(testRef)
	writeFile(t, filepath.Join(dd, "pgdata", "PG_VERSION"), []byte("old")) // PGDATA one level down
	fm := &fakeManager{e: e}
	e.svc.opt.Manager = fm
	if _, err := e.svc.RestoreWith(ctx, testRef, e.now, "", RestoreOptions{Force: true}); err == nil || !strings.Contains(err.Error(), "PG_VERSION") {
		t.Fatalf("restore over a directory without PG_VERSION = %v", err)
	}
	if len(fm.paused) != 0 {
		t.Fatal("nothing may be stopped before the directory is checked")
	}
	if _, err := os.Stat(filepath.Join(dd, "pgdata", "PG_VERSION")); err != nil {
		t.Fatalf("the directory was touched: %v", err)
	}
}

// The restored cluster dies before the lifecycle readiness check sees it, so Resume itself
// fails: the original data must come back and the project must be running on it, not left
// dead with the real data in <dir>.pre-restore-<time>.
func TestRestoreInPlaceRollsBackWhenResumeFails(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.addProject(t, testRef)
	e.storeBase(t, testRef, fakeDataDir(t), e.now.Add(-time.Hour), nil)
	dd := e.svc.opt.DataDir(testRef)
	writeFile(t, filepath.Join(dd, "PG_VERSION"), []byte("old"))
	fm := &fakeManager{e: e, failResume: 1}
	e.svc.opt.Manager = fm
	_, err := e.svc.RestoreWith(ctx, testRef, e.now, "", RestoreOptions{Force: true})
	if err == nil || !strings.Contains(err.Error(), "original data is back in place") {
		t.Fatalf("in-place restore with a failing start = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dd, "PG_VERSION")); string(b) != "old" {
		t.Fatalf("original data not back in place: %q", b)
	}
	if failed, _ := filepath.Glob(dd + ".failed-restore-*"); len(failed) != 1 {
		t.Fatalf("failed restore not kept for inspection: %v", failed)
	}
	if aside, _ := filepath.Glob(dd + ".pre-restore-*"); len(aside) != 0 {
		t.Fatalf("the moved-aside directory is still there: %v", aside)
	}
	if fm.resumeCalls != 2 {
		t.Fatalf("Resume calls = %d, want 2 (the failed one, then the original)", fm.resumeCalls)
	}
}

// A restore-as-new clone whose recovery failed holds nothing to back up. Its delete runs
// FinalBackup and aborts when that fails, so FinalBackup must say so with the sentinel.
func TestFinalBackupOfAClonethatNeverRecoveredIsNotRestorable(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.addProject(t, testRef)
	for _, kind := range []string{"restore.recovery_failed", "restore.cleanup_pending"} {
		_ = e.reg.AppendEvent(ctx, testRef, kind, nil)
		e.svc.opt.Access = &portAccess{ports: map[string]int{}}
		if _, err := e.svc.FinalBackup(ctx, testRef); !errors.Is(err, lifecycle.ErrNoRestorableState) {
			t.Fatalf("after %s: FinalBackup = %v, want ErrNoRestorableState", kind, err)
		}
		// Recovery finishing later supersedes the failure.
		_ = e.reg.AppendEvent(ctx, testRef, eventRecoveryFinished, nil)
		if _, err := e.svc.FinalBackup(ctx, testRef); errors.Is(err, lifecycle.ErrNoRestorableState) {
			t.Fatalf("after recovery finished: FinalBackup = %v, must not claim there is nothing to restore", err)
		}
		e.reg.DeleteProject(ctx, testRef)
		e.addProject(t, testRef)
	}
}

// A restore-as-new clone tagged restore.cleanup_pending (recovery outlasted
// RecoveryTimeout, restore reported success) promotes on its own later. The live cluster,
// not the events, decides: out of recovery it is backed up, still replaying it is not.
func TestCleanupPendingCloneThatPromotedCanBeBackedUp(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.addProject(t, testRef)
	_ = e.reg.AppendEvent(ctx, testRef, "restore.cleanup_pending", nil)
	e.svc.opt.Access = &portAccess{ports: map[string]int{}}

	// Still replaying: nothing to back up, and the delete may skip the final backup.
	e.svc.probe = func(context.Context, string) (bool, error) { return true, nil }
	e.svc.alter = func(context.Context, string, []string) error {
		t.Fatal("recovery settings cleared during recovery")
		return nil
	}
	if _, err := e.svc.FinalBackup(ctx, testRef); !errors.Is(err, lifecycle.ErrNoRestorableState) {
		t.Fatalf("clone still in recovery: FinalBackup = %v, want ErrNoRestorableState", err)
	}

	// Promoted: the backup is attempted (it fails later, there is no real cluster behind
	// the fake probe), the settings are cleared and recovery_finished is recorded.
	var cleared []string
	e.svc.probe = func(context.Context, string) (bool, error) { return false, nil }
	e.svc.alter = func(_ context.Context, _ string, g []string) error { cleared = g; return nil }
	if _, err := e.svc.FinalBackup(ctx, testRef); errors.Is(err, lifecycle.ErrNoRestorableState) {
		t.Fatalf("promoted clone: FinalBackup = %v, must not skip the final backup", err)
	}
	if len(cleared) == 0 {
		t.Fatal("recovery settings were not cleared on a cluster out of recovery")
	}
	if st := e.svc.restoreState(ctx, testRef); st != restoreStateFinished {
		t.Fatalf("restore state = %q, want finished", st)
	}
}

func TestFinishPendingRestores(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.addProject(t, testRef)
	e.svc.opt.Access = &portAccess{ports: map[string]int{}}
	e.svc.opt.RecoveryPoll = time.Millisecond
	_ = e.reg.AppendEvent(ctx, testRef, "restore.cleanup_pending", nil)
	if got := e.svc.PendingRestores(ctx); len(got) != 1 || got[0] != testRef {
		t.Fatalf("PendingRestores = %v", got)
	}
	n := 0
	e.svc.probe = func(context.Context, string) (bool, error) { n++; return n < 3, nil }
	e.svc.alter = func(context.Context, string, []string) error { return nil }
	if got := e.svc.FinishPendingRestores(ctx); len(got) != 1 {
		t.Fatalf("FinishPendingRestores = %v", got)
	}
	if got := e.svc.PendingRestores(ctx); len(got) != 0 {
		t.Fatalf("still pending after finish: %v", got)
	}
}
