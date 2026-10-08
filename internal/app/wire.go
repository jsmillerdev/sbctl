package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/notimpl"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/proxy"
	"github.com/supavise/supavise/internal/registry"
)

// The daemon grows by hooks, one file each (wire_mesh.go, wire_placement.go, ...), so that the
// parts of the cluster work add themselves to Serve without editing it. Serve builds a Wire after
// the node is open and before the Management API and the edge proxy are built, and runs wireHooks in
// order. A hook may do four things: set the collaborators it owns on w.API and w.Proxy, which are
// still plain options; register peer API handlers with mesh.Handle; start background work with
// w.Go; and publish what later hooks need with Provide. A hook that has nothing to do on this
// node (no cluster is configured) returns nil without doing anything, so a node that does not use
// the cluster features behaves as it did.

// wireHooks run in this order. A hook may Get what an earlier one provided.
var wireHooks = []struct {
	name string
	fn   func(ctx context.Context, w *Wire) error
}{
	{"mesh", wireMesh},                     // wire_mesh.go: peer server, sessions, forwarders; provides mesh.Mesh and cluster.Membership
	{"placement", wirePlacement},           // wire_placement.go: plane router, instance agent; provides placement.Resolver, InstanceOps, BackupOps
	{"fleet", wireFleet},                   // wire_fleet.go: the shared services' follower mode
	{"replicas", wireReplicas},             // wire_replicas.go: the replica controller and its service
	{"proxy", wireProxy},                   // wire_proxy.go: replica and balancer hosts, certificate mirror
	{"failover", wireFailover},             // wire_failover.go: the orchestrator, the monitor, the readiness route
	{"storagemigrate", wireStorageMigrate}, // wire_storagemigrate.go: the Storage migration service
}

// Wire is what a hook is given.
type Wire struct {
	Cfg *config.Config
	Log *slog.Logger
	// Node is the open node: registry, secrets, engine, plane, supervisor, artifacts.
	Node *lifecycle.Node
	// Options are what the binary told Serve about itself.
	Options Options
	// API and Proxy are the options the Management API and the edge proxy are built from after the
	// hooks ran. A hook sets the fields it owns.
	API   *api.Deps
	Proxy *proxy.Options

	runners []namedRunner
	stops   []func()
	values  map[reflect.Type]any
	// off are the features a hook switched off, with the reason (Off).
	off map[string]string
	// ready is closed when the shared services and the projects have started (Ready).
	ready     chan struct{}
	readyOnce sync.Once
	// fleetMu serializes the changes to the shared services' units: startFleet at boot and the role
	// reconciliation that wireFleet runs when the node's role changes.
	fleetMu sync.Mutex
	// serverChecks are the preflight checks of a server move that the hooks add (AddServerCheck).
	serverChecks []failover.ExtraChecks
}

type namedRunner struct {
	name string
	fn   func(ctx context.Context) error
}

func newWire(cfg *config.Config, log *slog.Logger, node *lifecycle.Node, o Options, apiDeps *api.Deps, popts *proxy.Options) *Wire {
	w := &Wire{Cfg: cfg, Log: log, Node: node, Options: o, API: apiDeps, Proxy: popts, values: map[reflect.Type]any{}, ready: make(chan struct{})}
	// What a node that is not in a cluster has: itself as the leader, and the registry's answers
	// about where projects live (all on this node).
	Provide[placement.Resolver](w, placement.RegistryResolver{Reg: node.Registry})
	if self, err := node.Registry.GetNode(context.Background(), registry.FounderNodeID); err == nil {
		Provide[cluster.Membership](w, cluster.Solo(*self))
	}
	return w
}

// Ready is closed once the daemon has started the shared services and every project that should run, or
// has stopped trying. Work that needs them (a move that registers a project with the pooler again)
// waits for it; it never closes on a daemon that stops first, so such work also watches its context.
func (w *Wire) Ready() <-chan struct{} { return w.ready }

// markReady closes Ready. Serve calls it once.
func (w *Wire) markReady() { w.readyOnce.Do(func() { close(w.ready) }) }

// AddServerCheck adds checks to the preflight of a server move (failover.ExtraChecks): what only the
// part that owns the matter can tell, such as whether the shared services' artifacts are in place.
// wireFailover joins them into the one ExtraChecks the orchestrator takes.
func (w *Wire) AddServerCheck(c failover.ExtraChecks) { w.serverChecks = append(w.serverChecks, c) }

// Go registers fn to run in the daemon's group of goroutines once the hooks are done. fn gets a
// context that ends when the daemon stops; its error stops the daemon.
func (w *Wire) Go(name string, fn func(ctx context.Context) error) {
	w.runners = append(w.runners, namedRunner{name, fn})
}

// OnStop registers fn to run when Serve returns, last registered first.
func (w *Wire) OnStop(fn func()) { w.stops = append(w.stops, fn) }

// Provide publishes v for later hooks under the type T, which is the interface a consumer asks
// for: Provide[mesh.Mesh](w, m), not Provide(w, m) when m is a concrete type. A later Provide of the
// same type replaces the earlier value.
func Provide[T any](w *Wire, v T) { w.values[reflect.TypeFor[T]()] = v }

// Get returns what an earlier hook provided under the type T.
func Get[T any](w *Wire) (T, bool) {
	v, ok := w.values[reflect.TypeFor[T]()].(T)
	return v, ok
}

// run calls the hooks in order. A hook that returns notimpl.Err is skipped; any other error stops
// the daemon before it serves.
func (w *Wire) run(ctx context.Context) error {
	for _, h := range wireHooks {
		err := h.fn(ctx, w)
		switch {
		case err == nil:
		case errors.Is(err, notimpl.Err):
			w.Log.Debug("wire hook not implemented", "hook", h.name)
		default:
			return fmt.Errorf("serve: %s: %w", h.name, err)
		}
	}
	w.verifyPorts()
	return nil
}

// start runs the goroutines the hooks registered in g.
func (w *Wire) start(g *errgroup.Group, ctx context.Context) {
	for _, r := range w.runners {
		g.Go(func() error {
			if err := r.fn(ctx); err != nil {
				return fmt.Errorf("serve: %s: %w", r.name, err)
			}
			return nil
		})
	}
}

// stop runs the cleanups, last registered first.
func (w *Wire) stop() {
	for i := len(w.stops) - 1; i >= 0; i-- {
		w.stops[i]()
	}
}
