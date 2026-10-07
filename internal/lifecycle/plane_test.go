package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
	"github.com/OWNER/sbctl/internal/units"
)

var testNow = time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)

// shortTempDir returns a temp directory with a short path: t.TempDir on macOS is long
// enough to push unix socket paths over the OS limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "sbt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func testPlane(t *testing.T) (*PostgresPlane, *config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.Domain = "example.test"
	cfg.BinPath = "/usr/local/bin/sbctl"
	return NewPostgresPlane(cfg, nil, fakeArts{}, registry.NewMemory(), PlaneOptions{}), cfg
}

func testProject(cfg *config.Config, ref string, seq int) *registry.Project {
	return &registry.Project{Ref: ref, Seq: seq, Class: DefaultClass, Limits: cfg.Defaults, Engine: registry.EnginePostgres}
}

func testKeys(t *testing.T, ref string) *secrets.ProjectKeys {
	t.Helper()
	k, err := secrets.NewProjectKeys(ref, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestPostgresSpec(t *testing.T) {
	pl, cfg := testPlane(t)
	cfg.Backup.WALRelay = "off" // TestPostgresSpecArchivesThroughTheRelay covers the relay form
	p := testProject(cfg, "abcdefghijklmnopqrst", 2)
	keys := testKeys(t, p.Ref)
	spec, err := pl.postgresSpec(context.Background(), p, keys)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Service != config.SvcPostgres || spec.Unit() != "sb-postgres@abcdefghijklmnopqrst.service" || spec.ArtifactDir != "/art/postgres" {
		t.Fatalf("spec = %+v", spec)
	}
	args := strings.Join(spec.Exec, " ")
	pp := pl.paths(p)
	for _, want := range []string{
		"bin/supabase-postgres-start -p 20006 ",
		"-c listen_addresses=127.0.0.1",
		"-c unix_socket_directories=" + pp.Sock,
		"-c hba_file=" + pp.HBA,
		"-c wal_level=logical",
		"-c archive_mode=on",
		"-c archive_command='/usr/local/bin/sbctl' wal push --ref abcdefghijklmnopqrst %p",
		"-c shared_buffers=32MB",
		"-c max_connections=60",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	if spec.Env["PGDATA"] != pp.Data || spec.Env["PGSODIUM_KEY_FILE"] != pp.RootKey || spec.Env["POSTGRES_USER"] != RoleAdmin || spec.Env["POSTGRES_PASSWORD"] != keys.AdminPassword {
		t.Fatalf("env = %v", spec.Env)
	}
	if _, ok := spec.Env["SBCTL_CONFIG"]; ok {
		t.Fatal("SBCTL_CONFIG set without a config path")
	}
	// The run script must survive shell quoting of the archive command.
	run, err := units.FormatRun(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(run), `'archive_command='\''/usr/local/bin/sbctl'\'' wal push --ref abcdefghijklmnopqrst %p'`) {
		t.Fatalf("run script quoting:\n%s", run)
	}

	// Class and overrides.
	p.Class = "micro"
	pl.opts.ArchiveCommand, pl.opts.ConfigPath = "off", "/etc/sbctl/config.toml"
	spec, _ = pl.postgresSpec(context.Background(), p, keys)
	args = strings.Join(spec.Exec, " ")
	if !strings.Contains(args, "-c shared_buffers=16MB") || !strings.Contains(args, "-c max_connections=30") || !strings.Contains(args, "-c archive_mode=off") || strings.Contains(args, "archive_command") {
		t.Fatalf("micro/off args:\n%s", args)
	}
	if spec.Env["SBCTL_CONFIG"] != "/etc/sbctl/config.toml" {
		t.Fatalf("env = %v", spec.Env)
	}
	p.Class = "bogus"
	if _, err := pl.postgresSpec(context.Background(), p, keys); err == nil {
		t.Fatal("unknown class accepted")
	}
}

// On a systemd node the cluster archives through the daemon's relay socket and its unit gets
// neither the config file nor SBCTL_CONFIG.
func TestPostgresSpecArchivesThroughTheRelay(t *testing.T) {
	pl, cfg := testPlane(t)
	cfg.Backup.WALRelay = "on"
	pl.opts.ConfigPath = "/etc/sbctl/config.toml"
	p := testProject(cfg, "abcdefghijklmnopqrst", 2)
	spec, err := pl.postgresSpec(context.Background(), p, testKeys(t, p.Ref))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(spec.Exec, " ")
	want := "archive_command='/usr/local/bin/sbctl' wal push --ref abcdefghijklmnopqrst --socket '" + cfg.Paths().WALSocket(p.Ref) + "' %p"
	if !strings.Contains(args, want) {
		t.Fatalf("args lack %q:\n%s", want, args)
	}
	if _, ok := spec.Env["SBCTL_CONFIG"]; ok {
		t.Fatal("SBCTL_CONFIG is exported although the cluster archives through the relay and cannot read the config")
	}
	// prepare creates the relay directory before the cluster starts, and tells the daemon.
	var ready []string
	pl.opts.ArchiveReady = func(ref string) { ready = append(ready, ref) }
	if err := pl.prepare(p, testKeys(t, p.Ref)); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(cfg.Paths().WALDir(p.Ref)); err != nil || !fi.IsDir() {
		t.Fatalf("no relay directory: %v", err)
	}
	if len(ready) != 1 || ready[0] != p.Ref {
		t.Fatalf("ArchiveReady calls = %v", ready)
	}
}

func TestAPISpecs(t *testing.T) {
	pl, cfg := testPlane(t)
	p := testProject(cfg, "abcdefghijklmnopqrst", 2)
	keys := testKeys(t, p.Ref)
	specs, err := pl.apiSpecs(context.Background(), p, keys)
	if err != nil || len(specs) != 2 {
		t.Fatalf("specs = %d %v", len(specs), err)
	}
	auth, rest := specs[0], specs[1]
	if auth.Service != config.SvcGoTrue || rest.Service != config.SvcPostgREST {
		t.Fatal("order must be gotrue, postgrest")
	}
	if len(auth.PreStart) != 1 || strings.Join(auth.PreStart[0], " ") != "bin/auth migrate" {
		t.Fatalf("prestart = %v", auth.PreStart)
	}
	ports := cfg.PortsFor(p.Ref, 2)
	wantAuth := map[string]string{
		"GOTRUE_API_HOST":        "127.0.0.1",
		"GOTRUE_API_PORT":        "20007",
		"GOTRUE_DB_DRIVER":       "postgres",
		"GOTRUE_DB_DATABASE_URL": "postgres://supabase_auth_admin:" + keys.AuthAdminPassword + "@127.0.0.1:20006/postgres?sslmode=disable",
		"API_EXTERNAL_URL":       "https://abcdefghijklmnopqrst.api.example.test/auth/v1",
		// Email links must keep the /auth/v1 prefix (GoTrue resolves the path absolutely).
		"GOTRUE_MAILER_URLPATHS_INVITE":       "https://abcdefghijklmnopqrst.api.example.test/auth/v1/verify",
		"GOTRUE_MAILER_URLPATHS_CONFIRMATION": "https://abcdefghijklmnopqrst.api.example.test/auth/v1/verify",
		"GOTRUE_MAILER_URLPATHS_RECOVERY":     "https://abcdefghijklmnopqrst.api.example.test/auth/v1/verify",
		"GOTRUE_MAILER_URLPATHS_EMAIL_CHANGE": "https://abcdefghijklmnopqrst.api.example.test/auth/v1/verify",
		"GOTRUE_JWT_SECRET":                   keys.JWTSecret,
		"GOTRUE_JWT_AUD":                      "authenticated",
		"GOTRUE_DISABLE_SIGNUP":               "false",
	}
	for k, v := range wantAuth {
		if auth.Env[k] != v {
			t.Errorf("gotrue %s = %q, want %q", k, auth.Env[k], v)
		}
	}
	wantRest := map[string]string{
		"PGRST_SERVER_HOST":  "127.0.0.1",
		"PGRST_SERVER_PORT":  "20008",
		"PGRST_DB_URI":       "postgres://authenticator:" + keys.AuthenticatorPassword + "@127.0.0.1:20006/postgres?sslmode=disable",
		"PGRST_DB_ANON_ROLE": "anon",
		"PGRST_JWT_SECRET":   keys.JWTSecret,
	}
	for k, v := range wantRest {
		if rest.Env[k] != v {
			t.Errorf("postgrest %s = %q, want %q", k, rest.Env[k], v)
		}
	}
	if ports.GoTrue != 20007 || ports.PostgREST != 20008 {
		t.Fatalf("ports = %+v", ports)
	}
	for _, s := range specs {
		if s.Limits != cfg.Defaults {
			t.Errorf("%s limits = %+v", s.Service, s.Limits)
		}
		if _, err := units.FormatEnv(s.Env); err != nil {
			t.Errorf("%s env does not render: %v", s.Service, err)
		}
	}

	// The system project: GoTrue only, for Studio sign-in, signup closed.
	sys := testProject(cfg, config.SystemRef, 0)
	sysSpecs, err := pl.apiSpecs(context.Background(), sys, testKeys(t, config.SystemRef))
	if err != nil || len(sysSpecs) != 1 {
		t.Fatalf("system specs = %d %v", len(sysSpecs), err)
	}
	e := sysSpecs[0].Env
	if e["GOTRUE_SITE_URL"] != "https://studio.example.test" || e["API_EXTERNAL_URL"] != "https://api.example.test/auth/v1" ||
		e["GOTRUE_API_PORT"] != "9999" || e["GOTRUE_DISABLE_SIGNUP"] != "true" || !strings.Contains(e["GOTRUE_DB_DATABASE_URL"], "@127.0.0.1:5433/postgres") {
		t.Fatalf("system gotrue env = %v", e)
	}
	sysPG, _ := pl.postgresSpec(context.Background(), sys, testKeys(t, config.SystemRef))
	if !strings.Contains(strings.Join(sysPG.Exec, " "), "-c max_connections=100") || !strings.Contains(strings.Join(sysPG.Exec, " "), "-p 5433") {
		t.Fatalf("system postgres args = %v", sysPG.Exec)
	}

	cfg.TLS.Mode = "off"
	specs, _ = pl.apiSpecs(context.Background(), p, keys)
	if !strings.HasPrefix(specs[0].Env["API_EXTERNAL_URL"], "http://") {
		t.Fatalf("tls off must use http: %s", specs[0].Env["API_EXTERNAL_URL"])
	}
}

func TestPrepareWritesPrivateFiles(t *testing.T) {
	pl, cfg := testPlane(t)
	p := testProject(cfg, "abcdefghijklmnopqrst", 1)
	keys := testKeys(t, p.Ref)
	if err := pl.prepare(p, keys); err != nil {
		t.Fatal(err)
	}
	pp := pl.paths(p)
	for path, mode := range map[string]os.FileMode{pp.HBA: 0o600, pp.RootKey: 0o600, pp.Sock: 0o700} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v, want %v", path, fi, err, mode)
		}
	}
	if b, _ := os.ReadFile(pp.RootKey); string(b) != keys.PGSodiumRootKey {
		t.Fatal("pgsodium root key was not written")
	}
	hba, _ := os.ReadFile(pp.HBA)
	if strings.Contains(string(hba), "trust") && strings.Contains(string(hba), "127.0.0.1/32           trust") {
		t.Fatal("loopback must not be trusted")
	}
	for _, line := range strings.Split(string(hba), "\n") {
		if strings.HasPrefix(line, "host") && strings.Contains(line, "trust") {
			t.Fatalf("TCP rule trusts: %s", line)
		}
	}
	if _, err := os.Stat(cfg.Paths().ProjectService(p.Ref, config.SvcPostgREST)); err != nil {
		t.Fatal(err)
	}
	// Idempotent.
	if err := pl.prepare(p, keys); err != nil {
		t.Fatal(err)
	}
	// The system project has no PostgREST directory.
	sys := testProject(cfg, config.SystemRef, 0)
	if err := pl.prepare(sys, testKeys(t, config.SystemRef)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.Paths().ProjectService(config.SystemRef, config.SvcPostgREST)); err == nil {
		t.Fatal("system project got a postgrest directory")
	}
}

func TestPrepareRejectsLongSocketPath(t *testing.T) {
	pl, cfg := testPlane(t)
	cfg.StateDir = "/" + strings.Repeat("d", 80)
	p := testProject(cfg, "abcdefghijklmnopqrst", 1)
	err := pl.prepare(p, testKeys(t, p.Ref))
	if err == nil || !strings.Contains(err.Error(), "shorter state_dir") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureLauncherInvariants(t *testing.T) {
	dir := t.TempDir()
	if err := ensureLauncherInvariants(dir); err == nil {
		t.Fatal("empty seed accepted")
	}
	os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o600)
	if err := ensureLauncherInvariants(dir); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "postmaster.opts")); err != nil || fi.Size() == 0 {
		t.Fatalf("postmaster.opts: %v %v", fi, err)
	}
	// An existing non-empty file is kept.
	os.WriteFile(filepath.Join(dir, "postmaster.opts"), []byte("keep"), 0o600)
	ensureLauncherInvariants(dir)
	if b, _ := os.ReadFile(filepath.Join(dir, "postmaster.opts")); string(b) != "keep" {
		t.Fatal("postmaster.opts overwritten")
	}
}

