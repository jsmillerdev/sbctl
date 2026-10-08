package branching

// Integration tests on real Postgres clusters (the slim-services artifacts, exec backend).
// They need SUPAVISE_TEST_UNPACKED to name a directory with unpacked postgres-17*, auth-* and
// postgrest-* artifacts for this platform, for example ~/.cache/sbctl/unpacked. The state
// directory is a short directory under /tmp, or SUPAVISE_TEST_STATE_DIR (CI points it at an
// XFS mount). Ports come from 39000-39999.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pelletier/go-toml/v2"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

type dirArts map[string]string

func (d dirArts) Dir(svc string) (string, error) {
	if dir, ok := d[svc]; ok {
		return dir, nil
	}
	return "", fmt.Errorf("no artifact for %s", svc)
}
func (d dirArts) Tag(svc string) (string, error) { return filepath.Base(d[svc]), nil }

func freePortBase(t *testing.T, n int) int {
	t.Helper()
	for base := 39000; base+n < 39900; base += n + 3 {
		var ls []net.Listener
		ok := true
		for i := 0; i < n; i++ {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				ok = false
				break
			}
			ls = append(ls, l)
		}
		for _, l := range ls {
			l.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatal("no free port range in 39000-39999")
	return 0
}

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// supaviseBinary builds cmd/supavise once: it is every cluster's archive_command.
func supaviseBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "supavise-branch-bin-")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "supavise")
		cmd := exec.Command("go", "build", "-o", builtBin, "./cmd/supavise")
		_, thisFile, _, _ := runtime.Caller(0)
		cmd.Dir = filepath.Join(filepath.Dir(thisFile), "..", "..")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/supavise: %v\n%s", err, out)
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

// stack is a live node: system cluster, engine, backup service and branching service.
type stack struct {
	t    *testing.T
	cfg  *config.Config
	node *lifecycle.Node
	bk   *backup.Service
	svc  *Service
	root string
}

