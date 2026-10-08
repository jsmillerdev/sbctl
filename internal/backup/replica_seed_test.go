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
