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

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/units"
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
	cfg.BinPath = "/usr/local/bin/supavise"
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
	if spec.Service != config.SvcPostgres || spec.Unit() != "supavise-postgres@abcdefghijklmnopqrst.service" || spec.ArtifactDir != "/art/postgres" {
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
		"-c archive_command='/usr/local/bin/supavise' wal push --ref abcdefghijklmnopqrst %p",
		"-c shared_buffers=256MB",
		"-c max_connections=60",
		"-c cron.use_background_workers=on",
		"-c cron.database_name=postgres",
		"-c cron.max_running_jobs=8",
		"-c max_worker_processes=16",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	if spec.Env["PGDATA"] != pp.Data || spec.Env["PGSODIUM_KEY_FILE"] != pp.RootKey || spec.Env["POSTGRES_USER"] != RoleAdmin || spec.Env["POSTGRES_PASSWORD"] != keys.AdminPassword {
		t.Fatalf("env = %v", spec.Env)
	}
	if _, ok := spec.Env["SUPAVISE_CONFIG"]; ok {
		t.Fatal("SUPAVISE_CONFIG set without a config path")
	}
	// The run script must survive shell quoting of the archive command.
	run, err := units.FormatRun(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(run), `'archive_command='\''/usr/local/bin/supavise'\'' wal push --ref abcdefghijklmnopqrst %p'`) {
		t.Fatalf("run script quoting:\n%s", run)
	}

	// Class and overrides.
	p.Class = "nano"
	pl.opts.ArchiveCommand, pl.opts.ConfigPath = "off", "/etc/supavise/config.toml"
	spec, _ = pl.postgresSpec(context.Background(), p, keys)
	args = strings.Join(spec.Exec, " ")
	if !strings.Contains(args, "-c shared_buffers=128MB") || !strings.Contains(args, "-c max_connections=60") || !strings.Contains(args, "-c archive_mode=off") || strings.Contains(args, "archive_command") {
		t.Fatalf("micro/off args:\n%s", args)
	}
	if spec.Env["SUPAVISE_CONFIG"] != "/etc/supavise/config.toml" {
		t.Fatalf("env = %v", spec.Env)
	}
	p.Class = "bogus"
	if _, err := pl.postgresSpec(context.Background(), p, keys); err == nil {
		t.Fatal("unknown class accepted")
	}
}

