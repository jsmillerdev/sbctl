package app

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
	"github.com/supavise/supavise/internal/secrets"
)

// fakeTenant is a shared service's tenant that records what it is asked.
type fakeTenant struct {
	svc  string
	mu   sync.Mutex
	logs *[]string
	fail error
}

func (f *fakeTenant) rec(s string) {
	f.mu.Lock()
	*f.logs = append(*f.logs, f.svc+" "+s)
	f.mu.Unlock()
}
func (f *fakeTenant) Service() string { return f.svc }
func (f *fakeTenant) EnsureTenant(_ context.Context, s fleet.TenantSpec) error {
	f.rec("ensure " + s.Ref + " port " + itoa(s.DBPort))
	return f.fail
}
func (f *fakeTenant) RemoveTenant(_ context.Context, ref string) error {
	f.rec("remove " + ref)
	return nil
}
func (f *fakeTenant) EnsureReplicaTenant(_ context.Context, s fleet.TenantSpec) error {
	f.rec("ensure replica " + s.ReplicaID + " of " + s.Ref + " port " + itoa(s.DBPort))
	return f.fail
}
func (f *fakeTenant) RemoveReplicaTenant(_ context.Context, id string) error {
	f.rec("remove replica " + id)
	return nil
}
func (f *fakeTenant) QuiesceTenant(_ context.Context, ref string) error {
	f.rec("quiesce " + ref)
	return nil
}

// plainTenant is a tenant that serves no replica, like Realtime and Storage.
type plainTenant struct{ f *fakeTenant }

func (p plainTenant) Service() string { return p.f.svc }
func (p plainTenant) EnsureTenant(ctx context.Context, s fleet.TenantSpec) error {
	return p.f.EnsureTenant(ctx, s)
}
func (p plainTenant) RemoveTenant(ctx context.Context, ref string) error {
	return p.f.RemoveTenant(ctx, ref)
}
func (p plainTenant) QuiesceTenant(ctx context.Context, ref string) error {
	return p.f.QuiesceTenant(ctx, ref)
}

func itoa(n int) string { return strconv.Itoa(n) }

type recPeers struct {
	mu      sync.Mutex
	tenants []string
	err     error
}

func (r *recPeers) RefreshPeers(_ context.Context, tenant string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tenants = append(r.tenants, tenant)
	return r.err
}

// tenantFixture is a registry with a project that has credentials, a second node and a replica on it,
// and a fleet of fake tenants over it.
func tenantFixture(t *testing.T) (fleet.Fleet, fleet.Deps, *[]string, string) {
	t.Helper()
	ctx := context.Background()
	reg := registry.NewMemory()
	const ref = "abcdefghijklmnopqrst"
	if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: "p", Class: "small", Status: registry.StatusActiveHealthy, NodeID: registry.FounderNodeID}); err != nil {
		t.Fatal(err)
	}
	if err := reg.CreateNode(ctx, &registry.Node{Name: "second", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	keys := &secrets.ProjectKeys{JWTSecret: "jwt", AdminPassword: "admin", DBPassword: "db", AnonKey: "anon", ServiceRoleKey: "service"}
	for name, v := range keys.Map() {
		if v == "" {
			continue
		}
		sealed, err := sec.Seal([]byte(v))
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.PutSecret(ctx, ref, name, sealed); err != nil {
			t.Fatal(err)
		}
	}
	id := registry.ReplicaIdentifier(ref, "us-east-1", "abc123")
	if err := reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: ref, NodeID: "n2", Status: "ACTIVE_HEALTHY"}); err != nil {
		t.Fatal(err)
	}
	var logs []string
	fl := fleet.Fleet{&fakeTenant{svc: config.SvcSupavisor, logs: &logs}, plainTenant{&fakeTenant{svc: config.SvcRealtime, logs: &logs}}}
	return fl, fleet.Deps{Cfg: config.Default(), Registry: reg, Secrets: sec}, &logs, id
}