func TestSocketDSNParses(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = "/tmp/it's a dir"
	dsn := RegistryDSN(cfg)
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cc := pc.ConnConfig
	if cc.Host != "/tmp/it's a dir/projects/system/postgres/sock" || cc.Port != 5433 || cc.User != RoleAdmin || cc.Database != "sbctl" || cc.Password != "" || pc.MaxConns != 6 {
		t.Fatalf("parsed = %+v max=%d", cc, pc.MaxConns)
	}
	cc2, err := pgx.ParseConfig(SystemSocketDSN(cfg, "_realtime"))
	if err != nil || cc2.Database != "_realtime" {
		t.Fatalf("%+v %v", cc2, err)
	}
}

func TestDSNURLEscapes(t *testing.T) {
	got := dsnURL("u", "p@ss/w:rd", 5, "postgres")
	if got != "postgres://u:p%40ss%2Fw%3Ard@127.0.0.1:5/postgres?sslmode=disable" {
		t.Fatalf("dsn = %s", got)
	}
}

// createDatabase refuses an existing cluster with ErrClusterExists, before it renders
// or writes anything into the project directory.
func TestCreateDatabaseRefusesExistingCluster(t *testing.T) {
	pl, cfg := testPlane(t)
	p := testProject(cfg, "abcdefghijklmnopqrst", 1)
	pp := pl.paths(p)
	if err := os.MkdirAll(pp.Data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pp.Data, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := pl.createDatabase(context.Background(), p, testKeys(t, p.Ref), nil)
	if !errors.Is(err, ErrClusterExists) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(pp.HBA); err == nil {
		t.Fatal("createDatabase wrote files before refusing")
	}
	if _, err := os.Stat(filepath.Join(pp.Data, "PG_VERSION")); err != nil {
		t.Fatal("existing data was touched")
	}
}

// recSup is a Supervisor that records calls, reports a fixed state and a configurable
// "changed" result from RenderChanged.
type recSup struct {
	calls   []string
	state   units.State
	changed bool
}

func (r *recSup) Render(context.Context, units.Spec) error {
	r.calls = append(r.calls, "render")
	return nil
}
func (r *recSup) RenderChanged(context.Context, units.Spec) (bool, error) {
	r.calls = append(r.calls, "render")
	return r.changed, nil
}
func (r *recSup) Start(context.Context, string) error { r.calls = append(r.calls, "start"); return nil }
func (r *recSup) Stop(context.Context, string) error  { r.calls = append(r.calls, "stop"); return nil }
func (r *recSup) Remove(context.Context, string) error {
	r.calls = append(r.calls, "remove")
	return nil
}
func (r *recSup) Status(context.Context, string) (units.Status, error) {
	return units.Status{State: r.state}, nil
}

// TestStartDatabaseAppliesChangedSettings: new rendered settings restart a running
// cluster, and leave a stopped or unchanged one alone.
func TestStartDatabaseAppliesChangedSettings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   units.State
		changed bool
		want    string
	}{
		{"running and changed", units.StateActive, true, "render stop start"},
		{"running and unchanged", units.StateActive, false, "render start"},
		{"stopped and changed", units.StateInactive, true, "render start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.StateDir = shortTempDir(t)
			cfg.Domain = "example.test"
			cfg.BinPath = "/usr/local/bin/sbctl"
			sup := &recSup{state: tc.state, changed: tc.changed}
			// The cluster never answers, so the wait after Start fails fast; only the
			// call order before it matters here.
			pl := NewPostgresPlane(cfg, sup, fakeArts{}, registry.NewMemory(), PlaneOptions{PostgresReadyTimeout: time.Millisecond})
			p := testProject(cfg, "abcdefghijklmnopqrst", 2)
			_ = pl.StartDatabase(context.Background(), p, testKeys(t, p.Ref))
			if got := strings.Join(sup.calls, " "); got != tc.want {
				t.Fatalf("calls = %q, want %q", got, tc.want)
			}
		})
	}
}

