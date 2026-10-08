package lifecycle

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
	"github.com/jsmillerdev/supavise/internal/sso"
	"github.com/jsmillerdev/supavise/internal/units"
)

func testAESGCM(t *testing.T) *secrets.AESGCM {
	t.Helper()
	s, err := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// With SystemAuth, supavise-gotrue@system speaks SAML with the node's own key and asks the daemon
// before it creates a user; sign-up is open to GoTrue and closed by the hook. A project's GoTrue
// gets none of it from here (its SAML is a saved setting), and without SystemAuth nothing changes.
func TestSystemGoTrueGetsDashboardSSO(t *testing.T) {
	pl, cfg := testPlane(t)
	sys := testProject(cfg, config.SystemRef, 0)
	keys := testKeys(t, config.SystemRef)
	before, err := pl.apiSpecs(context.Background(), sys, keys)
	if err != nil {
		t.Fatal(err)
	}
	if before[0].Env["GOTRUE_DISABLE_SIGNUP"] != "true" || before[0].Env["GOTRUE_SAML_ENABLED"] != "" {
		t.Fatalf("without SystemAuth: %v", before[0].Env)
	}

	key, _ := sso.NewSigningKey()
	pl.opts.SystemAuth = func(context.Context) (*SystemAuth, error) {
		return &SystemAuth{SigningKey: key, HookURL: "http://127.0.0.1:7000" + sso.HookPath, HookSecret: "v1,whsec_dGVzdA=="}, nil
	}
	specs, err := pl.apiSpecs(context.Background(), sys, keys)
	if err != nil || len(specs) != 1 {
		t.Fatalf("%v %v", specs, err)
	}
	e := specs[0].Env
	for k, want := range map[string]string{
		"GOTRUE_DISABLE_SIGNUP":                   "false",
		"GOTRUE_SAML_ENABLED":                     "true",
		"GOTRUE_SAML_PRIVATE_KEY":                 key,
		"GOTRUE_HOOK_BEFORE_USER_CREATED_ENABLED": "true",
		"GOTRUE_HOOK_BEFORE_USER_CREATED_URI":     "http://127.0.0.1:7000/internal/hooks/before-user-created",
		"GOTRUE_HOOK_BEFORE_USER_CREATED_SECRETS": "v1,whsec_dGVzdA==",
		"API_EXTERNAL_URL":                        "https://api.example.test/auth/v1",
	} {
		if e[k] != want {
			t.Errorf("%s = %q, want %q", k, e[k], want)
		}
	}
	if _, err := units.FormatEnv(e); err != nil {
		t.Fatalf("the environment does not render: %v", err)
	}
	// A project's GoTrue is untouched.
	p := testProject(cfg, "abcdefghijklmnopqrst", 1)
	ps, err := pl.apiSpecs(context.Background(), p, testKeys(t, p.Ref))
	if err != nil {
		t.Fatal(err)
	}
	if ps[0].Env["GOTRUE_SAML_ENABLED"] != "" || ps[0].Env["GOTRUE_HOOK_BEFORE_USER_CREATED_ENABLED"] != "" || ps[0].Env["GOTRUE_DISABLE_SIGNUP"] != "false" {
		t.Fatalf("the SSO of the dashboard reached a project: %v", ps[0].Env)
	}
}

func TestSystemAuthBuilder(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen.Admin = "127.0.0.1:7123"
	reg := registry.NewMemory()
	sec := testAESGCM(t)
	var open registry.Registry
	build := systemAuth(cfg, func() registry.Registry { return open }, sec)

	if sa, err := build(ctx); sa != nil || err != nil {
		t.Fatalf("before the registry is open: %v %v", sa, err)
	}
	open = reg
	if err := reg.CreateProject(ctx, &registry.Project{Ref: config.SystemRef, Name: "system", Class: "system", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	sa, err := build(ctx)
	if err != nil || sa == nil {
		t.Fatalf("%v %v", sa, err)
	}
	if err := sso.ValidSigningKey(sa.SigningKey); err != nil || sa.HookURL != "http://127.0.0.1:7123"+sso.HookPath || !strings.HasPrefix(sa.HookSecret, "v1,whsec_") {
		t.Fatalf("%+v %v", sa, err)
	}
	// The key is the one in the registry, and stays: a later render does not change it.
	again, _ := build(ctx)
	if again.SigningKey != sa.SigningKey {
		t.Fatal("the signing key changed between renders")
	}
	if want, _ := sso.HookSecret(sec); sa.HookSecret != want {
		t.Fatal("the daemon would not verify what GoTrue is told to sign with")
	}
	// Secrets that cannot derive a key (test doubles) leave the SSO configuration out.
	if sa, err := systemAuth(cfg, func() registry.Registry { return reg }, noDerive{})(ctx); sa != nil || err != nil {
		t.Fatalf("%v %v", sa, err)
	}
}

type noDerive struct{}

func (noDerive) Seal(b []byte) ([]byte, error) { return b, nil }
func (noDerive) Open(b []byte) ([]byte, error) { return b, nil }

// changeSup is a supervisor whose RenderChanged answer and unit state the test chooses.
type changeSup struct {
	mu      sync.Mutex
	log     []string
	changed bool
	state   units.State
}

func (c *changeSup) rec(s string) { c.mu.Lock(); c.log = append(c.log, s); c.mu.Unlock() }
func (c *changeSup) Render(_ context.Context, s units.Spec) error {
	c.rec("render " + s.Unit())
	return nil
}
func (c *changeSup) RenderChanged(_ context.Context, s units.Spec) (bool, error) {
	c.rec("render " + s.Unit())
	return c.changed, nil
}
func (c *changeSup) Start(_ context.Context, u string) error { c.rec("start " + u); return nil }
func (c *changeSup) Stop(_ context.Context, u string) error  { c.rec("stop " + u); return nil }
func (c *changeSup) Status(context.Context, string) (units.Status, error) {
	return units.Status{State: c.state}, nil
}
func (c *changeSup) Remove(context.Context, string) error { return nil }
func (c *changeSup) calls() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.log, ",")
}

// The daemon renders supavise-gotrue@system at every start; the unit restarts only when the files
// differ from the ones it runs on, and a unit that is not running is not started by it.
func TestRefreshSystemAuthRestartsOnlyOnAChange(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer health.Close()
	_, port, _ := net.SplitHostPort(health.Listener.Addr().String())
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.Domain = "example.test"
	cfg.Ports.SystemGoTrue, _ = strconv.Atoi(port)
	sup := &changeSup{state: units.StateActive}
	pl := NewPostgresPlane(cfg, sup, fakeArts{}, registry.NewMemory(), PlaneOptions{})
	sys := testProject(cfg, config.SystemRef, 0)
	keys := testKeys(t, config.SystemRef)
	ctx := context.Background()

	if err := pl.RefreshSystemAuth(ctx, sys, keys); err != nil {
		t.Fatal(err)
	}
	if got := sup.calls(); got != "render supavise-gotrue@system.service" {
		t.Fatalf("unchanged files: %s", got)
	}
	sup.changed = true
	sup.log = nil
	if err := pl.RefreshSystemAuth(ctx, sys, keys); err != nil {
		t.Fatal(err)
	}
	if got := sup.calls(); got != "render supavise-gotrue@system.service,stop supavise-gotrue@system.service,start supavise-gotrue@system.service" {
		t.Fatalf("changed files: %s", got)
	}
	sup.state, sup.log = units.StateInactive, nil
	if err := pl.RefreshSystemAuth(ctx, sys, keys); err != nil {
		t.Fatal(err)
	}
	if got := sup.calls(); got != "render supavise-gotrue@system.service" {
		t.Fatalf("a unit that is not running: %s", got)
	}
}

// depSup is a supervisor in which stopping the system cluster stops the sign-in service with it, as
// systemd does for a unit that requires it, and starting the cluster does not start it again.
type depSup struct {
	changeSup
	states map[string]units.State
}

func (d *depSup) Stop(_ context.Context, u string) error {
	d.rec("stop " + u)
	d.states[u] = units.StateInactive
	if u == "supavise-postgres@system.service" {
		d.states["supavise-gotrue@system.service"] = units.StateInactive
	}
	return nil
}
func (d *depSup) Start(_ context.Context, u string) error {
	d.rec("start " + u)
	d.states[u] = units.StateActive
	return nil
}
func (d *depSup) Status(_ context.Context, u string) (units.Status, error) {
	return units.Status{State: d.states[u]}, nil
}

// A release that changes how the system cluster is rendered restarts it when the daemon starts, which
// stops the dashboard's sign-in service: the daemon starts it again. A sign-in service that was not
// running before is not started.
func TestRefreshSystemStartsTheSignInServiceTheClusterRestartStopped(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer health.Close()
	_, port, _ := net.SplitHostPort(health.Listener.Addr().String())
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.Domain = "example.test"
	cfg.Ports.SystemGoTrue, _ = strconv.Atoi(port)
	sys := testProject(cfg, config.SystemRef, 0)
	keys := testKeys(t, config.SystemRef)
	ctx := context.Background()
	const gotrue, pg = "supavise-gotrue@system.service", "supavise-postgres@system.service"
	restart := func(d *depSup) func() error {
		return func() error { // StartDatabase on a cluster whose files changed
			_ = d.Stop(ctx, pg)
			return d.Start(ctx, pg)
		}
	}

	d := &depSup{states: map[string]units.State{gotrue: units.StateActive, pg: units.StateActive}}
	pl := NewPostgresPlane(cfg, d, fakeArts{}, registry.NewMemory(), PlaneOptions{})
	if err := pl.refreshSystem(ctx, sys, keys, restart(d)); err != nil {
		t.Fatal(err)
	}
	if d.states[gotrue] != units.StateActive {
		t.Fatalf("the sign-in service is %s after the cluster restarted: %s", d.states[gotrue], d.calls())
	}

	d = &depSup{states: map[string]units.State{gotrue: units.StateInactive, pg: units.StateActive}}
	pl = NewPostgresPlane(cfg, d, fakeArts{}, registry.NewMemory(), PlaneOptions{})
	if err := pl.refreshSystem(ctx, sys, keys, restart(d)); err != nil {
		t.Fatal(err)
	}
	if d.states[gotrue] != units.StateInactive {
		t.Fatalf("a sign-in service that was not running was started: %s", d.calls())
	}

	// A cluster that does not start is the cluster's error, and the sign-in service is not touched.
	d = &depSup{states: map[string]units.State{gotrue: units.StateActive, pg: units.StateActive}}
	pl = NewPostgresPlane(cfg, d, fakeArts{}, registry.NewMemory(), PlaneOptions{})
	err := pl.refreshSystem(ctx, sys, keys, func() error { return errors.New("postgres did not start") })
	var re *SystemRefreshError
	if !errors.As(err, &re) || re.Auth || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("err = %v", err)
	}
}