func newStack(t *testing.T) *stack {
	t.Helper()
	root := os.Getenv("SUPAVISE_TEST_UNPACKED")
	if root == "" || testing.Short() {
		t.Skip("SUPAVISE_TEST_UNPACKED not set")
	}
	arts := dirArts{}
	for svc, glob := range map[string]string{config.SvcPostgres: "postgres-17*", config.SvcGoTrue: "auth-*", config.SvcPostgREST: "postgrest-*"} {
		m, _ := filepath.Glob(filepath.Join(root, glob))
		if len(m) == 0 {
			t.Skipf("no %s artifact under %s", svc, root)
		}
		arts[svc] = m[len(m)-1]
	}
	state := os.Getenv("SUPAVISE_TEST_STATE_DIR")
	if state == "" {
		d, err := os.MkdirTemp("/tmp", "sbtb")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(d) })
		state = d
	} else {
		state = filepath.Join(state, fmt.Sprintf("sbtb%d", os.Getpid()))
		if err := os.MkdirAll(state, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(state) })
	}
	base := freePortBase(t, 40)
	cfg := config.Default()
	cfg.StateDir = state
	cfg.KeyPath = filepath.Join(state, "master.key")
	cfg.Supervisor = config.SupervisorExec
	cfg.Domain = "supavise.test"
	cfg.TLS.Mode = "off"
	cfg.BinPath = supaviseBinary(t)
	cfg.Backup.Backend = "file://" + filepath.Join(state, "backups")
	cfg.Backup.ArchiveTimeoutSeconds = 30
	// The free-disk check runs on the real disk here; CI's 8 GB loop file and a developer's
	// disk should not turn the default 2 GB reserve into a flaky refusal. The check itself is
	// covered by the unit tests.
	cfg.Branching.DiskReserveMB = 256
	cfg.Ports.SystemPostgres, cfg.Ports.SystemGoTrue, cfg.Ports.ProjectBase = base, base+1, base+2
	cfgPath := filepath.Join(state, "config.toml")
	b, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUPAVISE_CONFIG", cfgPath)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	oo := lifecycle.OpenOptions{Artifacts: arts, ConfigPath: cfgPath}
	t.Cleanup(func() {
		if err := lifecycle.StopAll(context.Background(), cfg, oo); err != nil {
			t.Errorf("StopAll: %v", err)
		}
	})
	n, err := lifecycle.InitSystem(ctx, cfg, oo, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)

	st := &stack{t: t, cfg: cfg, node: n, root: state}
	store, err := backup.OpenStore(ctx, cfg.Backup)
	if err != nil {
		t.Fatal(err)
	}
	if st.bk, err = backup.New(backup.Options{
		Config: cfg, Registry: n.Registry, Store: store, Secrets: n.Secrets,
		Access: backup.AccessFromRegistry(cfg, n.Registry, n.Secrets), ConfigPath: cfgPath,
		RecoveryTimeout: 3 * time.Minute, RecoveryPoll: 250 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	st.bk.SetManager(n.Engine)
	st.svc, err = New(Deps{Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets, Engine: n.Engine, Backup: st.bk, CreateWait: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.svc.Drain(context.Background()) })
	return st
}

func (st *stack) conn(ref, role string) *pgx.Conn {
	st.t.Helper()
	dsn, err := st.node.Engine.ConnString(context.Background(), ref, role)
	if err != nil {
		st.t.Fatal(err)
	}
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		st.t.Fatalf("connect to %s as %s: %v", ref, role, err)
	}
	st.t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func (st *stack) exec(ref, sql string) {
	st.t.Helper()
	if _, err := st.conn(ref, "postgres").Exec(context.Background(), sql); err != nil {
		st.t.Fatalf("%s: %s: %v", ref, sql, err)
	}
}

func (st *stack) count(ref, table string) int {
	st.t.Helper()
	var n int
	if err := st.conn(ref, "postgres").QueryRow(context.Background(), "select count(*) from "+table).Scan(&n); err != nil {
		st.t.Fatalf("%s: count %s: %v", ref, table, err)
	}
	return n
}

func (st *stack) migrations(ref string) []string {
	st.t.Helper()
	ms, err := st.svc.db.Migrations(context.Background(), ref)
	if err != nil {
		st.t.Fatal(err)
	}
	var v []string
	for _, m := range ms {
		v = append(v, m.Version)
	}
	return v
}

func (st *stack) wait(ref string) *Branch {
	st.t.Helper()
	b, err := st.svc.WaitIdle(context.Background(), ref)
	if err != nil {
		st.t.Fatal(err)
	}
	return b
}

func (st *stack) createBranch(parent, name string, mut func(*CreateInput)) *Branch {
	st.t.Helper()
	in := CreateInput{Name: name}
	if mut != nil {
		mut(&in)
	}
	started := time.Now()
	b, err := st.svc.Create(context.Background(), parent, in)
	if err != nil {
		st.t.Fatalf("create %s: %v", name, err)
	}
	b = st.wait(b.Ref)
	st.t.Logf("branch %s ready in %s: %s | %s", name, time.Since(started).Round(time.Millisecond), b.State, b.Detail)
	if b.State != registry.BranchMigrationsPassed {
		st.t.Fatalf("branch %s: %s: %s", name, b.State, b.Detail)
	}
	return b
}

// TestIntegrationBranching runs the whole feature on real clusters: schema-only and with_data
// branches, isolation in both directions, credentials, merge, push, reset, delete, expiry,
// and the base-backup path.
func TestIntegrationBranching(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	eng := st.node.Engine

	parent, err := eng.Create(ctx, lifecycle.CreateRequest{Name: "parent", Class: "micro"})
	if err != nil {
		t.Fatal(err)
	}
	pref := parent.Ref
	st.exec(pref, `create table public.items (id int primary key, name text);
		insert into public.items select g, 'row ' || g from generate_series(1, 200) g;
		create schema supabase_migrations;
		create table supabase_migrations.schema_migrations (version text not null primary key, statements text[], name text);
		insert into supabase_migrations.schema_migrations values
		  ('20260101000000', array['create table public.items (id int primary key, name text)'], 'items'),
		  ('20260102000000', array['create table public.tags (id int primary key)', 'alter table public.tags enable row level security'], 'tags')`)
	// Make the parent look like it was migrated: the tags table exists in the parent too.
	st.exec(pref, `create table public.tags (id int primary key); alter table public.tags enable row level security; insert into public.tags values (1)`)
	if err := st.svc.SetSeed(ctx, pref, `insert into public.items values (1, 'seeded')`); err != nil {
		t.Fatal(err)
	}
	pk, _ := eng.Keys(ctx, pref)

	// --- schema only: the parent's migrations and its seed, no rows -------------------------
	schema := st.createBranch(pref, "schema-only", nil)
	if schema.CloneMethod != MethodSchema || schema.WithData {
		t.Fatalf("branch = %+v", schema)
	}
	if got := st.migrations(schema.Ref); strings.Join(got, ",") != "20260101000000,20260102000000" {
		t.Fatalf("branch migrations = %v", got)
	}
	if n := st.count(schema.Ref, "public.items"); n != 1 { // only the seed
		t.Fatalf("schema-only branch has %d items, want the 1 seeded row", n)
	}
	if n := st.count(schema.Ref, "public.tags"); n != 0 {
		t.Fatalf("tags = %d", n)
	}
	bp, _ := eng.Keys(ctx, schema.Ref)
	if bp.DBPassword == pk.DBPassword || bp.JWTSecret == pk.JWTSecret {
		t.Fatal("a branch must have its own credentials")
	}
	if got := st.node.Cfg.PortsFor(schema.Ref, 2); got.Postgres == st.node.Cfg.PortsFor(pref, 1).Postgres {
		t.Fatal("a branch shares the parent's port")
	}
	// The registry knows the family.
	list, err := st.svc.List(ctx, pref)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v %v", list, err)
	}

	// --- with data: copy-on-write clone, or the base backup where the disk cannot clone -----------
	// SUPAVISE_TEST_EXPECT_METHOD names what this filesystem must do: clonefile, reflink or
	// base-backup (CI sets it for XFS and for ext4). Unset, a clone is expected.
	expect := os.Getenv("SUPAVISE_TEST_EXPECT_METHOD")
	if expect == MethodBackup {
		if _, err := st.bk.BaseBackup(ctx, pref); err != nil {
			t.Fatalf("base backup of the parent: %v", err)
		}
	}
	// A temporary CLI login role the parent issued (api createLoginRole) must not open the clone.
	st.exec(pref, `create role cli_login_parentcli login password 'parent-cli-password' valid until '2099-01-01 00:00:00+00'`)
	withData := st.createBranch(pref, "with-data", func(in *CreateInput) { in.WithData = true })
	var canLogin, hasPassword bool
	if err := st.conn(withData.Ref, lifecycle.RoleAdmin).QueryRow(ctx, `select rolcanlogin, rolpassword is not null from pg_authid where rolname = 'cli_login_parentcli'`).Scan(&canLogin, &hasPassword); err != nil {
		t.Fatalf("the parent's CLI login role on the clone: %v", err)
	}
	if canLogin || hasPassword {
		t.Fatalf("the parent's CLI login role still works on the clone: login=%v password=%v", canLogin, hasPassword)
	}
	t.Logf("with_data branch: method=%s detail=%s", withData.CloneMethod, withData.Detail)
	switch {
	case expect != "" && withData.CloneMethod != expect:
		t.Fatalf("method = %s, want %s: %+v", withData.CloneMethod, expect, withData)
	case expect == "" && !withData.WithData, expect == "" && withData.CloneMethod != MethodClonefile && withData.CloneMethod != MethodReflink:
		t.Fatalf("expected a copy-on-write clone on this filesystem, got %+v", withData)
	}
	if n := st.count(withData.Ref, "public.items"); n != 200 {
		t.Fatalf("clone has %d items, want 200", n)
	}
	if n := st.count(withData.Ref, "public.tags"); n != 1 {
		t.Fatalf("clone tags = %d", n)
	}
	// Isolation both ways.
	st.exec(withData.Ref, `insert into public.items values (1000, 'only on the branch'); delete from public.items where id <= 10`)
	if n := st.count(pref, "public.items"); n != 200 {
		t.Fatalf("a write on the branch changed the parent: %d items", n)
	}
	st.exec(pref, `insert into public.items values (2000, 'only on the parent')`)
	if n := st.count(withData.Ref, "public.items"); n != 191 {
		t.Fatalf("a write on the parent reached the branch: %d items", n)
	}
	// Credentials: rotated after the clone started, so the parent's password does not open the branch.
	wp, _ := eng.Keys(ctx, withData.Ref)
	if wp.DBPassword == pk.DBPassword || wp.AdminPassword == pk.AdminPassword || wp.JWTSecret == pk.JWTSecret || wp.AnonKey == pk.AnonKey {
		t.Fatal("clone kept the parent's credentials")
	}
	wports := st.node.Cfg.PortsFor(withData.Ref, 3)
	if c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable", pk.DBPassword, wports.Postgres)); err == nil {
		c.Close(ctx)
		t.Fatal("the parent's password opens the branch")
	}
	// The clone is a cluster of its own: its WAL archive is its own.
	var ac string
	if err := st.conn(withData.Ref, "postgres").QueryRow(ctx, `select current_setting('archive_command')`).Scan(&ac); err != nil || !strings.Contains(ac, "--ref "+withData.Ref) {
		t.Fatalf("archive_command = %q %v", ac, err)
	}
	// The services of the clone work with the rotated passwords.
	if hs := st.node.Plane; hs != nil {
		p, _ := st.node.Registry.GetProject(ctx, withData.Ref)
		for _, h := range st.node.Plane.Health(ctx, p, nil) {
			if !h.Healthy {
				t.Fatalf("clone service %s: %s %s", h.Name, h.Status, h.Error)
			}
		}
	}
	if err := func() error { _, err := st.svc.Delete(ctx, withData.Ref, DeleteOptions{}); return err }(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.cfg.Paths().Project(withData.Ref)); !os.IsNotExist(err) {
		t.Fatalf("data of a deleted branch left behind: %v", err)
	}

	// --- merge a new migration back, then push a parent migration down -----------------------
	st.exec(schema.Ref, `create table public.notes (id int primary key); insert into supabase_migrations.schema_migrations values
		('20260110000000', array['create table public.notes (id int primary key)'], 'notes')`)
	if _, err := st.svc.Merge(ctx, schema.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	if b := st.wait(schema.Ref); b.State != registry.BranchMigrationsPassed {
		t.Fatalf("merge: %s: %s", b.State, b.Detail)
	}
	if got := st.migrations(pref); got[len(got)-1] != "20260110000000" {
		t.Fatalf("parent migrations after merge = %v", got)
	}
	if n := st.count(pref, "public.notes"); n != 0 {
		t.Fatalf("notes in the parent = %d (the table must exist, empty)", n)
	}
	// A parent migration the branch lacks is pushed to it.
	st.exec(pref, `create table public.audit (id int); insert into supabase_migrations.schema_migrations values
		('20260111000000', array['create table public.audit (id int)'], 'audit')`)
	if _, err := st.svc.Push(ctx, schema.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	if b := st.wait(schema.Ref); b.State != registry.BranchMigrationsPassed {
		t.Fatalf("push: %s: %s", b.State, b.Detail)
	}
	if n := st.count(schema.Ref, "public.audit"); n != 0 {
		t.Fatalf("audit in the branch = %d", n)
	}
	// A diverged merge is refused and the parent is untouched.
	st.exec(pref, `insert into supabase_migrations.schema_migrations values ('20260112000000', array['select 1'], 'hotfix')`)
	st.exec(schema.Ref, `insert into supabase_migrations.schema_migrations values ('20260113000000', array['select 2'], 'feature')`)
	before := st.migrations(pref)
	if _, err := st.svc.Merge(ctx, schema.Ref, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	if b := st.wait(schema.Ref); b.State != registry.BranchMigrationsFailed || !strings.Contains(b.Detail, "diverged") {
		t.Fatalf("diverged merge: %s: %s", b.State, b.Detail)
	}
	if after := st.migrations(pref); len(after) != len(before) {
		t.Fatalf("a refused merge changed the parent: %v -> %v", before, after)
	}

	// --- reset: the branch is recreated from the parent, keeping ref, id and credentials ------
	st.exec(schema.Ref, `insert into public.items values (555, 'scratch')`)
	oldKeys, _ := eng.Keys(ctx, schema.Ref)
	if _, err := st.svc.Reset(ctx, schema.ID, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	reset := st.wait(schema.Ref)
	if reset.State != registry.BranchMigrationsPassed || reset.Ref != schema.Ref || reset.ID != schema.ID {
		t.Fatalf("reset: %+v", reset)
	}
	if n := st.count(reset.Ref, "public.items"); n != 1 { // only the seed again
		t.Fatalf("items after reset = %d", n)
	}
	if got := st.migrations(reset.Ref); got[len(got)-1] != "20260112000000" {
		t.Fatalf("migrations after reset = %v", got)
	}
	newKeys, _ := eng.Keys(ctx, reset.Ref)
	if newKeys.DBPassword != oldKeys.DBPassword || newKeys.JWTSecret != oldKeys.JWTSecret {
		t.Fatal("a schema-only reset must keep the branch's credentials")
	}

	// --- base-backup path (what an ext4 node does) -------------------------------------------
	if _, err := st.bk.BaseBackup(ctx, pref); err != nil {
		t.Fatalf("base backup of the parent: %v", err)
	}
	st.cfg.Branching.Clone = "backup"
	fromBackup := st.createBranch(pref, "from-backup", func(in *CreateInput) { in.WithData = true })
	st.cfg.Branching.Clone = ""
	if fromBackup.CloneMethod != MethodBackup {
		t.Fatalf("method = %s", fromBackup.CloneMethod)
	}
	if n := st.count(fromBackup.Ref, "public.items"); n != 201 { // 200 + the parent's later row
		t.Fatalf("restored branch items = %d, want 201", n)
	}
	fk, _ := eng.Keys(ctx, fromBackup.Ref)
	if fk.DBPassword == pk.DBPassword {
		t.Fatal("restored branch kept the parent's database password")
	}
	if fp, _ := st.node.Registry.GetProject(ctx, fromBackup.Ref); fp.Branch == nil || fp.Branch.ParentRef != pref || fp.Name != "from-backup" {
		t.Fatalf("restored project is not a branch of the parent: %+v", fp)
	}
	// Reset of a base-backup branch: the old cluster goes, the row stays, the restore goes into
	// the kept row, and the branch comes back with the parent's data and the same id and ref.
	st.exec(fromBackup.Ref, `insert into public.items values (777, 'scratch on the restored branch')`)
	st.cfg.Branching.Clone = "backup"
	if _, err := st.svc.Reset(ctx, fromBackup.ID, ActionInput{}); err != nil {
		t.Fatal(err)
	}
	again := st.wait(fromBackup.Ref)
	st.cfg.Branching.Clone = ""
	if again.State != registry.BranchMigrationsPassed || again.Ref != fromBackup.Ref || again.ID != fromBackup.ID || again.CloneMethod != MethodBackup {
		t.Fatalf("reset of the restored branch: %+v", again)
	}
	if n := st.count(again.Ref, "public.items"); n != 201 {
		t.Fatalf("restored branch items after reset = %d, want 201 (the scratch row must be gone)", n)
	}
	// The parent's archive is untouched by the restore, and the branch has its own.
	if _, err := st.svc.Delete(ctx, fromBackup.Ref, DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if objs, _ := st.bk.Store().List(ctx, fromBackup.Ref+"/"); len(objs) != 0 {
		t.Fatalf("an ephemeral branch left %d archive objects", len(objs))
	}
	if objs, _ := st.bk.Store().List(ctx, pref+"/"); len(objs) == 0 {
		t.Fatal("the parent's archive was purged")
	}

	// --- expiry --------------------------------------------------------------------------------
	short := st.createBranch(pref, "short-lived", func(in *CreateInput) { in.TTL = 2 * time.Second })
	time.Sleep(2500 * time.Millisecond)
	res, err := st.svc.Sweep(ctx, false)
	if err != nil || len(res.Expired) != 1 || res.Expired[0] != short.Ref {
		t.Fatalf("sweep = %+v %v", res, err)
	}
	if _, err := st.svc.Resolve(ctx, short.Ref); err == nil {
		t.Fatal("expired branch still exists")
	}

	// --- deleting the parent is refused while branches exist ---------------------------------
	if err := eng.Delete(ctx, pref); err == nil || !strings.Contains(err.Error(), "branches") {
		t.Fatalf("delete of a parent with branches: %v", err)
	}
	if _, err := os.Stat(st.cfg.Paths().ProjectService(pref, "postgres") + "/data/PG_VERSION"); err != nil {
		t.Fatalf("the refused delete touched the parent's data: %v", err)
	}
	if err := st.svc.DisableBranching(ctx, pref); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.svc.List(ctx, pref); len(list) != 1 {
		t.Fatalf("list after disable = %+v", list)
	}
	_ = strconv.Itoa
}

// TestIntegrationCloneSize measures a with_data branch of a parent of SUPAVISE_TEST_PARENT_MB
// megabytes (skipped when unset; CI uses 1024, a laptop run stays under 200). It reports the
// clone method, the wall time of the whole branch creation, the clone's own time, the
// parent's size and the disk the clone used, as one line "BRANCH_CLONE_RESULT {json}" and,
// in CI, as a table in the job summary. SUPAVISE_TEST_EXPECT_METHOD (reflink, clonefile,
// base-backup) makes the test fail when another method was used.
func TestIntegrationCloneSize(t *testing.T) {
	mb, _ := strconv.Atoi(os.Getenv("SUPAVISE_TEST_PARENT_MB"))
	if mb <= 0 {
		t.Skip("SUPAVISE_TEST_PARENT_MB not set")
	}
	st := newStack(t)
	ctx := context.Background()
	eng := st.node.Engine
	parent, err := eng.Create(ctx, lifecycle.CreateRequest{Name: "big parent", Class: "small"})
	if err != nil {
		t.Fatal(err)
	}
	pref := parent.Ref
	c := st.conn(pref, "postgres")
	if _, err := c.Exec(ctx, `create table public.big (id int primary key, payload text);
		alter table public.big alter column payload set storage external`); err != nil {
		t.Fatal(err)
	}
	// 100 KB rows, stored uncompressed: the table is as big on disk as it looks.
	const rowKB = 100
	rows := mb * 1024 / rowKB
	loadStart := time.Now()
	for done := 0; done < rows; {
		n := min(1000, rows-done)
		if _, err := c.Exec(ctx, fmt.Sprintf(`insert into public.big select g, repeat(md5(g::text), 3200) from generate_series(%d, %d) g`, done+1, done+n)); err != nil {
			t.Fatal(err)
		}
		done += n
	}
	if _, err := st.conn(pref, lifecycle.RoleAdmin).Exec(ctx, `checkpoint`); err != nil {
		t.Fatal(err)
	}
	var size int64
	if err := c.QueryRow(ctx, `select pg_database_size('postgres')`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	t.Logf("parent: %d rows, %s database, loaded in %s", rows, humanBytes(size), time.Since(loadStart).Round(time.Second))

	method := os.Getenv("SUPAVISE_TEST_EXPECT_METHOD")
	if method == MethodBackup {
		// The base-backup path restores the parent's latest base backup plus WAL.
		bstart := time.Now()
		if _, err := st.bk.BaseBackup(ctx, pref); err != nil {
			t.Fatal(err)
		}
		t.Logf("parent base backup: %s", time.Since(bstart).Round(time.Millisecond))
	}
	free0 := freeBytes(st.cfg.StateDir)
	start := time.Now()
	b := st.createBranch(pref, "size-test", func(in *CreateInput) { in.WithData = true })
	total := time.Since(start)
	free1 := freeBytes(st.cfg.StateDir)
	if method != "" && b.CloneMethod != method {
		t.Fatalf("clone method = %s, want %s (%s)", b.CloneMethod, method, b.Detail)
	}
	var n int
	if err := st.conn(b.Ref, "postgres").QueryRow(ctx, `select count(*) from public.big`).Scan(&n); err != nil || n != rows {
		t.Fatalf("branch has %d rows, want %d (%v)", n, rows, err)
	}
	// Writing to the branch does not touch the parent.
	st.exec(b.Ref, `delete from public.big where id <= 100`)
	var pn int
	if err := c.QueryRow(ctx, `select count(*) from public.big`).Scan(&pn); err != nil || pn != rows {
		t.Fatalf("parent has %d rows after a delete on the branch, want %d", pn, rows)
	}

	res := map[string]any{
		"method": b.CloneMethod, "filesystem": fsName(st.cfg.StateDir), "parent_db_bytes": size, "parent_rows": rows,
		"create_total_ms": total.Milliseconds(), "extra_disk_bytes": max(free0-free1, 0), "detail": b.Detail,
	}
	evs, _ := st.node.Registry.ListEvents(ctx, pref, 50)
	for _, e := range evs {
		if e.Kind == "branch.created" {
			var pl struct {
				Clone *CloneStats `json:"clone"`
			}
			if json.Unmarshal(e.Payload, &pl) == nil && pl.Clone != nil {
				res["clone"] = pl.Clone
			}
			break
		}
	}
	line, _ := json.Marshal(res)
	t.Logf("BRANCH_CLONE_RESULT %s", line)
	fmt.Printf("BRANCH_CLONE_RESULT %s\n", line)
	if f := os.Getenv("GITHUB_STEP_SUMMARY"); f != "" {
		out, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err == nil {
			defer out.Close()
			fmt.Fprintf(out, "\n#### Branch clone on %s (%s)\n\n| method | parent database | whole branch creation | clone copy | extra disk |\n|---|---|---|---|---|\n| %s | %s | %d ms | %s | %s |\n",
				fsName(st.cfg.StateDir), os.Getenv("SUPAVISE_TEST_LABEL"), b.CloneMethod, humanBytes(size), total.Milliseconds(), cloneMS(res), humanBytes(max(free0-free1, 0)))
		}
	}
	if _, err := st.svc.Delete(ctx, b.Ref, DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func cloneMS(res map[string]any) string {
	if c, ok := res["clone"].(*CloneStats); ok {
		return fmt.Sprintf("%d ms (%d files, %d WAL segments)", c.CopyMillis, c.Files, c.WALSegments)
	}
	return "n/a (base backup restore)"
}

// TestIntegrationCloneUnderWriteLoad clones a parent that is being written to and checkpointed
// the whole time, which is what the low-level backup procedure has to survive: the clone must
// recover, hold a state between the rows committed before and after, contain no duplicate or
// corrupt rows or index entries (amcheck), and have an empty unlogged table.
func TestIntegrationCloneUnderWriteLoad(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	if m := os.Getenv("SUPAVISE_TEST_EXPECT_METHOD"); m == MethodBackup {
		t.Skip("this filesystem uses the base-backup path; the write-load test covers the file-clone path")
	}
	parent, err := st.node.Engine.Create(ctx, lifecycle.CreateRequest{Name: "busy parent", Class: "small"})
	if err != nil {
		t.Fatal(err)
	}
	pref := parent.Ref
	st.exec(pref, `create table public.w (id bigserial primary key, wr int not null, v int not null, payload text not null);
		create index w_v on public.w (v);
		create unlogged table public.u (n int)`)

	const writers = 4
	conns := make([]*pgx.Conn, writers)
	for i := range conns {
		conns[i] = st.conn(pref, "postgres")
	}
	admin := st.conn(pref, lifecycle.RoleAdmin)
	loadCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var loadErrs []error
	note := func(err error) {
		if err != nil && loadCtx.Err() == nil {
			mu.Lock()
			loadErrs = append(loadErrs, err)
			mu.Unlock()
		}
	}
	for i, c := range conns {
		wg.Add(1)
		go func(i int, c *pgx.Conn) {
			defer wg.Done()
			for loadCtx.Err() == nil {
				// Each writer updates only its own rows: writers must not deadlock with each other.
				_, err := c.Exec(loadCtx, `insert into public.w (wr, v, payload) select $1, g, repeat('x', 300) from generate_series(1, 25) g`, i)
				note(err)
				_, err = c.Exec(loadCtx, `update public.w set v = v + 1 where id in (select id from public.w where wr = $1 order by id desc limit 25)`, i)
				note(err)
				_, err = c.Exec(loadCtx, `insert into public.u values (1)`)
				note(err)
			}
		}(i, c)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for loadCtx.Err() == nil {
			_, err := admin.Exec(loadCtx, `checkpoint`)
			note(err)
			select {
			case <-loadCtx.Done():
			case <-time.After(150 * time.Millisecond):
			}
		}
	}()
	probe := st.conn(pref, "postgres")
	maxID := func() int64 {
		var n int64
		if err := probe.QueryRow(ctx, `select coalesce(max(id), 0) from public.w`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	time.Sleep(time.Second)
	lo := maxID()
	b := st.createBranch(pref, "under-load", func(in *CreateInput) { in.WithData = true })
	hi := maxID()
	stop()
	wg.Wait()
	for _, err := range loadErrs {
		t.Errorf("writer: %v", err)
	}
	t.Logf("clone method %s; parent ids committed before the clone %d, after %d: %s", b.CloneMethod, lo, hi, b.Detail)
	if b.CloneMethod != MethodClonefile && b.CloneMethod != MethodReflink {
		t.Fatalf("method = %s, want a file clone", b.CloneMethod)
	}

	var maxB, n, distinct int64
	bc := st.conn(b.Ref, "postgres")
	if err := bc.QueryRow(ctx, `select coalesce(max(id), 0), count(*), count(distinct id) from public.w`).Scan(&maxB, &n, &distinct); err != nil {
		t.Fatal(err)
	}
	if maxB < lo || maxB > hi {
		t.Errorf("the clone's newest id %d is outside the window [%d, %d] committed around the clone", maxB, lo, hi)
	}
	if n != distinct {
		t.Errorf("the clone has %d rows but %d distinct ids", n, distinct)
	}
	if n < lo/2 { // sequences skip ids of rolled back inserts, but not by half
		t.Errorf("the clone has only %d rows for ids up to %d", n, maxB)
	}
	ba := st.conn(b.Ref, lifecycle.RoleAdmin)
	if _, err := ba.Exec(ctx, `create extension if not exists amcheck`); err != nil {
		t.Fatalf("amcheck: %v", err)
	}
	if _, err := ba.Exec(ctx, `select bt_index_check(index => 'public.w_pkey'::regclass, heapallindexed => true), bt_index_check(index => 'public.w_v'::regclass, heapallindexed => true)`); err != nil {
		t.Errorf("index check on the clone: %v", err)
	}
	var bad int
	if err := ba.QueryRow(ctx, `select count(*) from verify_heapam('public.w')`).Scan(&bad); err != nil || bad != 0 {
		t.Errorf("verify_heapam on the clone: %d problems (%v)", bad, err)
	}
	var unlogged int
	if err := bc.QueryRow(ctx, `select count(*) from public.u`).Scan(&unlogged); err != nil || unlogged != 0 {
		t.Errorf("the unlogged table has %d rows on the clone, want 0 (%v)", unlogged, err)
	}
	var tl int
	if err := ba.QueryRow(ctx, `select timeline_id from pg_control_checkpoint()`).Scan(&tl); err != nil || tl != 1 {
		t.Errorf("clone timeline = %d (%v), want 1", tl, err)
	}
	if _, err := st.svc.Delete(ctx, b.Ref, DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
}

// TestIntegrationCloneIsolatesTheParentsIntegrations gives the parent an enabled logical
// replication subscription (aimed at nothing that exists) and a pg_cron job, clones it, and
// checks that the branch has neither running: subscription disabled and detached from its slot,
// job inactive, and the node's ordinary settings back after the first start.
func TestIntegrationCloneIsolatesTheParentsIntegrations(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	parent, err := st.node.Engine.Create(ctx, lifecycle.CreateRequest{Name: "integrated parent", Class: "small"})
	if err != nil {
		t.Fatal(err)
	}
	pref := parent.Ref
	admin := st.conn(pref, lifecycle.RoleAdmin)
	for _, q := range []string{
		`create table public.t (id int primary key)`,
		`insert into public.t values (1), (2)`,
		// connect = false makes the subscription without contacting a publisher; it is then given
		// a slot name and enabled, which is what a live subscription looks like in the data
		// directory (subenabled, subslotname, subconninfo).
		`create subscription supavise_dummy connection 'host=127.0.0.1 port=1 dbname=nowhere user=nobody' publication nothing with (connect = false)`,
		`alter subscription supavise_dummy set (slot_name = 'supavise_dummy_slot')`,
		`alter subscription supavise_dummy enable`,
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	cron := true
	if _, err := admin.Exec(ctx, `create extension if not exists pg_cron`); err != nil {
		t.Logf("pg_cron is not usable on this cluster, the cron assertions are skipped: %v", err)
		cron = false
	} else if _, err := admin.Exec(ctx, `select cron.schedule('supavise-test-job', '* * * * *', 'select 1')`); err != nil {
		t.Logf("cannot schedule a pg_cron job, the cron assertions are skipped: %v", err)
		cron = false
	}
	var enabled bool
	if err := admin.QueryRow(ctx, `select subenabled from pg_subscription where subname = 'supavise_dummy'`).Scan(&enabled); err != nil || !enabled {
		t.Fatalf("the parent's subscription: enabled=%v err=%v", enabled, err)
	}
	var parentWorkers string
	if err := admin.QueryRow(ctx, `show max_logical_replication_workers`).Scan(&parentWorkers); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `checkpoint`); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SUPAVISE_TEST_EXPECT_METHOD") == MethodBackup {
		// Whatever the filesystem can do, take the base-backup path: its restore rewrites
		// postgresql.auto.conf with ALTER SYSTEM, which the first-start settings must survive.
		st.cfg.Branching.Clone = "backup"
		if _, err := st.bk.BaseBackup(ctx, pref); err != nil {
			t.Fatalf("base backup of the parent: %v", err)
		}
	}

	// Observe the first start itself, not only the end state: just before isolation runs the
	// cluster is on its first postmaster, which must already be running without logical
	// replication workers, cron launching jobs and pg_net on the parent's database. A clone that
	// starts with the parent's postgresql.auto.conf would show the node's settings here, and the
	// parent's enabled subscription would have an apply worker attached to its publisher.
	var firstStart sync.Map // branch ref -> string describing a violation, "" when quarantined
	realIsolate := st.svc.isolate
	st.svc.isolate = func(ctx context.Context, ref string) error {
		p, err := st.node.Registry.GetProject(ctx, ref)
		if err != nil {
			return err
		}
		c, err := connect(ctx, st.svc.adminSocketDSN(ref, p.Seq), "")
		if err != nil {
			return err
		}
		var workers, launch, netdb string
		var running int
		err = c.QueryRow(ctx, `select current_setting('max_logical_replication_workers'),
			coalesce(current_setting('cron.launch_active_jobs', true), 'unset'),
			coalesce(current_setting('pg_net.database_name', true), 'unset'),
			(select count(*) from pg_stat_activity where backend_type like 'logical replication%')`).Scan(&workers, &launch, &netdb, &running)
		closeConn(c)
		if err != nil {
			return err
		}
		bad := ""
		if workers != "0" || running != 0 {
			bad += fmt.Sprintf(" max_logical_replication_workers=%s, %d logical replication processes;", workers, running)
		}
		if launch == "on" {
			bad += " cron.launch_active_jobs=on;"
		}
		if netdb == "postgres" {
			bad += " pg_net.database_name=postgres;"
		}
		firstStart.Store(ref, bad)
		return realIsolate(ctx, ref)
	}
	defer func() { st.svc.isolate = realIsolate }()

	check := func(b *Branch, keepJobs bool, wantEgress string) {
		t.Helper()
		if b.Egress != wantEgress {
			t.Errorf("branch %s: egress = %q, want %q (%s)", b.Name, b.Egress, wantEgress, b.Detail)
		}
		if v, ok := firstStart.Load(b.Ref); !ok {
			t.Errorf("branch %s: the first start was not observed", b.Name)
		} else if v.(string) != "" {
			t.Errorf("branch %s: the first postmaster ran with the parent's integrations live:%s", b.Name, v)
		}
		ba := st.conn(b.Ref, lifecycle.RoleAdmin)
		var subs int
		var slot *string
		var on bool
		if err := ba.QueryRow(ctx, `select count(*), bool_or(subenabled), max(subslotname) from pg_subscription where subname = 'supavise_dummy'`).Scan(&subs, &on, &slot); err != nil {
			t.Fatal(err)
		}
		if subs != 1 || on || slot != nil {
			t.Errorf("branch %s: subscriptions=%d enabled=%v slot=%v, want 1 disabled, detached", b.Name, subs, on, slot)
		}
		if cron {
			var active bool
			if err := ba.QueryRow(ctx, `select active from cron.job where jobname = 'supavise-test-job'`).Scan(&active); err != nil {
				t.Fatal(err)
			}
			if active != keepJobs {
				t.Errorf("branch %s: cron job active = %v, want %v", b.Name, active, keepJobs)
			}
			// The jobs that were active are recorded, so that a later opt-in can restore them.
			var recorded int
			var hasTable bool
			if err := ba.QueryRow(ctx, `select to_regclass('`+PausedCronTable+`') is not null`).Scan(&hasTable); err != nil {
				t.Fatal(err)
			}
			if hasTable {
				if err := ba.QueryRow(ctx, `select count(*) from `+PausedCronTable+` p join cron.job j using (jobid) where j.jobname = 'supavise-test-job'`).Scan(&recorded); err != nil {
					t.Fatal(err)
				}
			}
			if keepJobs && hasTable || !keepJobs && recorded != 1 {
				t.Errorf("branch %s: paused-jobs table present=%v, recorded test job=%d (keep jobs %v)", b.Name, hasTable, recorded, keepJobs)
			}
		}
		// The first-start settings are gone and the cluster runs with the node's settings.
		var workers, launch string
		if err := ba.QueryRow(ctx, `show max_logical_replication_workers`).Scan(&workers); err != nil || workers != parentWorkers {
			t.Errorf("branch %s: max_logical_replication_workers = %q (%v), want %q", b.Name, workers, err, parentWorkers)
		}
		if cron {
			if err := ba.QueryRow(ctx, `show cron.launch_active_jobs`).Scan(&launch); err != nil || launch != "on" {
				t.Errorf("branch %s: cron.launch_active_jobs = %q (%v)", b.Name, launch, err)
			}
		}
		data := filepath.Join(st.cfg.Paths().ProjectService(b.Ref, config.SvcPostgres), "data")
		conf, _ := os.ReadFile(filepath.Join(data, "postgresql.auto.conf"))
		for _, q := range quarantineSettings {
			if strings.Contains(string(conf), q.key) {
				t.Errorf("branch %s: first-start setting %s left in postgresql.auto.conf:\n%s", b.Name, q.key, conf)
			}
		}
		if _, err := os.Stat(filepath.Join(data, quarantineSaved)); !os.IsNotExist(err) {
			t.Errorf("branch %s: the saved first-start lines are left behind (%v)", b.Name, err)
		}
		// The parent is untouched.
		var penabled bool
		if err := admin.QueryRow(ctx, `select subenabled from pg_subscription where subname = 'supavise_dummy'`).Scan(&penabled); err != nil || !penabled {
			t.Errorf("the parent's subscription changed: enabled=%v err=%v", penabled, err)
		}
		if n := st.count(b.Ref, "public.t"); n != 2 {
			t.Errorf("branch %s has %d rows, want 2", b.Name, n)
		}
	}
	b := st.createBranch(pref, "isolated", func(in *CreateInput) { in.WithData = true })
	t.Logf("method %s", b.CloneMethod)
	// This stack runs the exec supervisor: it cannot confine a unit's network, and the branch says so.
	// (The systemd path, a pg_net request to an external host that fails, is tests/linux/branching-egress.sh.)
	check(b, false, registry.EgressUnenforced)
	if !strings.Contains(b.Detail, "egress NOT blocked") || !strings.Contains(b.Detail, "cron jobs paused") {
		t.Errorf("the branch's detail does not say what it can reach: %s", b.Detail)
	}
	evs, _ := st.node.Registry.ListEvents(ctx, b.Ref, 50)
	var isolated bool
	for _, e := range evs {
		var res IsolateResult
		if e.Kind == "branch.isolated" && json.Unmarshal(e.Payload, &res) == nil && res.Subscriptions == 1 {
			isolated = true
			if cron && res.CronJobs != 1 {
				t.Errorf("isolation deactivated %d cron jobs, want 1", res.CronJobs)
			}
		}
	}
	if !isolated {
		t.Errorf("no branch.isolated event with one subscription in %d events", len(evs))
	}

	// keep_cron_jobs leaves the jobs as the parent had them; subscriptions are always detached.
	st.cfg.Branching.KeepCronJobs = true
	kept := st.createBranch(pref, "keeps-jobs", func(in *CreateInput) { in.WithData = true })
	check(kept, true, registry.EgressUnenforced)
	st.cfg.Branching.KeepCronJobs = false

	// allow_egress is the explicit opt-out: the parent's jobs stay as they were.
	open := st.createBranch(pref, "allows-egress", func(in *CreateInput) { in.WithData, in.AllowEgress = true, true })
	check(open, true, registry.EgressAllowed)
	if !strings.Contains(open.Detail, "allow_egress") {
		t.Errorf("the opt-out is not reported: %s", open.Detail)
	}
}

// A version that another merge applied while this one waited for the lock is refused, not run
// a second time.
func TestIntegrationApplyRefusesAVersionAlreadyApplied(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	parent, err := st.node.Engine.Create(ctx, lifecycle.CreateRequest{Name: "p", Class: "micro"})
	if err != nil {
		t.Fatal(err)
	}
	m := Migration{Version: "20260201000000", Name: "t", Statements: []string{"create table public.once (id int)"}}
	opts := ApplyOptions{Atomic: true, LockTimeout: 10 * time.Second}
	if _, err := st.svc.db.Apply(ctx, parent.Ref, []Migration{m}, opts); err != nil {
		t.Fatal(err)
	}
	// The same version again: the DDL would fail anyway, which is why the check comes before it.
	m2 := Migration{Version: m.Version, Name: "t", Statements: []string{"insert into public.once values (1)"}}
	_, err = st.svc.db.Apply(ctx, parent.Ref, []Migration{m2}, opts)
	if !errors.Is(err, ErrDiverged) {
		t.Fatalf("second apply = %v, want ErrDiverged", err)
	}
	if n := st.count(parent.Ref, "public.once"); n != 0 {
		t.Fatalf("the refused migration ran: %d rows", n)
	}
	// A Verify that refuses stops the apply before anything runs.
	m4 := Migration{Version: "20260203000000", Name: "verified", Statements: []string{"create table public.verified (id int)"}}
	vopts := opts
	vopts.Verify = func(cur []Migration) error {
		if len(cur) == 0 {
			t.Error("Verify saw an empty history")
		}
		return fmt.Errorf("%w: refused by Verify", ErrDiverged)
	}
	if _, err := st.svc.db.Apply(ctx, parent.Ref, []Migration{m4}, vopts); !errors.Is(err, ErrDiverged) {
		t.Fatalf("apply with a refusing Verify = %v, want ErrDiverged", err)
	}
	var verified bool
	if err := st.conn(parent.Ref, lifecycle.RoleAdmin).QueryRow(ctx, `select to_regclass('public.verified') is not null`).Scan(&verified); err != nil || verified {
		t.Fatalf("a refused apply ran: %v %v", verified, err)
	}
	// Two applies of the same new version at once: one wins, the other is refused after the lock.
	m3 := Migration{Version: "20260202000000", Name: "race", Statements: []string{"select pg_sleep(1)", "create table public.raced (id int)"}}
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := st.svc.db.Apply(ctx, parent.Ref, []Migration{m3}, opts); errs <- err }()
	}
	var ok, diverged int
	for i := 0; i < 2; i++ {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrDiverged):
			diverged++
		default:
			t.Errorf("racing apply: %v", err)
		}
	}
	if ok != 1 || diverged != 1 {
		t.Fatalf("racing applies: %d succeeded, %d refused (want 1 and 1)", ok, diverged)
	}
}