// On a systemd node the cluster archives through the daemon's relay socket and its unit gets
// neither the config file nor SUPAVISE_CONFIG.
func TestPostgresSpecArchivesThroughTheRelay(t *testing.T) {
	pl, cfg := testPlane(t)
	cfg.Backup.WALRelay = "on"
	pl.opts.ConfigPath = "/etc/supavise/config.toml"
	p := testProject(cfg, "abcdefghijklmnopqrst", 2)
	spec, err := pl.postgresSpec(context.Background(), p, testKeys(t, p.Ref))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(spec.Exec, " ")
	want := "archive_command='/usr/local/bin/supavise' wal push --ref abcdefghijklmnopqrst --socket '" + cfg.Paths().WALSocket(p.Ref) + "' %p"
	if !strings.Contains(args, want) {
		t.Fatalf("args lack %q:\n%s", want, args)
	}
	if _, ok := spec.Env["SUPAVISE_CONFIG"]; ok {
		t.Fatal("SUPAVISE_CONFIG is exported although the cluster archives through the relay and cannot read the config")
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
	if _, ok := e["GOTRUE_SMTP_HOST"]; ok {
		t.Fatalf("no [mail], no SMTP in the system gotrue env: %v", e)
	}
	// [mail] reaches supavise-gotrue@system (invitations by email) and no project's GoTrue.
	cfg.Mail = config.Mail{SMTPHost: "smtp.example.test", SMTPFrom: "ops@example.test"}
	withMail, _ := pl.apiSpecs(context.Background(), sys, testKeys(t, config.SystemRef))
	if withMail[0].Env["GOTRUE_SMTP_HOST"] != "smtp.example.test" || withMail[0].Env["GOTRUE_SMTP_ADMIN_EMAIL"] != "ops@example.test" {
		t.Fatalf("system gotrue env with mail = %v", withMail[0].Env)
	}
	if proj, _ := pl.apiSpecs(context.Background(), p, keys); proj[0].Env["GOTRUE_SMTP_HOST"] != "" {
		t.Fatal("a project's GoTrue must not get the dashboard's relay")
	}
	cfg.Mail = config.Mail{}
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
	if cc.Host != "/tmp/it's a dir/projects/system/postgres/sock" || cc.Port != 5433 || cc.User != RoleAdmin || cc.Database != "supavise" || cc.Password != "" || pc.MaxConns != 6 {
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
	since   time.Time // when the running unit started; zero: unknown
	// render, when set, rewrites the unit's files in RenderChanged, as a render that changed them does.
	render func(units.Spec)
}

func (r *recSup) Render(context.Context, units.Spec) error {
	r.calls = append(r.calls, "render")
	return nil
}
func (r *recSup) RenderChanged(_ context.Context, spec units.Spec) (bool, error) {
	r.calls = append(r.calls, "render")
	if r.changed && r.render != nil {
		r.render(spec)
	}
	return r.changed, nil
}
func (r *recSup) Start(context.Context, string) error { r.calls = append(r.calls, "start"); return nil }
func (r *recSup) Stop(context.Context, string) error  { r.calls = append(r.calls, "stop"); return nil }
func (r *recSup) Remove(context.Context, string) error {
	r.calls = append(r.calls, "remove")
	return nil
}
func (r *recSup) Status(context.Context, string) (units.Status, error) {
	return units.Status{State: r.state, Since: r.since}, nil
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
			cfg.BinPath = "/usr/local/bin/supavise"
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

// GoTrue and PostgREST keep the binary they started with unless the unit is stopped: a unit
// whose rendered files changed (a release or a setting changed what it runs) restarts, one
// that did not change is left alone, and a stopped one is only started.
func TestStartAPIRestartsUnitsWhoseFilesChanged(t *testing.T) {
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
			cfg.BinPath = "/usr/local/bin/supavise"
			sup := &recSup{state: tc.state, changed: tc.changed}
			pl := NewPostgresPlane(cfg, sup, fakeArts{}, registry.NewMemory(), PlaneOptions{ServiceReadyTimeout: time.Millisecond})
			p := testProject(cfg, "abcdefghijklmnopqrst", 2)
			// Nothing answers, so the wait after the first unit's Start fails; the order of the
			// calls before it is what is checked.
			_ = pl.startAPI(context.Background(), p, testKeys(t, p.Ref))
			if got := strings.Join(sup.calls, " "); got != tc.want {
				t.Fatalf("calls = %q, want %q", got, tc.want)
			}
		})
	}
}

