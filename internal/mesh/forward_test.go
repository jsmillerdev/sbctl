package mesh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// lookCounter counts how often the forwarders list the projects, and stands in for the change
// stream: events are sent by the test, and stream false makes Subscribe fail.
type lookCounter struct {
	Source
	lists   atomic.Int32
	stream  bool
	changes chan registry.Change
}

func (c *lookCounter) ListProjects(ctx context.Context) ([]registry.Project, error) {
	c.lists.Add(1)
	return c.Source.ListProjects(ctx)
}

func (c *lookCounter) Subscribe(context.Context) (<-chan registry.Change, error) {
	if !c.stream {
		return nil, errors.New("test: no change stream")
	}
	return c.changes, nil
}

func newLookCounter(h *harness, stream bool) *lookCounter {
	return &lookCounter{Source: h.reg, stream: stream, changes: make(chan registry.Change)}
}

func forwardersOver(h *harness, src Source, self string) *Forwarders {
	n := h.nodes[self]
	return &Forwarders{Cfg: n.cfg, Topology: regTopo{h.reg, self}, Source: src, Dialer: n.mgr, Log: quietLog(),
		Interval: 20 * time.Millisecond, Safety: time.Hour}
}

// With a change stream, the forwarders read the registry once at the start and then again only when
// the stream names a change, the membership moves or the safety look comes; a tick in between costs
// no registry read.
func TestForwardersReadTheRegistryOnlyWhenSomethingChanged(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	h.project(refA, 1, "n2")
	src := newLookCounter(h, true)
	f := forwardersOver(h, src, "n1")
	go f.Run(h.ctx)
	eventually(t, "the first reconcile", func() bool { return len(f.Ports()) == 3 })
	time.Sleep(300 * time.Millisecond) // fifteen ticks
	if n := src.lists.Load(); n != 1 {
		t.Fatalf("%d reads of the registry for an idle node, want 1", n)
	}

	src.changes <- registry.Change{Table: "projects", Key: refA, Op: "update"}
	eventually(t, "a read after a change event", func() bool { return src.lists.Load() == 2 })

	// A node that stops being active is a membership change, which the topology learns without any
	// event on the stream: the forwarders to it go within a tick.
	if err := h.reg.SetNodeState(h.ctx, "n2", registry.NodeFenced); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the forwarders to a fenced node to go", func() bool { return len(f.Ports()) == 0 })
	if n := src.lists.Load(); n != 3 {
		t.Fatalf("%d reads after the membership moved, want 3", n)
	}
}

// The safety look recomputes the desired set even when nothing announced a change, because the
// stream is best effort.
func TestForwardersLookAgainAfterTheSafetyInterval(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	h.project(refA, 1, "n2")
	src := newLookCounter(h, true)
	f := forwardersOver(h, src, "n1")
	f.Safety = 100 * time.Millisecond
	go f.Run(h.ctx)
	eventually(t, "the safety look", func() bool { return src.lists.Load() >= 3 })
}

// Without a change stream every look recomputes the desired set, as the forwarders always did.
func TestForwardersWithoutAStreamReadTheRegistryAtEveryLook(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	h.project(refA, 1, "n2")
	src := newLookCounter(h, false)
	f := forwardersOver(h, src, "n1")
	go f.Run(h.ctx)
	eventually(t, "a read at most looks", func() bool { return src.lists.Load() >= 5 })
}

// A port that cannot be bound is tried again at every look, and the reads stop once it is bound.
func TestForwardersRetryAPortThatCannotBeBoundYet(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	h.project(refA, 1, "n2")
	src := newLookCounter(h, true)
	f := forwardersOver(h, src, "n1")
	var busy atomic.Bool
	busy.Store(true)
	f.Listen = func(port int) (net.Listener, error) {
		if busy.Load() {
			return nil, fmt.Errorf("test: port %d is in use", port)
		}
		return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	}
	go f.Run(h.ctx)
	eventually(t, "retries while the port is in use", func() bool { return src.lists.Load() >= 5 })
	busy.Store(false)
	eventually(t, "the ports to bind", func() bool { return len(f.Ports()) == 3 })
	bound := src.lists.Load()
	time.Sleep(200 * time.Millisecond)
	if n := src.lists.Load(); n != bound {
		t.Fatalf("%d reads after the ports were bound, want %d", n, bound)
	}
}

// toNode reports whether f has n listeners and every one of them forwards to node.
func toNode(f *Forwarders, node string, n int) bool {
	ports := f.Ports()
	if len(ports) != n {
		return false
	}
	for _, to := range ports {
		if !strings.HasSuffix(to, " -> "+node) {
			return false
		}
	}
	return true
}

// within polls cond until it holds or d has passed, which is much shorter than eventually when the
// point of the test is that a look did not wait for the safety interval.
func within(t testing.TB, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %v", what, d)
}

