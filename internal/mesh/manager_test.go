package mesh

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

const (
	refA = "aaaaaaaaaaaaaaaaaaaa"
	refB = "bbbbbbbbbbbbbbbbbbbb"
	refC = "cccccccccccccccccccc"
)

// pingEndpoint registers the ping on every node so that the peers answer each other.
func pingEndpoint(h *harness) {
	for _, n := range h.nodes {
		n.mgr.o.Mux.Handle("GET "+peerapi.PathPing, PingHandler(func() peerapi.Ping {
			return peerapi.Ping{Node: n.id, Epoch: 1, Leader: "n1", Version: "test", Health: "healthy"}
		}))
	}
}

// Two nodes find each other, either side calls the other's peer API, and the round trip is known.
func TestTwoNodesCallEachOther(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	pingEndpoint(h)
	pings := make(chan string, 16)
	h.nodes["n1"].mgr.o.OnPing = func(node string, p peerapi.Ping, rtt time.Duration) { pings <- node + ">" + p.Node }
	h.start()
	n1, n2 := h.nodes["n1"].mgr, h.nodes["n2"].mgr
	eventually(t, "a session on both sides", func() bool { return n1.Connected("n2") && n2.Connected("n1") })

	for _, c := range []struct {
		from *Manager
		to   string
	}{{n1, "n2"}, {n2, "n1"}} {
		var p peerapi.Ping
		ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
		err := c.from.Call(ctx, c.to, "GET", peerapi.PathPing, nil, &p)
		cancel()
		if err != nil || p.Node != c.to || p.Version != "test" || p.Time.IsZero() {
			t.Fatalf("call %s: %+v, %v", c.to, p, err)
		}
	}
	select {
	case got := <-pings:
		if got != "n2>n2" {
			t.Fatalf("OnPing = %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ping was reported")
	}
	eventually(t, "a round trip", func() bool { _, ok := n1.RTT("n2"); return ok })
	if got := n1.Peers(); len(got) != 1 || got[0] != "n2" {
		t.Fatalf("peers = %v", got)
	}
	if p, at, ok := n1.LastPing("n2"); !ok || p.Node != "n2" || at.IsZero() {
		t.Fatalf("last ping = %+v %v %v", p, at, ok)
	}
}

// A handler's error comes back as a RemoteError with its status and code, a body goes both ways,
// and a handler that registers with RequireLeader answers only the leader.
func TestRPCBodiesErrorsAndLeaderOnly(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	n1, n2 := h.nodes["n1"], h.nodes["n2"]
	n2.mgr.o.Mux.Handle("POST /peer/v1/fence", func(w http.ResponseWriter, r *http.Request) {
		var req peerapi.FenceRequest
		if !DecodeBody(w, r, &req) {
			return
		}
		p, _ := PeerFrom(r.Context())
		RespondJSON(w, http.StatusOK, peerapi.FenceResponse{Epoch: req.Epoch + 1, Fenced: true, Stopped: []string{p.Node}})
	})
	n2.mgr.o.Mux.Handle("PUT /peer/v1/instances/{identifier}", RequireLeader(regTopo{h.reg, "n2"}, func(w http.ResponseWriter, r *http.Request) {
		RespondJSON(w, http.StatusOK, peerapi.InstanceStatus{Identifier: r.PathValue("identifier")})
	}))
	n2.mgr.o.Mux.Handle("POST /peer/v1/report", func(w http.ResponseWriter, r *http.Request) {
		RespondError(w, http.StatusConflict, "stale_epoch", "the node knows a higher epoch")
	})
	n1.mgr.o.Mux.Handle("PUT /peer/v1/instances/{identifier}", RequireLeader(regTopo{h.reg, "n1"}, func(w http.ResponseWriter, r *http.Request) {
		RespondJSON(w, http.StatusOK, peerapi.InstanceStatus{Identifier: "from-n1"})
	}))
	h.start()
	ctx := h.ctx

	var fr peerapi.FenceResponse
	if err := n1.mgr.Call(ctx, "n2", "POST", peerapi.PathFence, peerapi.FenceRequest{Epoch: 4, Leader: "n1"}, &fr); err != nil || fr.Epoch != 5 || !fr.Fenced || fr.Stopped[0] != "n1" {
		t.Fatalf("fence: %+v, %v", fr, err)
	}
	var st peerapi.InstanceStatus
	if err := n1.mgr.Call(ctx, "n2", "PUT", peerapi.InstancePath("x-rr-eu-west-1-abcdef"), peerapi.InstanceSpec{}, &st); err != nil || st.Identifier != "x-rr-eu-west-1-abcdef" {
		t.Fatalf("leader-only call from the leader: %+v, %v", st, err)
	}
	err := n2.mgr.Call(ctx, "n1", "PUT", peerapi.InstancePath("x-rr-eu-west-1-abcdef"), peerapi.InstanceSpec{}, &st)
	var re *RemoteError
	if !errors.As(err, &re) || re.Status != http.StatusForbidden || re.Code != "not_leader" || re.Node != "n1" {
		t.Fatalf("leader-only call from a follower: %v", err)
	}
	err = n1.mgr.Call(ctx, "n2", "POST", peerapi.PathReport, peerapi.Report{}, nil)
	if !errors.As(err, &re) || re.Status != 409 || re.Code != "stale_epoch" || errors.Is(err, ErrRefused) {
		t.Fatalf("handler error: %v", err)
	}
	// A path nobody registered is the Mux's 404 and carries a message.
	if err := n1.mgr.Call(ctx, "n2", "GET", "/peer/v1/nothing", nil, nil); !errors.As(err, &re) || re.Status != 404 {
		t.Fatalf("unknown path: %v", err)
	}
	// A call that ends with its context says so, not "no session".
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := n1.mgr.Call(cctx, "n2", "GET", peerapi.PathPing, nil, nil); err == nil {
		t.Fatal("a call with a cancelled context succeeded")
	}
	if err := n1.mgr.Call(ctx, "n9", "GET", peerapi.PathPing, nil, nil); !errors.Is(err, ErrNoSession) {
		t.Fatalf("call to an unknown node: %v", err)
	}
	if _, err := n1.mgr.Dial(ctx, "n1", Header{T: StreamRPC}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("dial self: %v", err)
	}
}

// Forwarders on both nodes make a project's ports answer on both, and the bytes reach the service
// on the node that runs it, in both directions.
func TestForwardersBridgeBothWays(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	h.project(refA, 1, "n1") // homed on n1
	h.project(refB, 2, "n2") // homed on n2
	n1, n2 := h.nodes["n1"], h.nodes["n2"]
	portA1 := n1.cfg.PortsFor(refA, 1).Postgres // the service of refA, on n1
	portB2 := n2.cfg.PortsFor(refB, 2).Postgres // the service of refB, on n2
	echoServer(t, portA1, "service-A@n1")
	echoServer(t, portB2, "service-B@n2")
	h.start()
	go n1.fwd.Run(h.ctx)
	go n2.fwd.Run(h.ctx)

	fwdA2 := n2.cfg.PortsFor(refA, 1).Postgres // refA's canonical port on n2: a forwarder
	fwdB1 := n1.cfg.PortsFor(refB, 2).Postgres // refB's canonical port on n1: a forwarder
	eventually(t, "A's port on n2 to forward", func() bool { got, err := roundTrip(fwdA2, "hi"); return err == nil && got == "service-A@n1:hi" })
	eventually(t, "B's port on n1 to forward", func() bool { got, err := roundTrip(fwdB1, "yo"); return err == nil && got == "service-B@n2:yo" })

	// Neither node forwards a port it serves.
	if _, ok := n1.fwd.Ports()[portA1]; ok {
		t.Error("n1 forwards the port of a project it hosts")
	}
	if _, ok := n2.fwd.Ports()[portB2]; ok {
		t.Error("n2 forwards the port of a project it hosts")
	}
	// Many connections at once share the session.
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := roundTrip(fwdA2, fmt.Sprint(i)); err != nil || got != fmt.Sprintf("service-A@n1:%d", i) {
				t.Errorf("connection %d: %q, %v", i, got, err)
			}
		}()
	}
	wg.Wait()
}