// Deleting a project also removes the bundler's unit instance of the project: the module
// cache of its uploads is private to that unit and goes with it.
type namedRemoveSup struct {
	recSup
	removed []string
}

func (n *namedRemoveSup) Remove(_ context.Context, unit string) error {
	n.removed = append(n.removed, unit)
	return nil
}

func TestPlaneDeleteRemovesTheBundlersInstanceOfTheProject(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	sup := &namedRemoveSup{}
	pl := NewPostgresPlane(cfg, sup, fakeArts{}, registry.NewMemory(), PlaneOptions{})
	const ref = "abcdefghijklmnopqrst"
	if err := pl.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range sup.removed {
		if u == "sb-edge-bundle@"+ref+".service" {
			found = true
		}
		if strings.Contains(u, "edge-bundle") && !strings.HasSuffix(u, "@"+ref+".service") {
			t.Errorf("removed %s, another project's or the singleton's unit", u)
		}
	}
	if !found {
		t.Errorf("the bundler's instance was not removed: %v", sup.removed)
	}
}

// A branch's Postgres unit is rendered with egress denied only once its isolation set the
// policy to denied: pending (first start), allowed (opt-out), unenforced, ordinary projects
// and schema-only branches are all rendered open.
func TestPostgresSpecDenyEgress(t *testing.T) {
	pl, cfg := testPlane(t)
	keys := testKeys(t, "abcdefghijklmnopqrst")
	for _, tc := range []struct {
		branch *registry.BranchInfo
		deny   bool
	}{
		{nil, false},
		{&registry.BranchInfo{ParentRef: "p"}, false},
		{&registry.BranchInfo{ParentRef: "p", Egress: registry.EgressPending}, false},
		{&registry.BranchInfo{ParentRef: "p", Egress: registry.EgressAllowed}, false},
		{&registry.BranchInfo{ParentRef: "p", Egress: registry.EgressUnenforced}, false},
		{&registry.BranchInfo{ParentRef: "p", Egress: registry.EgressDenied}, true},
	} {
		p := testProject(cfg, "abcdefghijklmnopqrst", 2)
		p.Branch = tc.branch
		spec, err := pl.postgresSpec(context.Background(), p, keys)
		if err != nil {
			t.Fatal(err)
		}
		if spec.DenyEgress != tc.deny {
			t.Errorf("branch %+v: DenyEgress = %v, want %v", tc.branch, spec.DenyEgress, tc.deny)
		}
	}
	// The API units never carry the restriction: only the cluster's own outbound calls matter.
	p := testProject(cfg, "abcdefghijklmnopqrst", 2)
	p.Branch = &registry.BranchInfo{ParentRef: "p", Egress: registry.EgressDenied}
	specs, err := pl.apiSpecs(context.Background(), p, keys)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if s.DenyEgress {
			t.Errorf("%s has DenyEgress", s.Unit())
		}
	}
}
