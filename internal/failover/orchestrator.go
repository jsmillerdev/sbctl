package failover

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// Deps is what an Orchestrator is built from. Cfg, Store and Members are required; the rest has
// a default or turns the part of a move that needs it into a no-op, which is how a node without
// shared services (the tests of one project) runs a move.
type Deps struct {
	Cfg *config.Config
	Log *slog.Logger
	// Store returns the node's current registry. It is a function because the node's registry
	// is read-only until its system cluster is promoted and writable after.
	Store   func() Store
	Members cluster.Membership

	Instances Instances
	Primaries Primaries
	Backups   BaseBackups
	Fleet     Fleet
	Peers     Peers
	Leader    Leader
	Takeover  Takeover
	Replicas  ReplicaSetup
	Locks     Locker
	// Marker is the epoch marker of the backup store (backup.Service implements it). Nil on a
	// node whose backups are files, where a second server cannot exist anyway.
	Marker backup.EpochMarkerStore
	// Provider fences and takes the address over. Nil is the manual provider.
	Provider Provider

	// LocalPrimaries and LocalServices are this node's side of the peer endpoints of peer.go.
	LocalPrimaries LocalPrimaries
	LocalServices  LocalServices

	// Extra adds checks to the preflight of a server move: what only the owner of the part knows
	// (the certificates are mirrored, the shared-service artifacts are present).
	Extra func(ctx context.Context, to registry.Node) []Check
	// PublicProbe checks the cluster's public address from outside, as a client would
	// (GET /healthz of the API host through the service address). Nil: the automatic server mode
	// has no such signal and does not act.
	PublicProbe func(ctx context.Context) error

	// Notify delivers an alert. Nil is alerts.Notify.
	Notify func(ctx context.Context, ev alerts.Event)
	Now    func() time.Time
	// Sleep waits for d or until ctx ends. Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Orchestrator moves projects, and the leadership of the whole server, between nodes. It
// implements Service.
type Orchestrator struct {
	d Deps

	mu   sync.Mutex
	busy bool

	lockMu sync.Mutex
	locks  map[string]*sync.Mutex

	probes probeCache
	// off is why the automatic mode is not running, "" while it is or was not asked for.
	offMu sync.Mutex
	off   string
}

// autoOff returns why the automatic mode is off ("" when it is on or not configured).
func (o *Orchestrator) autoOff() string {
	o.offMu.Lock()
	defer o.offMu.Unlock()
	return o.off
}

func (o *Orchestrator) setAutoOff(reason string) {
	o.offMu.Lock()
	o.off = reason
	o.offMu.Unlock()
}

var _ Service = (*Orchestrator)(nil)

// New builds an Orchestrator.
func New(d Deps) (*Orchestrator, error) {
	if d.Cfg == nil || d.Store == nil || d.Members == nil {
		return nil, errors.New("failover: New needs Cfg, Store and Members")
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	d.Log = d.Log.With("component", "failover")
	if d.Provider == nil {
		d.Provider = Manual{}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sleep == nil {
		d.Sleep = sleep
	}
	if d.Notify == nil {
		d.Notify = func(ctx context.Context, ev alerts.Event) { _ = alerts.Notify(ctx, ev) }
	}
	return &Orchestrator{d: d, locks: map[string]*sync.Mutex{}}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Orchestrator) conf() config.Failover { return o.d.Cfg.Failover }
func (o *Orchestrator) store() Store          { return o.d.Store() }
func (o *Orchestrator) self() registry.Node   { return o.d.Members.Self() }

// acquire marks a move as running. One runs at a time on a node: a second request is refused
// rather than queued, because an operator who typed the command twice wants to hear about it.
func (o *Orchestrator) acquire() (release func(), err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.busy {
		return nil, ErrBusy
	}
	o.busy = true
	return func() {
		o.mu.Lock()
		o.busy = false
		o.mu.Unlock()
	}, nil
}

// Busy reports whether a move is running on this node.
func (o *Orchestrator) Busy() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.busy
}

// lockProject serializes with the lifecycle operations on ref when the node has an engine lock to
// offer, and with other moves of the same ref in this process otherwise.
func (o *Orchestrator) lockProject(ctx context.Context, ref string) (func(), error) {
	if o.d.Locks != nil {
		return o.d.Locks.Lock(ctx, ref)
	}
	o.lockMu.Lock()
	m := o.locks[ref]
	if m == nil {
		m = &sync.Mutex{}
		o.locks[ref] = m
	}
	o.lockMu.Unlock()
	m.Lock()
	return m.Unlock, nil
}

func (o *Orchestrator) wait(ctx context.Context, d time.Duration) error { return o.d.Sleep(ctx, d) }

// alert sends an alert about a move. The alert layer never fails the work that raised it.
func (o *Orchestrator) alert(ctx context.Context, ev alerts.Event) {
	o.d.Notify(context.WithoutCancel(ctx), ev)
}

// progressKey carries the function a run reports its steps to.
type progressKey struct{}

// WithProgress returns a context whose moves report each recorded step to fn, as it is recorded.
// The control socket uses it to stream a move to the CLI.
func WithProgress(ctx context.Context, fn func(registry.MoveStep)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

func reportStep(ctx context.Context, s registry.MoveStep) {
	if fn, ok := ctx.Value(progressKey{}).(func(registry.MoveStep)); ok && fn != nil {
		fn(s)
	}
}

// node returns the registry row of id.
func (o *Orchestrator) node(ctx context.Context, id string) (registry.Node, error) {
	n, err := o.store().GetNode(ctx, id)
	if err != nil {
		return registry.Node{}, fmt.Errorf("failover: node %s: %w", id, err)
	}
	return *n, nil
}
