package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

const testReplicaID = testRef + "-rr-eu-abc123"

// seedEnv is a testEnv with one base backup of testRef in the store.
func seedEnv(t *testing.T) (*testEnv, Manifest) {
	t.Helper()
	e := newTestEnv(t)
	e.cfg.BinPath = "/opt/supavise/bin/supavise"
	e.svc.opt.ConfigPath = "/etc/supavise/other.toml"
	src := fakeDataDir(t)
	m := e.storeBase(t, testRef, src, e.now.Add(-time.Hour), nil)
	return e, m
}

func readConf(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSeedReplicaWritesStandbyFiles(t *testing.T) {
	e, m := seedEnv(t)
	ctx := context.Background()
	dd := filepath.Join(t.TempDir(), "replica", "data")
	err := e.svc.SeedReplica(ctx, ReplicaSeedPlan{Ref: testRef, Identifier: testReplicaID, DataDir: dd,
		PrimaryPort: 20003, ReplicationPassword: "Abc123_-.xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dd); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("data dir = %v, %v (Postgres refuses anything but 0700/0750)", fi, err)
	}
	for _, p := range []string{"PG_VERSION", "base/1/1259", "backup_label", "standby.signal", "postmaster.opts", "pg_wal/archive_status"} {
		if _, err := os.Stat(filepath.Join(dd, p)); err != nil {
			t.Errorf("%s missing: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dd, "recovery.signal")); err == nil {
		t.Error("recovery.signal exists: a standby is made with standby.signal only")
	}
	if fi, err := os.Stat(filepath.Join(dd, "postgresql.auto.conf")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("postgresql.auto.conf = %v, %v; it holds the replication password and must be 0600", fi, err)
	}
	conf := readConf(t, dd)
	for _, want := range []string{
		"# auto\n", // the source's own content is kept; ours is appended
		"primary_conninfo = 'host=127.0.0.1 port=20003 user=supabase_replication_admin password=Abc123_-.xyz application_name=" + testReplicaID + " sslmode=disable'\n",
		"restore_command = '/opt/supavise/bin/supavise --config /etc/supavise/other.toml wal fetch --ref " + testRef + " %f %p'\n",
		"archive_command = '/opt/supavise/bin/supavise --config /etc/supavise/other.toml wal push --ref " + testRef + " %p'\n",
		"archive_mode = on\n",
		"recovery_target_timeline = 'latest'\n",
		"hot_standby = on\n",
		"(backup " + m.ID + ")",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("postgresql.auto.conf lacks %q:\n%s", want, conf)
		}
	}
	for _, bad := range []string{"archive_mode = always", "recovery_target_time =", "recovery_target_action", "recovery_target = "} {
		if strings.Contains(conf, bad) {
			t.Errorf("postgresql.auto.conf has %q:\n%s", bad, conf)
		}
	}
	// A seeder never writes into a directory that already has data, and leaves it as it was.
	if err := e.svc.SeedReplica(ctx, ReplicaSeedPlan{Ref: testRef, Identifier: testReplicaID, DataDir: dd,
		PrimaryPort: 20003, ReplicationPassword: "x"}); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("second seed = %v", err)
	}
	if got := readConf(t, dd); got != conf {
		t.Error("a refused seed changed the data directory")
	}
}

func TestSeedReplicaQuotesThePassword(t *testing.T) {
	e, _ := seedEnv(t)
	dd := filepath.Join(t.TempDir(), "data")
	pw := `p w'd\x"y`
	if err := e.svc.SeedReplica(context.Background(), ReplicaSeedPlan{Ref: testRef, Identifier: testReplicaID, DataDir: dd,
		PrimaryPort: 20003, ReplicationPassword: pw}); err != nil {
		t.Fatal(err)
	}
	// libpq value: 'p w\'d\\x"y'; inside the postgresql.conf literal the backslashes and quotes double again.
	want := `password=''p w\\''d\\\\x"y'' application_name=`
	if conf := readConf(t, dd); !strings.Contains(conf, want) {
		t.Errorf("postgresql.auto.conf lacks %s:\n%s", want, conf)
	}
}

func TestSeedReplicaWithoutPrimaryIsAnArchiveStandby(t *testing.T) {
	e, _ := seedEnv(t)
	dd := filepath.Join(t.TempDir(), "data")
	// No identifier, port or password: the standby of `supavise failover --restore-missing`.
	if err := e.svc.SeedReplica(context.Background(), ReplicaSeedPlan{Ref: testRef, DataDir: dd, ReplicationPassword: "secret-not-written"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dd, "standby.signal")); err != nil {
		t.Errorf("standby.signal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dd, "recovery.signal")); err == nil {
		t.Error("recovery.signal exists")
	}
	conf := readConf(t, dd)
	for _, want := range []string{"restore_command = '", "recovery_target_timeline = 'latest'\n", "hot_standby = on\n"} {
		if !strings.Contains(conf, want) {
			t.Errorf("postgresql.auto.conf lacks %q:\n%s", want, conf)
		}
	}
	for _, bad := range []string{"primary_conninfo", "secret-not-written", "supabase_replication_admin"} {
		if strings.Contains(conf, bad) {
			t.Errorf("an archive-only standby has %q:\n%s", bad, conf)
		}
	}
}

func TestSeedReplicaRestoreCommandUsesTheNodesRelay(t *testing.T) {
	e, _ := seedEnv(t)
	e.cfg.Backup.WALRelay = "on"
	dd := filepath.Join(t.TempDir(), "data")
	if err := e.svc.SeedReplica(context.Background(), ReplicaSeedPlan{Ref: testRef, Identifier: testReplicaID, DataDir: dd,
		PrimaryPort: 20003, ReplicationPassword: "pw"}); err != nil {
		t.Fatal(err)
	}
	sock := e.cfg.Paths().WALSocket(testRef)
	conf := readConf(t, dd)
	for _, want := range []string{
		"restore_command = '/opt/supavise/bin/supavise wal fetch --ref " + testRef + " --socket " + sock + " %f %p'\n",
		"archive_command = '/opt/supavise/bin/supavise wal push --ref " + testRef + " --socket " + sock + " %p'\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("postgresql.auto.conf lacks %q:\n%s", want, conf)
		}
	}
}

func TestSeedReplicaPicksTheBackup(t *testing.T) {
	e, old := seedEnv(t)
	ctx := context.Background()
	src := fakeDataDir(t)
	writeFile(t, filepath.Join(src, "newer"), []byte("x"))
	newer := e.storeBase(t, testRef, src, e.now.Add(-time.Minute), nil)
	plan := ReplicaSeedPlan{Ref: testRef, Identifier: testReplicaID, PrimaryPort: 20003, ReplicationPassword: "pw"}

	plan.DataDir = filepath.Join(t.TempDir(), "newest")
	if err := e.svc.SeedReplica(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(plan.DataDir, "newer")); err != nil || !strings.Contains(readConf(t, plan.DataDir), newer.ID) {
		t.Errorf("the default backup is not the newest (%s): %v", newer.ID, err)
	}
	plan.DataDir, plan.BackupID = filepath.Join(t.TempDir(), "named"), old.ID
	if err := e.svc.SeedReplica(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(plan.DataDir, "newer")); err == nil || !strings.Contains(readConf(t, plan.DataDir), old.ID) {
		t.Errorf("BackupID %s was not honored: %v", old.ID, err)
	}
}

func TestSeedReplicaDropsRecoverySignalOfTheBackup(t *testing.T) {
	e := newTestEnv(t)
	src := fakeDataDir(t)
	writeFile(t, filepath.Join(src, "recovery.signal"), nil)
	writeFile(t, filepath.Join(src, "standby.signal"), nil)
	e.storeBase(t, testRef, src, e.now.Add(-time.Hour), nil)
	dd := filepath.Join(t.TempDir(), "data")
	if err := e.svc.SeedReplica(context.Background(), ReplicaSeedPlan{Ref: testRef, DataDir: dd}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dd, "recovery.signal")); err == nil {
		t.Error("recovery.signal of the backup survived")
	}
	if _, err := os.Stat(filepath.Join(dd, "standby.signal")); err != nil {
		t.Errorf("standby.signal: %v", err)
	}
}

func TestSeedReplicaRefusals(t *testing.T) {
	e, _ := seedEnv(t)
	ctx := context.Background()
	ok := ReplicaSeedPlan{Ref: testRef, Identifier: testReplicaID, DataDir: filepath.Join(t.TempDir(), "data"), PrimaryPort: 20003, ReplicationPassword: "pw"}
	for name, mod := range map[string]func(*ReplicaSeedPlan){
		"bad ref":                func(p *ReplicaSeedPlan) { p.Ref = "../x" },
		"relative directory":     func(p *ReplicaSeedPlan) { p.DataDir = "data" },
		"no directory":           func(p *ReplicaSeedPlan) { p.DataDir = "" },
		"port out of range":      func(p *ReplicaSeedPlan) { p.PrimaryPort = 70000 },
		"identifier of another":  func(p *ReplicaSeedPlan) { p.Identifier = testRef2 + "-rr-eu-abc123" },
		"not an identifier":      func(p *ReplicaSeedPlan) { p.Identifier = "replica-1" },
		"no identifier":          func(p *ReplicaSeedPlan) { p.Identifier = "" },
		"no password":            func(p *ReplicaSeedPlan) { p.ReplicationPassword = "" },
		"unknown base backup id": func(p *ReplicaSeedPlan) { p.BackupID = "20200101T000000Z-aaaaaa" },
		"project without backup": func(p *ReplicaSeedPlan) { p.Ref = testRef2; p.Identifier = testRef2 + "-rr-eu-abc123" },
	} {
		p := ok
		mod(&p)
		if err := e.svc.SeedReplica(ctx, p); err == nil {
			t.Errorf("%s: seeded", name)
		}
	}
	if _, err := os.Stat(ok.DataDir); err == nil {
		t.Error("a refused plan created the data directory")
	}
}

// A seed that fails after it started extracting leaves the directory empty, so the caller can
// retry on it; a directory that held data before is never touched.
func TestSeedReplicaFailureLeavesAnEmptyDirectory(t *testing.T) {
	e, m := seedEnv(t)
	ctx := context.Background()
	if err := e.store.Put(ctx, m.Dir()+"/"+dataName, strings.NewReader("this is not a zstd stream")); err != nil {
		t.Fatal(err)
	}
	dd := filepath.Join(t.TempDir(), "data")
	if err := e.svc.SeedReplica(ctx, ReplicaSeedPlan{Ref: testRef, DataDir: dd}); err == nil {
		t.Fatal("seeded from a corrupt backup")
	}
	if ents, err := os.ReadDir(dd); err != nil || len(ents) != 0 {
		t.Fatalf("data dir after the failure = %v, %v", ents, err)
	}

	held := filepath.Join(t.TempDir(), "held")
	writeFile(t, filepath.Join(held, "keep"), []byte("data"))
	if err := e.svc.SeedReplica(ctx, ReplicaSeedPlan{Ref: testRef, DataDir: held}); err == nil {
		t.Fatal("seeded into a directory that holds data")
	}
	if b, err := os.ReadFile(filepath.Join(held, "keep")); err != nil || string(b) != "data" {
		t.Fatalf("a refused seed removed what the directory held: %q, %v", b, err)
	}
}

// A seeded cluster is not promoted, whatever promote.ok an earlier life of the directory's project
// left on this node: the relay would trust it for as long as it exists.
func TestSeedReplicaDeletesPromoteOK(t *testing.T) {
	e, _ := seedEnv(t)
	ctx := context.Background()
	own, other := e.cfg.Paths().PromoteOK(testRef), e.cfg.Paths().PromoteOK(testRef2)
	writeFile(t, own, FormatPromoteOK(1))
	writeFile(t, other, FormatPromoteOK(1))
	if err := e.svc.SeedReplica(ctx, ReplicaSeedPlan{Ref: testRef, Identifier: testReplicaID, DataDir: filepath.Join(t.TempDir(), "streaming"),
		PrimaryPort: 20003, ReplicationPassword: "pw"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(own); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("promote.ok of the seeded project: %v; want it deleted", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("promote.ok of another project: %v; want it kept", err)
	}
	writeFile(t, own, FormatPromoteOK(2))
	if err := e.svc.SeedReplica(ctx, ReplicaSeedPlan{Ref: testRef, DataDir: filepath.Join(t.TempDir(), "archive-only")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(own); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("promote.ok after an archive-only seed: %v; want it deleted", err)
	}
}

func TestSeedReplicaOfTheSystemProject(t *testing.T) {
	e := newTestEnv(t)
	e.storeBase(t, config.SystemRef, fakeDataDir(t), e.now.Add(-time.Hour), nil)
	id := config.SystemRef + "-rr-eu-abc123"
	dd := filepath.Join(t.TempDir(), "data")
	if err := e.svc.SeedReplica(context.Background(), ReplicaSeedPlan{Ref: config.SystemRef, Identifier: id, DataDir: dd,
		PrimaryPort: 20001, ReplicationPassword: "pw"}); err != nil {
		t.Fatal(err)
	}
	conf := readConf(t, dd)
	for _, want := range []string{"application_name=" + id + " ", "wal fetch --ref " + config.SystemRef + " ", "wal push --ref " + config.SystemRef + " "} {
		if !strings.Contains(conf, want) {
			t.Errorf("postgresql.auto.conf lacks %q:\n%s", want, conf)
		}
	}
	if _, err := os.Stat(filepath.Join(dd, "standby.signal")); err != nil {
		t.Errorf("standby.signal: %v", err)
	}
}

// A seed that was cut off leaves a backup_label and no standby.signal. It is marked, and the next
// seed clears it; a finished seed leaves no marker.
func TestSeedReplicaClearsAnInterruptedSeed(t *testing.T) {
	e, _ := seedEnv(t)
	ctx := context.Background()
	dd := filepath.Join(t.TempDir(), "data")
	writeFile(t, filepath.Join(dd, seedMarker), nil)
	writeFile(t, filepath.Join(dd, "backup_label"), []byte("half"))
	writeFile(t, filepath.Join(dd, "base", "1", "partial"), []byte("half"))
	if !SeedUnfinished(dd) {
		t.Fatal("SeedUnfinished = false for a directory with the marker")
	}
	if err := e.svc.SeedReplica(ctx, ReplicaSeedPlan{Ref: testRef2, DataDir: dd}); err == nil {
		t.Fatal("seeded a project without a backup")
	}
	if !SeedUnfinished(dd) {
		t.Fatal("a seed that failed before it started cleared the directory of the earlier one")
	}
	if err := e.svc.SeedReplica(ctx, ReplicaSeedPlan{Ref: testRef, DataDir: dd}); err != nil {
		t.Fatalf("seed over an interrupted one: %v", err)
	}
	if SeedUnfinished(dd) {
		t.Error("a finished seed left the marker")
	}
	if _, err := os.Stat(filepath.Join(dd, "base", "1", "partial")); err == nil {
		t.Error("what the interrupted seed extracted survived")
	}
	for _, p := range []string{"standby.signal", "PG_VERSION"} {
		if _, err := os.Stat(filepath.Join(dd, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dd, "backup_label")); err != nil || strings.Contains(string(b), "half") {
		t.Errorf("backup_label = %q, %v; want the backup's", b, err)
	}
	if SeedUnfinished(filepath.Join(t.TempDir(), "missing")) {
		t.Error("SeedUnfinished = true for a missing directory")
	}
}

// The demotion of a primary in place: its data directory is already there, the standby block is
// written over whatever an earlier standby life left, and the project is not promoted any more.
func TestConfigureStandbyDemotesInPlace(t *testing.T) {
	e, _ := seedEnv(t)
	ctx := context.Background()
	dd := filepath.Join(t.TempDir(), "data")
	if err := e.svc.SeedReplica(ctx, ReplicaSeedPlan{Ref: testRef, Identifier: testReplicaID, DataDir: dd,
		PrimaryPort: 20003, ReplicationPassword: "old-secret"}); err != nil {
		t.Fatal(err)
	}
	// It was promoted: the standby block went, and so did standby.signal. Later it ran as a primary.
	if err := ClearStandbyConf(dd); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dd, "standby.signal")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dd, "recovery.signal"), nil)
	writeFile(t, e.cfg.Paths().PromoteOK(testRef), FormatPromoteOK(1))
	conf := readConf(t, dd)
	if err := os.WriteFile(filepath.Join(dd, "postgresql.auto.conf"), []byte(conf+"work_mem = '8MB'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	id := testRef + "-rr-eu-def456"
	if err := e.svc.ConfigureStandby(ReplicaSeedPlan{Ref: testRef, Identifier: id, DataDir: dd, PrimaryPort: 20004, ReplicationPassword: "new-secret"}); err != nil {
		t.Fatal(err)
	}
	got := readConf(t, dd)
	for _, want := range []string{"# auto\n", "work_mem = '8MB'\n", "archive_mode = on\n",
		"primary_conninfo = 'host=127.0.0.1 port=20004 user=supabase_replication_admin password=new-secret application_name=" + id + " sslmode=disable'\n",
		"restore_command = '", "recovery_target_timeline = 'latest'\n", "hot_standby = on\n", "# --- supavise standby " + id + " of " + testRef + " ---\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("postgresql.auto.conf lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "old-secret") || strings.Contains(got, "(backup") {
		t.Errorf("postgresql.auto.conf has a stale value:\n%s", got)
	}
	for _, p := range []string{"standby.signal", "PG_VERSION", "backup_label"} {
		if _, err := os.Stat(filepath.Join(dd, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dd, "recovery.signal")); err == nil {
		t.Error("recovery.signal survived")
	}
	if _, err := os.Stat(e.cfg.Paths().PromoteOK(testRef)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("promote.ok after the demotion: %v; want it deleted", err)
	}
	// Run again with the same plan: one block, not two.
	if err := e.svc.ConfigureStandby(ReplicaSeedPlan{Ref: testRef, Identifier: id, DataDir: dd, PrimaryPort: 20004, ReplicationPassword: "new-secret"}); err != nil {
		t.Fatal(err)
	}
	if again := readConf(t, dd); strings.Count(again, "primary_conninfo") != 1 || strings.Count(again, "work_mem") != 1 {
		t.Errorf("a second ConfigureStandby doubled the block:\n%s", again)
	}
	if err := e.svc.ConfigureStandby(ReplicaSeedPlan{Ref: testRef, DataDir: "data"}); err == nil {
		t.Error("ConfigureStandby accepted a relative directory")
	}
}

func TestBackupIDOf(t *testing.T) {
	e := newTestEnv(t)
	m := e.fakeBackup(t, testRef, e.now.Add(-time.Hour), 1, 3)
	rows, _ := e.reg.ListBackups(context.Background(), testRef)
	if got := BackupIDOf(&rows[0]); got != m.ID {
		t.Errorf("BackupIDOf = %q, want %q", got, m.ID)
	}
	if BackupIDOf(nil) != "" || BackupIDOf(&registry.Backup{}) != "" {
		t.Error("BackupIDOf of nothing is not empty")
	}
}

func TestEnsureBase(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var taken atomic.Int32
	e.svc.opt.TakeBase = func(ctx context.Context, ref string) (*registry.Backup, error) {
		taken.Add(1)
		stop := e.now
		m := e.fakeBackup(t, ref, stop, 1, 3)
		return &registry.Backup{Ref: ref, Location: e.store.URL(m.Dir()), Status: registry.BackupCompleted}, nil
	}
	hour := time.Hour

	// No backup yet: one is taken.
	b, err := e.svc.EnsureBase(ctx, testRef, 24*hour)
	if err != nil || taken.Load() != 1 || BackupIDOf(b) == "" {
		t.Fatalf("EnsureBase with no backup = %+v, %v (taken %d)", b, err, taken.Load())
	}
	// The WAL it starts from is not in the archive yet: unusable, so another is taken.
	e.now = e.now.Add(time.Minute)
	if _, err := e.svc.EnsureBase(ctx, testRef, 24*hour); err != nil || taken.Load() != 2 {
		t.Fatalf("EnsureBase with an unusable backup: %v (taken %d)", err, taken.Load())
	}
	e.fakeWAL(t, testRef, walName(1, 3))
	// A usable backup younger than maxAge is returned as it is, with the registry's row.
	e.now = e.now.Add(30 * time.Minute)
	b, err = e.svc.EnsureBase(ctx, testRef, hour)
	if err != nil || taken.Load() != 2 {
		t.Fatalf("EnsureBase with a young backup: %v (taken %d)", err, taken.Load())
	}
	if b.ID == 0 || b.Status != registry.BackupCompleted || b.FinishedAt == nil {
		t.Errorf("row = %+v; want the registry's row of the backup", b)
	}
	ms, _ := e.svc.ListBackups(ctx, testRef)
	if got, want := BackupIDOf(b), ms[len(ms)-1].ID; got != want {
		t.Errorf("returned backup %s, want the newest %s", got, want)
	}
	// Older than maxAge: a new one. maxAge zero always takes one.
	e.now = e.now.Add(2 * hour)
	if _, err := e.svc.EnsureBase(ctx, testRef, hour); err != nil || taken.Load() != 3 {
		t.Fatalf("EnsureBase with an old backup: %v (taken %d)", err, taken.Load())
	}
	if _, err := e.svc.EnsureBase(ctx, testRef, 0); err != nil || taken.Load() != 4 {
		t.Fatalf("EnsureBase with maxAge 0: %v (taken %d)", err, taken.Load())
	}
	if _, err := e.svc.EnsureBase(ctx, "../x", hour); err == nil {
		t.Error("EnsureBase accepted a bad ref")
	}
	// A failed backup is the caller's error, not a stale backup.
	e.svc.opt.TakeBase = func(context.Context, string) (*registry.Backup, error) { return nil, errors.New("disk full") }
	if _, err := e.svc.EnsureBase(ctx, testRef, 0); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("EnsureBase after a failed backup = %v", err)
	}
	// Without TakeBase the service's own BaseBackup runs; this one has no database access.
	e.svc.opt.TakeBase = nil
	if _, err := e.svc.EnsureBase(ctx, testRef, 0); err == nil || !strings.Contains(err.Error(), "database access") {
		t.Fatalf("EnsureBase over BaseBackup = %v", err)
	}
}

// Backups that are not on the archive's timeline history cannot seed a standby.
func TestEnsureBaseSkipsABackupOffTheHistory(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	// Timeline 2 forked off timeline 1 at 0/1000000; the backup of timeline 1 ended at 0/1000100.
	e.putHistory(t, testRef, 2, "1\t0/1000000\tno recovery target specified\n")
	m := e.fakeBackup(t, testRef, e.now.Add(-time.Minute), 1, 3)
	e.fakeWAL(t, testRef, m.StartWAL)
	var taken atomic.Int32
	e.svc.opt.TakeBase = func(context.Context, string) (*registry.Backup, error) { taken.Add(1); return &registry.Backup{}, nil }
	if _, err := e.svc.EnsureBase(ctx, testRef, 24*time.Hour); err != nil || taken.Load() != 1 {
		t.Fatalf("EnsureBase = %v (taken %d); the backup of timeline 1 is past the fork", err, taken.Load())
	}
}

// Two replicas of one project set up together share the backup the first call takes.
func TestEnsureBaseCallsForOneRefRunOneAtATime(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var taken atomic.Int32
	gate := make(chan struct{})
	e.svc.opt.TakeBase = func(ctx context.Context, ref string) (*registry.Backup, error) {
		taken.Add(1)
		<-gate
		m := e.fakeBackup(t, ref, e.now, 1, 3)
		e.fakeWAL(t, ref, m.StartWAL)
		return &registry.Backup{Ref: ref, Location: e.store.URL(m.Dir())}, nil
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.svc.EnsureBase(ctx, testRef, time.Hour); err != nil {
				t.Error(err)
			}
		}()
	}
	for taken.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if n := taken.Load(); n != 1 {
		t.Fatalf("backups taken = %d, want 1", n)
	}
}

// A caller that waits for another one's backup gives up when its context ends, and leaves the
// lock to the caller that holds it.
func TestEnsureBaseWaitEndsWithItsContext(t *testing.T) {
	e := newTestEnv(t)
	var taken atomic.Int32
	gate := make(chan struct{})
	e.svc.opt.TakeBase = func(ctx context.Context, ref string) (*registry.Backup, error) {
		taken.Add(1)
		<-gate
		return &registry.Backup{Ref: ref}, nil
	}
	first := make(chan error, 1)
	go func() {
		_, err := e.svc.EnsureBase(context.Background(), testRef, 0)
		first <- err
	}()
	for taken.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := e.svc.EnsureBase(ctx, testRef, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("EnsureBase behind another call = %v; want the context's error", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("EnsureBase waited %s for a lock after its context ended", d)
	}
	// Another ref is not held up by it.
	e.svc.opt.TakeBase = func(ctx context.Context, ref string) (*registry.Backup, error) {
		return &registry.Backup{Ref: ref}, nil
	}
	if _, err := e.svc.EnsureBase(context.Background(), testRef2, 0); err != nil {
		t.Fatalf("EnsureBase of another ref: %v", err)
	}
	close(gate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	// The lock is free again.
	if _, err := e.svc.EnsureBase(context.Background(), testRef, 0); err != nil {
		t.Fatalf("EnsureBase after the others finished: %v", err)
	}
	if n := taken.Load(); n != 1 {
		t.Errorf("blocking TakeBase ran %d times, want 1", n)
	}
}

func TestClearStandbyConfKeepsTheRestOfTheFile(t *testing.T) {
	e, _ := seedEnv(t)
	dd := filepath.Join(t.TempDir(), "data")
	if err := e.svc.SeedReplica(context.Background(), ReplicaSeedPlan{Ref: testRef, Identifier: testReplicaID, DataDir: dd,
		PrimaryPort: 20003, ReplicationPassword: "very-secret"}); err != nil {
		t.Fatal(err)
	}
	// Settings the primary has of its own, before and after the standby block.
	conf := readConf(t, dd)
	if err := os.WriteFile(filepath.Join(dd, "postgresql.auto.conf"), []byte("work_mem = '8MB'\nhot_standby_feedback = 'on'\n"+conf+"max_connections = '60'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ClearStandbyConf(dd); err != nil {
		t.Fatal(err)
	}
	got := readConf(t, dd)
	for _, gone := range []string{"primary_conninfo", "very-secret", "restore_command", "recovery_target_timeline", "hot_standby = ", "supavise standby"} {
		if strings.Contains(got, gone) {
			t.Errorf("postgresql.auto.conf still has %q:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"# auto\n", "work_mem = '8MB'", "hot_standby_feedback = 'on'", "max_connections = '60'", "archive_mode = on", "archive_command = "} {
		if !strings.Contains(got, kept) {
			t.Errorf("postgresql.auto.conf lost %q:\n%s", kept, got)
		}
	}
	if fi, err := os.Stat(filepath.Join(dd, "postgresql.auto.conf")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("postgresql.auto.conf = %v, %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(dd, "postgresql.auto.conf.tmp")); err == nil {
		t.Error("a temporary file was left behind")
	}
	// Nothing to clear: the file is not rewritten. A directory without the file is fine too.
	before, _ := os.Stat(filepath.Join(dd, "postgresql.auto.conf"))
	if err := ClearStandbyConf(dd); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(filepath.Join(dd, "postgresql.auto.conf")); !after.ModTime().Equal(before.ModTime()) {
		t.Error("a file without a standby block was rewritten")
	}
	if err := ClearStandbyConf(t.TempDir()); err != nil {
		t.Fatalf("directory without postgresql.auto.conf: %v", err)
	}
}

// The seeding marker is the last thing a clear removes, and only when everything else went: a directory
// that could not be emptied still says it is half seeded.
func TestClearDirKeepsTheSeedMarkerWhileAnythingRemains(t *testing.T) {
	dd := t.TempDir()
	for _, f := range []string{"PG_VERSION", "backup_label", seedMarker} {
		if err := os.WriteFile(filepath.Join(dd, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory that cannot be removed: no permission on its parent entry's contents.
	locked := filepath.Join(dd, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("root removes a directory whatever its mode")
	}
	if err := clearDir(dd); err == nil {
		t.Fatal("clearDir reported success with a directory it could not remove")
	}
	if !SeedUnfinished(dd) {
		t.Fatal("the marker went before the directory was empty")
	}
	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := clearDir(dd); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(dd); len(ents) != 0 {
		t.Fatalf("directory not empty after the clear: %v", ents)
	}
}

// A standby cannot be made by a service that does not know where promote.ok is, and a backup id that
// would start a line in postgresql.auto.conf is refused.
func TestConfigureStandbyChecksItsPlan(t *testing.T) {
	e, _ := seedEnv(t)
	dd := fakeDataDir(t)
	plan := ReplicaSeedPlan{Ref: testRef, DataDir: dd}
	if err := e.svc.ConfigureStandby(ReplicaSeedPlan{Ref: testRef, DataDir: dd, BackupID: "b1\nrestore_command = 'x'"}); err == nil || !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("a backup id with a newline = %v", err)
	}
	if conf, err := os.ReadFile(filepath.Join(dd, "postgresql.auto.conf")); err == nil && strings.Contains(string(conf), "restore_command = 'x'") {
		t.Fatal("the injected line reached postgresql.auto.conf")
	}
	bare := *e.svc
	bare.opt.Config = nil
	if err := bare.ConfigureStandby(plan); err == nil || !strings.Contains(err.Error(), "promote.ok") {
		t.Fatalf("a service without a Config made a standby: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dd, "standby.signal")); err == nil {
		t.Fatal("standby.signal written by a service that could not delete promote.ok")
	}
	if err := e.svc.ConfigureStandby(plan); err != nil {
		t.Fatal(err)
	}
}

// Every cycle of ConfigureStandby leaves one block, not another pair of archive lines.
func TestConfigureStandbyTwiceLeavesOneBlock(t *testing.T) {
	e, _ := seedEnv(t)
	dd := fakeDataDir(t)
	plan := ReplicaSeedPlan{Ref: testRef, Identifier: testReplicaID, DataDir: dd, PrimaryPort: 20003, ReplicationPassword: "pw"}
	for i := 0; i < 3; i++ {
		if err := e.svc.ConfigureStandby(plan); err != nil {
			t.Fatal(err)
		}
	}
	conf := readConf(t, dd)
	for _, once := range []string{"archive_mode = on", "archive_command = ", "primary_conninfo = ", "restore_command = ", "# --- supavise standby"} {
		if n := strings.Count(conf, once); n != 1 {
			t.Errorf("%q appears %d times:\n%s", once, n, conf)
		}
	}
}

// The leader records a base backup that its home took without writing the registry.
func TestRecordBaseWritesTheRowAndTheEventOnce(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if err := e.reg.CreateProject(ctx, &registry.Project{Ref: testRef, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	b := RemoteBase{ID: "20261008T120000Z-abcdef", Reason: ReasonFinal, Timeline: 3, StartLSN: "0/3000028", StopLSN: "0/3000120", SizeBytes: 4096}

	// The backup must be complete in the store, and be the one the home reports.
	if _, err := e.svc.RecordBase(ctx, testRef, b); err == nil || !strings.Contains(err.Error(), "not complete in the store") {
		t.Fatalf("a backup with no manifest = %v", err)
	}
	started := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	m := &Manifest{Version: manifestVersion, ID: b.ID, Ref: testRef, Timeline: 3, StartLSN: "0/3000028", StopLSN: "0/3000999", StartTime: started}
	if err := e.svc.writeManifest(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RecordBase(ctx, testRef, b); err == nil || !strings.Contains(err.Error(), "0/3000999") {
		t.Fatalf("a report that disagrees with the manifest = %v", err)
	}
	if rows, _ := e.reg.ListBackups(ctx, testRef); len(rows) != 0 {
		t.Fatalf("a refused report left %d rows", len(rows))
	}
	m.StopLSN = "0/3000120"
	if err := e.svc.writeManifest(ctx, m); err != nil {
		t.Fatal(err)
	}

	row, err := e.svc.RecordBase(ctx, testRef, b)
	if err != nil {
		t.Fatal(err)
	}
	if !row.StartedAt.Equal(started) {
		t.Fatalf("the row starts at %v, the backup did at %v", row.StartedAt, started)
	}
	if row.ID == 0 || row.Status != registry.BackupCompleted || row.Timeline != 3 || row.StopLSN != "0/3000120" || row.SizeBytes != 4096 ||
		!strings.HasSuffix(row.Location, testRef+"/base/20261008T120000Z-abcdef") || row.FinishedAt == nil {
		t.Fatalf("row = %+v", row)
	}
	again, err := e.svc.RecordBase(ctx, testRef, b)
	if err != nil || again.ID != row.ID {
		t.Fatalf("a report repeated = %+v, %v", again, err)
	}
	if rows, _ := e.reg.ListBackups(ctx, testRef); len(rows) != 1 {
		t.Fatalf("%d rows after a repeated report", len(rows))
	}
	evs, _ := e.reg.ListEvents(ctx, testRef, 10)
	if len(evs) != 1 || evs[0].Kind != "backup.completed" {
		t.Fatalf("events = %+v", evs)
	}
	for _, bad := range []string{"", "a/b", "a b", "a\nb", "..", "20261008T120000Z-abcdef/..", "20261008T120000Z-ABCDEF", "b1"} {
		if _, err := e.svc.RecordBase(ctx, testRef, RemoteBase{ID: bad}); err == nil {
			t.Errorf("RecordBase accepted the id %q", bad)
		}
	}
}
