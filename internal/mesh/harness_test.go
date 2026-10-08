package mesh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	mrand "math/rand/v2"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// testCA is a certificate authority for tests. The real one is derived from the master key
// (internal/cluster); the mesh only needs a CA certificate and leaves signed by it.
type testCA struct {
	key  ed25519.PrivateKey
	cert *x509.Certificate
}

func newTestCA(t testing.TB) *testCA {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{key: key, cert: cert}
}

// issue signs a leaf for node id; the serial is random.
func (ca *testCA) issue(t testing.TB, id string, notAfter time.Time) *Credentials {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	serial.Add(serial, big.NewInt(2))
	uri, _ := url.Parse(NodeURI(id))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: id}, URIs: []*url.URL{uri},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCredentials(der, key, ca.cert)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// regTopo is a Topology over a registry, for the nodes of a test: the registry is the one
// source of truth that every node reads.
type regTopo struct {
	reg  registry.Registry
	self string
}

func (r regTopo) Self() registry.Node {
	n, err := r.reg.GetNode(context.Background(), r.self)
	if err != nil {
		return registry.Node{ID: r.self}
	}
	return *n
}
func (r regTopo) Nodes() []registry.Node {
	ns, _ := r.reg.ListNodes(context.Background())
	return ns
}
func (r regTopo) Leader() (registry.Node, bool) {
	c, err := r.reg.GetCluster(context.Background())
	if err != nil {
		return registry.Node{}, false
	}
	n, err := r.reg.GetNode(context.Background(), c.Leader)
	if err != nil {
		return registry.Node{}, false
	}
	return *n, true
}
func (r regTopo) IsLeader() bool {
	l, ok := r.Leader()
	return ok && l.ID == r.self
}

// tnode is one node of a test cluster: a manager listening on loopback, with its own ports.
type tnode struct {
	id    string
	cfg   *config.Config
	mgr   *Manager
	ln    net.Listener
	creds *Credentials
	fwd   *Forwarders
	// canDial is consulted by the node's TCPDial: false makes every outbound connection fail.
	canDialMu sync.Mutex
	canDial   bool
}

func (n *tnode) setCanDial(v bool) { n.canDialMu.Lock(); n.canDial = v; n.canDialMu.Unlock() }

