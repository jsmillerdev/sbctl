package backup

import (
	"context"
	"errors"
	fs2 "io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/registry"
)

// fakeBackup stores a manifest, a data object and a registry row for a base backup
// that finished at stop and starts from WAL segment startSeg of timeline tli.
func (e *testEnv) fakeBackup(t *testing.T, ref string, stop time.Time, tli, startSeg uint32) Manifest {
	t.Helper()
	ctx := context.Background()
	start := stop.Add(-time.Minute)
	m := Manifest{
		Version: manifestVersion, ID: backupID(start), Ref: ref, Reason: ReasonScheduled,
		Timeline: int(tli), StartLSN: "0/1000028", StopLSN: "0/1000100",
		StartWAL: walName(tli, startSeg), StopWAL: walName(tli, startSeg),
		StartTime: start, StopTime: stop, Data: dataName, SizeBytes: 10, StoredBytes: 5, Files: 1,
	}
	if err := e.store.Put(ctx, m.Dir()+"/"+dataName, strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.writeManifest(ctx, &m); err != nil {
		t.Fatal(err)
	}
	fin := stop
	if err := e.reg.CreateBackup(ctx, &registry.Backup{Ref: ref, Kind: "base", Status: registry.BackupCompleted,
		Location: e.store.URL(baseDir(ref) + m.ID), StartedAt: start, FinishedAt: &fin, Timeline: int(tli)}); err != nil {
		t.Fatal(err)
	}
	return m
}

func (e *testEnv) fakeWAL(t *testing.T, ref string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := e.store.Put(context.Background(), walKey(ref, n), strings.NewReader("w")); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *testEnv) walNames(t *testing.T, ref string) []string {
	t.Helper()
	objs, err := e.store.List(context.Background(), walDir(ref))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, o := range objs {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(o.Key, walDir(ref)), ".zst"))
	}
	return out
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func TestRetainedBackups(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	mk := func(ageDays int) Manifest {
		return Manifest{ID: backupID(now.Add(-days(ageDays))), StopTime: now.Add(-days(ageDays))}
	}
	ids := func(ms []Manifest) []string {
		var o []string
		for _, m := range ms {
			o = append(o, m.ID[:8])
		}
		return o
	}
	all := []Manifest{mk(20), mk(12), mk(9), mk(5), mk(1)} // oldest first
	keep, drop := retainedBackups(all, now, 7, nil)
	// Within 7 days: 5 and 1. Anchor: newest before the window = 9 days old.
	if len(keep) != 3 || len(drop) != 2 {
		t.Fatalf("keep=%v drop=%v", ids(keep), ids(drop))
	}
	if keep[0].ID != all[2].ID || drop[0].ID != all[0].ID || drop[1].ID != all[1].ID {
		t.Fatalf("keep=%v drop=%v", ids(keep), ids(drop))
	}

	// Everything is older than the window: exactly one survives, the newest.
	keep, drop = retainedBackups([]Manifest{mk(40), mk(30), mk(20)}, now, 7, nil)
	if len(keep) != 1 || keep[0].ID != mk(20).ID || len(drop) != 2 {
		t.Fatalf("keep=%v drop=%v", ids(keep), ids(drop))
	}
	// A single old backup is never dropped.
	if keep, drop = retainedBackups([]Manifest{mk(400)}, now, 1, nil); len(keep) != 1 || len(drop) != 0 {
		t.Fatalf("keep=%v drop=%v", ids(keep), ids(drop))
	}
}

// After an in-place restore the newest pre-window backup can be off the timeline history.
// It must not become the anchor and crowd out the older, usable backup.
func TestRetainedBackupsAnchorMustBeOnHistory(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	mk := func(ageDays, tli int) Manifest {
		return Manifest{ID: backupID(now.Add(-days(ageDays))), StopTime: now.Add(-days(ageDays)), Timeline: tli}
	}
	a, b, c := mk(8, 1), mk(7, 1), mk(0, 2) // b is off the history: it ended after timeline 2 forked
	usable := func(m *Manifest) bool { return m.Timeline == 2 || m.ID == a.ID }
	keep, drop := retainedBackups([]Manifest{a, b, c}, now, 1, usable)
	if len(keep) != 2 || keep[0].ID != a.ID || keep[1].ID != c.ID || len(drop) != 1 || drop[0].ID != b.ID {
		t.Fatalf("keep=%v drop=%v: want a and c kept, b dropped", keep, drop)
	}
	// Nothing old is usable and nothing is inside the window: the newest backup is kept anyway.
	keep, drop = retainedBackups([]Manifest{a, b}, now, 1, func(*Manifest) bool { return false })
	if len(keep) != 1 || keep[0].ID != b.ID || len(drop) != 1 {
		t.Fatalf("keep=%v drop=%v", keep, drop)
	}
}

