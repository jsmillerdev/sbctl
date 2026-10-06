package backup

// Integration tests that run a real PostgreSQL and the real sbctl binary as its
// archive_command and restore_command. They need the Postgres artifact's bin
// directory: SBCTL_TEST_PG_BIN, or the unpacked artifact under
// ~/.cache/sbctl/unpacked for this OS and architecture. Without one they are
// skipped, as they are with -short or SBCTL_TEST_PG=0. Ports come from 35000-35999.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
)

// pgBinDir returns the directory holding initdb, pg_ctl and postgres, or skips.
func pgBinDir(t *testing.T) string {
	t.Helper()
	if testing.Short() || os.Getenv("SBCTL_TEST_PG") == "0" {
		t.Skip("PostgreSQL integration test disabled")
	}
	if d := os.Getenv("SBCTL_TEST_PG_BIN"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	m, _ := filepath.Glob(filepath.Join(home, ".cache", "sbctl", "unpacked", "postgres-*-"+runtime.GOOS+"-"+runtime.GOARCH, "bin"))
	if len(m) == 0 {
		t.Skip("no Postgres artifact: set SBCTL_TEST_PG_BIN to its bin directory")
	}
	return m[len(m)-1]
}

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// sbctlBinary builds cmd/sbctl once per test run.
func sbctlBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "sbctl-test-bin-")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "sbctl")
		cmd := exec.Command("go", "build", "-o", builtBin, "./cmd/sbctl")
		cmd.Dir = filepath.Join("..", "..")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/sbctl: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtBin
}

func TestMain(m *testing.M) {
	code := m.Run()
	if builtBin != "" {
		os.RemoveAll(filepath.Dir(builtBin))
	}
	os.Exit(code)
}

// freePort returns an unused TCP port in 35000-35999.
func freePort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 200; i++ {
		p := 35000 + int(time.Now().UnixNano()/1000+int64(i*7))%1000
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			l.Close()
			return p
		}
	}
	t.Fatal("no free port in 35000-35999")
	return 0
}

// pgInstance is one cluster started with the artifact's own pg_ctl.
type pgInstance struct {
	t    *testing.T
	bin  string
	dir  string
	port int
	log  string
}

func (p *pgInstance) ctl(args ...string) ([]byte, error) {
	cmd := exec.Command(filepath.Join(p.bin, "pg_ctl"), append([]string{"-D", p.dir}, args...)...)
	return cmd.CombinedOutput()
}

func (p *pgInstance) start() {
	p.t.Helper()
	out, err := p.ctl("-w", "-t", "120", "-l", p.log, "-o", fmt.Sprintf("-p %d", p.port), "start")
	if err != nil {
		log, _ := os.ReadFile(p.log)
		p.t.Fatalf("pg_ctl start: %v\n%s\n--- server log ---\n%s", err, out, tail(log, 4000))
	}
	p.t.Cleanup(p.stop)
}

// stop shuts the server down; it is safe to call when it is not running.
func (p *pgInstance) stop() {
	if _, err := os.Stat(filepath.Join(p.dir, "postmaster.pid")); err != nil {
		return
	}
	if _, err := p.ctl("-w", "-t", "60", "-m", "fast", "stop"); err != nil {
		p.ctl("-m", "immediate", "stop")
	}
}

func (p *pgInstance) dsn() string {
	return fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", p.port)
}

