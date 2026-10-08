package proxy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// meshNode is one end of a real mesh: a mesh.Manager on a loopback port with a certificate signed by a
// test CA, the way the daemon starts it (internal/app/wire_mesh.go), without the cluster code around it.
type meshNode struct {
	id  string
	mgr *mesh.Manager
	mux *mesh.Mux
}

// meshTopo is the mesh's view of the cluster, read from the registry the two nodes share.
type meshTopo struct {
	reg  registry.Registry
	self string
}

func (m meshTopo) Self() registry.Node {
	n, err := m.reg.GetNode(context.Background(), m.self)
	if err != nil {
		return registry.Node{ID: m.self}
	}
	return *n
}

func (m meshTopo) Nodes() []registry.Node {
	ns, _ := m.reg.ListNodes(context.Background())
	return ns
}

func (m meshTopo) Leader() (registry.Node, bool) {
	c, err := m.reg.GetCluster(context.Background())
	if err != nil {
		return registry.Node{}, false
	}
	n, err := m.reg.GetNode(context.Background(), c.Leader)
	if err != nil {
		return registry.Node{}, false
	}
	return *n, true
}

func (m meshTopo) IsLeader() bool {
	l, ok := m.Leader()
	return ok && l.ID == m.self
}

// realMesh starts the nodes n1 (the leader) and n2 over one registry and returns them once n2 can reach n1.
func realMesh(t *testing.T) (leader, follower meshNode, reg registry.Registry) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg = registry.NewMemory()

	caPub, caKey, _ := ed25519.GenerateKey(rand.Reader)
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, caPub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)

	type spec struct {
		id    string
		creds *mesh.Credentials
		ln    net.Listener
	}
	specs := map[string]*spec{}
	for i, id := range []string{registry.FounderNodeID, "n2"} {
		pub, key, _ := ed25519.GenerateKey(rand.Reader)
		uri, _ := url.Parse(mesh.NodeURI(id))
		tpl := &x509.Certificate{
			SerialNumber: big.NewInt(int64(100 + i)), Subject: pkix.Name{CommonName: id}, URIs: []*url.URL{uri},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, ca, pub, caKey)
		if err != nil {
			t.Fatal(err)
		}
		creds, err := mesh.NewCredentials(der, key, ca)
		if err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		specs[id] = &spec{id: id, creds: creds, ln: ln}
		node := registry.Node{ID: id, Name: "node-" + id, State: registry.NodeActive, PeerAddr: ln.Addr().String(), CertSerial: creds.Serial}
		if id == registry.FounderNodeID {
			if err := reg.UpdateNode(ctx, &registry.Node{ID: id, Name: "primary", PeerAddr: node.PeerAddr}); err != nil {
				t.Fatal(err)
			}
			if err := reg.SetNodeCert(ctx, id, creds.Serial); err != nil {
				t.Fatal(err)
			}
		} else if err := reg.CreateNode(ctx, &node); err != nil {
			t.Fatal(err)
		}
	}
	nodes := map[string]meshNode{}
	for id, sp := range specs {
		mux := mesh.NewMux()
		mgr := mesh.New(mesh.Options{
			Topology: meshTopo{reg, id}, Creds: func() *mesh.Credentials { return sp.creds }, Mux: mux, Log: quietLog(),
			PingEvery: 100 * time.Millisecond, Tick: 50 * time.Millisecond, DialDelay: 150 * time.Millisecond, RevokeGrace: 200 * time.Millisecond,
		})
		go func() { _ = mgr.Serve(ctx, sp.ln) }()
		go func() { _ = mgr.Run(ctx) }()
		nodes[id] = meshNode{id: id, mgr: mgr, mux: mux}
	}
	return nodes[registry.FounderNodeID], nodes["n2"], reg
}

// TestMeshCertsAgainstTheRealMesh runs the follower's mirror over mesh.Manager.Call and the leader's
// CertsHandler on the peer server of a real mesh: the query string reaches the handler, a 304 comes back as
// *mesh.RemoteError{Status: 304}, the caller is the node its certificate names, and a node that is not the
// leader answers 409. (TestMeshCerts does the same over an in-process mux.)
func TestMeshCertsAgainstTheRealMesh(t *testing.T) {
	ctx := context.Background()
	leader, follower, reg := realMesh(t)

	store := t.TempDir()
	issuer := "acme.test-dir"
	crt, key := testCert(t, "api.example.com")
	writeSite(t, store, issuer, "api.example.com", crt, key)
	writeFile(t, filepath.Join(store, "acme", issuer, "users", "default", "default.key"), "account")
	writeFile(t, filepath.Join(store, "locks", "x.lock"), "lock")
	var leads atomic.Bool
	leads.Store(true)
	leader.mux.Handle("GET "+peerapi.PathCerts, CertsHandler(store, leads.Load, quietLog()))

	view := meshTopo{reg, "n2"}
	src := MeshCerts{RPC: follower.mgr, Leader: func() (string, bool) { n, ok := view.Leader(); return n.ID, ok }}
	local := t.TempDir()
	m := newCertMirror(local, &CertSync{Source: src, Interval: time.Hour}, quietLog())
	told := 0
	m.onSnapshot = func(context.Context, peerapi.CertSnapshot) { told++ }

	// The first fetch may have to wait for the session to come up.
	var err error
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if err = m.sync(ctx); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("the first fetch over the mesh: %v", err)
	}
	got, want := tree(t, local), tree(t, store)
	delete(want, "locks/x.lock")
	if len(got) != len(want) {
		t.Errorf("the follower holds %d files, the leader offers %d", len(got), len(want))
	}
	for p, c := range want {
		if got[p] != c {
			t.Errorf("%s differs after the first fetch", p)
		}
	}

	// The tag travels as a query parameter and the answer 304 comes back as ErrCertsNotModified.
	if _, err := src.Certs(ctx, m.tag); !errors.Is(err, ErrCertsNotModified) {
		t.Errorf("the current tag over the mesh: %v, want ErrCertsNotModified", err)
	}
	if err := m.sync(ctx); err != nil {
		t.Errorf("a fetch the leader answers with 304: %v", err)
	}
	if _, err := src.Certs(ctx, "stale"); err != nil {
		t.Errorf("a stale tag over the mesh: %v", err)
	}
	// A renewal reaches the follower.
	crtN, keyN := testCert(t, "api.example.com")
	writeSite(t, store, issuer, "api.example.com", crtN, keyN)
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if tree(t, local)["certificates/"+issuer+"/api.example.com/api.example.com.crt"] != string(crtN) {
		t.Error("a renewal did not reach the follower")
	}
	if told != 3 {
		t.Errorf("onSnapshot was told %d times in three fetches", told)
	}

	// A node that is not the leader says so.
	leads.Store(false)
	var re *mesh.RemoteError
	if _, err := src.Certs(ctx, ""); !errors.As(err, &re) || re.Status != http.StatusConflict || re.Code != "not_leader" {
		t.Errorf("a node that is not the leader: %v", err)
	}
}
