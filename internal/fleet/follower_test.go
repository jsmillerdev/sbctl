package fleet

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/units"
)

// standbyRegistry is a registry on a standby: it reads, and every secret write is refused, as the
// registry's read-only pool refuses it.
type standbyRegistry struct {
	registry.Registry
	writes int
}

func (s *standbyRegistry) PutSecret(context.Context, string, string, []byte) error {
	s.writes++
	return registry.ErrReadOnly
}

func always(v bool) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return v, nil }
}

// followerRig is a managerRig whose Manager is a follower. The leader's secrets exist already, as
// they do on a node that joined a cluster: a leader's Start made them, and they replicated.
type followerRig struct {
	*managerRig
	standby *standbyRegistry
}

func newFollowerRig(t *testing.T) *followerRig {
	t.Helper()
	r := newManagerRig(t, nil)
	if err := r.m.Start(context.Background()); err != nil { // the leader's first start generates the secrets
		t.Fatal(err)
	}
	standby := &standbyRegistry{Registry: r.n.reg}
	r.sup.mu.Lock()
	r.sup.calls = nil
	r.sup.mu.Unlock()
	d := r.m.d
	d.Registry, d.Follower = standby, always(true)
	m, err := NewManager(d)
	if err != nil {
		t.Fatal(err)
	}
	r.m = m
	return &followerRig{managerRig: r, standby: standby}
}

func TestFollowerStartsSupavisorWithoutPrepareAndParksTheRest(t *testing.T) {
	r := newFollowerRig(t)
	if err := r.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	log := r.sup.log()
	if !strings.Contains(log, "render supavise-supavisor.service") || !strings.Contains(log, "\nstart supavise-supavisor.service") {
		t.Fatalf("supavisor was not started:\n%s", log)
	}
	for _, svc := range []string{"pgmeta", "realtime", "storage", "studio"} {
		unit := "supavise-" + svc + ".service"
		if !strings.Contains(log, "render "+unit) {
			t.Errorf("%s was not rendered:\n%s", svc, log)
		}
		if strings.Contains(log, "start "+unit) {
			t.Errorf("%s was started on a follower:\n%s", svc, log)
		}
	}
	for unit, spec := range r.sup.specs {
		if len(spec.PreStart) != 0 {
			t.Errorf("%s has a PreStart on a follower: %v", unit, spec.PreStart)
		}
	}
	// The same environment as the leader's: the same VAULT_ENC_KEY opens the replicated tenants.
	sv := r.sup.specs["supavise-supavisor.service"]
	if sv.Env["VAULT_ENC_KEY"] == "" || !strings.Contains(sv.Env["DATABASE_URL"], "/_supavisor") {
		t.Errorf("supavisor env = %v", sv.Env)
	}
	if r.standby.writes != 0 {
		t.Errorf("a follower wrote %d secrets to the registry", r.standby.writes)
	}
}

// What a leader renders, a follower renders too, minus the PreStart: promotion changes the files
// the PreStart is in and nothing else.
func TestFollowerSupavisorDiffersFromTheLeadersOnlyByThePreStart(t *testing.T) {
	r := newManagerRig(t, nil)
	ctx := context.Background()
	leader, err := r.m.Specs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	d := r.m.d
	d.Follower = always(true)
	fm, err := NewManager(d)
	if err != nil {
		t.Fatal(err)
	}
	follower, err := fm.Specs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(leader) != len(follower) {
		t.Fatalf("%d specs vs %d", len(leader), len(follower))
	}
	for i := range leader {
		l, f := leader[i], follower[i]
		l.PreStart = nil
		if l.Service != f.Service || l.Exec != nil && strings.Join(l.Exec, " ") != strings.Join(f.Exec, " ") || len(l.Env) != len(f.Env) {
			t.Errorf("%s differs beyond the PreStart:\n%+v\n%+v", l.Service, l, f)
		}
		for k, v := range l.Env {
			if f.Env[k] != v && k != downstreamCertMarkerEnv {
				t.Errorf("%s: %s differs", l.Service, k)
			}
		}
		if len(f.PreStart) != 0 {
			t.Errorf("%s keeps its PreStart on a follower", f.Service)
		}
	}
}