// A forwarder follows the registry: it appears when a project's home moves away, retargets when
// the home moves to a third node, disappears when the project is deleted or the node is no longer
// active, and a port that something else still holds is bound as soon as it is free.
func TestForwardersFollowTheRegistry(t *testing.T) {
	h := newHarness(t, "n1", "n2", "n3")
	n1 := h.nodes["n1"]
	h.project(refA, 1, "n1")
	h.start()
	go n1.fwd.Run(h.ctx)
	port := n1.cfg.PortsFor(refA, 1).Postgres
	time.Sleep(200 * time.Millisecond)
	if len(n1.fwd.Ports()) != 0 {
		t.Fatalf("forwarders for a project homed here: %v", n1.fwd.Ports())
	}

	// The old home still listens on the port: the forwarder waits for it to go.
	held, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetProjectNode(h.ctx, refA, "n2", 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, ok := n1.fwd.Ports()[port]; ok {
		t.Fatal("bound a port that is in use")
	}
	held.Close()
	want := func(s string) func() bool {
		return func() bool { return n1.fwd.Ports()[port] == s }
	}
	eventually(t, "a forwarder to n2", want(fmt.Sprintf("postgres/%s -> n2", refA)))

	if err := h.reg.SetProjectNode(h.ctx, refA, "n3", 1); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the forwarder to retarget to n3", want(fmt.Sprintf("postgres/%s -> n3", refA)))

	// A node that is not active has no forwarder: connections to it would hang.
	if err := h.reg.SetNodeState(h.ctx, "n3", registry.NodeFenced); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the forwarder to go with its node", func() bool { _, ok := n1.fwd.Ports()[port]; return !ok })
	if err := h.reg.SetNodeState(h.ctx, "n3", registry.NodeActive); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the forwarder to come back", want(fmt.Sprintf("postgres/%s -> n3", refA)))

	// Suspending frees the ports of a project that is about to run here.
	resume := n1.fwd.Suspend(refA)
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("the port is not free after Suspend: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	l.Close()
	if _, ok := n1.fwd.Ports()[port]; ok {
		t.Fatal("the forwarder came back while suspended")
	}
	resume()
	eventually(t, "the forwarder after resume", want(fmt.Sprintf("postgres/%s -> n3", refA)))

	if err := h.reg.SetProjectNode(h.ctx, refA, "n1", 1); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the forwarder to go when the project comes home", func() bool { _, ok := n1.fwd.Ports()[port]; return !ok })
}

// The replica ports are forwarders to the node that holds the replica, except on that node; the
// shared-service ports are forwarders to the leader on every other node; a fenced node has none.
func TestForwardersForReplicasServicesAndFencing(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	h.project(refA, 1, "n1")
	if err := h.reg.CreateReplica(h.ctx, &registry.Replica{Identifier: refA + "-rr-eu-west-1-abc123", Ref: refA, NodeID: "n2"}); err != nil {
		t.Fatal(err)
	}
	n1, n2 := h.nodes["n1"], h.nodes["n2"]
	var fenced bool
	var fencedMu sync.Mutex
	n2.fwd.Fenced = func() bool { fencedMu.Lock(); defer fencedMu.Unlock(); return fenced }
	h.start()
	go n1.fwd.Run(h.ctx)
	go n2.fwd.Run(h.ctx)

	rp1 := n1.cfg.ReplicaPorts(refA, 1)
	rp2 := n2.cfg.ReplicaPorts(refA, 1)
	eventually(t, "n1 to forward the replica ports to n2", func() bool {
		p := n1.fwd.Ports()
		return p[rp1.Postgres] == fmt.Sprintf("replica-postgres/%s -> n2", refA) && p[rp1.PostgREST] == fmt.Sprintf("replica-postgrest/%s -> n2", refA)
	})
	if _, ok := n2.fwd.Ports()[rp2.Postgres]; ok {
		t.Error("n2 forwards the port of the replica it holds")
	}
	// n2 is a follower: its canonical ports for refA and the shared-service ports forward to n1.
	studio := n2.cfg.Ports.Studio
	eventually(t, "n2 to forward the project and the services to n1", func() bool {
		p := n2.fwd.Ports()
		return p[n2.cfg.PortsFor(refA, 1).Postgres] == fmt.Sprintf("postgres/%s -> n1", refA) && p[studio] == "svc:studio/ -> n1"
	})
	if _, ok := n1.fwd.Ports()[n1.cfg.Ports.Studio]; ok {
		t.Error("the leader forwards a shared service")
	}
	fencedMu.Lock()
	fenced = true
	fencedMu.Unlock()
	eventually(t, "a fenced node to forward nothing", func() bool { return len(n2.fwd.Ports()) == 0 })
}

// A node dials the other when it can; when only the higher of the two can dial, it does so after
// the delay and the lower node uses the session it did not open.
func TestSessionWhenOnlyOneSideCanDial(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	pingEndpoint(h)
	h.nodes["n1"].setCanDial(false) // n1 is the lower id and would dial first; it cannot
	h.start()
	n1, n2 := h.nodes["n1"].mgr, h.nodes["n2"].mgr
	eventually(t, "n2 to dial n1 after the delay", func() bool { return n1.Connected("n2") && n2.Connected("n1") })
	pc := n2.sessions["n1"]
	if pc == nil || !pc.dialed {
		t.Fatal("the session was not opened by n2")
	}
	// n1 opens a stream on the session n2 dialed.
	var p peerapi.Ping
	if err := n1.Call(h.ctx, "n2", "GET", peerapi.PathPing, nil, &p); err != nil || p.Node != "n2" {
		t.Fatalf("n1 calling n2 over n2's session: %+v, %v", p, err)
	}
	// And the other way: only n2 can dial, and it can open streams too.
	if err := n2.Call(h.ctx, "n1", "GET", peerapi.PathPing, nil, &p); err != nil || p.Node != "n1" {
		t.Fatalf("n2 calling n1: %+v, %v", p, err)
	}
}

// When both nodes dial at once there is one session, and it is the one the lower node id opened.
func TestSimultaneousDialsKeepTheLowerNodesSession(t *testing.T) {
	for range 5 {
		h := newHarness(t, "n1", "n2")
		pingEndpoint(h)
		go h.nodes["n1"].mgr.Serve(h.ctx, h.nodes["n1"].ln)
		go h.nodes["n2"].mgr.Serve(h.ctx, h.nodes["n2"].ln)
		var wg sync.WaitGroup
		for _, pair := range [][2]string{{"n1", "n2"}, {"n2", "n1"}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := h.nodes[pair[0]].mgr.ensure(h.ctx, pair[1]); err != nil {
					t.Errorf("%s ensure %s: %v", pair[0], pair[1], err)
				}
			}()
		}
		wg.Wait()
		n1, n2 := h.nodes["n1"].mgr, h.nodes["n2"].mgr
		eventually(t, "both sides to hold the session n1 dialed", func() bool {
			n1.mu.Lock()
			a := n1.sessions["n2"]
			n1.mu.Unlock()
			n2.mu.Lock()
			b := n2.sessions["n1"]
			n2.mu.Unlock()
			return a != nil && b != nil && a.dialed && !b.dialed && !a.sess.IsClosed() && !b.sess.IsClosed()
		})
		var p peerapi.Ping
		if err := n2.Call(h.ctx, "n1", "GET", peerapi.PathPing, nil, &p); err != nil || p.Node != "n1" {
			t.Fatalf("call after the tie-break: %+v, %v", p, err)
		}
	}
}

// A node the registry removes loses its sessions after the grace period and cannot connect again;
// a certificate whose serial is not the registry's is refused at the handshake.
func TestRevokedNodeIsCutOff(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	pingEndpoint(h)
	h.start()
	n1, n2 := h.nodes["n1"].mgr, h.nodes["n2"].mgr
	eventually(t, "a session", func() bool { return n1.Connected("n2") && n2.Connected("n1") })

	if err := h.reg.SetNodeState(h.ctx, "n2", registry.NodeLeft); err != nil {
		t.Fatal(err)
	}
	eventually(t, "n1 to close the session of the removed node", func() bool { return !n1.Connected("n2") })
	eventually(t, "n2 to notice", func() bool { return !n2.Connected("n1") })
	ctx, cancel := context.WithTimeout(h.ctx, 3*time.Second)
	defer cancel()
	if err := n2.Call(ctx, "n1", "GET", peerapi.PathPing, nil, nil); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a removed node reached the leader: %v", err)
	}

	// Back to active with another certificate than the registry's: refused.
	if err := h.reg.SetNodeState(h.ctx, "n2", registry.NodeActive); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetNodeCert(h.ctx, "n2", "feedface"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := n2.Call(ctx, "n1", "GET", peerapi.PathPing, nil, nil); err == nil {
		t.Fatal("a certificate that is not the registry's was accepted")
	}
	// With the recorded serial it works again.
	if err := h.reg.SetNodeCert(h.ctx, "n2", h.nodes["n2"].creds.Serial); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the node to be admitted again", func() bool {
		c, cancel := context.WithTimeout(h.ctx, 2*time.Second)
		defer cancel()
		return n2.Call(c, "n1", "GET", peerapi.PathPing, nil, nil) == nil
	})
}

