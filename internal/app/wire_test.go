package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/proxy"
	"github.com/supavise/supavise/internal/registry"
)

func testWire(t *testing.T) *Wire {
	t.Helper()
	node := &lifecycle.Node{Registry: registry.NewMemory()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newWire(config.Default(), log, node, Options{}, &api.Deps{}, &proxy.Options{})
}

// On a server with no cluster every hook runs and does nothing but the watch for a cluster identity.
func TestHooksOfASingleServerStartOnlyTheIdentityWatcher(t *testing.T) {
	w := testWire(t)
	if err := w.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The mesh hook of a server that never joined a cluster watches for the cluster identity that
	// `supavise node token` writes, and starts nothing else.
	if len(w.runners) != 1 || w.runners[0].name != "cluster identity" || len(w.stops) != 0 {
		t.Fatalf("the hooks started more than the identity watcher: %d runners, %d stops", len(w.runners), len(w.stops))
	}
	seen := map[string]bool{}
	for _, h := range wireHooks {
		if h.name == "" || seen[h.name] || h.fn == nil {
			t.Errorf("hook %q is unnamed, listed twice or nil", h.name)
		}
		seen[h.name] = true
	}
}

// A reconciliation that waits behind the boot start of the shared services stops waiting when a newer
// role cancels it, and the lock is not lost by the wait that gave up.
func TestFleetLockGivesUpWhenItsContextEnds(t *testing.T) {
	var l ctxLock // the zero value is unlocked
	if err := l.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { got <- l.Lock(ctx) }()
	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled wait returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait for the lock did not end with its context")
	}
	l.Unlock()
	if err := l.Lock(context.Background()); err != nil {
		t.Fatalf("the lock was not free after the unlock: %v", err)
	}
	l.Unlock()
}

func TestWireStartsWhatHooksRegisterAndStopsLastFirst(t *testing.T) {
	old := wireHooks
	defer func() { wireHooks = old }()
	var order []string
	wireHooks = []struct {
		name string
		fn   func(ctx context.Context, w *Wire) error
	}{
		{"first", func(ctx context.Context, w *Wire) error {
			Provide[mesh.Mesh](w, nil)
			w.OnStop(func() { order = append(order, "stop first") })
			w.Go("worker", func(ctx context.Context) error { order = append(order, "worker"); return nil })
			return nil
		}},
		{"second", func(ctx context.Context, w *Wire) error {
			w.OnStop(func() { order = append(order, "stop second") })
			return nil
		}},
	}
	w := testWire(t)
	if err := w.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	g, ctx := errgroup.WithContext(context.Background())
	w.start(g, ctx)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	w.stop()
	if got := strings.Join(order, ","); got != "worker,stop second,stop first" {
		t.Fatalf("order = %s", got)
	}
}

func TestWireRunNamesAFailingHookAndStops(t *testing.T) {
	old := wireHooks
	defer func() { wireHooks = old }()
	boom := errors.New("boom")
	called := false
	wireHooks = []struct {
		name string
		fn   func(ctx context.Context, w *Wire) error
	}{
		{"mesh", func(context.Context, *Wire) error { return boom }},
		{"later", func(context.Context, *Wire) error { called = true; return nil }},
	}
	err := testWire(t).run(context.Background())
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "mesh") || called {
		t.Fatalf("run = %v, later hook called: %v", err, called)
	}
	// A worker's error stops the group and names the worker.
	w := testWire(t)
	w.Go("peer server", func(context.Context) error { return boom })
	g, ctx := errgroup.WithContext(context.Background())
	w.start(g, ctx)
	if err := g.Wait(); !errors.Is(err, boom) || !strings.Contains(err.Error(), "peer server") {
		t.Fatalf("worker error = %v", err)
	}
}

// A node with no cluster has itself as the leader and the registry's answers to "where is it".
func TestWireDefaults(t *testing.T) {
	w := testWire(t)
	m, ok := Get[cluster.Membership](w)
	if !ok || !m.IsLeader() || m.Self().ID != registry.FounderNodeID {
		t.Fatalf("membership: %v %v", m, ok)
	}
	r, ok := Get[placement.Resolver](w)
	if !ok {
		t.Fatal("no resolver")
	}
	if _, err := r.HomeOf(context.Background(), "nope"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("resolver: %v", err)
	}
	if _, ok := Get[mesh.Mesh](w); ok {
		t.Fatal("a mesh nobody provided")
	}
	// Providing replaces; an interface is found under its interface type only.
	Provide[cluster.Membership](w, cluster.NewStatic(cluster.Snapshot{Role: cluster.RoleFollower}))
	if m, _ := Get[cluster.Membership](w); m.IsLeader() {
		t.Fatal("the replacement membership was not used")
	}
	Provide(w, cluster.Solo(registry.Node{}))
	if _, ok := Get[*cluster.Static](w); !ok {
		t.Fatal("a concrete value is found under its concrete type")
	}
	if m, _ := Get[cluster.Membership](w); m.IsLeader() {
		t.Fatal("providing the concrete type replaced the interface")
	}
}

// A hook reads what an earlier one provided, so the order of the table is part of the wiring: the mesh
// before everything, placement before the failover orchestrator that needs its ports, the fleet before
// the replica controller (the pooler) and the failover ports (tenants, services), the controller
// before the proxy (the lag of a replica) and the failover orchestrator (replica setup).
func TestHookOrderGivesEachHookWhatItReads(t *testing.T) {
	pos := map[string]int{}
	for i, h := range wireHooks {
		pos[h.name] = i
	}
	for _, c := range []struct{ first, then string }{
		{"host", "mesh"}, {"mesh", "placement"}, {"mesh", "fleet"}, {"placement", "replicas"}, {"fleet", "replicas"},
		{"replicas", "proxy"}, {"placement", "failover"}, {"fleet", "failover"}, {"replicas", "failover"}, {"proxy", "failover"},
	} {
		a, aok := pos[c.first]
		b, bok := pos[c.then]
		if !aok || !bok || a >= b {
			t.Errorf("hook %q must run before %q (positions %d, %d)", c.first, c.then, a, b)
		}
	}
}