func (p *pgInstance) connect(ctx context.Context) *pgx.Conn {
	p.t.Helper()
	c, err := pgx.Connect(ctx, p.dsn())
	if err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// newSourceCluster runs initdb and writes the settings a project cluster gets:
// WAL archiving through the sbctl binary, small memory, 1 MiB WAL segments so a
// switch is cheap, TCP only on the given port.
func newSourceCluster(t *testing.T, bin, root, ref, archiveSettings string) *pgInstance {
	t.Helper()
	p := &pgInstance{t: t, bin: bin, dir: filepath.Join(root, "src", "pgdata"), port: freePort(t), log: filepath.Join(root, "src.log")}
	if err := os.MkdirAll(filepath.Dir(p.dir), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(filepath.Join(bin, "initdb"), "-D", p.dir, "-U", "postgres", "-A", "trust", "--no-sync",
		"-E", "UTF8", "--locale=C", "--wal-segsize=1").CombinedOutput()
	if err != nil {
		t.Fatalf("initdb: %v\n%s", err, out)
	}
	conf := fmt.Sprintf(`
listen_addresses = '127.0.0.1'
unix_socket_directories = ''
shared_buffers = 16MB
max_connections = 30
wal_level = replica
min_wal_size = 8MB
max_wal_size = 32MB
%s`, archiveSettings)
	f, err := os.OpenFile(filepath.Join(p.dir, "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(conf); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

// portAccess is a backup.Access over fixed loopback ports (trust authentication).
type portAccess struct {
	mu    sync.Mutex
	ports map[string]int
}

func (a *portAccess) set(ref string, port int) { a.mu.Lock(); a.ports[ref] = port; a.mu.Unlock() }

func (a *portAccess) ConnString(_ context.Context, ref, _ string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	port, ok := a.ports[ref]
	if !ok {
		return "", fmt.Errorf("no cluster for %s", ref)
	}
	return fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", port), nil
}

// pgManager is the slice of lifecycle.Manager that restore uses, over real clusters.
type pgManager struct {
	lifecycle.Manager
	t      *testing.T
	e      *testEnv
	bin    string
	root   string
	access *portAccess
	insts  map[string]*pgInstance
}

func (m *pgManager) Create(ctx context.Context, req lifecycle.CreateRequest) (*registry.Project, error) {
	p := &registry.Project{Ref: req.Ref, Name: req.Name, Engine: registry.EnginePostgres, Status: registry.StatusActiveHealthy, Region: req.Region}
	dir := filepath.Join(m.root, "restored-"+req.Ref, "pgdata")
	if err := req.Seed(ctx, p, dir); err != nil {
		return nil, err
	}
	inst := &pgInstance{t: m.t, bin: m.bin, dir: dir, port: freePort(m.t), log: filepath.Join(m.root, "restored-"+req.Ref+".log")}
	m.insts[req.Ref] = inst
	m.access.set(req.Ref, inst.port)
	// Return as soon as the server accepts connections, like lifecycle's health check:
	// with hot_standby on that can be while recovery is still replaying WAL.
	inst.start()
	if err := m.e.reg.CreateProject(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (m *pgManager) Pause(_ context.Context, ref string) error {
	m.insts[ref].stop()
	return nil
}

func (m *pgManager) Resume(_ context.Context, ref string) error {
	m.insts[ref].start()
	return nil
}

func waitFor(t *testing.T, what string, timeout time.Duration, fn func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		ok, err := fn()
		if ok {
			return
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (last error: %v)", what, last)
}

func queryIDs(t *testing.T, ctx context.Context, c *pgx.Conn) []int {
	t.Helper()
	rows, err := c.Query(ctx, "select id from public.t order by id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func idsString(ids []int) string { return fmt.Sprint(ids) }

// TestPointInTimeRestore is the workstream's required test: write, back up, write,
// note a time, destroy, then restore to the noted time (as a new project and in
// place) from a base backup plus WAL fetched through the real sbctl binary. The
// backend is a local directory.
func TestPointInTimeRestore(t *testing.T) {
	root := t.TempDir()
	archiveDir := filepath.Join(root, "archive")
	st, err := NewFileStore(archiveDir)
	if err != nil {
		t.Fatal(err)
	}
	runPointInTimeRestore(t, root, st, fmt.Sprintf("[backup]\nbackend = %q\nretention_days = 7\n", "file://"+archiveDir))
}

// TestPointInTimeRestoreS3Fake runs the scenario over the S3 backend against an
// in-process fake S3 server (gofakes3), which the sbctl children reach over loopback.
func TestPointInTimeRestoreS3Fake(t *testing.T) {
	pgBinDir(t)
	runPointInTimeRestoreS3(t, fakeS3(t))
}

// TestPointInTimeRestoreS3 is the same test over a real S3-compatible service: CI
// only, skipped without SBCTL_TEST_S3_* (see store_s3_test.go).
func TestPointInTimeRestoreS3(t *testing.T) {
	pgBinDir(t)
	runPointInTimeRestoreS3(t, s3FromEnv(t))
}

func runPointInTimeRestoreS3(t *testing.T, o S3Options) {
	st, err := NewS3Store(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	cleanS3(t, st)
	toml := fmt.Sprintf("[backup]\nbackend = %q\ns3_endpoint = %q\ns3_region = %q\ns3_force_path_style = true\ns3_access_key_id = %q\ns3_secret_access_key = %q\nretention_days = 7\n",
		"s3://"+o.Bucket+"/"+o.Prefix, o.Endpoint, o.Region, o.AccessKeyID, o.SecretKey)
	runPointInTimeRestore(t, t.TempDir(), st, toml)
}

// runPointInTimeRestore runs the scenario with store as the backend and backupToml as
// the [backup] section the sbctl children (archive_command, restore_command) read.
func runPointInTimeRestore(t *testing.T, root string, st Store, backupToml string) {
	bin := pgBinDir(t)
	sbctl := sbctlBinary(t)
	ctx := context.Background()
	e := newTestEnv(t)
	e.now = time.Now() // base backups and restores run on the real clock here
	e.svc.opt.Now = time.Now

	cfgPath := filepath.Join(root, "sbctl.toml")
	writeFile(t, cfgPath, []byte(fmt.Sprintf("state_dir = %q\nbin_path = %q\n\n%s", filepath.Join(root, "state"), sbctl, backupToml)))
	e.cfg.BinPath = sbctl
	e.svc.opt.Store, e.store = st, nil
	e.svc.opt.ConfigPath = cfgPath

	src := newSourceCluster(t, bin, root, testRef, ArchiveSettings(e.cfg, testRef, cfgPath))
	src.start()
	keys := e.addProject(t, testRef)
	_ = keys
	access := &portAccess{ports: map[string]int{testRef: src.port}}
	mgr := &pgManager{t: t, e: e, bin: bin, root: root, access: access, insts: map[string]*pgInstance{testRef: src}}
	e.svc.opt.Access, e.svc.opt.Manager = access, mgr
	e.svc.opt.DataDir = func(string) string { return src.dir }

	c := src.connect(ctx)
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := c.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	mustExec("create table public.t (id int primary key)")
	mustExec("insert into public.t values (1)")

	// Base backup of the running cluster.
	rec, err := e.svc.BaseBackup(ctx, testRef)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != registry.BackupCompleted || rec.Timeline != 1 || rec.StartLSN == "" || rec.StopLSN == "" || rec.SizeBytes <= 0 {
		t.Fatalf("registry row = %+v", rec)
	}
	ms, err := e.svc.ListBackups(ctx, testRef)
	if err != nil || len(ms) != 1 || ms[0].Files == 0 || ms[0].StartWAL == "" {
		t.Fatalf("manifests = %+v, %v", ms, err)
	}
	t.Logf("base backup %s: %d files, %d bytes stored, WAL %s..%s", ms[0].ID, ms[0].Files, ms[0].StoredBytes, ms[0].StartWAL, ms[0].StopWAL)

	mustExec("insert into public.t values (2)")
	var target time.Time
	if err := c.QueryRow(ctx, "select clock_timestamp()").Scan(&target); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond) // the drop must commit strictly after the target
	mustExec("drop table public.t")

	// Get the WAL holding the drop into the archive.
	var dropSeg string
	if err := c.QueryRow(ctx, "select pg_walfile_name(pg_current_wal_lsn())").Scan(&dropSeg); err != nil {
		t.Fatal(err)
	}
	mustExec("select pg_switch_wal()")
	waitFor(t, "WAL archiving of "+dropSeg, 60*time.Second, func() (bool, error) {
		var ok bool
		err := c.QueryRow(ctx, "select coalesce(last_archived_wal >= $1, false) from pg_stat_archiver", dropSeg).Scan(&ok)
		return ok, err
	})
	var failed int64
	if err := c.QueryRow(ctx, "select failed_count from pg_stat_archiver").Scan(&failed); err != nil || failed != 0 {
		t.Fatalf("archive_command failed %d times (err %v)", failed, err)
	}
	if _, err := c.Exec(ctx, "select * from public.t"); err == nil {
		t.Fatal("the table should be gone before the restore")
	}

	// Restore to the noted time as a new project: the source stays as it is.
	p, err := e.svc.Restore(ctx, testRef, target, testRef2)
	if err != nil {
		t.Fatal(err)
	}
	if p.Ref != testRef2 {
		t.Fatalf("restored project = %+v", p)
	}
	clone := mgr.insts[testRef2]
	rc := clone.connect(ctx)
	if ids := queryIDs(t, ctx, rc); idsString(ids) != "[1 2]" {
		t.Fatalf("restored rows = %v, want [1 2]", ids)
	}
	var recovery bool
	var cmd string
	if err := rc.QueryRow(ctx, "select pg_is_in_recovery(), current_setting('restore_command')").Scan(&recovery, &cmd); err != nil {
		t.Fatal(err)
	}
	if recovery {
		t.Error("restored cluster is still in recovery")
	}
	// restore_command reloads; recovery_target_* only change at the next restart, so
	// for those the file is what counts (checked below).
	waitFor(t, "restore_command to be cleared", 15*time.Second, func() (bool, error) {
		if err := rc.QueryRow(ctx, "select current_setting('restore_command')").Scan(&cmd); err != nil {
			return false, err
		}
		return cmd == "", fmt.Errorf("restore_command=%q", cmd)
	})
	autoConf, _ := os.ReadFile(filepath.Join(clone.dir, "postgresql.auto.conf"))
	if strings.Contains(string(autoConf), "restore_command") || strings.Contains(string(autoConf), "recovery_target") {
		t.Errorf("recovery settings left in postgresql.auto.conf:\n%s", autoConf)
	}
	if !strings.Contains(string(autoConf), "wal push --ref "+testRef2) {
		t.Errorf("the clone does not archive to its own ref:\n%s", autoConf)
	}
	// The clone archives to its own ref (its new timeline's history file), never the source's.
	waitFor(t, "the clone's first archived file", 60*time.Second, func() (bool, error) {
		_, err := st.Stat(ctx, walKey(testRef2, "00000002.history"))
		return err == nil, err
	})
	if _, err := st.Stat(ctx, walKey(testRef, "00000002.history")); err == nil {
		t.Error("the clone wrote timeline 2 into the source project's archive")
	}
	clone.stop()

	// Restore in place: refused without force, then done with it.
	if _, err := e.svc.RestoreWith(ctx, testRef, target, "", RestoreOptions{}); !errors.Is(err, ErrForceRequired) {
		t.Fatalf("in-place restore without force = %v", err)
	}
	c.Close(ctx)
	if _, err := e.svc.RestoreWith(ctx, testRef, target, "", RestoreOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	c2 := src.connect(ctx)
	if ids := queryIDs(t, ctx, c2); idsString(ids) != "[1 2]" {
		t.Fatalf("rows after in-place restore = %v, want [1 2]", ids)
	}
	asides, _ := filepath.Glob(src.dir + ".pre-restore-*")
	if len(asides) != 1 {
		t.Fatalf("old data directory not kept: %v", asides)
	}
	// The old timeline's drop is not part of the restored history.
	var tl int
	if err := c2.QueryRow(ctx, "select timeline_id from pg_control_checkpoint()").Scan(&tl); err != nil || tl < 2 {
		t.Errorf("timeline after restore = %d, %v", tl, err)
	}
	// The recovery settings are gone once RestoreWith returned: it waited for promotion
	// first (the manager returned while recovery could still be running).
	var inRec bool
	var restoreCmd string
	if err := c2.QueryRow(ctx, "select pg_is_in_recovery(), current_setting('restore_command')").Scan(&inRec, &restoreCmd); err != nil || inRec || restoreCmd != "" {
		t.Fatalf("after in-place restore: in recovery %v, restore_command %q, %v", inRec, restoreCmd, err)
	}
	// The restore took a base backup of the new timeline itself.
	waitFor(t, "archiving on the new timeline", 60*time.Second, func() (bool, error) {
		_, err := st.Stat(ctx, walKey(testRef, "00000002.history"))
		return err == nil, err
	})
	ms, err = e.svc.ListBackups(ctx, testRef)
	if err != nil {
		t.Fatal(err)
	}
	var post *Manifest
	for i := range ms {
		if ms[i].Reason == ReasonRestore {
			post = &ms[i]
		}
	}
	if post == nil || post.Timeline != 2 {
		t.Fatalf("no post-restore base backup on timeline 2: %+v", ms)
	}
	if _, err := e.svc.BaseBackup(ctx, testRef); err != nil {
		t.Fatalf("base backup after restore: %v", err)
	}

	// Restore to the end of the archive as another new project: a row written on the new
	// timeline after the restore must be there, with no target time to satisfy. The source
	// is running, so its newest WAL is archived first.
	if _, err := c2.Exec(ctx, "insert into public.t values (3)"); err != nil {
		t.Fatal(err)
	}
	p3, err := e.svc.RestoreWith(ctx, testRef, time.Time{}, testRef3, RestoreOptions{Latest: true})
	if err != nil {
		t.Fatal(err)
	}
	clone3 := mgr.insts[p3.Ref]
	if ids := queryIDs(t, ctx, clone3.connect(ctx)); idsString(ids) != "[1 2 3]" {
		t.Fatalf("rows restored to the latest state = %v, want [1 2 3]", ids)
	}
	clone3.stop()
}

// TestBaseBackupRefusals checks the guard rails: a cluster without archiving cannot
// be backed up usefully, and the error says what to configure.
func TestBaseBackupRefusals(t *testing.T) {
	bin := pgBinDir(t)
	ctx := context.Background()
	e := newTestEnv(t)
	src := newSourceCluster(t, bin, e.root, testRef, "") // archive_mode off
	src.start()
	e.addProject(t, testRef)
	e.svc.opt.Access = &portAccess{ports: map[string]int{testRef: src.port}}

	rec, err := e.svc.BaseBackup(ctx, testRef)
	if err == nil || !strings.Contains(err.Error(), "archive") {
		t.Fatalf("BaseBackup without archiving = %v", err)
	}
	if rec == nil || rec.Status != registry.BackupFailed || rec.Error == "" {
		t.Fatalf("registry row = %+v", rec)
	}
	if objs, _ := e.store.List(ctx, testRef+"/"); len(objs) != 0 {
		t.Fatalf("failed backup left objects: %v", objs)
	}
	if rows, _ := e.reg.ListBackups(ctx, testRef); len(rows) != 1 || rows[0].Status != registry.BackupFailed {
		t.Fatalf("rows = %+v", rows)
	}
}
