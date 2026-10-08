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
	// FenceNode stops everything on this node that could write as a primary or serve as the leader and
	// removes the launchers of the clusters, and returns the refs it stopped (cluster.FenceLocal over
	// the node's supervisor). When it is set, a fence of the whole node uses it in place of stopping
	// the primaries and the shared services one by one; a fence of one project never does.
	FenceNode func(ctx context.Context) (stopped []string, err error)

	// Extra adds checks to the preflight of a server move.
	Extra ExtraChecks
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
	// delegating is the node that runs a switchover for this node's own move (delegate.go) while
	// the move holds the slot: that node's quiesce and resume are the move itself, not another move.
	delegating string

	lockMu sync.Mutex
	locks  map[string]*sync.Mutex

	// quiesceMu serializes the quiesce and the resume this node answers as the leader (peer.go): a
	// repeated request waits for the one that is stopping clusters and then answers from the record,
	// and a resume does not start clusters a quiesce is still stopping.
	quiesceMu sync.Mutex

	// deleg is the switchover this node runs for a leader that asked (delegate.go).
	delegMu sync.Mutex
	deleg   *delegated

	probes    probeCache
	takeovers probeCache
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
	for _, g := range d.Gaps() {
		d.Log.Warn("a part of the move is not wired", "port", g.Port, "effect", g.Effect)
	}
	return &Orchestrator{d: d, locks: map[string]*sync.Mutex{}}, nil
}

// Gap is a part of a move that has nothing wired to it, and what a move leaves undone because of it.
type Gap struct {
	Port   string
	Effect string
}

// Gaps lists the ports of d that are not wired and that a move can do without, with what a move then
// leaves undone. A move goes on without them, which is how a node without shared services runs one in
// a test, and also how a daemon whose wiring lacks an adapter would run it: silently, if nobody said.
// New logs each gap, and Readiness shows the ones that leave a service broken after a move.
func (d Deps) Gaps() []Gap {
	var gaps []Gap
	add := func(missing bool, port, effect string) {
		if missing {
			gaps = append(gaps, Gap{Port: port, Effect: effect})
		}
	}
	add(d.LocalPrimaries == nil, "LocalPrimaries", "this node cannot stop, start or fence a primary when a peer asks: a quiesce, a fence and the project calls of the leader are answered 501")
	add(d.Fleet == nil, "Fleet", "Realtime and the pooler are not told to let go of a project before it stops, and are not registered with it again after a move: Realtime and Supavisor keep serving the old address")
	add(d.LocalServices == nil, "LocalServices", "the shared services (Studio, Realtime, Storage and the rest) keep running while the leader stops for a switchover, and do not start again when it is undone")
	add(d.Locks == nil, "Locker", "a project move is kept apart from pause, resume, delete and upgrade of the same project by a lock of this process only, which those do not take")
	add(d.Extra == nil, "ExtraChecks", "the certificates and the shared-service artifacts are not checked before a server move")
	add(d.Backups == nil, "BaseBackups", "no base backup is taken on the new timeline after a move; the backup timer takes the next one")
	add(d.Replicas == nil, "ReplicaSetup", "the old home of a failed-over project gets no new replica by itself; the move prints the supavise replicas add command")
	add(d.Takeover == nil, "Takeover", "a server move does not wait for the daemon to lead before it moves the projects")
	return gaps
}

// Gaps lists what is not wired for this orchestrator (Deps.Gaps).
func (o *Orchestrator) Gaps() []Gap { return o.d.Gaps() }

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

// acquireForDelegate is acquire for a request from the node that this node's own move has asked to
// run a switchover. The move holds the slot for the whole delegation, and the node asks the leader
// to quiesce in the middle of it: that request is the move continuing, so it gets through without
// taking the slot a second time. Any other caller finds the slot held, as before.
func (o *Orchestrator) acquireForDelegate(node string) (release func(), err error) {
	o.mu.Lock()
	if o.busy && o.delegating != "" && o.delegating == node {
		o.mu.Unlock()
		return func() {}, nil
	}
	o.mu.Unlock()
	return o.acquire()
}

// delegate marks the slot as held for a switchover that node runs, until the returned function is called.
func (o *Orchestrator) delegate(node string) (done func()) {
	o.mu.Lock()
	o.delegating = node
	o.mu.Unlock()
	return func() {
		o.mu.Lock()
		o.delegating = ""
		o.mu.Unlock()
	}
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
