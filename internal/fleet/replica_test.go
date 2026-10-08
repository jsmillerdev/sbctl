package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

const testReplica = testRef + "-rr-eu-west-1-abc123"

func replicaSpec(k *secrets.ProjectKeys) TenantSpec {
	s := testSpec(k, 10003)
	s.ReplicaID = testReplica
	return s
}

func newReplicaTenant(t *testing.T, n *testNode, apiURL string, managerCalls *int) *supavisorTenant {
	t.Helper()
	cl, _ := testClient(config.SvcSupavisor)
	return &supavisorTenant{
		cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: apiURL, secret: "s", now: time.Now,
		setManager: func(context.Context, TenantSpec, string) error { *managerCalls++; return nil },
	}
}

func TestSupavisorReplicaTenantLifecycle(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	api := newFakeAPI(t, bearerOK("s"), pathTenant("/api/tenants/"), 201)
	var managerCalls int
	tn := newReplicaTenant(t, n, api.srv.URL, &managerCalls)
	ctx := context.Background()
	spec := replicaSpec(k)

	if err := tn.EnsureReplicaTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET PUT" {
		t.Fatalf("calls = %s", got)
	}
	put := api.last()
	if put.Path != "/api/tenants/"+testReplica {
		t.Errorf("the external id is the replica's identifier: path = %s", put.Path)
	}
	tenant := put.Body["tenant"].(map[string]any)
	if tenant["db_host"] != "127.0.0.1" || tenant["db_port"] != float64(10003) || tenant["db_database"] != "postgres" {
		t.Errorf("tenant = %v", tenant)
	}
	u := tenant["users"].([]any)[0].(map[string]any)
	if u["db_user"] != "pgbouncer" || u["db_password"] != managerPassword(k.AdminPassword) || u["is_manager"] != true {
		t.Errorf("manager user = %v", u)
	}
	// The pgbouncer role's password is the project's, set by the primary's own tenant and
	// replicated; a standby cannot be written.
	if managerCalls != 0 {
		t.Errorf("the replica tenant set the manager role %d times", managerCalls)
	}

	// Unchanged: nothing is sent, so the pools stay. The fingerprint is the replica's own.
	if err := tn.EnsureReplicaTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET PUT GET" {
		t.Fatalf("calls = %s", got)
	}
	if tn.store.get(ctx, testRef, config.SvcSupavisor) != "" {
		t.Error("the replica's fingerprint took the project tenant's place")
	}
	if tn.store.get(ctx, testRef, supavisorReplicaService(testReplica)) == "" {
		t.Error("no fingerprint recorded for the replica tenant")
	}
	// The replica moved (a new port): sent again.
	spec.DBPort = 10006
	if err := tn.EnsureReplicaTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET PUT GET GET PUT" || api.last().Body["tenant"].(map[string]any)["db_port"] != float64(10006) {
		t.Fatalf("calls = %s", got)
	}

	// The project's own tenant is a different tenant.
	if err := tn.EnsureTenant(ctx, testSpec(k, 20003)); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	both := api.tenants[testRef] && api.tenants[testReplica]
	api.mu.Unlock()
	if !both || managerCalls != 1 {
		t.Errorf("project and replica tenants: both=%v, manager roles %d", both, managerCalls)
	}

	// Removing the replica's tenant leaves the project's.
	if err := tn.RemoveReplicaTenant(ctx, testReplica); err != nil {
		t.Fatal(err)
	}
	last := api.last()
	if last.Method != "DELETE" || last.Path != "/api/tenants/"+testReplica {
		t.Fatalf("last call = %s %s", last.Method, last.Path)
	}
	if tn.store.get(ctx, testRef, supavisorReplicaService(testReplica)) != "" {
		t.Error("fingerprint survived the removal")
	}
	api.mu.Lock()
	still := api.tenants[testRef]
	api.mu.Unlock()
	if !still {
		t.Error("removing the replica's tenant removed the project's")
	}
	if err := tn.RemoveReplicaTenant(ctx, testReplica); err != nil {
		t.Fatalf("removing a missing tenant: %v", err)
	}
}

