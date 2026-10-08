package lifecycle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/fleet"
	"github.com/jsmillerdev/supavise/internal/projectconfig"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// fakeSettings is a Settings with canned answers.
type fakeSettings struct {
	auth, rest map[string]string
	pg         []string
	storage    projectconfig.StorageSettings
	realtime   projectconfig.RealtimeSettings
	pooler     projectconfig.PoolerSettings
	err        error
}

func (f *fakeSettings) AuthEnv(context.Context, string, string) (map[string]string, error) {
	return f.auth, f.err
}
func (f *fakeSettings) PostgRESTEnv(context.Context, string) (map[string]string, error) {
	return f.rest, f.err
}
func (f *fakeSettings) PostgresSettings(context.Context, string) ([]string, error) {
	return f.pg, f.err
}
func (f *fakeSettings) StorageSettings(context.Context, string) (projectconfig.StorageSettings, error) {
	return f.storage, f.err
}
func (f *fakeSettings) RealtimeSettings(context.Context, string) (projectconfig.RealtimeSettings, error) {
	return f.realtime, f.err
}
func (f *fakeSettings) PoolerSettings(context.Context, string) (projectconfig.PoolerSettings, error) {
	return f.pooler, f.err
}

func TestSavedSettingsAreRenderedIntoUnits(t *testing.T) {
	pl, cfg := testPlane(t)
	pl.opts.Settings = &fakeSettings{
		auth: map[string]string{"GOTRUE_SITE_URL": "https://app.example.com", "GOTRUE_JWT_EXP": "900", "GOTRUE_DB_MAX_POOL_SIZE": "", "GOTRUE_DB_CONN_PERCENTAGE": "10"},
		rest: map[string]string{"PGRST_DB_SCHEMAS": "public,extra", "PGRST_DB_MAX_ROWS": "25"},
		pg:   []string{"max_connections=80", "statement_timeout=30s", "shared_buffers=64MB"},
	}
	p := testProject(cfg, "abcdefghijklmnopqrst", 2)
	keys := testKeys(t, p.Ref)
	specs, err := pl.apiSpecs(context.Background(), p, keys)
	if err != nil {
		t.Fatal(err)
	}
	auth, rest := specs[0].Env, specs[1].Env
	if auth["GOTRUE_SITE_URL"] != "https://app.example.com" || auth["GOTRUE_JWT_EXP"] != "900" || auth["GOTRUE_DB_CONN_PERCENTAGE"] != "10" {
		t.Errorf("auth env: %v", auth)
	}
	if _, has := auth["GOTRUE_DB_MAX_POOL_SIZE"]; has {
		t.Error("an empty override must remove the variable")
	}
	if auth["GOTRUE_API_PORT"] == "" || auth["GOTRUE_JWT_SECRET"] != keys.JWTSecret {
		t.Error("the base environment must survive")
	}
	if rest["PGRST_DB_SCHEMAS"] != "public,extra" || rest["PGRST_DB_MAX_ROWS"] != "25" || rest["PGRST_DB_POOL"] != "5" {
		t.Errorf("postgrest env: %v", rest)
	}
	spec, err := pl.postgresSpec(context.Background(), p, keys)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(spec.Exec, " ")
	// Class sizing first, the saved command-line settings after it (the last one wins), and
	// the ALTER SYSTEM kind not on the command line at all.
	iClass := strings.Index(args, "-c max_connections=60")
	iSaved := strings.Index(args, "-c max_connections=80")
	if iClass < 0 || iSaved < iClass || !strings.Contains(args, "-c shared_buffers=64MB") {
		t.Errorf("exec: %s", args)
	}
	if strings.Contains(args, "statement_timeout") {
		t.Errorf("a setting applied with ALTER SYSTEM must not be on the command line: %s", args)
	}
	// The system project never reads user settings.
	sys := testProject(cfg, config.SystemRef, 0)
	sys.Class = ClassSystem
	if sp, err := pl.apiSpecs(context.Background(), sys, testKeys(t, config.SystemRef)); err != nil || sp[0].Env["GOTRUE_SITE_URL"] == "https://app.example.com" {
		t.Errorf("system project rendered user settings: %v %v", sp[0].Env["GOTRUE_SITE_URL"], err)
	}
	pl.opts.Settings = &fakeSettings{err: errors.New("registry down")}
	if _, err := pl.apiSpecs(context.Background(), p, keys); err == nil {
		t.Error("an unreadable settings source must fail the render, not silently fall back to defaults")
	}
}