func TestFollowerStartStopsAServiceTheNodeRanAsLeader(t *testing.T) {
	r := newManagerRig(t, nil)
	ctx := context.Background()
	if err := r.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.sup.mu.Lock()
	r.sup.calls = nil
	r.sup.mu.Unlock()
	if err := r.m.Apply(ctx, ModeFollower); err != nil {
		t.Fatal(err)
	}
	log := r.sup.log()
	for _, svc := range []string{"pgmeta", "realtime", "storage", "studio"} {
		unit := "supavise-" + svc + ".service"
		if !strings.Contains(log, "stop "+unit) {
			t.Errorf("%s keeps running after the node was demoted:\n%s", svc, log)
		}
		if st, _ := r.sup.Status(ctx, unit); st.State == units.StateActive {
			t.Errorf("%s is active", unit)
		}
	}
	// Supavisor restarts: its files lost the PreStart.
	if !strings.Contains(log, "stop supavise-supavisor.service\nstart supavise-supavisor.service") {
		t.Errorf("supavisor was not restarted without bin/prepare:\n%s", log)
	}
	// ... and the promotion puts it all back.
	r.sup.mu.Lock()
	r.sup.calls = nil
	r.sup.mu.Unlock()
	if err := r.m.Apply(ctx, ModeLeader); err != nil {
		t.Fatal(err)
	}
	log = r.sup.log()
	for _, svc := range Services {
		if !strings.Contains(log, "start supavise-"+svc+".service") {
			t.Errorf("%s was not started on promotion:\n%s", svc, log)
		}
	}
	if len(r.sup.specs["supavise-supavisor.service"].PreStart) == 0 || len(r.sup.specs["supavise-realtime.service"].PreStart) == 0 {
		t.Error("the leader's Supavisor and Realtime lost their PreStart")
	}
}

func TestApplyStoppedStopsEverythingInReverseOrder(t *testing.T) {
	r := newManagerRig(t, nil)
	ctx := context.Background()
	if err := r.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.sup.mu.Lock()
	r.sup.calls = nil
	r.sup.mu.Unlock()
	if err := r.m.Apply(ctx, ModeStopped); err != nil {
		t.Fatal(err)
	}
	want := "stop supavise-studio.service|stop supavise-storage.service|stop supavise-realtime.service|stop supavise-supavisor.service|stop supavise-pgmeta.service"
	if got := strings.ReplaceAll(r.sup.log(), "\n", "|"); got != want {
		t.Fatalf("calls:\n%s", got)
	}
}

func TestFollowerStartFailsWhenSupavisorCannotStartButNotForAParkedService(t *testing.T) {
	r := newFollowerRig(t)
	delete(r.m.d.Artifacts.(fakeArtifacts), config.SvcRealtime) // a follower without the artifact still follows
	if err := r.m.Start(context.Background()); err != nil {
		t.Fatalf("a parked service's missing artifact stopped the follower: %v", err)
	}
	r.sup.failOn["start supavise-supavisor.service"] = errors.New("exec format error")
	err := r.m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "supavisor") {
		t.Fatalf("err = %v", err)
	}
}