func TestWALNeeded(t *testing.T) {
	retained := []Manifest{{StartWAL: walName(2, 10)}, {StartWAL: walName(2, 20)}}
	for name, want := range map[string]bool{
		walName(1, 500):                     false, // earlier timeline than every retained backup
		walName(2, 9):                       false,
		walName(2, 10):                      true, // the oldest retained backup's start segment
		walName(2, 11) + ".partial":         true,
		walName(2, 10) + ".00000028.backup": true,
		walName(3, 1):                       true, // later timeline: needed to follow the history
		"00000001.history":                  true, // history files are always kept
		"00000002.history":                  true,
		"junk":                              true, // unknown names are never deleted
	} {
		if got := walNeeded(name, retained); got != want {
			t.Errorf("walNeeded(%s) = %v, want %v", name, got, want)
		}
	}
	if walNeeded(walName(2, 1), []Manifest{{StartWAL: "bogus"}}) != true {
		t.Error("an unparseable backup start must keep everything")
	}
}

func TestPruneDeletesOldBackupsAndWAL(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.addProject(t, testRef)
	e.cfg.Backup.RetentionDays = 7

	old := e.fakeBackup(t, testRef, e.now.Add(-days(20)), 1, 10)
	anchor := e.fakeBackup(t, testRef, e.now.Add(-days(9)), 1, 30)
	recent := e.fakeBackup(t, testRef, e.now.Add(-days(2)), 1, 50)
	e.fakeWAL(t, testRef, walName(1, 5), walName(1, 10), walName(1, 29), walName(1, 30), walName(1, 55), walName(2, 1))
	e.putHistory(t, testRef, 2, "1\t0/2000000\tno recovery target specified\n") // fork after every backup above

	res, err := e.svc.Prune(ctx, testRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.DeletedBackups) != 1 || res.DeletedBackups[0] != old.ID {
		t.Fatalf("deleted backups = %v", res.DeletedBackups)
	}
	if len(res.KeptBackups) != 2 || res.KeptBackups[0] != anchor.ID || res.KeptBackups[1] != recent.ID {
		t.Fatalf("kept backups = %v", res.KeptBackups)
	}
	left, _ := e.svc.ListBackups(ctx, testRef)
	if len(left) != 2 {
		t.Fatalf("backups left: %d", len(left))
	}
	if objs, _ := e.store.List(ctx, old.Dir()+"/"); len(objs) != 0 {
		t.Errorf("old backup objects remain: %v", objs)
	}
	// WAL before the oldest retained start (segment 30) is gone; the rest stays.
	got := e.walNames(t, testRef)
	want := []string{walName(1, 30), walName(1, 55), "00000002.history", walName(2, 1)} // key order
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("WAL left = %v, want %v", got, want)
	}
	if res.DeletedWAL != 3 {
		t.Errorf("DeletedWAL = %d", res.DeletedWAL)
	}
	rows, _ := e.reg.ListBackups(ctx, testRef)
	if len(rows) != 2 {
		t.Errorf("registry rows left = %d, want 2", len(rows))
	}

	// Running it again is a no-op.
	res, err = e.svc.Prune(ctx, testRef)
	if err != nil || len(res.DeletedBackups) != 0 || res.DeletedWAL != 0 {
		t.Fatalf("second prune = %+v, %v", res, err)
	}
}

// Prune must not let a timeline-1 backup taken after an in-place restore (off the history
// of timeline 2) displace the older usable one, nor delete the WAL that one needs.
func TestPruneAnchorIsOnTheTimelineHistory(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.addProject(t, testRef)
	e.cfg.Backup.RetentionDays = 7
	e.putHistory(t, testRef, 2, "1\t0/2000000\tno recovery target specified\n")

	a := e.fakeBackup(t, testRef, e.now.Add(-days(9)), 1, 10) // stops at 0/1000100: before the fork
	b := e.fakeBackup(t, testRef, e.now.Add(-days(8)), 1, 40)
	b.StopLSN = "0/3000000" // taken after the fork: unusable for timeline 2
	if err := e.svc.writeManifest(ctx, &b); err != nil {
		t.Fatal(err)
	}
	c := e.fakeBackup(t, testRef, e.now.Add(-days(1)), 2, 50)
	e.fakeWAL(t, testRef, walName(1, 10), walName(1, 39), walName(2, 50))

	res, err := e.svc.Prune(ctx, testRef)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.KeptBackups, ",") != a.ID+","+c.ID || len(res.DeletedBackups) != 1 || res.DeletedBackups[0] != b.ID {
		t.Fatalf("prune = %+v; want a and c kept, b deleted", res)
	}
	if got := strings.Join(e.walNames(t, testRef), ","); got != strings.Join([]string{walName(1, 10), walName(1, 39), "00000002.history", walName(2, 50)}, ",") {
		t.Fatalf("WAL left = %s: a's WAL must survive", got)
	}
}

