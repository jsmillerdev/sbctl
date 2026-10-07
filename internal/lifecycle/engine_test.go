package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/fleet"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// fakePlane records calls and fails on demand; it stands in for PostgresPlane.
type fakePlane struct {
	mu      sync.Mutex
	calls   []string
	failOn  map[string]error
	seeded  bool
	keysIn  *secrets.ProjectKeys
	reconf  []string // JWT secrets passed to Reconfigure
	healthy bool
	// started holds a copy of the project of every Start, in order.
	started []registry.Project
	// failOnce fails the next call named by the key and then forgets it.
	failOnce map[string]error
}

func newFakePlane() *fakePlane {
	return &fakePlane{failOn: map[string]error{}, failOnce: map[string]error{}, healthy: true}
}

func (f *fakePlane) rec(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	name := strings.SplitN(call, " ", 2)[0]
	if err, ok := f.failOnce[name]; ok {
		delete(f.failOnce, name)
		return err
	}
	return f.failOn[name]
}

func (f *fakePlane) has(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func (f *fakePlane) Create(_ context.Context, p *registry.Project, k *secrets.ProjectKeys, seed DataSeeder) error {
	f.keysIn, f.seeded = k, seed != nil
	return f.rec("Create " + p.Ref)
}
func (f *fakePlane) Delete(_ context.Context, ref string) error { return f.rec("Delete " + ref) }
func (f *fakePlane) Snapshot(context.Context, string) (*registry.Backup, error) {
	return nil, ErrNoSnapshot
}
func (f *fakePlane) Route(context.Context, string) (Upstreams, error) { return Upstreams{}, nil }
func (f *fakePlane) Usage(context.Context, string) (Usage, error)     { return Usage{}, nil }
func (f *fakePlane) Start(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys) error {
	f.mu.Lock()
	f.started = append(f.started, *p)
	f.mu.Unlock()
	return f.rec("Start " + p.Ref)
}
func (f *fakePlane) StartDatabase(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys) error {
	return f.rec("StartDatabase " + p.Ref)
}
func (f *fakePlane) Stop(_ context.Context, ref string) error { return f.rec("Stop " + ref) }
func (f *fakePlane) Reconfigure(_ context.Context, p *registry.Project, k *secrets.ProjectKeys) error {
	f.reconf = append(f.reconf, k.JWTSecret)
	return f.rec("Reconfigure " + p.Ref)
}
func (f *fakePlane) Health(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys) []ServiceHealth {
	return []ServiceHealth{{Name: config.SvcPostgres, Healthy: f.healthy, Status: "x"}}
}

type fakeArts struct{}

func (fakeArts) Dir(svc string) (string, error) { return "/art/" + svc, nil }
func (fakeArts) Tag(svc string) (string, error) { return svc + "-tag", nil }

type fakeTenant struct {
	mu      sync.Mutex
	ensured []fleet.TenantSpec
	removed []string
	err     error
}

func (f *fakeTenant) Service() string { return config.SvcRealtime }
func (f *fakeTenant) EnsureTenant(_ context.Context, s fleet.TenantSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, s)
	return f.err
}
func (f *fakeTenant) RemoveTenant(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, ref)
	return f.err
}

type fakeBackup struct {
	calls []string
	err   error
}

func (f *fakeBackup) BaseBackup(_ context.Context, ref string) (*registry.Backup, error) {
	f.calls = append(f.calls, ref)
	return &registry.Backup{Ref: ref}, f.err
}

type harness struct {
	e      *Engine
	reg    *registry.Memory
	plane  *fakePlane
	tenant *fakeTenant
	backup *fakeBackup
	cfg    *config.Config
	sec    secrets.Secrets
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.Domain = "example.test"
	key := make([]byte, 32)
	sec, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{reg: registry.NewMemory(), plane: newFakePlane(), tenant: &fakeTenant{}, backup: &fakeBackup{}, cfg: cfg, sec: sec}
	h.e = NewEngine(cfg, h.reg, sec, fakeArts{}, h.plane, Options{Fleet: fleet.Fleet{h.tenant}, Backup: h.backup})
	return h
}

