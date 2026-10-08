package app

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/failover"
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

type fakeCertSource struct {
	snap peerapi.CertSnapshot
	err  error
}

func (f fakeCertSource) Certs(context.Context, string) (peerapi.CertSnapshot, error) {
	return f.snap, f.err
}

// A node that is about to take over compares its mirror with the leader's certificates, for itself only
// and only while it mirrors; a leader that does not answer is the usual failover and passes.
func TestCertificatesCheck(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, data string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("certificates/ca/api.example.test/api.example.test.crt", "one")
	write("certificates/ca/studio.example.test/studio.example.test.crt", "two")
	leader := peerapi.CertSnapshot{Files: []peerapi.CertFile{
		{Path: "certificates/ca/api.example.test/api.example.test.crt", Data: []byte("one")},
		{Path: "certificates/ca/api.example.test/api.example.test.key", Data: []byte("secret")},
		{Path: "certificates/ca/studio.example.test/studio.example.test.crt", Data: []byte("two")},
	}}
	role := proxy.NewCertRole(false)
	self := func() string { return "n2" }
	check := func(src proxy.CertSource, to string) []failover.Check {
		return certificatesCheck(src, dir, role, self)(context.Background(), registry.Node{ID: to, Name: "second"})
	}
	got := check(fakeCertSource{snap: leader}, "n2")
	if len(got) != 1 || !got[0].OK || got[0].Blocking || !strings.Contains(got[0].Detail, "2 certificate(s) match") {
		t.Fatalf("a current mirror: %+v", got)
	}
	// The leader renewed one: the check says which, and still does not stop the move.
	leader.Files[2].Data = []byte("two, renewed")
	got = check(fakeCertSource{snap: leader}, "n2")
	if len(got) != 1 || got[0].OK || got[0].Blocking || !strings.Contains(got[0].Detail, "1 of 2") || !strings.Contains(got[0].Detail, "studio.example.test.crt") {
		t.Fatalf("a stale mirror: %+v", got)
	}
	// A certificate the node does not hold at all differs too.
	leader.Files = append(leader.Files, peerapi.CertFile{Path: "certificates/ca/new.example.test/new.example.test.crt", Data: []byte("three")})
	if got = check(fakeCertSource{snap: leader}, "n2"); !strings.Contains(got[0].Detail, "2 of 3") {
		t.Fatalf("a missing certificate: %+v", got)
	}
	// A leader that is down is the case of a failover.
	got = check(fakeCertSource{err: errors.New("no session")}, "n2")
	if len(got) != 1 || !got[0].OK || !strings.Contains(got[0].Detail, "last mirrored") {
		t.Fatalf("a leader that does not answer: %+v", got)
	}
	// Another node's mirror is its own to check, and a node that issues has nothing mirrored.
	if got = check(fakeCertSource{snap: leader}, "n3"); got != nil {
		t.Fatalf("a check of another node: %+v", got)
	}
	role.Promote()
	if got = check(fakeCertSource{snap: leader}, "n2"); got != nil {
		t.Fatalf("a check of a node that manages its certificates: %+v", got)
	}
}

// The paths of the leader's snapshot are not trusted: a path that leaves the certificate directory is
// counted as a certificate the node does not hold, and nothing outside the directory is read, even
// when a file there has the bytes the snapshot names.
func TestCertificatesCheckReadsNothingOutsideItsDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "store")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside.crt"), []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "abs.crt"), []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := peerapi.CertSnapshot{Files: []peerapi.CertFile{
		{Path: "../outside.crt", Data: []byte("same")},
		{Path: "certificates/../../outside.crt", Data: []byte("same")},
		{Path: filepath.Join(root, "abs.crt"), Data: []byte("same")},
	}}
	got := certificatesCheck(fakeCertSource{snap: snap}, dir, proxy.NewCertRole(false), func() string { return "n2" })(context.Background(), registry.Node{ID: "n2", Name: "second"})
	if len(got) != 1 || got[0].OK || got[0].Blocking || !strings.Contains(got[0].Detail, "3 of 3") {
		t.Fatalf("paths that leave the directory: %+v", got)
	}
}