type harness struct {
	t     *testing.T
	ctx   context.Context
	ca    *testCA
	reg   *registry.Memory
	nodes map[string]*tnode
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// freePort finds a loopback port nothing listens on.
func freePort(t testing.TB) int { return freePorts(t, 1)[0] }

// freeWindow finds size consecutive loopback ports nothing listens on, with the lowest between lo and hi.
func freeWindow(t testing.TB, lo, hi, size int) int {
	t.Helper()
	for range 200 {
		base := lo + mrand.IntN(hi-lo)
		var lns []net.Listener
		ok := true
		for p := base; p < base+size; p++ {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
			if err != nil {
				ok = false
				break
			}
			lns = append(lns, ln)
		}
		for _, ln := range lns {
			ln.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatalf("no window of %d free ports between %d and %d", size, lo, hi)
	return 0
}

// freePorts finds n different loopback ports nothing listens on: it holds them all until it has
// them all, because asking for one at a time can return the same port twice.
func freePorts(t testing.TB, n int) []int {
	t.Helper()
	var lns []net.Listener
	var ports []int
	for range n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		lns = append(lns, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range lns {
		ln.Close()
	}
	return ports
}

// newHarness starts the named nodes (the first is the founder and the leader) sharing one registry,
// each with a manager on a loopback port and its own port plan, so that a forwarder of one node
// does not collide with the service of another in the same process.
func newHarness(t *testing.T, ids ...string) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx, ca: newTestCA(t), reg: registry.NewMemory(), nodes: map[string]*tnode{}}
	for _, id := range ids {
		n := &tnode{id: id, canDial: true, cfg: config.Default()}
		// Project 1's Postgres port is ProjectBase+3, project 2's +6. The shared services get ports of
		// their own too: the defaults (3000, 8080, ...) may be taken on the machine that runs the test.
		// The replica range must end below the project range, so each gets a window of its own.
		n.cfg.Ports.ReplicaBase = freeWindow(t, 20000, 25000, 8) - 3
		n.cfg.Ports.ProjectBase = freeWindow(t, 26000, 31000, 8) - 3
		ps := freePorts(t, 7)
		for i, p := range []*int{&n.cfg.Ports.Studio, &n.cfg.Ports.PGMeta, &n.cfg.Ports.Realtime, &n.cfg.Ports.Storage, &n.cfg.Ports.Imgproxy, &n.cfg.Ports.EdgeRuntime} {
			*p = ps[i]
		}
		n.cfg.Listen.Admin = fmt.Sprintf("127.0.0.1:%d", ps[6])
		n.creds = h.ca.issue(t, id, time.Now().Add(time.Hour))
		var err error
		if n.ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		node := registry.Node{ID: id, Name: "node-" + id, State: registry.NodeActive, PeerAddr: n.ln.Addr().String(), CertSerial: n.creds.Serial}
		if id == registry.FounderNodeID {
			if err := h.reg.UpdateNode(ctx, &registry.Node{ID: id, Name: "primary", PeerAddr: node.PeerAddr}); err != nil {
				t.Fatal(err)
			}
			if err := h.reg.SetNodeCert(ctx, id, node.CertSerial); err != nil {
				t.Fatal(err)
			}
		} else if err := h.reg.CreateNode(ctx, &node); err != nil {
			t.Fatal(err)
		}
		h.nodes[id] = n
	}
	for _, n := range h.nodes {
		n.mgr = New(Options{
			Topology: regTopo{h.reg, n.id},
			Creds:    func() *Credentials { return n.creds },
			Mux:      NewMux(),
			Authz:    &Authorizer{Cfg: n.cfg, Topology: regTopo{h.reg, n.id}, Source: h.reg},
			Log:      quietLog(),
			TCPDial: func(ctx context.Context, addr string) (net.Conn, error) {
				n.canDialMu.Lock()
				ok := n.canDial
				n.canDialMu.Unlock()
				if !ok {
					return nil, fmt.Errorf("test: node %s cannot dial", n.id)
				}
				var d net.Dialer
				return d.DialContext(ctx, "tcp", addr)
			},
			PingEvery: 100 * time.Millisecond, Tick: 50 * time.Millisecond, DialDelay: 150 * time.Millisecond, RevokeGrace: 200 * time.Millisecond,
		})
		n.fwd = &Forwarders{Cfg: n.cfg, Topology: regTopo{h.reg, n.id}, Source: h.reg, Dialer: n.mgr, Log: quietLog(), Interval: 50 * time.Millisecond}
	}
	return h
}

// start runs every node's manager and listener.
func (h *harness) start() {
	for _, n := range h.nodes {
		go n.mgr.Serve(h.ctx, n.ln)
		go n.mgr.Run(h.ctx)
	}
}

// project registers a project homed on node with the given ref and sequence.
func (h *harness) project(ref string, seq int, home string) {
	h.t.Helper()
	if err := h.reg.CreateProject(h.ctx, &registry.Project{Ref: ref, Name: ref[:4], Seq: seq, NodeID: home}); err != nil {
		h.t.Fatal(err)
	}
}

// eventually polls cond until it holds.
func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// echoServer listens on 127.0.0.1:port and answers every connection with prefix and its input.
func echoServer(t testing.TB, port int, prefix string) {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 64)
				n, _ := c.Read(buf)
				_, _ = c.Write(append([]byte(prefix+":"), buf[:n]...))
			}()
		}
	}()
}

// roundTrip connects to port, sends msg and returns what comes back.
func roundTrip(port int, msg string) (string, error) {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		return "", err
	}
	out, err := io.ReadAll(c)
	return string(out), err
}
