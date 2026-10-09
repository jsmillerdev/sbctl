package mesh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// authzFixture is a cluster of four nodes: n1 leads, n2 is active, n3 is joining and n4 is fenced.
// Project A is homed on n1 and has a replica on n2; project B is homed on n2; system is on n1.
func authzFixture(t *testing.T) (*registry.Memory, *config.Config) {
	t.Helper()
	ctx := context.Background()
	reg := registry.NewMemory()
	for _, n := range []registry.Node{
		{ID: "n2", Name: "two", State: registry.NodeActive},
		{ID: "n3", Name: "three", State: registry.NodeJoining},
		{ID: "n4", Name: "four", State: registry.NodeFenced},
	} {
		if err := reg.CreateNode(ctx, &n); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []registry.Project{
		{Ref: "system", Name: "system", NodeID: "n1"}, {Ref: refA, Name: "a", Seq: 1, NodeID: "n1"}, {Ref: refB, Name: "b", Seq: 2, NodeID: "n2"},
	} {
		if err := reg.CreateProject(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.CreateReplica(ctx, &registry.Replica{Identifier: refA + "-rr-eu-west-1-abc123", Ref: refA, NodeID: "n2"}); err != nil {
		t.Fatal(err)
	}
	return reg, config.Default()
}

func TestAuthorizationTable(t *testing.T) {
	reg, cfg := authzFixture(t)
	az := func(self string) *Authorizer {
		return &Authorizer{Cfg: cfg, Topology: regTopo{reg, self}, Source: reg, Now: func() time.Time { return time.Now() }}
	}
	active := Peer{Node: "n2", State: registry.NodeActive}
	joining := Peer{Node: "n3", State: registry.NodeJoining}
	fenced := Peer{Node: "n4", State: registry.NodeFenced}
	post := func(ref string) Header { return Header{T: StreamForward, Kind: KindPostgres, Ref: ref} }
	svc := func(k Kind) Header { return Header{T: StreamForward, Kind: k} }
	replica := func(ref string) Header { return Header{T: StreamForward, Kind: KindReplicaPostgres, Ref: ref} }

	for _, tc := range []struct {
		name string
		self string // the node that receives the stream
		peer Peer
		h    Header
		port int // 0: refused
	}{
		// n1, the leader and the home of A and of system.
		{"home project", "n1", active, post(refA), cfg.PortsFor(refA, 1).Postgres},
		{"gotrue of a home project", "n1", active, Header{T: StreamForward, Kind: KindGoTrue, Ref: refA}, cfg.PortsFor(refA, 1).GoTrue},
		{"postgrest of a home project", "n1", active, Header{T: StreamForward, Kind: KindPostgREST, Ref: refA}, cfg.PortsFor(refA, 1).PostgREST},
		{"system postgres on the leader", "n1", active, post("system"), 5433},
		{"a project homed elsewhere", "n1", active, post(refB), 0},
		{"an unknown project", "n1", active, post("zzzzzzzzzzzzzzzzzzzz"), 0},
		{"system has no postgrest", "n1", active, Header{T: StreamForward, Kind: KindPostgREST, Ref: "system"}, 0},
		{"a service on the leader", "n1", active, svc(KindStudio), cfg.Ports.Studio},
		{"admin on the leader", "n1", active, svc(KindAdmin), 7000},
		{"replica ports of a project whose replica is elsewhere", "n1", active, replica(refA), 0},
		{"a bad header", "n1", active, Header{T: StreamForward, Kind: KindPostgres}, 0},
		{"a port is not a kind", "n1", active, Header{T: StreamForward, Kind: "20003", Ref: refA}, 0},
		{"an rpc header", "n1", active, Header{T: StreamRPC}, 0},
		{"no certificate", "n1", Peer{}, post(refA), 0},
		{"unknown peer state", "n1", Peer{Node: "n9", State: registry.NodeLeft}, post(refA), 0},
		// A joining node streams the system cluster and nothing else.
		{"joining: system postgres", "n1", joining, post("system"), 5433},
		{"joining: another project", "n1", joining, post(refA), 0},
		{"joining: a service", "n1", joining, svc(KindStudio), 0},
		{"joining: system replica port", "n1", joining, replica("system"), 0},
		// A fenced node is served nothing.
		{"fenced: system postgres", "n1", fenced, post("system"), 0},
		{"fenced: a project", "n1", fenced, post(refA), 0},
		{"fenced: a service", "n1", fenced, svc(KindStudio), 0},
		// n2, a follower: it hosts B and holds A's replica.
		{"follower: its project", "n2", Peer{Node: "n1", State: registry.NodeActive}, post(refB), cfg.PortsFor(refB, 2).Postgres},
		{"follower: its replica", "n2", Peer{Node: "n1", State: registry.NodeActive}, replica(refA), cfg.ReplicaPorts(refA, 1).Postgres},
		{"follower: the replica's postgrest", "n2", Peer{Node: "n1", State: registry.NodeActive}, Header{T: StreamForward, Kind: KindReplicaPostgREST, Ref: refA}, cfg.ReplicaPorts(refA, 1).PostgREST},
		{"follower: a replica it does not hold", "n2", Peer{Node: "n1", State: registry.NodeActive}, replica(refB), 0},
		{"follower: a project homed on the leader", "n2", Peer{Node: "n1", State: registry.NodeActive}, post(refA), 0},
		{"follower: a shared service", "n2", Peer{Node: "n1", State: registry.NodeActive}, svc(KindStudio), 0},
		{"follower: system postgres", "n2", Peer{Node: "n1", State: registry.NodeActive}, post("system"), 0},
	} {
		got, err := az(tc.self).Resolve(context.Background(), tc.peer, tc.h)
		var refusal *Refusal
		switch {
		case tc.port == 0 && (err == nil || !errors.As(err, &refusal)):
			t.Errorf("%s: port %d, %v: want a refusal", tc.name, got, err)
		case tc.port != 0 && (err != nil || got != tc.port):
			t.Errorf("%s: port %d, %v, want %d", tc.name, got, err, tc.port)
		}
	}
}

// The request policy by caller state.
func TestRequestPolicy(t *testing.T) {
	active := Peer{Node: "n2", State: registry.NodeActive}
	joining := Peer{Node: "n3", State: registry.NodeJoining}
	fenced := Peer{Node: "n4", State: registry.NodeFenced}
	for _, tc := range []struct {
		name string
		p    Peer
		path string
		ok   bool
	}{
		{"anonymous: join", Peer{}, peerapi.PathJoin, true},
		{"anonymous: confirm", Peer{}, peerapi.PathJoinConfirm, false},
		{"anonymous: ping", Peer{}, peerapi.PathPing, false},
		{"anonymous: an instance", Peer{}, peerapi.InstancePath("x"), false},
		{"active: ping", active, peerapi.PathPing, true},
		{"active: plane", active, peerapi.PlanePath(refA, peerapi.PlaneStart), true},
		{"active: fence", active, peerapi.PathFence, true},
		{"joining: ping", joining, peerapi.PathPing, true},
		{"joining: confirm", joining, peerapi.PathJoinConfirm, true},
		{"joining: rejoin again", joining, peerapi.PathRejoin, true},
		{"joining: report", joining, peerapi.PathReport, false},
		{"joining: config", joining, peerapi.PathConfig, false},
		{"fenced: ping", fenced, peerapi.PathPing, true},
		{"fenced: fence", fenced, peerapi.PathFence, true},
		{"fenced: rejoin", fenced, peerapi.PathRejoin, true},
		{"fenced: report", fenced, peerapi.PathReport, false},
		{"fenced: certs", fenced, peerapi.PathCerts, false},
		{"fenced: an instance", fenced, peerapi.InstancePath("x"), false},
	} {
		if ok, why := rpcAllowed(tc.p, tc.path); ok != tc.ok || (!ok && why == "") {
			t.Errorf("%s: allowed %v (%q), want %v", tc.name, ok, why, tc.ok)
		}
	}
}

// At the stream level: a refused forward stream is closed before any byte, and an authorized one
// reaches the loopback port; a stream for a port nobody listens on is closed too.
func TestRefusedAndServedStreams(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	h.project(refA, 1, "n1")
	h.project(refB, 2, "n2")
	n1, n2 := h.nodes["n1"], h.nodes["n2"]
	echoServer(t, n1.cfg.PortsFor(refA, 1).Postgres, "A")
	h.start()
	eventually(t, "a session", func() bool { return n2.mgr.Connected("n1") })

	read := func(st io.Reader) (string, error) {
		b := make([]byte, 16)
		n, err := st.Read(b)
		return string(b[:n]), err
	}
	// Authorized: n2 asks n1 for project A's Postgres.
	st, err := Forward(h.ctx, n2.mgr, "n1", KindPostgres, refA)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.Write([]byte("ping"))
	_ = st.SetReadDeadline(time.Now().Add(3 * time.Second))
	if got, err := read(st); err != nil || got != "A:ping" {
		t.Fatalf("authorized stream: %q, %v", got, err)
	}
	st.Close()

	for name, hdr := range map[string]Header{
		"a project homed on the caller":  {T: StreamForward, Kind: KindPostgres, Ref: refB},
		"an unknown project":             {T: StreamForward, Kind: KindPostgres, Ref: "zzzzzzzzzzzzzzzzzzzz"},
		"a replica that is not there":    {T: StreamForward, Kind: KindReplicaPostgres, Ref: refA},
		"a shared service on a follower": {T: StreamForward, Kind: KindStudio},
		"nothing listening":              {T: StreamForward, Kind: KindGoTrue, Ref: refA},
	} {
		target := "n1"
		if name == "a shared service on a follower" {
			target = "n2" // n1, the leader, asks the follower for a service it does not run
		}
		from := n2.mgr
		if target == "n2" {
			from = n1.mgr
		}
		st, err := from.Dial(h.ctx, target, hdr)
		if err != nil {
			t.Fatalf("%s: dial: %v", name, err)
		}
		_ = st.SetReadDeadline(time.Now().Add(3 * time.Second))
		if got, err := read(st); got != "" || err == nil {
			t.Errorf("%s: read %q, %v: want the stream closed with no byte", name, got, err)
		}
		st.Close()
	}
	// Dial validates the header before it opens anything.
	if _, err := n2.mgr.Dial(h.ctx, "n1", Header{T: StreamForward, Kind: "svc:shell"}); err == nil {
		t.Error("a header that does not validate was sent")
	}
}

// The placements the authorizer remembers do not pile up: an admitted peer that asks for refs that do
// not exist leaves entries that the next sweep drops once they have expired.
func TestPlacementCacheIsSwept(t *testing.T) {
	reg, cfg := authzFixture(t)
	var mu sync.Mutex
	now := time.Now()
	az := &Authorizer{Cfg: cfg, Topology: regTopo{reg, "n1"}, Source: reg, Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }}
	ask := func(from, to int) {
		for i := from; i < to; i++ {
			_, _ = az.place(context.Background(), fmt.Sprintf("%020d", i), false)
		}
	}
	size := func() int {
		n := 0
		az.cache.Range(func(any, any) bool { n++; return true })
		return n
	}
	ask(0, 300)
	mu.Lock()
	now = now.Add(2 * placementTTL)
	mu.Unlock()
	ask(300, 600)
	if n := size(); n > 300 {
		t.Fatalf("%d placements kept after 600 refs, the first 300 long expired", n)
	}
}

// slowSource counts the registry reads of the authorizer and holds GetProject until release is
// closed, so that concurrent callers overlap.
type slowSource struct {
	Source
	gets, lists atomic.Int32
	release     chan struct{}
}

func (s *slowSource) GetProject(ctx context.Context, ref string) (*registry.Project, error) {
	s.gets.Add(1)
	<-s.release
	return s.Source.GetProject(ctx, ref)
}

func (s *slowSource) ListReplicas(ctx context.Context, ref string) ([]registry.Replica, error) {
	s.lists.Add(1)
	return s.Source.ListReplicas(ctx, ref)
}

// Streams that ask for a ref the authorizer does not remember at the same moment share one registry
// read, and replicas are read only for the replica kinds.
func TestPlacementSharesReadsAndFetchesReplicasOnlyForReplicaKinds(t *testing.T) {
	reg, cfg := authzFixture(t)
	src := &slowSource{Source: reg, release: make(chan struct{})}
	az := &Authorizer{Cfg: cfg, Topology: regTopo{reg, "n2"}, Source: src}
	active := Peer{Node: "n1", State: registry.NodeActive}
	resolve := func(k Kind) error {
		_, err := az.Resolve(context.Background(), active, Header{T: StreamForward, Kind: k, Ref: refB})
		return err
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := resolve(KindPostgres); err != nil {
				t.Errorf("resolve: %v", err)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(src.release)
	wg.Wait()
	if g, l := src.gets.Load(), src.lists.Load(); g != 1 || l != 0 {
		t.Fatalf("twenty simultaneous streams of a project kind: %d project reads and %d replica reads, want 1 and 0", g, l)
	}
	if err := resolve(KindGoTrue); err != nil || src.gets.Load() != 1 {
		t.Fatalf("a second kind of the same project: %v, %d project reads", err, src.gets.Load())
	}

	// The replica kinds need the replicas, which the remembered placement lacks: one more read of
	// both, and then the placement serves every kind.
	if err := resolve(KindReplicaPostgres); err == nil {
		t.Fatal("a replica of a project that has none here was admitted")
	}
	if g, l := src.gets.Load(), src.lists.Load(); g != 2 || l != 1 {
		t.Fatalf("after a replica kind: %d project reads and %d replica reads, want 2 and 1", g, l)
	}
	_ = resolve(KindReplicaPostgREST)
	_ = resolve(KindPostgres)
	if g, l := src.gets.Load(), src.lists.Load(); g != 2 || l != 1 {
		t.Fatalf("a remembered placement was read again: %d project reads and %d replica reads", g, l)
	}
}