// A project that moves to another node is followed by the registry's own change stream alone, in the
// debounce and not in a tick: the tick and the safety look are an hour away.
func TestForwardersFollowAHomeMoveOnTheStreamAlone(t *testing.T) {
	h := newHarness(t, "n1", "n2", "n3")
	h.project(refA, 1, "n2")
	f := forwardersOver(h, h.reg, "n1")
	f.Interval = time.Hour
	go f.Run(h.ctx)
	within(t, 5*time.Second, "the forwarders to n2", func() bool { return toNode(f, "n2", 3) })
	cl, err := h.reg.GetCluster(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetProjectNode(h.ctx, refA, "n3", cl.Epoch); err != nil {
		t.Fatal(err)
	}
	within(t, time.Second, "the forwarders to follow the move to n3", func() bool { return toNode(f, "n3", 3) })
}

// A change of leader reaches no change stream (the cluster row has no trigger that notifies), so the
// look at the next tick has to see it: the shared-service ports follow the leader, with the safety
// look an hour away.
func TestForwardersFollowANewLeaderWithinATick(t *testing.T) {
	h := newHarness(t, "n1", "n2", "n3")
	src := newLookCounter(h, true) // a stream that never says anything
	f := forwardersOver(h, src, "n2")
	go f.Run(h.ctx)
	within(t, 5*time.Second, "the service ports to forward to the leader n1", func() bool { return toNode(f, "n1", len(ServiceKinds)) })
	cl, err := h.reg.GetCluster(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetLeader(h.ctx, "n3", cl.Epoch+1); err != nil {
		t.Fatal(err)
	}
	within(t, time.Second, "the service ports to follow the new leader n3", func() bool { return toNode(f, "n3", len(ServiceKinds)) })
	// n2 itself is promoted: it serves them, so nothing forwards.
	if err := h.reg.SetLeader(h.ctx, "n2", cl.Epoch+2); err != nil {
		t.Fatal(err)
	}
	within(t, time.Second, "the service forwarders to go when this node leads", func() bool { return len(f.Ports()) == 0 })
}

// A node that is fenced forwards nothing, and says so at the next tick even though the registry did
// not change.
func TestForwardersDropEverythingWhenFencedWithinATick(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	h.project(refA, 1, "n2")
	src := newLookCounter(h, true)
	f := forwardersOver(h, src, "n1")
	var fenced atomic.Bool
	f.Fenced = fenced.Load
	go f.Run(h.ctx)
	within(t, 5*time.Second, "the forwarders", func() bool { return len(f.Ports()) == 3 })
	fenced.Store(true)
	within(t, time.Second, "the forwarders to go", func() bool { return len(f.Ports()) == 0 })
	fenced.Store(false)
	within(t, time.Second, "the forwarders to come back", func() bool { return len(f.Ports()) == 3 })
}

// A change stream that ends mid-run puts the forwarders back on a recompute at every tick, so a move
// the registry makes after that is followed within a tick even with the safety look an hour away.
func TestForwardersFallBackToEveryTickWhenTheStreamEnds(t *testing.T) {
	h := newHarness(t, "n1", "n2", "n3")
	h.project(refA, 1, "n2")
	src := newLookCounter(h, true)
	f := forwardersOver(h, src, "n1")
	go f.Run(h.ctx)
	within(t, 5*time.Second, "the forwarders to n2", func() bool { return toNode(f, "n2", 3) })
	close(src.changes)
	time.Sleep(100 * time.Millisecond) // a few ticks for the close to be seen
	cl, err := h.reg.GetCluster(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetProjectNode(h.ctx, refA, "n3", cl.Epoch); err != nil {
		t.Fatal(err)
	}
	within(t, time.Second, "the forwarders to follow a move that no stream announced", func() bool { return toNode(f, "n3", 3) })
}

// An event the stream never delivers is caught by the safety look, which is the bound the design
// gives a missed event.
func TestForwardersCatchAMissedEventAtTheSafetyLook(t *testing.T) {
	h := newHarness(t, "n1", "n2", "n3")
	h.project(refA, 1, "n2")
	src := newLookCounter(h, true)
	f := forwardersOver(h, src, "n1")
	f.Safety = 300 * time.Millisecond
	go f.Run(h.ctx)
	within(t, 5*time.Second, "the forwarders to n2", func() bool { return toNode(f, "n2", 3) })
	cl, err := h.reg.GetCluster(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetProjectNode(h.ctx, refA, "n3", cl.Epoch); err != nil {
		t.Fatal(err)
	}
	within(t, 2*time.Second, "the safety look to follow the move", func() bool { return toNode(f, "n3", 3) })
}

// A suspension keeps the retry going: while a project is suspended every tick looks again, and when
// it is resumed the forwarders come back without waiting for the safety look.
func TestForwardersResumeAfterASuspensionWithoutTheSafetyLook(t *testing.T) {
	h := newHarness(t, "n1", "n2")
	h.project(refA, 1, "n2")
	src := newLookCounter(h, true)
	f := forwardersOver(h, src, "n1")
	go f.Run(h.ctx)
	within(t, 5*time.Second, "the forwarders", func() bool { return len(f.Ports()) == 3 })
	resume := f.Suspend(refA)
	if n := len(f.Ports()); n != 0 {
		t.Fatalf("%d listeners right after Suspend", n)
	}
	time.Sleep(100 * time.Millisecond) // several ticks
	if n := len(f.Ports()); n != 0 {
		t.Fatalf("%d listeners while suspended", n)
	}
	resume()
	within(t, time.Second, "the forwarders to return", func() bool { return len(f.Ports()) == 3 })
}