// A client that is not in the cluster: with the wrong CA it fails the handshake; with no
// certificate it reaches the join endpoint and nothing else.
func TestStrangersAndTheJoinEndpoint(t *testing.T) {
	h := newHarness(t, "n1")
	n1 := h.nodes["n1"]
	n1.mgr.o.Mux.Handle("GET "+peerapi.PathJoin, func(w http.ResponseWriter, r *http.Request) {
		p, _ := PeerFrom(r.Context())
		RespondJSON(w, 200, peerapi.JoinChallenge{Nonce: []byte(fmt.Sprintf("anonymous=%v", p.Node == ""))})
	})
	n1.mgr.o.Mux.Handle("GET "+peerapi.PathPing, PingHandler(func() peerapi.Ping { return peerapi.Ping{Node: "n1"} }))
	h.start()
	addr := n1.ln.Addr().String()

	// Another cluster's certificate is refused.
	other := newTestCA(t).issue(t, "n2", time.Now().Add(time.Hour))
	cfg := clientTLS(func() *Credentials { return other }, "n1", nil, time.Now)
	cfg.VerifyConnection = nil // the stranger trusts the server; it is the server that must refuse
	if conn, err := tls.Dial("tcp", addr, cfg); err == nil {
		// TLS 1.3 reports a rejected client certificate on the first read.
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, rerr := conn.Read(make([]byte, 1))
		conn.Close()
		if rerr == nil {
			t.Fatal("a certificate of another CA was accepted")
		}
	}

	// A joiner pins the CA by fingerprint, presents no certificate, and reaches the join endpoint.
	pin := Fingerprint(h.ca.cert.Raw)
	st := dialAnonymous(t, addr, PinnedTLS(pin, nil, time.Now))
	var ch peerapi.JoinChallenge
	if err := call(h.ctx, st, "n1", "GET", peerapi.PathJoin, nil, &ch); err != nil || string(ch.Nonce) != "anonymous=true" {
		t.Fatalf("join challenge: %q, %v", ch.Nonce, err)
	}
	st = dialAnonymous(t, addr, PinnedTLS(pin, nil, time.Now))
	var re *RemoteError
	if err := call(h.ctx, st, "n1", "GET", peerapi.PathPing, nil, nil); !errors.As(err, &re) || re.Status != 403 || !errors.Is(err, ErrRefused) {
		t.Fatalf("an anonymous ping: %v", err)
	}
	// A forward stream from a caller with no certificate is closed without a byte.
	fst := dialAnonymous(t, addr, PinnedTLS(pin, nil, time.Now), Header{T: StreamForward, Kind: KindPostgres, Ref: "system"})
	_ = fst.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, err := fst.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("an anonymous forward stream got %d bytes, %v", n, err)
	}
	// A client that does not speak the mesh protocol is turned away by the server, which checks the
	// ALPN itself (TLS 1.3 reports it on the first read).
	for _, protos := range [][]string{nil, {"h2"}} {
		conn, err := tls.Dial("tcp", addr, &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: protos})
		if err != nil {
			continue
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Fatalf("a client with ALPN %v was served", protos)
		}
		conn.Close()
	}
	// A pin of another CA fails the handshake.
	if conn, err := tls.Dial("tcp", addr, PinnedTLS(Fingerprint(newTestCA(t).cert.Raw), nil, time.Now)); err == nil {
		conn.Close()
		t.Fatal("a wrong pin was accepted")
	} else if !strings.Contains(err.Error(), "pins") {
		t.Fatalf("wrong pin: %v", err)
	}
}