func TestSplitPostgresSettings(t *testing.T) {
	cmd, alter := SplitPostgresSettings([]string{"work_mem=8MB", "max_connections=80", "max_wal_size=2GB", "log_connections=on", "max_worker_processes=12"})
	if strings.Join(cmd, ",") != "work_mem=8MB,max_connections=80,max_wal_size=2GB,max_worker_processes=12" || strings.Join(alter, ",") != "log_connections=on" {
		t.Fatalf("%v | %v", cmd, alter)
	}
}

// configPlane over fakePlane.
type cfgPlane struct {
	*fakePlane
	mu       sync.Mutex
	services []string
	pgCalls  []bool
	pending  bool
	pwSet    []string
	pwErr    error
	svcErr   error
}

func (c *cfgPlane) ReconfigureService(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys, svc string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.services = append(c.services, p.Ref+" "+svc)
	return c.svcErr
}
func (c *cfgPlane) ApplyPostgresSettings(ctx context.Context, _ *registry.Project, _ *secrets.ProjectKeys, restart bool, before func(context.Context)) (bool, error) {
	c.pgCalls = append(c.pgCalls, restart)
	if restart && before != nil {
		before(ctx)
	}
	return c.pending, nil
}
func (c *cfgPlane) SetRolePassword(_ context.Context, _ *registry.Project, role, pw string) error {
	c.pwSet = append(c.pwSet, role+":"+pw)
	return c.pwErr
}

type refreshTenant struct {
	fakeTenant
	refreshed []string
	quiesced  []string
	err       error
}

func (r *refreshTenant) QuiesceTenant(_ context.Context, ref string) error {
	r.quiesced = append(r.quiesced, ref)
	return r.err
}

func (r *refreshTenant) RefreshTenant(_ context.Context, ref string) error {
	r.refreshed = append(r.refreshed, ref)
	return r.err
}

func configHarness(t *testing.T) (*harness, *cfgPlane, *fakeSettings, *refreshTenant) {
	t.Helper()
	h := newHarness(t)
	cp := &cfgPlane{fakePlane: h.plane}
	set := &fakeSettings{storage: projectconfig.StorageSettings{FileSizeLimit: 100 << 20}}
	rt := &refreshTenant{}
	h.e = NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, cp, Options{Fleet: fleet.Fleet{rt}, Settings: set})
	return h, cp, set, rt
}

