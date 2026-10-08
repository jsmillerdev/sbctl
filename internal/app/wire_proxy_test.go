package app

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/proxy"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
)

// rttMesh is a mesh.Mesh that knows two round-trip times.
type rttMesh struct{}

func (rttMesh) Dial(context.Context, string, mesh.Header) (net.Conn, error) {
	return nil, mesh.ErrNoSession
}
func (rttMesh) Call(context.Context, string, string, string, any, any) error {
	return mesh.ErrNoSession
}
func (rttMesh) Connected(string) bool { return true }
func (rttMesh) Peers() []string       { return []string{"n2"} }
func (rttMesh) RTT(node string) (time.Duration, bool) {
	if node == "n2" {
		return 7 * time.Millisecond, true
	}
	return 0, false
}

type fakeReplicas struct{ replicas.Service }

// snapshotOf is the cluster of n1 and n2 as n2 sees it, or as n1 does when it leads.
func snapshotOf(role cluster.Role) cluster.Snapshot {
	n1, n2 := registry.Node{ID: "n1", Name: "primary", State: registry.NodeActive}, registry.Node{ID: "n2", Name: "eu-1", State: registry.NodeActive}
	self := n2
	if role == cluster.RoleLeader {
		self = n1
	}
	return cluster.Snapshot{Self: self, Nodes: []registry.Node{n1, n2}, Leader: "n1", Epoch: 3, Role: role}
}

func proxyClusterOf(role cluster.Role) *cluster.Static { return cluster.NewStatic(snapshotOf(role)) }

// A node with no cluster has nothing to wire: the proxy keeps the options it was given.
func TestWireProxyDoesNothingWithoutACluster(t *testing.T) {
	w := testWire(t)
	if err := wireProxy(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if w.Proxy.Cluster != nil || len(w.runners) != 0 {
		t.Errorf("a node without a cluster got %+v and %d runners", w.Proxy.Cluster, len(w.runners))
	}
	if _, ok := Get[*proxy.CertRole](w); ok {
		t.Error("a certificate role without a cluster")
	}
}

func TestWireProxyGivesTheProxyItsClusterAndFollowsTheRole(t *testing.T) {
	old := mesh.DefaultMux
	mesh.DefaultMux = mesh.NewMux()
	defer func() { mesh.DefaultMux = old }()

	w := testWire(t)
	ms := proxyClusterOf(cluster.RoleFollower)
	Provide[mesh.Mesh](w, rttMesh{})
	Provide[cluster.Membership](w, ms)
	Provide[replicas.Service](w, fakeReplicas{})
	if err := wireProxy(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	// The leader's certificate store is served to the nodes of the cluster.
	patterns := mesh.DefaultMux.Patterns()
	if len(patterns) != 1 || patterns[0] != "GET "+peerapi.PathCerts {
		t.Errorf("peer endpoints registered: %v", patterns)
	}

	c := w.Proxy.Cluster
	if c == nil || c.Certs == nil || c.Certs.Role == nil || c.Certs.Source == nil || c.Lag == nil {
		t.Fatalf("cluster options %+v", c)
	}
	if c.Self() != "n2" {
		t.Errorf("Self = %q", c.Self())
	}
	if d, ok := c.RTT("n2"); !ok || d != 7*time.Millisecond {
		t.Errorf("RTT = %v %v", d, ok)
	}
	role, ok := Get[*proxy.CertRole](w)
	if !ok || role != c.Certs.Role {
		t.Fatal("the certificate role is not provided")
	}
	if role.Managing() {
		t.Error("a follower manages certificates")
	}

	// The role follows the membership: the follower is promoted, then fenced.
	g, ctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(ctx)
	w.start(g, ctx)
	eventually := func(what string, want bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if role.Managing() == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	ms.Set(snapshotOf(cluster.RoleLeader))
	eventually("the promotion", true)
	ms.Set(cluster.Snapshot{Self: registry.Node{ID: "n1"}, Leader: "n2", Epoch: 4, Role: cluster.RoleFenced})
	eventually("the fence", false)
	cancel()
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
}

// A leader starts as the one that manages.
func TestWireProxyLeaderManages(t *testing.T) {
	old := mesh.DefaultMux
	mesh.DefaultMux = mesh.NewMux()
	defer func() { mesh.DefaultMux = old }()
	w := testWire(t)
	Provide[mesh.Mesh](w, rttMesh{})
	Provide[cluster.Membership](w, proxyClusterOf(cluster.RoleLeader))
	if err := wireProxy(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	role, _ := Get[*proxy.CertRole](w)
	if role == nil || !role.Managing() {
		t.Error("the leader does not manage")
	}
	if w.Proxy.Cluster.Lag != nil {
		t.Error("a lag source without a replica controller")
	}
}