func (h *harness) create(t *testing.T) *registry.Project {
	t.Helper()
	p, err := h.e.Create(context.Background(), CreateRequest{Name: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCreateHappyPath(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	if p.Status != registry.StatusActiveHealthy || p.Seq != 1 || p.Class != DefaultClass || !secrets.ValidRef(p.Ref) {
		t.Fatalf("project = %+v", p)
	}
	if p.Versions[config.SvcGoTrue] != "gotrue-tag" || p.Limits != h.cfg.Defaults {
		t.Fatalf("versions/limits = %v %v", p.Versions, p.Limits)
	}
	routes, _ := h.reg.ListRoutes(ctx)
	if len(routes) != 1 || routes[0].Host != p.Ref+".api.example.test" || routes[0].Ref != p.Ref {
		t.Fatalf("routes = %+v", routes)
	}
	keys, err := h.e.Keys(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if *keys != *h.plane.keysIn {
		t.Fatal("stored keys differ from the keys the data plane received")
	}
	if _, err := secrets.ParseHS256(keys.AnonKey, keys.JWTSecret); err != nil {
		t.Fatal(err)
	}
	if len(h.tenant.ensured) != 1 || h.tenant.ensured[0].Ref != p.Ref || h.tenant.ensured[0].DBPort != h.cfg.PortsFor(p.Ref, 1).Postgres ||
		h.tenant.ensured[0].JWTSecret != keys.JWTSecret || h.tenant.ensured[0].Host != p.Ref+".api.example.test" {
		t.Fatalf("tenant spec = %+v", h.tenant.ensured)
	}
	// The fleet is called after the units are up.
	if h.plane.calls[0] != "Create "+p.Ref {
		t.Fatalf("calls = %v", h.plane.calls)
	}
	evs, _ := h.reg.ListEvents(ctx, p.Ref, 10)
	if len(evs) != 1 || evs[0].Kind != "project.created" {
		t.Fatalf("events = %+v", evs)
	}
	org, err := h.reg.GetOrganization(ctx, "default")
	if err != nil || p.OrgID != org.ID {
		t.Fatalf("org = %+v %v", org, err)
	}
}

func TestCreateRequestOptions(t *testing.T) {
	h := newHarness(t)
	k, err := secrets.NewProjectKeys("abcdefghijklmnopqrst", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	seed := DataSeeder(func(context.Context, *registry.Project, string) error { return nil })
	lim := config.Limits{MemoryMax: "512M", CPUQuota: "50%"}
	p, err := h.e.Create(context.Background(), CreateRequest{Ref: "abcdefghijklmnopqrst", Class: "micro", Keys: k, Seed: seed, Limits: &lim, DBPassword: "pw", OrgSlug: "acme", Region: "eu-west-2"})
	if err == nil {
		t.Fatal("unknown organization must fail")
	}
	if _, err := h.reg.CreateOrganization(context.Background(), "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	p, err = h.e.Create(context.Background(), CreateRequest{Ref: "abcdefghijklmnopqrst", Class: "micro", Keys: k, Seed: seed, Limits: &lim, DBPassword: "pw", OrgSlug: "acme", Region: "eu-west-2"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Ref != "abcdefghijklmnopqrst" || p.Class != "micro" || p.Limits != lim || p.Region != "eu-west-2" || p.Name != p.Ref {
		t.Fatalf("project = %+v", p)
	}
	if !h.plane.seeded || h.plane.keysIn.JWTSecret != k.JWTSecret || h.plane.keysIn.DBPassword != "pw" {
		t.Fatalf("seed=%v keys=%+v", h.plane.seeded, h.plane.keysIn)
	}
	if k.DBPassword == "pw" {
		t.Fatal("caller's keys were modified")
	}
	// Same ref again.
	if _, err := h.e.Create(context.Background(), CreateRequest{Ref: p.Ref, OrgSlug: "acme"}); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, req := range []CreateRequest{{Ref: "system"}, {Ref: "SHORT"}, {Class: "huge"}, {Class: ClassSystem}} {
		if _, err := h.e.Create(ctx, req); err == nil {
			t.Errorf("%+v: expected error", req)
		}
	}
	h.cfg.Domain = ""
	if _, err := h.e.Create(ctx, CreateRequest{}); err == nil || !strings.Contains(err.Error(), "domain") {
		t.Fatalf("no domain: %v", err)
	}
	if ps, _ := h.reg.ListProjects(ctx); len(ps) != 0 {
		t.Fatalf("validation errors must not leave rows: %+v", ps)
	}
}

// A failure after the row exists leaves INIT_FAILED, removes units, route and tenants,
// and records why.
func TestCreateFailureLeavesInitFailedAndCleansUp(t *testing.T) {
	for _, stage := range []string{"plane", "fleet"} {
		t.Run(stage, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			boom := errors.New("boom")
			if stage == "plane" {
				h.plane.failOn["Create"] = boom
			} else {
				h.tenant.err = boom
			}
			_, err := h.e.Create(ctx, CreateRequest{Ref: "abcdefghijklmnopqrst"})
			if !errors.Is(err, boom) || !strings.Contains(err.Error(), "INIT_FAILED") {
				t.Fatalf("err = %v", err)
			}
			p, err := h.reg.GetProject(ctx, "abcdefghijklmnopqrst")
			if err != nil || p.Status != registry.StatusInitFailed {
				t.Fatalf("project = %+v %v", p, err)
			}
			if !h.plane.has("Delete abcdefghijklmnopqrst") {
				t.Fatalf("data plane was not cleaned up: %v", h.plane.calls)
			}
			if routes, _ := h.reg.ListRoutes(ctx); len(routes) != 0 {
				t.Fatalf("routes = %+v", routes)
			}
			if stage == "fleet" && len(h.tenant.removed) != 1 {
				t.Fatalf("tenants not removed: %v", h.tenant.removed)
			}
			evs, _ := h.reg.ListEvents(ctx, p.Ref, 10)
			if len(evs) != 1 || evs[0].Kind != "project.init_failed" || !strings.Contains(string(evs[0].Payload), "boom") {
				t.Fatalf("events = %+v", evs)
			}
			// An INIT_FAILED project can be deleted without a backup attempt.
			if err := h.e.Delete(ctx, p.Ref); err != nil {
				t.Fatal(err)
			}
			if len(h.backup.calls) != 0 {
				t.Fatalf("backup of a failed project: %v", h.backup.calls)
			}
			if _, err := h.reg.GetProject(ctx, p.Ref); !errors.Is(err, registry.ErrNotFound) {
				t.Fatalf("row still there: %v", err)
			}
		})
	}
}

func TestCreateFailureSurvivesCancelledContext(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.plane.failOn["Create"] = context.Canceled
	cancel()
	// CreateProject on the memory registry ignores ctx; the cleanup must still run.
	_, err := h.e.Create(ctx, CreateRequest{Ref: "abcdefghijklmnopqrst"})
	if err == nil {
		t.Fatal("expected error")
	}
	p, _ := h.reg.GetProject(context.Background(), "abcdefghijklmnopqrst")
	if p == nil || p.Status != registry.StatusInitFailed || !h.plane.has("Delete abcdefghijklmnopqrst") {
		t.Fatalf("cleanup did not run: %+v %v", p, h.plane.calls)
	}
}

func TestPauseResume(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	if err := h.e.Resume(ctx, p.Ref); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("resume of active: %v", err)
	}
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.reg.GetProject(ctx, p.Ref); got.Status != registry.StatusInactive {
		t.Fatalf("status = %s", got.Status)
	}
	if err := h.e.Pause(ctx, p.Ref); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("pause twice: %v", err)
	}
	if err := h.e.Resume(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.reg.GetProject(ctx, p.Ref); got.Status != registry.StatusActiveHealthy {
		t.Fatalf("status = %s", got.Status)
	}
	if err := h.e.Pause(ctx, "nosuchproject"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := h.e.Pause(ctx, config.SystemRef); err == nil {
		t.Fatal("system must not pause")
	}
}

func TestResumeFailureStaysInactive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	h.plane.calls = nil
	h.plane.failOn["Start"] = errors.New("no postgres")
	if err := h.e.Resume(ctx, p.Ref); err == nil {
		t.Fatal("expected error")
	}
	if got, _ := h.reg.GetProject(ctx, p.Ref); got.Status != registry.StatusInactive {
		t.Fatalf("status = %s", got.Status)
	}
	if !h.plane.has("Stop " + p.Ref) {
		t.Fatalf("partial start was not stopped: %v", h.plane.calls)
	}
	delete(h.plane.failOn, "Start")
	if err := h.e.Resume(ctx, p.Ref); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestPauseFailureMarksUnhealthy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	h.plane.failOn["Stop"] = errors.New("stuck")
	if err := h.e.Pause(ctx, p.Ref); err == nil {
		t.Fatal("expected error")
	}
	if got, _ := h.reg.GetProject(ctx, p.Ref); got.Status != registry.StatusActiveUnhealthy {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	t.Run("active takes a final backup first", func(t *testing.T) {
		h := newHarness(t)
		p := h.create(t)
		h.plane.calls = nil
		if err := h.e.Delete(ctx, p.Ref); err != nil {
			t.Fatal(err)
		}
		if len(h.backup.calls) != 1 || h.plane.has("StartDatabase "+p.Ref) || !h.plane.has("Delete "+p.Ref) {
			t.Fatalf("backup=%v calls=%v", h.backup.calls, h.plane.calls)
		}
		if len(h.tenant.removed) != 1 {
			t.Fatalf("tenants removed: %v", h.tenant.removed)
		}
		if routes, _ := h.reg.ListRoutes(ctx); len(routes) != 0 {
			t.Fatalf("routes: %+v", routes)
		}
		if _, err := h.reg.GetProject(ctx, p.Ref); !errors.Is(err, registry.ErrNotFound) {
			t.Fatal("row remains")
		}
		if _, err := h.reg.GetSecrets(ctx, p.Ref); err != nil {
			// secrets cascade with the project; Memory returns an empty map or ErrNotFound
			if !errors.Is(err, registry.ErrNotFound) {
				t.Fatal(err)
			}
		}
	})
	t.Run("paused starts the database for the backup then stops it", func(t *testing.T) {
		h := newHarness(t)
		p := h.create(t)
		if err := h.e.Pause(ctx, p.Ref); err != nil {
			t.Fatal(err)
		}
		h.plane.calls = nil
		if err := h.e.Delete(ctx, p.Ref); err != nil {
			t.Fatal(err)
		}
		want := []string{"StartDatabase " + p.Ref, "Stop " + p.Ref, "Delete " + p.Ref}
		if strings.Join(h.plane.calls, "|") != strings.Join(want, "|") || len(h.backup.calls) != 1 {
			t.Fatalf("calls = %v backup = %v", h.plane.calls, h.backup.calls)
		}
	})
	t.Run("backup failure keeps the project", func(t *testing.T) {
		h := newHarness(t)
		p := h.create(t)
		h.backup.err = errors.New("s3 down")
		h.plane.calls = nil
		err := h.e.Delete(ctx, p.Ref)
		if err == nil || !strings.Contains(err.Error(), "s3 down") {
			t.Fatalf("err = %v", err)
		}
		got, gerr := h.reg.GetProject(ctx, p.Ref)
		if gerr != nil || got.Status != registry.StatusActiveHealthy || h.plane.has("Delete "+p.Ref) || len(h.tenant.removed) != 0 {
			t.Fatalf("project not intact: %+v %v calls=%v", got, gerr, h.plane.calls)
		}
		if err := h.e.DeleteWith(ctx, p.Ref, DeleteOptions{SkipFinalBackup: true}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("no backup engine skips it", func(t *testing.T) {
		h := newHarness(t)
		h.e.opts.Backup = nil
		p := h.create(t)
		if err := h.e.Delete(ctx, p.Ref); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("tenant cleanup failure does not block deletion", func(t *testing.T) {
		h := newHarness(t)
		p := h.create(t)
		h.tenant.err = errors.New("realtime down")
		if err := h.e.Delete(ctx, p.Ref); err != nil {
			t.Fatal(err)
		}
		if _, err := h.reg.GetProject(ctx, p.Ref); !errors.Is(err, registry.ErrNotFound) {
			t.Fatal("row remains")
		}
	})
	t.Run("data plane failure leaves GOING_DOWN so delete can be repeated", func(t *testing.T) {
		h := newHarness(t)
		p := h.create(t)
		h.plane.failOn["Delete"] = errors.New("busy")
		if err := h.e.Delete(ctx, p.Ref); err == nil {
			t.Fatal("expected error")
		}
		if got, _ := h.reg.GetProject(ctx, p.Ref); got.Status != registry.StatusGoingDown {
			t.Fatalf("status = %s", got.Status)
		}
		delete(h.plane.failOn, "Delete")
		if err := h.e.Delete(ctx, p.Ref); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("system cannot be deleted", func(t *testing.T) {
		h := newHarness(t)
		h.reg.CreateProject(ctx, &registry.Project{Ref: config.SystemRef, Name: "system", Status: registry.StatusActiveHealthy})
		if err := h.e.Delete(ctx, config.SystemRef); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRotateKeys(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	old, _ := h.e.Keys(ctx, p.Ref)
	nk, err := h.e.RotateKeys(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if nk.JWTSecret == old.JWTSecret || nk.AnonKey == old.AnonKey || nk.ServiceRoleKey == old.ServiceRoleKey ||
		nk.PublishableKey == old.PublishableKey || nk.SecretKey == old.SecretKey {
		t.Fatal("a key was not rotated")
	}
	if nk.DBPassword != old.DBPassword || nk.AdminPassword != old.AdminPassword || nk.PGSodiumRootKey != old.PGSodiumRootKey {
		t.Fatal("database credentials must not change")
	}
	if _, err := secrets.ParseHS256(old.AnonKey, nk.JWTSecret); err == nil {
		t.Fatal("old anon key still verifies")
	}
	if _, err := secrets.ParseHS256(nk.ServiceRoleKey, nk.JWTSecret); err != nil {
		t.Fatal(err)
	}
	stored, _ := h.e.Keys(ctx, p.Ref)
	if *stored != *nk {
		t.Fatal("rotated keys were not stored")
	}
	if len(h.plane.reconf) != 1 || h.plane.reconf[0] != nk.JWTSecret {
		t.Fatalf("reconfigure got %v", h.plane.reconf)
	}
	if got := h.tenant.ensured[len(h.tenant.ensured)-1]; got.JWTSecret != nk.JWTSecret {
		t.Fatal("fleet tenants were not updated")
	}
}

func TestRotateKeysRestoresOnFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	old, _ := h.e.Keys(ctx, p.Ref)
	h.plane.failOn["Reconfigure"] = errors.New("gotrue will not start")
	if _, err := h.e.RotateKeys(ctx, p.Ref); err == nil {
		t.Fatal("expected error")
	}
	h.plane.failOn = map[string]error{}
	got, _ := h.e.Keys(ctx, p.Ref)
	if *got != *old {
		t.Fatal("previous keys were not restored")
	}
	if n := len(h.plane.reconf); n != 2 || h.plane.reconf[1] != old.JWTSecret {
		t.Fatalf("units not restarted on the old secret: %v", h.plane.reconf)
	}
}

func TestRotateKeysPausedOnlyStoresKeys(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	h.e.Pause(ctx, p.Ref)
	nk, err := h.e.RotateKeys(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.plane.reconf) != 0 {
		t.Fatal("paused project must not be restarted")
	}
	if got, _ := h.e.Keys(ctx, p.Ref); got.JWTSecret != nk.JWTSecret {
		t.Fatal("keys not stored")
	}
}

func TestHealthUpdatesStatus(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	h.plane.healthy = false
	hs, err := h.e.Health(ctx, p.Ref)
	if err != nil || healthyAll(hs) {
		t.Fatalf("health = %+v %v", hs, err)
	}
	if got, _ := h.reg.GetProject(ctx, p.Ref); got.Status != registry.StatusActiveUnhealthy {
		t.Fatalf("status = %s", got.Status)
	}
	h.plane.healthy = true
	h.e.Health(ctx, p.Ref)
	if got, _ := h.reg.GetProject(ctx, p.Ref); got.Status != registry.StatusActiveHealthy {
		t.Fatalf("status = %s", got.Status)
	}
	// A paused project keeps its status however unhealthy its units are.
	h.e.Pause(ctx, p.Ref)
	h.plane.healthy = false
	h.e.Health(ctx, p.Ref)
	if got, _ := h.reg.GetProject(ctx, p.Ref); got.Status != registry.StatusInactive {
		t.Fatalf("status = %s", got.Status)
	}
}

func healthyAll(hs []ServiceHealth) bool {
	for _, h := range hs {
		if !h.Healthy {
			return false
		}
	}
	return true
}

func TestConnString(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	keys, _ := h.e.Keys(ctx, p.Ref)
	dsn, err := h.e.ConnString(ctx, p.Ref, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable", keys.DBPassword, h.cfg.PortsFor(p.Ref, p.Seq).Postgres)
	if dsn != want {
		t.Fatalf("dsn = %s", dsn)
	}
	if _, err := h.e.ConnString(ctx, p.Ref, "authenticator"); err == nil {
		t.Fatal("only postgres and supabase_admin are supported")
	}
}

func TestStartActive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := h.create(t), h.create(t)
	if err := h.e.Pause(ctx, b.Ref); err != nil {
		t.Fatal(err)
	}
	h.plane.calls = nil
	if errs := h.e.StartActive(ctx); len(errs) != 0 {
		t.Fatal(errs)
	}
	if !h.plane.has("Start "+a.Ref) || h.plane.has("Start "+b.Ref) {
		t.Fatalf("calls = %v", h.plane.calls)
	}
	h.plane.failOn["Start"] = errors.New("nope")
	errs := h.e.StartActive(ctx)
	if errs[a.Ref] == nil {
		t.Fatal("expected a failure for the active project")
	}
	if got, _ := h.reg.GetProject(ctx, a.Ref); got.Status != registry.StatusActiveUnhealthy {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestClasses(t *testing.T) {
	for _, n := range ClassNames() {
		c, err := ClassFor(n)
		if err != nil || c.MaxConnections <= 0 || c.SharedBuffers == "" {
			t.Fatalf("class %s: %+v %v", n, c, err)
		}
	}
	if _, err := ClassFor("nope"); err == nil {
		t.Fatal("unknown class accepted")
	}
	// New projects are Micro, as on hosted, and the default keeps the 1 GB cap it always had.
	d, _ := ClassFor("")
	if d.Name != "micro" || d.MaxConnections != 60 || d.Limits() != (config.Limits{MemoryMax: "1G", CPUQuota: "100%"}) {
		t.Fatalf("default class = %+v", d)
	}
	for _, n := range ClassNames() {
		if n == ClassSystem {
			t.Fatal("system class must not be user selectable")
		}
	}
}

// A seed carries its own role passwords, so fresh keys could never log in.
func TestCreateSeedWithoutKeysIsRejected(t *testing.T) {
	h := newHarness(t)
	seed := DataSeeder(func(context.Context, *registry.Project, string) error { return nil })
	_, err := h.e.Create(context.Background(), CreateRequest{Seed: seed})
	if err == nil || !strings.Contains(err.Error(), "Keys") {
		t.Fatalf("err = %v", err)
	}
	if ps, _ := h.reg.ListProjects(context.Background()); len(ps) != 0 || len(h.plane.calls) != 0 {
		t.Fatalf("rows=%+v calls=%v", ps, h.plane.calls)
	}
}

// An existing cluster is somebody's data: Create refuses, does not call the data
// plane's Delete (which removes the directory) and forgets the row it made.
func TestCreateOverExistingClusterLeavesDataAlone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.plane.failOn["Create"] = fmt.Errorf("%w: /somewhere", ErrClusterExists)
	_, err := h.e.Create(ctx, CreateRequest{Ref: "abcdefghijklmnopqrst"})
	if !errors.Is(err, ErrClusterExists) || !strings.Contains(err.Error(), "untouched") {
		t.Fatalf("err = %v", err)
	}
	if h.plane.has("Delete abcdefghijklmnopqrst") {
		t.Fatalf("existing data was deleted: %v", h.plane.calls)
	}
	if _, err := h.reg.GetProject(ctx, "abcdefghijklmnopqrst"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("row remains: %v", err)
	}
}

// With the Postgres registry, a second Engine (another process) waits for the first
// one's operation on the same ref, and different refs do not block each other.
func TestLockIsCrossProcessWithPostgresRegistry(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	reg, err := registry.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	cfg := config.Default()
	e1 := NewEngine(cfg, reg, nil, fakeArts{}, newFakePlane(), Options{})
	e2 := NewEngine(cfg, reg, nil, fakeArts{}, newFakePlane(), Options{})
	unlock, err := e1.lock(ctx, "abcdefghijklmnopqrst")
	if err != nil {
		t.Fatal(err)
	}
	other, err := e2.lock(ctx, "bbbbbbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	other()
	got := make(chan struct{})
	go func() {
		u, err := e2.lock(ctx, "abcdefghijklmnopqrst")
		if err == nil {
			u()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("second engine took the lock while the first held it")
	case <-time.After(500 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("second engine never got the lock")
	}
}

func TestDeleteSkipsFinalBackupWhenNothingIsRestorable(t *testing.T) {
	h := newHarness(t)
	p := h.create(t)
	h.backup.err = fmt.Errorf("backup %s: %w (the clone never finished recovery)", p.Ref, ErrNoRestorableState)
	if err := h.e.Delete(context.Background(), p.Ref); err != nil {
		t.Fatalf("delete of a project with no restorable state failed: %v", err)
	}
	if _, err := h.reg.GetProject(context.Background(), p.Ref); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("project row remains: %v", err)
	}
	evs, _ := h.reg.ListEvents(context.Background(), p.Ref, 50)
	var skipped bool
	for _, e := range evs {
		skipped = skipped || e.Kind == "project.final_backup_skipped"
	}
	if !skipped {
		t.Error("the skipped final backup was not recorded as an event")
	}
	// Any other backup error still keeps the project.
	q := h.create(t)
	h.backup.err = errors.New("disk full")
	if err := h.e.Delete(context.Background(), q.Ref); err == nil {
		t.Fatal("a real backup failure must keep the project")
	}
	if got, _ := h.reg.GetProject(context.Background(), q.Ref); got == nil || got.Status == registry.StatusGoingDown {
		t.Fatalf("project not restored to its previous status: %+v", got)
	}
}

func TestRecoverMovesInterruptedProjects(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mk := func(status registry.Status, route bool) string {
		p := h.create(t)
		if !route {
			if err := h.reg.DeleteRoute(ctx, h.cfg.ProjectHost(p.Ref)); err != nil {
				t.Fatal(err)
			}
		}
		if err := h.reg.SetProjectStatus(ctx, p.Ref, status); err != nil {
			t.Fatal(err)
		}
		return p.Ref
	}
	pausing := mk(registry.StatusPausing, true)
	resuming := mk(registry.StatusComingUp, true)
	creating := mk(registry.StatusComingUp, false)
	goingDown := mk(registry.StatusGoingDown, true)
	healthy := mk(registry.StatusActiveHealthy, true)

	got := h.e.Recover(ctx)
	want := map[string]registry.Status{pausing: registry.StatusInactive, resuming: registry.StatusInactive, creating: registry.StatusInitFailed}
	if len(got) != len(want) {
		t.Fatalf("recovered %+v, want %d projects", got, len(want))
	}
	for ref, status := range want {
		p, _ := h.reg.GetProject(ctx, ref)
		if p.Status != status {
			t.Errorf("%s is %s, want %s", ref, p.Status, status)
		}
	}
	for _, ref := range []string{goingDown, healthy} {
		p, _ := h.reg.GetProject(ctx, ref)
		wantStatus := registry.StatusActiveHealthy
		if ref == goingDown {
			wantStatus = registry.StatusGoingDown
		}
		if p.Status != wantStatus {
			t.Errorf("%s was touched: %s", ref, p.Status)
		}
	}
	if !h.plane.has("Stop "+pausing) || !h.plane.has("Stop "+resuming) || h.plane.has("Stop "+creating) {
		t.Errorf("units stopped for the wrong projects: %v", h.plane.calls)
	}
	if again := h.e.Recover(ctx); len(again) != 0 {
		t.Errorf("a second pass found work: %+v", again)
	}
}

func TestCreateRegionIsARealRegionCode(t *testing.T) {
	h := newHarness(t)
	for in, want := range map[string]string{"": "us-east-1", "local": "us-east-1", "eu-central-1": "eu-central-1", "Frankfurt": "us-east-1"} {
		p, err := h.e.Create(context.Background(), CreateRequest{Name: "r", Region: in})
		if err != nil {
			t.Fatal(err)
		}
		if p.Region != want {
			t.Errorf("region %q -> %q, want %q", in, p.Region, want)
		}
	}
}

type fakeTimers struct{ calls []string }

func (f *fakeTimers) StartTimer(_ context.Context, ref string) error {
	f.calls = append(f.calls, "start "+ref)
	return nil
}
func (f *fakeTimers) StopTimer(_ context.Context, ref string) error {
	f.calls = append(f.calls, "stop "+ref)
	return errors.New("not running") // logged, never fatal
}

func TestBackupTimersFollowTheProject(t *testing.T) {
	h := newHarness(t)
	ft := &fakeTimers{}
	h.e.opts.Timers = ft
	ctx := context.Background()
	p := h.create(t)
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Resume(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if errs := h.e.StartActive(ctx); len(errs) != 0 {
		t.Fatal(errs)
	}
	if err := h.e.Delete(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	want := []string{"start " + p.Ref, "stop " + p.Ref, "start " + p.Ref, "start " + p.Ref, "stop " + p.Ref}
	if fmt.Sprint(ft.calls) != fmt.Sprint(want) {
		t.Errorf("timer calls = %v, want %v", ft.calls, want)
	}
}

// A delete cut off after its final backup settled is finished by Recover without a
// second backup; one cut off during the backup returns the project to where it was.
func TestRecoverFinishesOrRevertsAnInterruptedDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("backup settled: removal finished, no second backup", func(t *testing.T) {
		h := newHarness(t)
		p := h.create(t)
		h.plane.failOn["Delete"] = errors.New("daemon stopped")
		if err := h.e.Delete(ctx, p.Ref); err == nil {
			t.Fatal("delete should have failed at the data plane")
		}
		if got, _ := h.reg.GetProject(ctx, p.Ref); got.Status != registry.StatusGoingDown {
			t.Fatalf("status = %s, want GOING_DOWN", got.Status)
		}
		delete(h.plane.failOn, "Delete")
		h.backup.calls = nil
		rs := h.e.Recover(ctx)
		if len(rs) != 1 || rs[0].To != StatusDeleted {
			t.Fatalf("recovered = %+v", rs)
		}
		if _, err := h.reg.GetProject(ctx, p.Ref); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("project row remains: %v", err)
		}
		if len(h.backup.calls) != 0 {
			t.Fatalf("a second final backup ran: %v", h.backup.calls)
		}
	})

	t.Run("a retried delete does not back up again", func(t *testing.T) {
		h := newHarness(t)
		p := h.create(t)
		h.plane.failOn["Delete"] = errors.New("first attempt cut off")
		_ = h.e.Delete(ctx, p.Ref)
		delete(h.plane.failOn, "Delete")
		h.backup.calls = nil
		h.backup.err = errors.New("the cluster is gone") // would abort the delete if it ran
		if err := h.e.Delete(ctx, p.Ref); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if len(h.backup.calls) != 0 {
			t.Fatalf("retry ran the backup: %v", h.backup.calls)
		}
	})

	t.Run("backup cut off: the paused project is kept and stopped", func(t *testing.T) {
		h := newHarness(t)
		p := h.create(t)
		if err := h.e.Pause(ctx, p.Ref); err != nil {
			t.Fatal(err)
		}
		// What DeleteWith leaves when the process dies inside the backup.
		h.e.event(ctx, p.Ref, EventDeleteStarted, map[string]string{"prev": string(registry.StatusInactive)})
		if err := h.reg.SetProjectStatus(ctx, p.Ref, registry.StatusGoingDown); err != nil {
			t.Fatal(err)
		}
		h.plane.calls = nil
		rs := h.e.Recover(ctx)
		if len(rs) != 1 || rs[0].To != registry.StatusInactive {
			t.Fatalf("recovered = %+v", rs)
		}
		if got, _ := h.reg.GetProject(ctx, p.Ref); got.Status != registry.StatusInactive {
			t.Fatalf("status = %s, want INACTIVE", got.Status)
		}
		if !h.plane.has("Stop " + p.Ref) {
			t.Errorf("the database the backup started was not stopped: %v", h.plane.calls)
		}
	})

	t.Run("no record: only logged", func(t *testing.T) {
		h := newHarness(t)
		p := h.create(t)
		_ = h.reg.SetProjectStatus(ctx, p.Ref, registry.StatusGoingDown)
		if rs := h.e.Recover(ctx); len(rs) != 0 {
			t.Fatalf("recovered = %+v", rs)
		}
	})
}

// The nightly timer stops as soon as the final backup is settled, even when the rest of
// the delete fails.
func TestDeleteStopsTheTimerAfterTheFinalBackup(t *testing.T) {
	h := newHarness(t)
	ft := &fakeTimers{}
	h.e.opts.Timers = ft
	p := h.create(t)
	h.plane.failOn["Delete"] = errors.New("units would not go")
	ft.calls = nil
	if err := h.e.Delete(context.Background(), p.Ref); err == nil {
		t.Fatal("expected the data plane failure")
	}
	if fmt.Sprint(ft.calls) != fmt.Sprint([]string{"stop " + p.Ref}) {
		t.Fatalf("timer calls = %v, want a stop", ft.calls)
	}
}

// A restart cut off after its pause leaves the project INACTIVE with a request nobody
// finished: Recover flags it and ResumeRecovered brings it back.
func TestRecoverResumesAnInterruptedRestart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cut := h.create(t)
	stale := h.create(t)
	for _, p := range []*registry.Project{cut, stale} {
		h.e.event(ctx, p.Ref, EventRestartRequested, nil)
	}
	if err := h.e.Pause(ctx, cut.Ref); err != nil { // the restart's pause, then the process died
		t.Fatal(err)
	}
	rs := h.e.Recover(ctx)
	if len(rs) != 1 || rs[0].Ref != cut.Ref || !rs[0].Resume {
		t.Fatalf("recovered = %+v, want only %s flagged for resume", rs, cut.Ref)
	}
	if errs := h.e.ResumeRecovered(ctx, rs); len(errs) != 0 {
		t.Fatal(errs)
	}
	if got, _ := h.reg.GetProject(ctx, cut.Ref); got.Status != registry.StatusActiveHealthy {
		t.Fatalf("status = %s, want ACTIVE_HEALTHY", got.Status)
	}
	if h.e.restartPending(ctx, cut.Ref) || h.e.restartPending(ctx, stale.Ref) {
		t.Fatal("restart intents were not closed")
	}
	// A manual pause later is not undone by the next start.
	if err := h.e.Pause(ctx, stale.Ref); err != nil {
		t.Fatal(err)
	}
	if rs := h.e.Recover(ctx); len(rs) != 0 {
		t.Fatalf("a stale restart intent resumed a deliberately paused project: %+v", rs)
	}
}

// racyRegistry runs after() once ListProjects has returned its rows, which is the window
// in which an API pause or delete lands between StartActive's listing and startOne's lock.
type racyRegistry struct {
	*registry.Memory
	after func()
}

func (r racyRegistry) ListProjects(ctx context.Context) ([]registry.Project, error) {
	ps, err := r.Memory.ListProjects(ctx)
	if r.after != nil {
		r.after()
	}
	return ps, err
}

// StartActive reads the project list before taking each project's lock. A pause that
// lands in between must stay a pause: no Start, no backup timer, status untouched.
func TestStartActiveDoesNotUndoAPauseThatLandedAfterTheListing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	mem := h.reg
	racy := racyRegistry{Memory: mem}
	e := NewEngine(h.cfg, racy, h.sec, fakeArts{}, h.plane, Options{Fleet: fleet.Fleet{h.tenant}, Backup: h.backup})
	racy.after = func() { _ = mem.SetProjectStatus(ctx, p.Ref, registry.StatusInactive) }
	e.reg = racy
	h.plane.calls = nil
	if errs := e.StartActive(ctx); len(errs) != 0 {
		t.Fatal(errs)
	}
	if h.plane.has("Start " + p.Ref) {
		t.Fatalf("a project paused after the listing was started again: %v", h.plane.calls)
	}
	if got, _ := mem.GetProject(ctx, p.Ref); got.Status != registry.StatusInactive {
		t.Fatalf("status = %s; the registry said INACTIVE", got.Status)
	}
}

// A project deleted after the listing is skipped, not an error.
func TestStartActiveSkipsAProjectDeletedAfterTheListing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.create(t)
	mem := h.reg
	racy := racyRegistry{Memory: mem}
	e := NewEngine(h.cfg, racy, h.sec, fakeArts{}, h.plane, Options{Fleet: fleet.Fleet{h.tenant}, Backup: h.backup})
	racy.after = func() { _ = mem.DeleteProject(ctx, p.Ref) }
	e.reg = racy
	h.plane.calls = nil
	if errs := e.StartActive(ctx); len(errs) != 0 {
		t.Fatal(errs)
	}
	if h.plane.has("Start " + p.Ref) {
		t.Fatalf("a deleted project was started: %v", h.plane.calls)
	}
}
