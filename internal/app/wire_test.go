package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

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

// Until a hook is implemented its stub says so, and Serve goes on without it.
func TestUnimplementedHooksAreSkipped(t *testing.T) {
	w := testWire(t)
	if err := w.run(context.Background()); err != nil {
		t.Fatalf("run with the stubs: %v", err)
	}
	if len(w.runners) != 0 || len(w.stops) != 0 {
		t.Fatalf("a stub started something: %d runners, %d stops", len(w.runners), len(w.stops))
	}
	seen := map[string]bool{}
	for _, h := range wireHooks {
		if h.name == "" || seen[h.name] || h.fn == nil {
			t.Errorf("hook %q is unnamed, listed twice or nil", h.name)
		}
		seen[h.name] = true
	}
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