func TestFollowerStatusCountsOnlySupavisor(t *testing.T) {
	r := newFollowerRig(t)
	ctx := context.Background()
	if err := r.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	hs := r.m.Status(ctx)
	if !AllHealthy(hs) {
		t.Fatalf("a follower with Supavisor up is unhealthy: %+v", hs)
	}
	for _, h := range hs {
		switch h.Service {
		case config.SvcSupavisor:
			if !h.Healthy || h.Optional {
				t.Errorf("%+v", h)
			}
		default:
			if h.Healthy || !h.Optional || h.Status != "STOPPED" || !strings.Contains(h.Error, "parked") {
				t.Errorf("%+v", h)
			}
		}
	}
	// A parked service that runs is the fault: its port belongs to the forwarder.
	r.sup.mu.Lock()
	r.sup.state["supavise-realtime.service"] = units.StateActive
	r.sup.mu.Unlock()
	r.health["realtime"].setUp(true)
	if AllHealthy(r.m.Status(ctx)) {
		t.Fatal("Realtime running on a follower was not reported")
	}
}

// `supavise fleet status` has no registry to ask: it follows what Start recorded.
func TestStatusWithoutARegistryFollowsTheRecordedMode(t *testing.T) {
	r := newFollowerRig(t)
	ctx := context.Background()
	if err := r.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(modeFile(r.n.cfg))
	if err != nil || strings.TrimSpace(string(b)) != "follower" {
		t.Fatalf("mode file = %q, %v", b, err)
	}
	stateless, err := NewManager(Deps{Cfg: r.n.cfg, Supervisor: r.sup})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range stateless.Status(ctx) {
		if h.Service == config.SvcRealtime && !h.Optional {
			t.Errorf("Realtime is not optional on a follower: %+v", h)
		}
	}
	if err := r.m.Apply(ctx, ModeLeader); err != nil {
		t.Fatal(err)
	}
	for _, h := range stateless.Status(ctx) {
		if h.Service == config.SvcRealtime && h.Optional {
			t.Errorf("Realtime is optional on a leader: %+v", h)
		}
	}
}

func TestFollowerDoesNotRefreshStudio(t *testing.T) {
	r := newFollowerRig(t)
	ctx := context.Background()
	touchRunFile(t, r.n.cfg, config.SvcStudio)
	r.sup.mu.Lock()
	r.sup.calls = nil
	r.sup.mu.Unlock()
	if err := r.m.RefreshStudio(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.sup.log(); got != "" {
		t.Fatalf("a follower touched Studio:\n%s", got)
	}
}

func TestIsFollowerAsksTheDepsThenTheRegistry(t *testing.T) {
	ctx := context.Background()
	n := newTestNode(t)
	if f, err := n.deps().isFollower(ctx); f || err != nil {
		t.Errorf("an in-memory registry is a leader: %v, %v", f, err)
	}
	d := n.deps()
	d.Follower = always(true)
	if f, err := d.isFollower(ctx); !f || err != nil {
		t.Errorf("Deps.Follower: %v, %v", f, err)
	}
	boom := errors.New("registry down")
	d.Follower = func(context.Context) (bool, error) { return false, boom }
	if _, err := d.isFollower(ctx); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	// An error is not an answer: Start does not guess.
	d.Registry, d.Supervisor, d.Artifacts = n.reg, newFakeSupervisor(), allArtifacts()
	m, _ := NewManager(d)
	if err := m.Start(ctx); !errors.Is(err, boom) {
		t.Errorf("Start = %v", err)
	}
}

// A follower leaves the tenant calls to the leader, whatever asks for them.
func TestFollowerTenantsSkipTheLeadersCalls(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	api := newFakeAPI(t, bearerOK("s"), pathTenant("/api/tenants/"), 201)
	cl, _ := testClient("supavisor")
	following, managerCalls := true, 0
	gate := followerGate{following: func(context.Context) (bool, error) { return following, nil }}
	sv := &supavisorTenant{followerGate: gate, cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: api.srv.URL, secret: "s", now: time.Now,
		setManager: func(context.Context, TenantSpec, string) error { managerCalls++; return nil }}
	rt := &realtimeTenant{followerGate: gate, cl: cl, store: sv.store, base: api.srv.URL, secret: "s", now: time.Now}
	st := &storageTenant{followerGate: gate, cl: cl, store: sv.store, base: api.srv.URL, adminKey: "k", fileSize: 1}
	ctx := context.Background()
	spec := testSpec(k, 20003)
	for _, tn := range (Fleet{sv, rt, st}) {
		if err := tn.EnsureTenant(ctx, spec); err != nil {
			t.Errorf("%s: %v", tn.Service(), err)
		}
		if err := tn.RemoveTenant(ctx, testRef); err != nil {
			t.Errorf("%s: %v", tn.Service(), err)
		}
	}
	if err := (Fleet{sv, rt, st}).QuiesceTenant(ctx, testRef); err != nil {
		t.Error(err)
	}
	if err := sv.EnsureReplicaTenant(ctx, replicaSpec(k)); err != nil {
		t.Error(err)
	}
	if err := sv.RemoveReplicaTenant(ctx, testReplica); err != nil {
		t.Error(err)
	}
	for _, tn := range []TenantChecker{rt, st} {
		if ok, err := tn.HasTenant(ctx, testRef); !ok || err != nil {
			t.Errorf("a parked service is blamed for a missing tenant: %v, %v", ok, err)
		}
	}
	if len(api.calls) != 0 || managerCalls != 0 {
		t.Fatalf("a follower sent %d tenant calls and set %d manager roles", len(api.calls), managerCalls)
	}
	if _, err := n.reg.GetSecret(ctx, testRef, fingerprintName(config.SvcSupavisor)); err == nil {
		t.Error("a follower recorded a fingerprint")
	}
	// The one call a follower makes: forget the cached target of a tenant row that changed.
	if err := sv.RefreshTenant(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET" || !strings.HasSuffix(api.last().Path, "/terminate") {
		t.Fatalf("calls = %s %s", got, api.last().Path)
	}

	// The same tenants, promoted: the gate is asked on every call, so a Lazy that lived through
	// the promotion works at once.
	following = false
	if err := sv.EnsureTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET GET PUT" || managerCalls != 1 {
		t.Fatalf("calls after promotion = %s, manager roles %d", got, managerCalls)
	}
}