// The pooler tenant of a replica is made from the replica's row and port, and the other nodes are told.
func TestReplicaPoolerMakesTheTenantAndTellsThePeers(t *testing.T) {
	fl, deps, logs, id := tenantFixture(t)
	peers := &recPeers{}
	p := &replicaPooler{fleet: fl, deps: deps, peers: peers, log: quiet()}
	const ref = "abcdefghijklmnopqrst"
	if err := p.EnsureReplicaTenant(context.Background(), ref, id); err != nil {
		t.Fatal(err)
	}
	want := "supavisor ensure replica " + id + " of " + ref + " port "
	if len(*logs) != 1 || !strings.HasPrefix((*logs)[0], want) {
		t.Fatalf("tenant calls: %v, want one starting %q (Realtime serves no replica)", *logs, want)
	}
	if len(peers.tenants) != 1 || peers.tenants[0] != id {
		t.Fatalf("peers told about %v", peers.tenants)
	}
	// A replica named for another project is refused before anything is sent.
	*logs, peers.tenants = nil, nil
	if err := p.EnsureReplicaTenant(context.Background(), "bcdefghijklmnopqrstu", id); err == nil || len(*logs) != 0 {
		t.Fatalf("a replica of another project: %v, %v", err, *logs)
	}
	// A node that does not answer is not the replica's failure.
	peers.err = errors.New("node n2: no session")
	if err := p.RemoveReplicaTenant(context.Background(), id); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(*logs) != 1 || (*logs)[0] != "supavisor remove replica "+id || len(peers.tenants) != 1 {
		t.Fatalf("calls %v, peers %v", *logs, peers.tenants)
	}
	// A tenant call that fails is the replica's failure, and nobody is told.
	peers.tenants = nil
	fl[0].(*fakeTenant).fail = errors.New("supavisor: connection refused")
	if err := p.EnsureReplicaTenant(context.Background(), ref, id); err == nil || len(peers.tenants) != 0 {
		t.Fatalf("a failed tenant: %v, peers told %v", err, peers.tenants)
	}
	var _ replicas.Pooler = p
}

// A project that moves is registered again from its stored credentials, and quiesced by the shared services.
func TestProjectTenantsQuiesceAndEnsure(t *testing.T) {
	fl, deps, logs, _ := tenantFixture(t)
	peers := &recPeers{}
	pt := &projectTenants{fleet: fl, deps: deps, peers: peers, log: quiet()}
	const ref = "abcdefghijklmnopqrst"
	if err := pt.QuiesceTenant(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if len(*logs) != 2 || (*logs)[0] != "supavisor quiesce "+ref || (*logs)[1] != "realtime quiesce "+ref {
		t.Fatalf("quiesce calls: %v", *logs)
	}
	*logs = nil
	if err := pt.EnsureTenant(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if len(*logs) != 2 || !strings.HasPrefix((*logs)[0], "supavisor ensure "+ref+" port ") || !strings.HasPrefix((*logs)[1], "realtime ensure "+ref+" port ") {
		t.Fatalf("ensure calls: %v", *logs)
	}
	if len(peers.tenants) != 1 || peers.tenants[0] != ref {
		t.Fatalf("peers told about %v", peers.tenants)
	}
	if err := pt.EnsureTenant(context.Background(), "bcdefghijklmnopqrstu"); err == nil {
		t.Fatal("a project the registry does not know was registered")
	}
}

// A leader with no WAL position to read (an in-memory registry) still tells the peers: they refresh
// at once, with no position to wait for.
func TestPeerRefresherWithoutAPositionSendsNone(t *testing.T) {
	fm := &fakeMesh{}
	mem := clusterOf("n1", cluster.RoleLeader, "n1", nodeRow("n1", registry.NodeActive), nodeRow("n2", registry.NodeActive))
	p := &peerRefresher{mem: mem, rpc: fm, log: quiet()}
	if err := p.RefreshPeers(context.Background(), "abcdefghijklmnopqrst"); err != nil {
		t.Fatal(err)
	}
	if len(fm.calls) != 1 || fm.calls[0].Node != "n2" || fm.calls[0].In.LSN != "" {
		t.Fatalf("calls %+v", fm.calls)
	}
}

// With the host layer behind this release the cluster features do not start, and the replica
// controller does not run on a node that already belongs to a cluster.
func TestHostBehindKeepsTheClusterFeaturesOff(t *testing.T) {
	behind := hostsetup.Status{Have: 1, Want: 2, Known: true}
	w := testWire(t)
	Provide[hostsetup.Status](w, behind)
	reason, is := hostBehind(w)
	if !is || !strings.Contains(reason, "supavise system converge") || !strings.Contains(reason, "revision 1") {
		t.Fatalf("hostBehind = %q, %v", reason, is)
	}
	if _, is := hostBehind(testWire(t)); is {
		t.Fatal("a node that tells nothing about its host is behind")
	}
	Provide[hostsetup.Status](w, hostsetup.Status{Have: 2, Want: 2, Known: true})
	if _, is := hostBehind(w); is {
		t.Fatal("a converged node is behind")
	}
	Provide[hostsetup.Status](w, hostsetup.Status{Want: 2, Err: errors.New("unreadable")})
	if _, is := hostBehind(w); is {
		t.Fatal("a marker nobody could read is not proof that the node is behind")
	}

	// A server that has no cluster identity does not become a cluster while it is behind.
	mesh.ResetDefaultMux()
	t.Cleanup(mesh.ResetDefaultMux)
	w2 := testWire(t)
	Provide[hostsetup.Status](w2, behind)
	if err := wireMesh(context.Background(), w2); err != nil {
		t.Fatal(err)
	}
	if len(w2.runners) != 0 {
		t.Fatalf("a host that is behind watches for a cluster identity: %v", w2.runners)
	}
	if r, off := w2.offReason("cluster"); !off || !strings.Contains(r, "converge") {
		t.Fatalf("cluster off: %q, %v", r, off)
	}

	// A node that belongs to a cluster keeps its mesh and runs no controller of new replicas.
	w3 := clusterNodeWire(t)
	t.Cleanup(w3.stop)
	Provide[hostsetup.Status](w3, behind)
	if err := w3.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !w3.clustered() {
		t.Fatal("a joined node lost its mesh because its host is behind")
	}
	for _, name := range []string{"replica controller", "failover monitor"} {
		if r, off := w3.offReason(name); !off || !strings.Contains(r, "converge") {
			t.Errorf("%s: off %v, reason %q", name, off, r)
		}
	}
	for _, r := range w3.runners {
		if r.name == "replicas" || r.name == "failover monitor" {
			t.Errorf("%s runs on a host that is behind", r.name)
		}
	}
}

// recSupervisor records the units it is asked to stop and start.
type recSupervisor struct {
	nullSupervisor
	mu    sync.Mutex
	calls []string
}

func (r *recSupervisor) Stop(_ context.Context, unit string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "stop "+unit)
	return nil
}

func (r *recSupervisor) Start(_ context.Context, unit string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "start "+unit)
	return nil
}

