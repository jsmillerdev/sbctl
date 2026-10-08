package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/lifecycle"
)

func TestRestoreWindowWithoutBackups(t *testing.T) {
	e := newTestEnv(t)
	w, err := e.svc.RestoreWindow(context.Background(), testRef, true)
	if err != nil || len(w.Backups) != 0 || !w.Earliest.IsZero() || !w.Latest.IsZero() {
		t.Fatalf("window = %+v, %v", w, err)
	}
}

func TestRestoreWindowOfARunningProject(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	h := time.Hour
	old := e.putManifest(t, testRef, 1, "0/1000028", "0/1000100", e.now.Add(-48*h))
	mid := e.putManifest(t, testRef, 1, "0/2000028", "0/2000100", e.now.Add(-24*h))
	// Another project's backups never count.
	e.putManifest(t, testRef2, 1, "0/1000028", "0/1000100", e.now.Add(-100*h))

	w, err := e.svc.RestoreWindow(ctx, testRef, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Backups) != 2 || w.Backups[0].ID != old.ID || w.Backups[1].ID != mid.ID {
		t.Fatalf("backups = %+v", w.Backups)
	}
	if !w.Earliest.Equal(old.StopTime) || !w.Latest.Equal(e.now) {
		t.Fatalf("window %s .. %s, want %s .. %s", w.Earliest, w.Latest, old.StopTime, e.now)
	}
}

func TestRestoreWindowSkipsBackupsRestoreCannotUse(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	h := time.Hour
	early := e.putManifest(t, testRef, 1, "0/1000028", "0/1000100", e.now.Add(-30*h)) // before the fork
	e.putManifest(t, testRef, 1, "0/5000028", "0/5000100", e.now.Add(-10*h))          // after the fork: off the history
	other := e.putManifest(t, testRef, 2, "0/6000028", "0/6000100", e.now.Add(-2*h))  // on the new timeline
	e.putHistory(t, testRef, 2, "1\t0/3000000\tno recovery target specified\n")

	w, err := e.svc.RestoreWindow(ctx, testRef, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Backups) != 2 || w.Backups[0].ID != early.ID || w.Backups[1].ID != other.ID {
		t.Fatalf("backups = %+v, want the one before the fork and the one on timeline 2", w.Backups)
	}

	// A backup whose first WAL file was pruned or lost cannot start a restore either.
	if err := e.store.Delete(ctx, walKey(testRef, early.StartWAL)); err != nil {
		t.Fatal(err)
	}
	w, err = e.svc.RestoreWindow(ctx, testRef, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Backups) != 1 || w.Backups[0].ID != other.ID || !w.Earliest.Equal(other.StopTime) {
		t.Fatalf("after losing a WAL file: %+v earliest %s", w.Backups, w.Earliest)
	}

	// The window agrees with what PlanRestore picks at its edges.
	if _, err := e.svc.PlanRestore(ctx, testRef, w.Earliest.Add(-time.Second), ""); err == nil {
		t.Error("a target before the window has a base backup")
	}
	if p, err := e.svc.PlanRestore(ctx, testRef, w.Earliest, ""); err != nil || p.Manifest.ID != other.ID {
		t.Errorf("a target at the start of the window: %+v, %v", p, err)
	}
}

func TestRestoreWindowOfAProjectThatIsNotRunning(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m := e.putManifest(t, testRef, 1, "0/1000028", "0/1000100", e.now.Add(-24*time.Hour))
	w, err := e.svc.RestoreWindow(ctx, testRef, false)
	if err != nil {
		t.Fatal(err)
	}
	// With no newer archived object the window ends where the newest backup ends...
	objs, _ := e.store.List(ctx, walDir(testRef))
	var newest time.Time
	for _, o := range objs {
		if o.ModTime.After(newest) {
			newest = o.ModTime
		}
	}
	// ...or at the newest archived object, when that is later (the file store stamps real time).
	want := m.StopTime
	if newest.After(want) {
		want = newest
	}
	if !w.Latest.Equal(want) {
		t.Fatalf("latest = %s, want %s", w.Latest, want)
	}
	if w.Latest.After(e.now.Add(48 * time.Hour)) {
		t.Fatalf("latest %s is in the future", w.Latest)
	}
}

func TestRestoreInPlaceModes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		req  func(m Manifest) lifecycle.RestoreRequest
		want string
	}{
		{"time", func(Manifest) lifecycle.RestoreRequest {
			return lifecycle.RestoreRequest{Target: time.Date(2026, 10, 6, 11, 30, 0, 0, time.UTC)}
		}, "recovery_target_time"},
		{"backup", func(m Manifest) lifecycle.RestoreRequest { return lifecycle.RestoreRequest{BackupID: m.ID} }, "recovery_target = 'immediate'"},
		{"backup and time", func(m Manifest) lifecycle.RestoreRequest {
			return lifecycle.RestoreRequest{BackupID: m.ID, Target: time.Date(2026, 10, 6, 11, 45, 0, 0, time.UTC)}
		}, "recovery_target_time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.addProject(t, testRef)
			m := e.storeBase(t, testRef, fakeDataDir(t), e.now.Add(-time.Hour), nil)
			dd := e.svc.opt.DataDir(testRef)
			writeFile(t, filepath.Join(dd, "PG_VERSION"), []byte("old"))
			fm := &fakeManager{e: e}
			e.svc.opt.Manager = fm
			if err := e.svc.RestoreInPlace(ctx, testRef, tc.req(m)); err != nil {
				t.Fatal(err)
			}
			conf, err := os.ReadFile(filepath.Join(dd, "postgresql.auto.conf"))
			if err != nil || !strings.Contains(string(conf), tc.want) {
				t.Fatalf("postgresql.auto.conf = %q, %v; want %q", conf, err, tc.want)
			}
			if len(fm.paused) != 1 || len(fm.resumed) != 1 {
				t.Fatalf("paused %v resumed %v", fm.paused, fm.resumed)
			}
		})
	}
}

// An in-place restore replaces PGDATA, which is <project>/postgres/data (lifecycle's layout), not
// the project's postgres directory, which also holds the socket directory and the launcher's files.
func TestDefaultDataDirIsPGDATA(t *testing.T) {
	e := newTestEnv(t)
	want := filepath.Join(e.cfg.StateDir, "projects", testRef, "postgres", "data")
	if got := e.svc.opt.DataDir(testRef); got != want {
		t.Fatalf("DataDir = %s, want %s", got, want)
	}
}