func TestApplyConfigTouchesOnlyTheOwningService(t *testing.T) {
	h, cp, set, rt := configHarness(t)
	ctx := context.Background()
	p := h.create(t)
	rt.ensured = nil

	for svc, want := range map[projectconfig.Service]string{
		projectconfig.Auth:      p.Ref + " " + config.SvcGoTrue,
		projectconfig.PostgREST: p.Ref + " " + config.SvcPostgREST,
	} {
		cp.services = nil
		res, err := h.e.ApplyConfig(ctx, p.Ref, svc, ApplyOptions{})
		if err != nil || !res.Applied || len(cp.services) != 1 || cp.services[0] != want {
			t.Fatalf("%s: %+v %v services=%v", svc, res, err, cp.services)
		}
	}
	if len(cp.fakePlane.reconf) != 0 {
		t.Fatal("applying one service must not restart them all")
	}
	if _, err := h.e.ApplyConfig(ctx, p.Ref, projectconfig.Storage, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(rt.ensured) != 1 || rt.ensured[0].Storage.FileSizeLimit != 100<<20 {
		t.Fatalf("storage tenant spec: %+v", rt.ensured)
	}
	// The pooler's pool size and client limit go to the Supavisor tenant, and are what a later
	// EnsureTenant (a resume, a key rotation) sends too.
	rt.ensured, cp.services = nil, nil
	set.pooler = projectconfig.PoolerSettings{PoolSize: 40, MaxClients: 300}
	if _, err := h.e.ApplyConfig(ctx, p.Ref, projectconfig.Pooler, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(rt.ensured) != 1 || rt.ensured[0].PoolSize != 40 || rt.ensured[0].MaxClients != 300 {
		t.Fatalf("pooler tenant spec: %+v", rt.ensured)
	}
	if len(cp.services) != 0 || len(cp.fakePlane.reconf) != 0 {
		t.Fatalf("applying the pooler settings must not restart a unit: %v", cp.services)
	}
	cp.pending = true
	res, err := h.e.ApplyConfig(ctx, p.Ref, projectconfig.Postgres, ApplyOptions{RestartDatabase: true})
	if err != nil || !res.PendingRestart || len(cp.pgCalls) != 1 || !cp.pgCalls[0] {
		t.Fatalf("postgres: %+v %v %v", res, err, cp.pgCalls)
	}
	if len(rt.quiesced) != 1 || rt.quiesced[0] != p.Ref {
		t.Fatalf("the shared services must be asked to let go of the database before a restart: %v", rt.quiesced)
	}
	set.err = errors.New("boom")
	if _, err := h.e.ApplyConfig(ctx, p.Ref, projectconfig.Realtime, ApplyOptions{}); err == nil {
		t.Fatal("a settings read error must fail the apply")
	}

	// A paused project has nothing running: the settings apply when it resumes.
	h.reg.SetProjectStatus(ctx, p.Ref, registry.StatusInactive)
	cp.services = nil
	if res, err := h.e.ApplyConfig(ctx, p.Ref, projectconfig.Auth, ApplyOptions{}); err != nil || res.Applied || len(cp.services) != 0 {
		t.Fatalf("paused: %+v %v", res, err)
	}
	h.reg.SetProjectStatus(ctx, p.Ref, registry.StatusComingUp)
	if _, err := h.e.ApplyConfig(ctx, p.Ref, projectconfig.Auth, ApplyOptions{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("transitional status: %v", err)
	}
}

func TestSetDatabasePassword(t *testing.T) {
	h, cp, _, rt := configHarness(t)
	ctx := context.Background()
	p := h.create(t)
	before, _ := h.e.Keys(ctx, p.Ref)

	if err := h.e.SetDatabasePassword(ctx, p.Ref, "a-new-Password-1"); err != nil {
		t.Fatal(err)
	}
	after, _ := h.e.Keys(ctx, p.Ref)
	if after.DBPassword != "a-new-Password-1" || after.AdminPassword != before.AdminPassword || after.JWTSecret != before.JWTSecret {
		t.Fatalf("only the postgres password may change: %+v", after)
	}
	if len(cp.pwSet) != 1 || cp.pwSet[0] != "postgres:a-new-Password-1" || len(rt.refreshed) != 1 || rt.refreshed[0] != p.Ref {
		t.Fatalf("role %v, pooler refreshed %v", cp.pwSet, rt.refreshed)
	}
	dsn, _ := h.e.ConnString(ctx, p.Ref, RolePostgres)
	if !strings.Contains(dsn, "a-new-Password-1") {
		t.Fatalf("ConnString uses the old password: %s", dsn)
	}

	// The cluster refuses: the stored password stays.
	cp.pwErr = errors.New("role does not exist")
	if err := h.e.SetDatabasePassword(ctx, p.Ref, "another-Password-2"); err == nil {
		t.Fatal("expected an error")
	}
	if k, _ := h.e.Keys(ctx, p.Ref); k.DBPassword != "a-new-Password-1" {
		t.Fatalf("a failed change replaced the stored password: %s", k.DBPassword)
	}
	cp.pwErr = nil
	// A pooler that cannot refresh does not fail the change.
	rt.err = errors.New("supavisor down")
	if err := h.e.SetDatabasePassword(ctx, p.Ref, "third-Password-3"); err != nil {
		t.Fatalf("pooler refresh failure must only warn: %v", err)
	}
	if err := h.e.SetDatabasePassword(ctx, p.Ref, "  "); err == nil {
		t.Fatal("blank password accepted")
	}
	h.reg.SetProjectStatus(ctx, p.Ref, registry.StatusInactive)
	if err := h.e.SetDatabasePassword(ctx, p.Ref, "fourth-Password-4"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("paused project: %v", err)
	}
}

func TestTemplateBaseURL(t *testing.T) {
	cfg := config.Default()
	for admin, want := range map[string]string{
		"127.0.0.1:7000": "http://127.0.0.1:7000/internal/templates",
		":7100":          "http://127.0.0.1:7100/internal/templates",
		"0.0.0.0:7200":   "http://127.0.0.1:7200/internal/templates",
		"[::1]:7300":     "http://[::1]:7300/internal/templates",
		"garbage":        "http://127.0.0.1:7000/internal/templates",
	} {
		cfg.Listen.Admin = admin
		if got := TemplateBaseURL(cfg); got != want {
			t.Errorf("%q: %s, want %s", admin, got, want)
		}
	}
}

func TestPauseQuiescesTheSharedServicesFirst(t *testing.T) {
	h, _, _, rt := configHarness(t)
	ctx := context.Background()
	p := h.create(t)
	rt.err = errors.New("realtime down") // a failing quiesce must not stop the pause
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if len(rt.quiesced) != 1 || rt.quiesced[0] != p.Ref {
		t.Fatalf("quiesced: %v", rt.quiesced)
	}
	if got, _ := h.reg.GetProject(ctx, p.Ref); got.Status != registry.StatusInactive {
		t.Fatalf("status %s", got.Status)
	}
}

// Settings saved while a project is paused reach the cluster and the shared services when it
// resumes: the Postgres ALTER SYSTEM part and the Storage and Realtime tenant settings are
// not rendered into any unit, so Resume has to apply them.
func TestResumeAppliesSettingsSavedWhilePaused(t *testing.T) {
	h, cp, set, rt := configHarness(t)
	ctx := context.Background()
	p := h.create(t)
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	set.storage = projectconfig.StorageSettings{FileSizeLimit: 1 << 20}
	set.pg = []string{"statement_timeout=7s"}
	rt.ensured, cp.pgCalls = nil, nil
	// Saving while paused touches nothing.
	if res, err := h.e.ApplyConfig(ctx, p.Ref, projectconfig.Storage, ApplyOptions{}); err != nil || res.Applied || len(rt.ensured) != 0 {
		t.Fatalf("paused save: %+v %v %v", res, err, rt.ensured)
	}
	if err := h.e.Resume(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if len(cp.pgCalls) != 1 || cp.pgCalls[0] {
		t.Fatalf("the saved Postgres settings must be applied once, without a restart: %v", cp.pgCalls)
	}
	if len(rt.ensured) != 1 || rt.ensured[0].Ref != p.Ref || rt.ensured[0].Storage.FileSizeLimit != 1<<20 {
		t.Fatalf("the tenant must get the settings saved while paused: %+v", rt.ensured)
	}

	// A failing apply is recorded and does not undo the resume.
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	rt.fakeTenant.err = errors.New("storage down")
	if err := h.e.Resume(ctx, p.Ref); err != nil {
		t.Fatalf("resume must succeed when a tenant apply fails: %v", err)
	}
	evs, _ := h.reg.ListEvents(ctx, p.Ref, 50)
	found := false
	for _, ev := range evs {
		found = found || ev.Kind == "project.config_apply_failed"
	}
	if !found {
		t.Fatal("a failed apply on resume must leave an event")
	}
	if got, _ := h.reg.GetProject(ctx, p.Ref); got.Status != registry.StatusActiveHealthy {
		t.Fatalf("status %s", got.Status)
	}
}