// writeAPIFiles writes the rendered files of the project's GoTrue and PostgREST with the given
// modification time, the way a render leaves them.
func writeAPIFiles(t *testing.T, cfg *config.Config, ref string, mtime time.Time) {
	t.Helper()
	for _, svc := range []string{config.SvcGoTrue, config.SvcPostgREST} {
		files := units.FilesFor(cfg, units.Spec{Service: svc, Ref: ref})
		for _, f := range []string{files.Env, files.Run} {
			if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(f, mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// While an upgrade moves the node forward, the daemon renders the API units and leaves the running
// ones on their old files; the restart is the rollout's. The same holds for files that an earlier
// render wrote after the process started, which is how a held-back restart shows later.
func TestStartAPIDefersRestartsWhenAskedAndFindsHeldBackFiles(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		changed  bool
		started  time.Time // when the unit's process started
		deferred bool
		want     string
	}{
		{"changed now", true, now.Add(-time.Hour), false, "render stop start"},
		{"changed now, deferred", true, now.Add(-time.Hour), true, "render start"},
		{"rendered after the process started", false, now.Add(-time.Hour), false, "render stop start"},
		{"rendered after the process started, deferred", false, now.Add(-time.Hour), true, "render start"},
		{"process started after the render", false, now.Add(time.Hour), false, "render start"},
		{"start time unknown", false, time.Time{}, false, "render start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.StateDir = shortTempDir(t)
			cfg.Domain = "example.test"
			cfg.BinPath = "/usr/local/bin/supavise"
			writeAPIFiles(t, cfg, "abcdefghijklmnopqrst", now)
			sup := &recSup{state: units.StateActive, changed: tc.changed, since: tc.started}
			pl := NewPostgresPlane(cfg, sup, fakeArts{}, registry.NewMemory(), PlaneOptions{ServiceReadyTimeout: time.Millisecond})
			p := testProject(cfg, "abcdefghijklmnopqrst", 2)
			ctx := context.Background()
			if tc.deferred {
				ctx = DeferRestarts(ctx)
			}
			_ = pl.startAPI(ctx, p, testKeys(t, p.Ref))
			if got := strings.Join(sup.calls, " "); got != tc.want {
				t.Fatalf("calls = %q, want %q", got, tc.want)
			}
		})
	}
}

// The rollout asks whether a project has a restart held back, and does it.
func TestRestartPendingRestartsOnlyUnitsOnOlderFiles(t *testing.T) {
	now := time.Now()
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.Domain = "example.test"
	cfg.BinPath = "/usr/local/bin/supavise"
	writeAPIFiles(t, cfg, "abcdefghijklmnopqrst", now)
	p := testProject(cfg, "abcdefghijklmnopqrst", 2)

	sup := &recSup{state: units.StateActive, since: now.Add(time.Hour)} // started after the files
	pl := NewPostgresPlane(cfg, sup, fakeArts{}, registry.NewMemory(), PlaneOptions{ServiceReadyTimeout: time.Millisecond})
	if pending, err := pl.PendingRestart(context.Background(), p, testKeys(t, p.Ref)); err != nil || pending {
		t.Fatalf("up to date: pending = %v, %v", pending, err)
	}
	if restarted, err := pl.RestartPending(context.Background(), p, testKeys(t, p.Ref)); err != nil || restarted {
		t.Fatalf("up to date: restarted = %v, %v", restarted, err)
	}
	if strings.Contains(strings.Join(sup.calls, " "), "stop") {
		t.Fatalf("a unit on its current files was stopped: %v", sup.calls)
	}

	sup = &recSup{state: units.StateActive, since: now.Add(-time.Hour)} // started before the files
	pl = NewPostgresPlane(cfg, sup, fakeArts{}, registry.NewMemory(), PlaneOptions{ServiceReadyTimeout: time.Millisecond})
	if pending, err := pl.PendingRestart(context.Background(), p, testKeys(t, p.Ref)); err != nil || !pending {
		t.Fatalf("held back: pending = %v, %v", pending, err)
	}
	sup.calls = nil
	// Nothing answers, so the wait after the start fails; the stop of each unit comes first.
	_, _ = pl.RestartPending(context.Background(), p, testKeys(t, p.Ref))
	// (The fake supervisor keeps reporting the unit active, so the start that follows stops it
	// once more; a real one reports it inactive.)
	if got := strings.Join(sup.calls, " "); !strings.Contains(got, "stop stop") || !strings.Contains(got, "start") {
		t.Fatalf("calls = %q: both API units stop, then start", got)
	}
}

// A restart of the cluster stops the project's GoTrue and PostgREST with it, so it is held back
// with theirs: while an upgrade moves the node, the daemon renders the cluster's files and leaves a
// running cluster on its old ones.
func TestStartDatabaseDefersTheRestartWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    units.State
		deferred bool
		want     string
	}{
		{"running, changed", units.StateActive, false, "render stop start"},
		{"running, changed, deferred", units.StateActive, true, "render start"},
		{"stopped, changed, deferred", units.StateInactive, true, "render start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.StateDir = shortTempDir(t)
			cfg.Domain = "example.test"
			cfg.BinPath = "/usr/local/bin/supavise"
			sup := &recSup{state: tc.state, changed: true}
			pl := NewPostgresPlane(cfg, sup, fakeArts{}, registry.NewMemory(), PlaneOptions{PostgresReadyTimeout: time.Millisecond})
			p := testProject(cfg, "abcdefghijklmnopqrst", 2)
			ctx := context.Background()
			if tc.deferred {
				ctx = DeferRestarts(ctx)
			}
			_ = pl.StartDatabase(ctx, p, testKeys(t, p.Ref))
			if got := strings.Join(sup.calls, " "); got != tc.want {
				t.Fatalf("calls = %q, want %q", got, tc.want)
			}
		})
	}
}