func TestSupavisorReplicaTenantRejectsBadInput(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	api := newFakeAPI(t, bearerOK("s"), pathTenant("/api/tenants/"), 201)
	var managerCalls int
	tn := newReplicaTenant(t, n, api.srv.URL, &managerCalls)
	ctx := context.Background()
	for name, edit := range map[string]func(*TenantSpec){
		"no identifier":     func(s *TenantSpec) { s.ReplicaID = "" },
		"the project's ref": func(s *TenantSpec) { s.ReplicaID = testRef },
		"another project":   func(s *TenantSpec) { s.ReplicaID = "zzzzzzzzzzzzzzzzzzzz-rr-eu-west-1-abc123" },
		"a bad shape":       func(s *TenantSpec) { s.ReplicaID = testRef + "-rr-eu-west-1" },
		"no port":           func(s *TenantSpec) { s.DBPort = 0 },
		"no password":       func(s *TenantSpec) { s.DBPassword = "" },
		"the system":        func(s *TenantSpec) { s.Ref, s.ReplicaID = "system", "system-rr-eu-west-1-abc123" },
	} {
		spec := replicaSpec(k)
		edit(&spec)
		if err := tn.EnsureReplicaTenant(ctx, spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := tn.RemoveReplicaTenant(ctx, testRef); err == nil {
		t.Error("removing the project's ref as a replica was accepted")
	}
	if len(api.calls) != 0 {
		t.Errorf("%d calls for invalid input", len(api.calls))
	}
}

// RefreshTenant and HasTenant take a replica identifier as well as a ref.
func TestSupavisorRefreshAndPresenceTakeAReplicaIdentifier(t *testing.T) {
	n := newTestNode(t)
	api := newFakeAPI(t, bearerOK("s"), pathTenant("/api/tenants/"), 201)
	var managerCalls int
	tn := newReplicaTenant(t, n, api.srv.URL, &managerCalls)
	ctx := context.Background()
	if err := tn.RefreshTenant(ctx, testReplica); err != nil {
		t.Fatal(err)
	}
	if got := api.last(); got.Method != "GET" || got.Path != "/api/tenants/"+testReplica+"/terminate" {
		t.Fatalf("call = %s %s", got.Method, got.Path)
	}
	if ok, err := tn.HasTenant(ctx, testReplica); ok || err != nil {
		t.Fatalf("HasTenant of a tenant Supavisor lacks = %v, %v", ok, err)
	}
	for _, bad := range []string{"system", "short", testRef + "-rr-x", "../etc"} {
		if err := tn.RefreshTenant(ctx, bad); err == nil {
			t.Errorf("refresh of %q accepted", bad)
		}
	}
}

func TestTenantSpecForReplica(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	p, _ := n.reg.GetProject(context.Background(), testRef)
	r := &registry.Replica{Identifier: testReplica, Ref: testRef, NodeID: "n2"}
	s := TenantSpecForReplica(n.cfg, p, r, k)
	want := n.cfg.ReplicaPorts(testRef, p.Seq).Postgres
	if s.Ref != testRef || s.ReplicaID != testReplica || s.DBPort != want || s.DBHost != "127.0.0.1" || s.DBPassword != k.AdminPassword || s.PostgresPassword != k.DBPassword {
		t.Fatalf("spec = %+v", s)
	}
	if s.DBPort == n.cfg.PortsFor(testRef, p.Seq).Postgres {
		t.Error("the replica tenant points at the primary's port")
	}
	// The tenant body is the project's own with the replica's port.
	body := supavisorBody(s, managerPassword(s.DBPassword))
	if sub(body, "tenant", "db_port") != want {
		t.Errorf("body = %v", body)
	}
}

func TestLoadReplicaTenantSpec(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	ctx := context.Background()
	n.cfg.Ports.ProjectBase, n.cfg.Ports.ReplicaBase = 30000, 12000
	if err := n.reg.CreateNode(ctx, &registry.Node{ID: "n2", Name: "second", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	if err := n.reg.CreateReplica(ctx, &registry.Replica{Identifier: testReplica, Ref: testRef, NodeID: "n2"}); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadReplicaTenantSpec(ctx, n.deps(), testReplica)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := n.reg.GetProject(ctx, testRef)
	if spec.ReplicaID != testReplica || spec.DBPort != 12000+3*p.Seq || spec.DBPassword != k.AdminPassword || spec.Ref != testRef {
		t.Fatalf("spec = %+v", spec)
	}
	if _, err := LoadReplicaTenantSpec(ctx, n.deps(), testRef+"-rr-eu-west-1-zzzzzz"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("unknown replica = %v", err)
	}
	// A project whose replica would have no port.
	n.cfg.Ports.ReplicaBase = 29997
	if _, err := LoadReplicaTenantSpec(ctx, n.deps(), testReplica); err == nil || !strings.Contains(err.Error(), "sequence") {
		t.Errorf("a project past MaxReplicaSeq = %v", err)
	}
}

type replicaStub struct {
	stubTenant
	ensured []string
	removed []string
	err     error
}

func (r *replicaStub) EnsureReplicaTenant(_ context.Context, s TenantSpec) error {
	r.ensured = append(r.ensured, s.ReplicaID)
	return r.err
}

func (r *replicaStub) RemoveReplicaTenant(_ context.Context, id string) error {
	r.removed = append(r.removed, id)
	return r.err
}

func TestFleetReplicaTenantsGoOnlyToServicesThatPoolReplicas(t *testing.T) {
	var order []string
	plain := &stubTenant{name: "realtime", order: &order}
	a := &replicaStub{stubTenant: stubTenant{name: "supavisor", order: &order}}
	f := Fleet{a, plain}
	ctx := context.Background()
	if err := f.EnsureReplicaTenant(ctx, TenantSpec{ReplicaID: testReplica}); err != nil {
		t.Fatal(err)
	}
	if err := f.RemoveReplicaTenant(ctx, testReplica); err != nil {
		t.Fatal(err)
	}
	if len(a.ensured) != 1 || len(a.removed) != 1 || len(order) != 0 {
		t.Fatalf("ensured %v removed %v other calls %v", a.ensured, a.removed, order)
	}
	a.err = errors.New("boom")
	if err := f.EnsureReplicaTenant(ctx, TenantSpec{}); err == nil {
		t.Error("an error was swallowed")
	}
	if err := (Fleet{}).EnsureReplicaTenant(ctx, TenantSpec{}); err != nil {
		t.Errorf("an empty fleet: %v", err)
	}
}

// A node that never rendered Supavisor has nothing to pool a replica with: the lazy entry skips it.
func TestLazyReplicaTenantSkipsAServiceTheNodeNeverRendered(t *testing.T) {
	n := newTestNode(t)
	lz := NewLazy(Deps{Cfg: n.cfg})
	lz.Bind(n.reg, n.sec)
	f := lz.Fleet()
	if err := f.EnsureReplicaTenant(context.Background(), TenantSpec{Ref: testRef, ReplicaID: testReplica}); err != nil {
		t.Fatal(err)
	}
	if err := f.RemoveReplicaTenant(context.Background(), testReplica); err != nil {
		t.Fatal(err)
	}
}