func TestFollowerGateErrorStopsTheCall(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	api := newFakeAPI(t, bearerOK("s"), pathTenant("/api/tenants/"), 201)
	cl, _ := testClient("supavisor")
	boom := errors.New("registry down")
	sv := &supavisorTenant{followerGate: followerGate{following: func(context.Context) (bool, error) { return false, boom }},
		cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: api.srv.URL, secret: "s", now: time.Now,
		setManager: func(context.Context, TenantSpec, string) error { return nil }}
	if err := sv.EnsureTenant(context.Background(), testSpec(k, 20003)); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if len(api.calls) != 0 {
		t.Fatal("a call went out although the node could not tell what it is")
	}
}

// Setup on a standby generates nothing and hands out tenants that skip the leader's calls.
func TestSetupOnAFollowerWritesNothing(t *testing.T) {
	r := newManagerRig(t, nil)
	if err := r.m.Start(context.Background()); err != nil { // the leader's secrets
		t.Fatal(err)
	}
	d := startingDeps(r)
	standby := &standbyRegistry{Registry: r.n.reg}
	d.Registry, d.Follower = standby, always(true)
	d.Retry = Retry{Attempts: 1}
	f, err := Setup(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if standby.writes != 0 {
		t.Fatalf("%d writes", standby.writes)
	}
	if err := f.EnsureTenant(context.Background(), testSpec(r.n.project(t, testRef), 20003)); err != nil {
		t.Fatalf("a follower's EnsureTenant: %v", err)
	}
	// Without the leader's secrets a follower says so, instead of making its own.
	empty := newManagerRig(t, nil)
	d = startingDeps(empty)
	d.Start, d.Follower = false, always(true)
	if _, err := Setup(context.Background(), d); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("Setup on a follower with no replicated secrets = %v", err)
	}
}