// dialAnonymous opens a session to addr with cfg and returns an rpc stream (or, with a header, a
// stream that starts with it).
func dialAnonymous(t *testing.T, addr string, cfg *tls.Config, header ...Header) net.Conn {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	sess, err := NewSession(conn, true)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	h := Header{T: StreamRPC}
	if len(header) > 0 {
		h = header[0]
	}
	if err := WriteHeader(st, h); err != nil {
		t.Fatal(err)
	}
	return st
}

// A session that dies is replaced: the next connection through a forwarder opens another, and the
// upkeep brings the session back for the pings.
func TestForwardingSurvivesASessionLoss(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	h.project(refA, 1, "n1")
	n1, n2 := h.nodes["n1"], h.nodes["n2"]
	echoServer(t, n1.cfg.PortsFor(refA, 1).Postgres, "A@n1")
	h.start()
	go n2.fwd.Run(h.ctx)
	port := n2.cfg.PortsFor(refA, 1).Postgres
	eventually(t, "forwarding", func() bool { got, err := roundTrip(port, "1"); return err == nil && got == "A@n1:1" })

	for range 3 {
		n2.mgr.mu.Lock()
		pc := n2.mgr.sessions["n1"]
		n2.mgr.mu.Unlock()
		if pc == nil {
			t.Fatal("no session to close")
		}
		_ = pc.sess.Close()
		eventually(t, "forwarding after the session was closed", func() bool {
			got, err := roundTrip(port, "2")
			return err == nil && got == "A@n1:2"
		})
		eventually(t, "both sides to hold a session again", func() bool { return n1.mgr.Connected("n2") && n2.mgr.Connected("n1") })
	}
}