// A planned stop of the leader stops the dashboard's sign-in service first and the shared services after
// it, the system cluster staying up; a start brings them back in the other order.
func TestLocalServicesStopTheSignInServiceThenTheSharedServices(t *testing.T) {
	sup := &recSupervisor{}
	mgr, err := fleet.NewManager(fleet.Deps{Cfg: config.Default(), Supervisor: sup})
	if err != nil {
		t.Fatal(err)
	}
	l := &localServices{mgr: mgr, sup: sup}
	if err := l.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sup.calls) < 2 || sup.calls[0] != "stop supavise-gotrue@system.service" {
		t.Fatalf("calls: %v", sup.calls)
	}
	for _, c := range sup.calls {
		if strings.Contains(c, "postgres@system") {
			t.Fatalf("the system cluster was stopped: %v", sup.calls)
		}
	}
	var sawStudio bool
	for _, c := range sup.calls[1:] {
		sawStudio = sawStudio || strings.Contains(c, "studio")
	}
	if !sawStudio {
		t.Fatalf("the shared services were not stopped: %v", sup.calls)
	}
	// With nothing to render from, the shared services do not start; the sign-in service is still tried.
	sup.calls = nil
	if err := l.Start(context.Background()); err == nil {
		t.Fatal("a manager with no registry started the shared services")
	}
	if len(sup.calls) == 0 || sup.calls[len(sup.calls)-1] != "start supavise-gotrue@system.service" {
		t.Fatalf("start calls: %v", sup.calls)
	}
}

// The artifacts check speaks for the node it runs on only.
func TestArtifactsCheckLooksAtTheTargetOnlyWhenItIsThisNode(t *testing.T) {
	mgr, err := fleet.NewManager(fleet.Deps{Cfg: config.Default(), Supervisor: nullSupervisor{}})
	if err != nil {
		t.Fatal(err)
	}
	check := artifactsCheck(mgr, func() string { return "n2" })
	if got := check(context.Background(), registry.Node{ID: "n3"}); got != nil {
		t.Fatalf("a check of another node: %+v", got)
	}
	got := check(context.Background(), registry.Node{ID: "n2", Name: "second"})
	if len(got) != 1 || got[0].Name != "shared-service artifacts" || got[0].OK || !got[0].Blocking {
		t.Fatalf("a manager that cannot render blocks the move: %+v", got)
	}
}