func TestPruneRemovesStaleTempFilesOfCrashedPuts(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.cfg.Backup.RetentionDays = 7
	e.fakeBackup(t, testRef, e.now.Add(-days(1)), 1, 1)
	tmp := filepath.Join(e.store.root, testRef, "wal", "000000010000000000000001.zst"+tmpMarker+"123")
	writeFile(t, tmp, []byte("half"))
	if res, err := e.svc.Prune(ctx, testRef); err != nil || res.DeletedOrphans != 0 {
		t.Fatalf("a fresh temp file belongs to a Put in flight: %+v, %v", res, err)
	}
	real := e.now
	e.now = time.Now().Add(2 * orphanAge)
	res, err := e.svc.Prune(ctx, testRef)
	e.now = real
	if err != nil || res.DeletedOrphans != 1 {
		t.Fatalf("stale temp file: %+v, %v", res, err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, fs2.ErrNotExist) {
		t.Fatalf("temp file left: %v", err)
	}
}

func TestPruneKeepsTheOnlyBackupAndWithoutBackupsDeletesNothing(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.cfg.Backup.RetentionDays = 1

	e.fakeWAL(t, testRef, walName(1, 1), walName(1, 2))
	if res, err := e.svc.Prune(ctx, testRef); err != nil || res.DeletedWAL != 0 {
		t.Fatalf("prune with no base backup = %+v, %v (WAL alone restores nothing, but is not ours to delete)", res, err)
	}

	m := e.fakeBackup(t, testRef, e.now.Add(-days(100)), 1, 2)
	res, err := e.svc.Prune(ctx, testRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.DeletedBackups) != 0 || len(res.KeptBackups) != 1 || res.KeptBackups[0] != m.ID {
		t.Fatalf("prune = %+v", res)
	}
	if got := e.walNames(t, testRef); len(got) != 1 || got[0] != walName(1, 2) {
		t.Fatalf("WAL left = %v", got)
	}
}

func TestPruneDisabledWithZeroRetention(t *testing.T) {
	e := newTestEnv(t)
	e.cfg.Backup.RetentionDays = 0
	e.fakeBackup(t, testRef, e.now.Add(-days(100)), 1, 2)
	e.fakeBackup(t, testRef, e.now.Add(-days(50)), 1, 4)
	res, err := e.svc.Prune(context.Background(), testRef)
	if err != nil || len(res.DeletedBackups) != 0 {
		t.Fatalf("prune = %+v, %v", res, err)
	}
}

func TestPruneRemovesAbandonedUploadsOnly(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.cfg.Backup.RetentionDays = 7
	e.fakeBackup(t, testRef, e.now.Add(-days(1)), 1, 1)

	// An upload with no manifest: abandoned when old, in flight when fresh. File mtimes
	// are real time, so shift the service clock instead.
	if err := e.store.Put(ctx, baseDir(testRef)+"20200101T000000Z/"+dataName, strings.NewReader("half")); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Prune(ctx, testRef)
	if err != nil || res.DeletedOrphans != 0 {
		t.Fatalf("fresh upload must survive: %+v, %v", res, err)
	}
	real := e.now
	e.now = time.Now().Add(2 * orphanAge)
	res, err = e.svc.Prune(ctx, testRef)
	e.now = real
	if err != nil || res.DeletedOrphans != 1 {
		t.Fatalf("abandoned upload: %+v, %v", res, err)
	}
	if ids, _ := e.store.ListDirs(ctx, baseDir(testRef)); len(ids) != 1 {
		t.Fatalf("base dirs = %v", ids)
	}
}

func TestPruneAllCoversDeletedProjects(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.cfg.Backup.RetentionDays = 7
	e.addProject(t, testRef)
	// testRef2 has no registry row (project deleted) but keeps its final backup in the store.
	e.fakeBackup(t, testRef, e.now.Add(-days(30)), 1, 1)
	e.fakeBackup(t, testRef, e.now.Add(-days(1)), 1, 9)
	e.fakeBackup(t, testRef2, e.now.Add(-days(30)), 1, 1)
	e.fakeBackup(t, testRef2, e.now.Add(-days(29)), 1, 3)
	rs, err := e.svc.PruneAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("PruneAll results = %d, want one per ref in the store", len(rs))
	}
	for _, r := range rs {
		if r.Ref == testRef2 && len(r.KeptBackups) != 1 {
			t.Errorf("deleted project's newest backup must be kept: %+v", r)
		}
	}
}