// Close ends the peer API server at once, without waiting for the context that Run or Serve got.
func TestManagerCloseDoesNotWaitForTheContext(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	h.start()
	n1 := h.nodes["n1"].mgr
	eventually(t, "a session", func() bool { return n1.Connected("n2") })
	done := make(chan struct{})
	go func() { n1.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waits for the context")
	}
	if n1.Connected("n2") {
		t.Fatal("a session survived Close")
	}
}

// anonClient is a joiner's client: it pins the cluster CA and presents no certificate.
func anonClient(t *testing.T, h *harness, n *tnode) *Client {
	t.Helper()
	c, err := DialClient(h.ctx, n.ln.Addr().String(), "n1", PinnedTLS(Fingerprint(h.ca.cert.Raw), nil, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// A caller with no certificate costs the daemon little: its request is bounded to a few KiB and has
// to arrive in time, its session is small, there are few of them, and an idle one is closed.
func TestAnonymousCallersAreCutOff(t *testing.T) {
	h := newHarness(t, "n1")
	n1 := h.nodes["n1"]
	n1.mgr.o.AnonReadTimeout = 300 * time.Millisecond
	n1.mgr.o.AnonLife = 400 * time.Millisecond
	n1.mgr.o.MaxAnonSessions = 3
	var got atomic.Int64
	n1.mgr.o.Mux.Handle("POST "+peerapi.PathJoin, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		if !DecodeBody(w, r, &req) {
			return
		}
		got.Add(int64(len(req["csr"])))
		RespondJSON(w, http.StatusOK, map[string]int{"n": len(req["csr"])})
	})
	h.start()
	addr := n1.ln.Addr().String()
	pin := Fingerprint(h.ca.cert.Raw)
	var re *RemoteError

	// A request of ordinary size goes through; one of 100 KiB is refused before the handler runs.
	c := anonClient(t, h, n1)
	var out map[string]int
	if err := c.Call(h.ctx, "POST", peerapi.PathJoin, map[string]string{"csr": strings.Repeat("a", 4<<10)}, &out); err != nil || out["n"] != 4<<10 {
		t.Fatalf("a request of 4 KiB: %v, %v", out, err)
	}
	err := c.Call(h.ctx, "POST", peerapi.PathJoin, map[string]string{"csr": strings.Repeat("a", 100<<10)}, nil)
	if !errors.As(err, &re) || re.Status != http.StatusRequestEntityTooLarge || re.Code != "too_large" {
		t.Fatalf("a request of 100 KiB: %v", err)
	}
	if got.Load() != 4<<10 {
		t.Fatalf("the handler saw %d bytes", got.Load())
	}

	// A request that announces a body and does not send it is answered 408 when the time is up.
	st := dialAnonymous(t, addr, PinnedTLS(pin, nil, time.Now))
	_ = st.SetDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if _, err := fmt.Fprintf(st, "POST %s HTTP/1.1\r\nHost: n1\r\nContent-Length: 100\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n{", peerapi.PathJoin); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(st), nil)
	if err != nil || resp.StatusCode != http.StatusRequestTimeout || time.Since(start) > 3*time.Second {
		t.Fatalf("a body that never comes: %v, %v after %s", resp, err, time.Since(start))
	}

	// Streams: sixteen are served at a time and the rest are closed unread.
	conn, err := tls.Dial("tcp", addr, PinnedTLS(pin, nil, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sess, err := NewSession(conn, true)
	if err != nil {
		t.Fatal(err)
	}
	var streams []net.Conn
	for range maxAnonStreams + 4 {
		s, err := sess.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteHeader(s, Header{T: StreamRPC}); err != nil {
			t.Fatal(err)
		}
		streams = append(streams, s)
	}
	open, closed := 0, 0
	for _, s := range streams {
		_ = s.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		var ne net.Error
		if _, err := s.Read(make([]byte, 1)); errors.As(err, &ne) && ne.Timeout() {
			open++
		} else {
			closed++
		}
	}
	if open != maxAnonStreams || closed != 4 {
		t.Fatalf("%d streams were kept and %d closed, want %d and 4", open, closed, maxAnonStreams)
	}

	// An idle session is closed once it is AnonLife old.
	idleConn, err := tls.Dial("tcp", addr, PinnedTLS(pin, nil, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	defer idleConn.Close()
	idle, err := NewSession(idleConn, true)
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { _, err := idle.AcceptStream(); ended <- err }() // returns when the far end closes the connection
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("an idle session without a certificate was not closed")
	}
}

// Only so many callers with no certificate hold a session at once; a slot frees when one leaves.
func TestAnonymousSessionsAreFew(t *testing.T) {
	h := newHarness(t, "n1")
	n1 := h.nodes["n1"]
	n1.mgr.o.MaxAnonSessions = 2
	n1.mgr.o.Mux.Handle("GET "+peerapi.PathJoin, func(w http.ResponseWriter, r *http.Request) { RespondJSON(w, 200, peerapi.JoinChallenge{}) })
	h.start()
	a, b := anonClient(t, h, n1), anonClient(t, h, n1)
	for _, c := range []*Client{a, b} {
		if err := c.Call(h.ctx, "GET", peerapi.PathJoin, nil, nil); err != nil {
			t.Fatalf("a session within the limit: %v", err)
		}
	}
	// The server closes the connection after the handshake, which the client sees when it first uses it.
	conn, err := tls.Dial("tcp", n1.ln.Addr().String(), PinnedTLS(Fingerprint(h.ca.cert.Raw), nil, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if sess, err := NewSession(conn, true); err == nil {
		st, err := sess.OpenStream()
		if err == nil {
			if err = WriteHeader(st, Header{T: StreamRPC}); err == nil {
				err = call(h.ctx, st, "n1", "GET", peerapi.PathJoin, nil, nil)
			}
		}
		if err == nil {
			t.Fatal("a third session without a certificate was served")
		}
	}
	a.Close()
	eventually(t, "a slot to be free again", func() bool {
		c, err := DialClient(h.ctx, n1.ln.Addr().String(), "n1", PinnedTLS(Fingerprint(h.ca.cert.Raw), nil, time.Now))
		if err != nil {
			return false
		}
		defer c.Close()
		return c.Call(h.ctx, "GET", peerapi.PathJoin, nil, nil) == nil
	})
}

// failingListener fails the first n accepts with err.
type failingListener struct {
	net.Listener
	n   atomic.Int32
	err error
}

func (l *failingListener) Accept() (net.Conn, error) {
	if l.n.Add(-1) >= 0 {
		return nil, l.err
	}
	return l.Listener.Accept()
}

// Accept errors about resources do not stop the peer server; the others end it with the error.
func TestServeOutlivesResourceErrorsOnAccept(t *testing.T) {
	h := newHarness(t, "n1")
	n1 := h.nodes["n1"]
	n1.mgr.o.Mux.Handle("GET "+peerapi.PathJoin, func(w http.ResponseWriter, r *http.Request) { RespondJSON(w, 200, peerapi.JoinChallenge{}) })
	fl := &failingListener{Listener: n1.ln, err: &net.OpError{Op: "accept", Err: os.NewSyscallError("accept", syscall.EMFILE)}}
	fl.n.Store(3)
	done := make(chan error, 1)
	go func() { done <- n1.mgr.Serve(h.ctx, fl) }()
	c := anonClient(t, h, n1)
	if err := c.Call(h.ctx, "GET", peerapi.PathJoin, nil, nil); err != nil {
		t.Fatalf("a call after three failed accepts: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("Serve ended: %v", err)
	default:
	}

	h2 := newHarness(t, "n1")
	boom := errors.New("the listener is broken")
	fatal := &failingListener{Listener: h2.nodes["n1"].ln, err: boom}
	fatal.n.Store(1)
	if err := h2.nodes["n1"].mgr.Serve(h2.ctx, fatal); !errors.Is(err, boom) {
		t.Fatalf("Serve after a fatal accept error: %v", err)
	}
}

// deadAccept is a listener that cannot accept.
type deadAccept struct{ net.Listener }

func (deadAccept) Accept() (net.Conn, error) { return nil, errors.New("the listener is broken") }

// A forwarder whose listener stops accepting is forgotten, and the next look binds the port again.
func TestForwarderRebindsAListenerThatStoppedAccepting(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	h.project(refA, 1, "n1")
	n2 := h.nodes["n2"]
	var mu sync.Mutex
	binds := 0
	n2.fwd.Listen = func(port int) (net.Listener, error) {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		if binds++; binds == 1 {
			return deadAccept{ln}, nil
		}
		return ln, nil
	}
	// A follower forwards the project's three ports and the leader's seven shared services.
	n2.fwd.Reconcile(h.ctx)
	mu.Lock()
	total := binds
	mu.Unlock()
	if total != 10 {
		t.Fatalf("%d forwarders bound", total)
	}
	eventually(t, "the broken listener to be dropped", func() bool { return len(n2.fwd.Ports()) == total-1 })
	n2.fwd.Reconcile(h.ctx)
	if got := len(n2.fwd.Ports()); got != total {
		t.Fatalf("%d forwarders after the next look, want %d", got, total)
	}
	mu.Lock()
	defer mu.Unlock()
	if binds != total+1 {
		t.Fatalf("%d binds, want %d", binds, total+1)
	}
}

// A Client whose session ended opens another to the same address; a closed Client does not.
func TestClientReconnects(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	pingEndpoint(h)
	n1, n2 := h.nodes["n1"], h.nodes["n2"]
	// Only n1 runs: a client with n2's certificate beside n2's own manager would lose the tie-break
	// between two sessions of the pair.
	go n1.mgr.Serve(h.ctx, n1.ln)
	go n1.mgr.Run(h.ctx)
	c, err := DialClient(h.ctx, n1.ln.Addr().String(), "n1", ClientTLS(func() *Credentials { return n2.creds }, "n1", nil, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	var p peerapi.Ping
	if err := c.Call(h.ctx, "GET", peerapi.PathPing, nil, &p); err != nil || p.Node != "n1" {
		t.Fatalf("first call: %+v, %v", p, err)
	}
	first := c.sess
	_ = first.Close()
	if err := c.Call(h.ctx, "GET", peerapi.PathPing, nil, &p); err != nil {
		t.Fatalf("the call after the session ended: %v", err)
	}
	if c.sess == first || c.sess.IsClosed() {
		t.Fatal("the client kept the dead session")
	}
	// A forward stream takes the same way.
	_ = c.sess.Close()
	st, err := c.Dial(h.ctx, "n1", Header{T: StreamForward, Kind: KindPostgres, Ref: "system"})
	if err != nil {
		t.Fatalf("a forward stream after the session ended: %v", err)
	}
	st.Close()
	c.Close()
	if err := c.Call(h.ctx, "GET", peerapi.PathPing, nil, &p); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a call on a closed client: %v", err)
	}
}
