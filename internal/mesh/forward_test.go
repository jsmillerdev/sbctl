package mesh

import (
	"context"
	"errors"
	"fmt"
	"net"
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