// namedSup records the unit each start and stop names.
type namedSup struct {
	recSup
	units []string
}

func (n *namedSup) Start(ctx context.Context, unit string) error {
	n.units = append(n.units, "start "+unit)
	return n.recSup.Start(ctx, unit)
}

func (n *namedSup) Stop(ctx context.Context, unit string) error {
	n.units = append(n.units, "stop "+unit)
	return n.recSup.Stop(ctx, unit)
}

// The rollout finds a cluster the daemon left on older files and restarts it: GoTrue and PostgREST
// stop first (their units require the cluster), then the cluster, and it starts again on the
// rendered files. A project whose cluster is current is not touched, and neither is one whose only
// newer file is a setting an Owner saved without restarting.
func TestRestartPendingRestartsAClusterOnOlderFiles(t *testing.T) {
	const ref = "abcdefghijklmnopqrst"
	now := time.Now()
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.Domain = "example.test"
	cfg.BinPath = "/usr/local/bin/supavise"
	p := testProject(cfg, ref, 2)
	pgSpec := units.Spec{Service: config.SvcPostgres, Ref: ref}
	version := "v1"
	writeVersion := func(spec units.Spec) {
		files := units.FilesFor(cfg, spec)
		for _, f := range []string{files.Env, files.Run} {
			if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f, []byte(version+spec.Service), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeAPIFiles(t, cfg, ref, now.Add(-2*time.Hour))
	writeVersion(pgSpec)
	pgFiles := units.FilesFor(cfg, pgSpec)
	newPlane := func(started time.Time) (*PostgresPlane, *namedSup) {
		sup := &namedSup{recSup: recSup{state: units.StateActive, since: started, render: writeVersion}}
		return NewPostgresPlane(cfg, sup, fakeArts{}, registry.NewMemory(), PlaneOptions{PostgresReadyTimeout: time.Millisecond, ServiceReadyTimeout: time.Millisecond}), sup
	}

	pl, sup := newPlane(now.Add(time.Hour)) // started after every file
	if pending, err := pl.PendingRestart(context.Background(), p, testKeys(t, ref)); err != nil || pending {
		t.Fatalf("current: pending = %v, %v", pending, err)
	}
	if restarted, err := pl.RestartPending(context.Background(), p, testKeys(t, ref)); err != nil || restarted || len(sup.units) != 0 {
		t.Fatalf("current: restarted = %v, %v, units %v", restarted, err, sup.units)
	}

	// An Owner saved a setting without restarting: the cluster's files are newer than its process,
	// and the upgrade owes no restart for that.
	pl, _ = newPlane(now.Add(-time.Hour))
	if err := os.Chtimes(pgFiles.Env, now, now); err != nil {
		t.Fatal(err)
	}
	if pending, err := pl.PendingRestart(context.Background(), p, testKeys(t, ref)); err != nil || pending {
		t.Fatalf("saved setting: pending = %v, %v", pending, err)
	}

	// The new release renders different files: the daemon's start leaves the cluster running and
	// records the owed restart.
	pl, sup = newPlane(now.Add(-time.Hour))
	version, sup.changed = "v2", true
	_ = pl.StartDatabase(DeferRestarts(context.Background()), p, testKeys(t, ref))
	if len(sup.units) != 1 || !strings.HasPrefix(sup.units[0], "start ") {
		t.Fatalf("a deferred start touched the running cluster: %v", sup.units)
	}
	if !HeldRestart(cfg, ref) {
		t.Fatal("the deferred restart left no mark")
	}
	sup.changed, sup.units = false, nil
	if pending, err := pl.PendingRestart(context.Background(), p, testKeys(t, ref)); err != nil || !pending {
		t.Fatalf("held back: pending = %v, %v", pending, err)
	}
	// A later start of the daemon, with no upgrade deferring, leaves the cluster alone too: its
	// restart is the rollout's, and the mark keeps it from being forgotten.
	_ = pl.StartDatabase(context.Background(), p, testKeys(t, ref))
	if len(sup.units) != 1 || !strings.HasPrefix(sup.units[0], "start ") || !HeldRestart(cfg, ref) {
		t.Fatalf("a later start did not leave the held-back cluster alone: %v, held %v", sup.units, HeldRestart(cfg, ref))
	}
	sup.units = nil
	// The rollout restarts it, even when it carries a deferral. (Nothing answers, so the wait for the
	// cluster ends the call; the order of the stops and the start is what is checked.)
	_, _ = pl.RestartPending(DeferRestarts(context.Background()), p, testKeys(t, ref))
	cluster := config.UnitName(config.SvcPostgres, ref)
	var want []string
	if hasPostgREST(ref) {
		want = append(want, "stop "+config.UnitName(config.SvcPostgREST, ref))
	}
	want = append(want, "stop "+config.UnitName(config.SvcGoTrue, ref), "stop "+cluster, "start "+cluster)
	if got := strings.Join(sup.units, ", "); got != strings.Join(want, ", ") {
		t.Fatalf("units:\n got %s\nwant %s", got, strings.Join(want, ", "))
	}
	if HeldRestart(cfg, ref) {
		t.Fatal("the restart left its mark behind")
	}
}

// A rollback renders the old release's files back. A cluster the rollout never restarted still runs
// them, so the daemon of the old release must not restart it (its mark names the digest of the
// files it runs); a cluster the rollout did restart runs the new files and is restarted onto the
// old ones. The same holds for GoTrue and PostgREST, and the render that put the files back must not
// look like a restart that is owed later.
func TestRollbackRestartsOnlyUnitsTheRolloutRestarted(t *testing.T) {
	const reached, untouched = "abcdefghijklmnopqrst", "tsrqponmlkjihgfedcba"
	now := time.Now()
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.Domain = "example.test"
	cfg.BinPath = "/usr/local/bin/supavise"
	version := "v1"
	writeVersion := func(spec units.Spec) {
		files := units.FilesFor(cfg, spec)
		for _, f := range []string{files.Env, files.Run} {
			if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f, []byte(version+spec.Service), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(f, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	specs := func(ref string) []units.Spec {
		out := []units.Spec{{Service: config.SvcPostgres, Ref: ref}, {Service: config.SvcGoTrue, Ref: ref}}
		if hasPostgREST(ref) {
			out = append(out, units.Spec{Service: config.SvcPostgREST, Ref: ref})
		}
		return out
	}
	for _, ref := range []string{reached, untouched} {
		for _, spec := range specs(ref) {
			writeVersion(spec)
		}
	}
	started := now.Add(-time.Hour)
	newPlane := func() (*PostgresPlane, *namedSup) {
		sup := &namedSup{recSup: recSup{state: units.StateActive, since: started, render: writeVersion}}
		return NewPostgresPlane(cfg, sup, fakeArts{}, registry.NewMemory(), PlaneOptions{PostgresReadyTimeout: time.Millisecond, ServiceReadyTimeout: time.Millisecond}), sup
	}
	startAll := func(pl *PostgresPlane, ctx context.Context, ref string) {
		p := testProject(cfg, ref, 2)
		_ = pl.StartDatabase(ctx, p, testKeys(t, ref))
		for _, spec := range specs(ref)[1:] {
			_ = pl.renderStopIfChanged(ctx, spec)
		}
	}

	// The new release's daemon renders v2 for both projects and holds the restarts back; the
	// rollout restarts one of them, which is the canary that failed.
	pl, sup := newPlane()
	version, sup.changed = "v2", true
	for _, ref := range []string{reached, untouched} {
		startAll(pl, DeferRestarts(context.Background()), ref)
	}
	for _, u := range sup.units {
		if strings.HasPrefix(u, "stop ") {
			t.Fatalf("the deferred starts stopped a unit: %v", sup.units)
		}
	}
	sup.changed = false
	_, _ = pl.RestartPending(context.Background(), testProject(cfg, reached, 2), testKeys(t, reached))
	// The restarted project's process is newer than its files now.
	restartedAt := time.Now().Add(time.Hour)
	sup.since = restartedAt

	// The old release's daemon renders v1 back, with no deferral (the units are the old daemon's to
	// start, which a daemon built before the marks would also restart in full).
	version = "v1"
	pl2, sup2 := newPlane()
	sup2.changed, sup2.since = true, started
	startAll(pl2, context.Background(), untouched)
	for _, u := range sup2.units {
		if strings.HasPrefix(u, "stop ") {
			t.Fatalf("the old daemon restarted a unit the rollout never reached: %v", sup2.units)
		}
	}
	if HeldRestart(cfg, untouched) {
		t.Fatal("the marks of the project the rollout never reached are still there")
	}
	// A later start finds nothing owed either: the files were put back with the process's start time.
	sup2.units, sup2.changed = nil, false
	startAll(pl2, context.Background(), untouched)
	for _, u := range sup2.units {
		if strings.HasPrefix(u, "stop ") {
			t.Fatalf("a later start restarted a unit that runs the rendered files: %v", sup2.units)
		}
	}

	// The project the rollout restarted runs v2: the old daemon restarts it onto v1.
	pl3, sup3 := newPlane()
	sup3.changed, sup3.since = true, restartedAt
	startAll(pl3, context.Background(), reached)
	var stops int
	for _, u := range sup3.units {
		if strings.HasPrefix(u, "stop ") {
			stops++
		}
	}
	if stops == 0 {
		t.Fatalf("the old daemon did not restart the project the rollout had restarted: %v", sup3.units)
	}
}

// A mark older than the process of its unit is stale: the unit restarted by itself since.
func TestHeldMarkOlderThanTheProcessIsDropped(t *testing.T) {
	const ref = "abcdefghijklmnopqrst"
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.Domain = "example.test"
	cfg.BinPath = "/usr/local/bin/supavise"
	writeAPIFiles(t, cfg, ref, time.Now().Add(-2*time.Hour))
	spec := units.Spec{Service: config.SvcGoTrue, Ref: ref}
	sup := &recSup{state: units.StateActive, since: time.Now().Add(time.Hour)} // started after the mark
	pl := NewPostgresPlane(cfg, sup, fakeArts{}, registry.NewMemory(), PlaneOptions{})
	pl.holdBack(spec, "abc")
	if !HeldRestart(cfg, ref) {
		t.Fatal("no mark written")
	}
	p := testProject(cfg, ref, 2)
	if pending, err := pl.PendingRestart(context.Background(), p, testKeys(t, ref)); err != nil || pending {
		t.Fatalf("stale mark: pending = %v, %v", pending, err)
	}
	if HeldRestart(cfg, ref) {
		t.Fatal("the stale mark stayed")
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
		if u == "supavise-edge-bundle@"+ref+".service" {
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

// pg_cron connects over libpq unless it runs its jobs in background workers, and the
// cluster's pg_hba.conf does not trust a loopback connection: without these settings every
// job fails with "connection failed". The system cluster hosts no jobs of users but is
// rendered the same way, and a saved max_worker_processes still wins.
func TestPostgresSpecRunsCronInBackgroundWorkers(t *testing.T) {
	pl, cfg := testPlane(t)
	cfg.Backup.WALRelay = "off"
	for _, ref := range []string{"abcdefghijklmnopqrst", config.SystemRef} {
		p := testProject(cfg, ref, 2)
		spec, err := pl.postgresSpec(context.Background(), p, testKeys(t, ref))
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Join(spec.Exec, " ")
		for _, want := range []string{"-c cron.use_background_workers=on", "-c cron.max_running_jobs=8", "-c max_worker_processes=16"} {
			if !strings.Contains(args, want) {
				t.Errorf("%s: args lack %q:\n%s", ref, want, args)
			}
		}
	}

	pl.opts.Settings = &fakeSettings{pg: []string{"max_worker_processes=24", "work_mem=8MB"}}
	p := testProject(cfg, "abcdefghijklmnopqrst", 2)
	spec, err := pl.postgresSpec(context.Background(), p, testKeys(t, p.Ref))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(spec.Exec, " ")
	if i, j := strings.LastIndex(args, "max_worker_processes=16"), strings.LastIndex(args, "max_worker_processes=24"); i < 0 || j < i {
		t.Errorf("a saved max_worker_processes must come after the default:\n%s", args)
	}
	// work_mem is part of the size, so a saved one goes after it on the command line.
	if i, j := strings.LastIndex(args, "-c work_mem=4MB"), strings.LastIndex(args, "-c work_mem=8MB"); i < 0 || j < i {
		t.Errorf("a saved work_mem must come after the size's:\n%s", args)
	}
	// The hba file must keep refusing a passwordless loopback connection: background workers
	// are the fix, not trust on 127.0.0.1.
	for _, l := range strings.Split(hbaRules, "\n") {
		if f := strings.Fields(l); len(f) >= 5 && f[0] == "host" && f[4] == "trust" {
			t.Errorf("pg_hba.conf trusts a TCP connection: %q", l)
		}
	}
}

// The rollout looks twice at a project whose files the daemon never held back: PendingRestart decides
// to restart it, and RestartPending restarts it. The first look renders the new files, so the second
// would find them in place and nothing to do, unless the first leaves the owed restart on record.
// That is the window of a daemon that started while the project was UPGRADING (it skipped the
// project, and so held nothing back) before the rollout reached it.
func TestPendingRestartLeavesTheOwedRestartOnRecordForRestartPending(t *testing.T) {
	const ref = "abcdefghijklmnopqrst"
	now := time.Now()
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.Domain = "example.test"
	cfg.BinPath = "/usr/local/bin/supavise"
	p := testProject(cfg, ref, 2)
	pgSpec := units.Spec{Service: config.SvcPostgres, Ref: ref}
	version := "v1"
	writeVersion := func(spec units.Spec) {
		files := units.FilesFor(cfg, spec)
		for _, f := range []string{files.Env, files.Run} {
			if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f, []byte(version+spec.Service), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(f, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeAPIFiles(t, cfg, ref, now.Add(-2*time.Hour))
	writeVersion(pgSpec)
	sup := &namedSup{recSup: recSup{state: units.StateActive, since: now.Add(-time.Hour), render: writeVersion}}
	pl := NewPostgresPlane(cfg, sup, fakeArts{}, registry.NewMemory(), PlaneOptions{PostgresReadyTimeout: time.Millisecond, ServiceReadyTimeout: time.Millisecond})

	// The release renders other files: the first look renders them, the unit runs the old ones.
	version, sup.changed = "v2", true
	ctx := context.Background()
	if pending, err := pl.PendingRestart(ctx, p, testKeys(t, ref)); err != nil || !pending {
		t.Fatalf("first look: pending = %v, %v", pending, err)
	}
	if !HeldRestart(cfg, ref) {
		t.Fatal("the look that rendered the files left no mark of the restart it found")
	}
	sup.changed = false // the files are rendered; nothing changes at the next look
	if pending, err := pl.PendingRestart(ctx, p, testKeys(t, ref)); err != nil || !pending {
		t.Fatalf("second look: pending = %v, %v", pending, err)
	}
	sup.units = nil
	// Nothing answers, so the wait for the cluster ends the call; the stops and the start are the point.
	_, _ = pl.RestartPending(ctx, p, testKeys(t, ref))
	cluster := config.UnitName(config.SvcPostgres, ref)
	if got := strings.Join(sup.units, ", "); !strings.Contains(got, "stop "+cluster) || !strings.Contains(got, "start "+cluster) {
		t.Fatalf("the cluster on older files was not restarted: %s", got)
	}
	if HeldRestart(cfg, ref) {
		t.Fatal("the restart left its mark behind")
	}
}
