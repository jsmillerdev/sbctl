package replicas

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/registry"
)

// Options is what a Controller is built from. Registry and Config are required. Without Ops and
// Backups the Controller answers the Service calls from the registry and never runs a setup: that
// is what `supavise replicas` and a node that is not in a cluster get.
type Options struct {
	Registry registry.Registry
	Config   *config.Config
	Log      *slog.Logger
	// Members says whether this node leads and at which epoch; nil means it leads at epoch 1.
	Members cluster.Membership
	// Ops performs replica operations on any node (placement.InstanceOps).
	Ops placement.InstanceOps
	// Backups hands out the base backup a replica starts from (backup.Service).
	Backups backup.BaseBackupEnsurer
	// Pooler keeps the Supavisor tenants of replicas; nil skips them.
	Pooler Pooler
	// Admit judges capacity; nil admits everything (the node refuses at launch what it cannot hold).
	Admit Admitter
	// Alert raises an alert; nil means alerts.Notify.
	Alert func(ctx context.Context, ev alerts.Event)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// NewID returns the six characters that end a replica's identifier; nil draws them at random.
	NewID func() string
	// Interval is how often the controller looks at every replica (and polls the nodes); zero
	// means 10 seconds. A cluster of one node with no replicas looks once a minute.
	Interval time.Duration
	// MinGap is the shortest time between two passes, so a burst of registry changes makes one;
	// zero means 500 ms.
	MinGap   time.Duration
	Timeouts Timeouts
	// SnapshotPath, when set, is a file the controller keeps up to date with what it observes,
	// for `supavise replicas ls` (see ReadSnapshot).
	SnapshotPath string
}

// Timeouts bound how long a setup may wait at a step, and how long a call may keep failing,
// before the setup fails with the step's code. Zero fields take the defaults.
type Timeouts struct {
	// Retry is how long a call to a node or the backup store may fail, trying again with a
	// growing pause, before the setup fails. Default 10 minutes.
	Retry time.Duration
	// Initiate is how long the node may take to start seeding after the instance exists. Default 5 minutes.
	Initiate time.Duration
	// Download is the time allowed for the base backup to arrive: this much, or four times the
	// estimate when that is longer. Default 30 minutes.
	Download time.Duration
	// Replay is the time allowed for the standby to replay the archive and start streaming:
	// this much, or four times the estimate when that is longer. Default 2 hours.
	Replay time.Duration
	// Complete is how long the standby may take to accept connections once it streams. Default 10 minutes.
	Complete time.Duration
	// Busy is how long RemoveAll and RemoveOn wait for a worker that is acting on a replica
	// (it may be creating the instance at this moment) before they report the removal as pending.
	// Default 30 seconds.
	Busy time.Duration
	// Restart is how long the call that restarts a replica's units on its node may take. Default 5 minutes.
	Restart time.Duration
}

func (t Timeouts) withDefaults() Timeouts {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&t.Retry, 10*time.Minute)
	def(&t.Initiate, 5*time.Minute)
	def(&t.Download, 30*time.Minute)
	def(&t.Replay, 2*time.Hour)
	def(&t.Complete, 10*time.Minute)
	def(&t.Busy, 30*time.Second)
	def(&t.Restart, 5*time.Minute)
	return t
}

// Controller is the replica controller (design 2.7). It implements Service, Remover and ReportSink.
// Run drives it on the leader; the registry rows are its desired state and each pass is level
// triggered, so a restart of the daemon resumes every setup from the step its row holds.
type Controller struct {
	o   Options
	cfg *config.Config
	reg registry.Registry
	log *slog.Logger
	to  Timeouts
	now func() time.Time

	// wmu orders the controller's writes of a replica's status with a removal, so that a setup
	// that finishes a step never writes over GOING_DOWN.
	wmu sync.Mutex
	// setupMu makes a Setup request, from its first read to the check after its insert, one step
	// among this process's requests (setup.go).
	setupMu sync.Mutex

	mu    sync.Mutex
	state map[string]*replicaState
	// busy holds the identifiers a worker is acting on; a pass starts no second one.
	busy map[string]bool
	// rate is the measured download rate of base backups in bytes per second; zero before the first.
	rate float64
	// capacityAlerted holds the nodes that have a replica_capacity alert open.
	capacityAlerted map[string]bool
	warned          map[string]bool

	// runCtx is the context of Run while it runs; Restart's background call ends with it.
	runCtx context.Context
	// stopped is set when Run is winding down: no new background call may start.
	stopped bool

	// workers holds a token for each running worker, so a pass never starts more than maxWorkers.
	workers chan struct{}
	// resume is the row the next pass starts at, after one that ran out of worker tokens, so that
	// the rows at the end of the list are not starved.
	resume int

	wg   sync.WaitGroup
	wake chan struct{}
}

// maxWorkers bounds how many replicas the controller acts on at once. A pass that has more to do
// goes on with the rest in the next one.
const maxWorkers = 32

var (
	_ Service    = (*Controller)(nil)
	_ Remover    = (*Controller)(nil)
	_ ReportSink = (*Controller)(nil)
)

// New returns a Controller over o.
func New(o Options) *Controller {
	c := &Controller{
		o: o, cfg: o.Config, reg: o.Registry, log: o.Log, to: o.Timeouts.withDefaults(), now: o.Now,
		state: map[string]*replicaState{}, busy: map[string]bool{}, capacityAlerted: map[string]bool{},
		warned: map[string]bool{}, wake: make(chan struct{}, 1), workers: make(chan struct{}, maxWorkers),
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

func (c *Controller) interval() time.Duration {
	if c.o.Interval > 0 {
		return c.o.Interval
	}
	return 10 * time.Second
}

func (c *Controller) minGap() time.Duration {
	if c.o.MinGap > 0 {
		return c.o.MinGap
	}
	return 500 * time.Millisecond
}

// leader reports whether this node may drive replicas, and the epoch it acts in.
func (c *Controller) leader() (epoch int64, ok bool) {
	if c.o.Members == nil {
		return 1, true
	}
	return c.o.Members.Epoch(), c.o.Members.IsLeader()
}

// running reports whether the controller can drive setups: it needs the node operations and the
// backup service.
func (c *Controller) running() bool { return c.o.Ops != nil && c.o.Backups != nil }

func (c *Controller) alert(ctx context.Context, ev alerts.Event) {
	if c.o.Alert != nil {
		c.o.Alert(ctx, ev)
		return
	}
	_ = alerts.Notify(ctx, ev)
}

// kick asks Run for a pass soon.
func (c *Controller) kick() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Run drives the replicas until ctx ends: a pass at start, then one every Interval and after each
// change to the replicas, nodes or projects tables. It returns ctx's error.
func (c *Controller) Run(ctx context.Context) error {
	changes, err := c.reg.Subscribe(ctx)
	if err != nil {
		c.log.Warn("replicas: cannot follow registry changes, polling instead", "error", err)
		changes = nil
	}
	c.mu.Lock()
	c.runCtx, c.stopped = ctx, false
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.stopped = true
		c.mu.Unlock()
		c.wg.Wait()
		c.mu.Lock()
		c.runCtx = nil
		c.mu.Unlock()
	}()
	last := time.Time{}
	for {
		if wait := c.minGap() - c.now().Sub(last); wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		last = c.now()
		idle := c.pass(ctx)
		every := c.interval()
		if idle {
			every = max(every, time.Minute)
		}
		t := time.NewTimer(every)
	wait:
		for {
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
				break wait
			case <-c.wake:
				break wait
			case ch, ok := <-changes:
				if !ok {
					changes = nil
					continue
				}
				if ch.Table == "replicas" || ch.Table == "nodes" || ch.Table == "projects" {
					break wait
				}
			}
		}
		t.Stop()
	}
}

// Tick runs one pass and waits for the work it started. Tests drive the controller with it.
func (c *Controller) Tick(ctx context.Context) {
	c.pass(ctx)
	c.wg.Wait()
}

// pass looks at every replica once. It returns true when there is nothing to look at: a cluster of
// one node with no replica rows. A node that does not lead, or cannot drive replicas, looks again
// at the usual interval, which costs nothing.
func (c *Controller) pass(ctx context.Context) (idle bool) {
	if _, ok := c.leader(); !ok || !c.running() {
		return false
	}
	nodes, err := c.reg.ListNodes(ctx)
	if err != nil {
		c.log.Warn("replicas: list nodes", "error", err)
		return false
	}
	rows, err := c.reg.ListReplicas(ctx, "")
	if err != nil {
		c.log.Warn("replicas: list replicas", "error", err)
		return false
	}
	if len(nodes) <= 1 && len(rows) == 0 {
		return true
	}
	projects, err := c.reg.ListProjects(ctx)
	if err != nil {
		c.log.Warn("replicas: list projects", "error", err)
		return false
	}
	if c.reconcileDefaults(ctx, nodes, projects, rows) > 0 {
		if rows, err = c.reg.ListReplicas(ctx, ""); err != nil {
			c.log.Warn("replicas: list replicas", "error", err)
			return false
		}
	}
	c.admit(ctx, nodes, projects, rows)
	if rows, err = c.reg.ListReplicas(ctx, ""); err != nil {
		c.log.Warn("replicas: list replicas", "error", err)
		return false
	}
	live := make(map[string]bool, len(rows))
	status := make(map[string]registry.Status, len(projects))
	for _, p := range projects {
		status[p.Ref] = p.Status
	}
	for i := range rows {
		live[rows[i].Identifier] = true
	}
	start, n, left := c.resume, len(rows), -1
	for k := range n {
		i := (start + k) % n
		if needsWork(&rows[i]) && !c.spawn(ctx, rows[i], status[rows[i].Ref]) && left < 0 {
			left = i // out of workers: begin here next time
		}
	}
	c.resume = max(left, 0)
	c.forget(live)
	c.writeSnapshot(rows)
	return false
}

// needsWork reports whether a worker has anything to do for the row: not a setup that waits for
// admission (the pass admits it) and not a failed one (Studio's user drops it).
func needsWork(r *registry.Replica) bool {
	switch {
	case r.Status == statusGoingDown:
		return true
	case r.Origin == registry.ReplicaSystem:
		return true
	case r.Status == registry.ReplicaInitError:
		return false
	case settingUp(r):
		return r.InitStep != StepRequested
	}
	return active(r)
}

// spawn starts a worker for the replica unless one is running for it. It reports false when there
// is no free worker, so the replica waits for the next pass.
func (c *Controller) spawn(ctx context.Context, row registry.Replica, project registry.Status) bool {
	id := row.Identifier
	c.mu.Lock()
	if c.busy[id] {
		c.mu.Unlock()
		return true
	}
	select {
	case c.workers <- struct{}{}:
	default:
		c.mu.Unlock()
		return false
	}
	c.busy[id] = true
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() {
			c.mu.Lock()
			delete(c.busy, id)
			c.mu.Unlock()
			<-c.workers
		}()
		c.work(ctx, &row, project)
	}()
	return true
}

// forget drops what is kept in memory for replicas that no longer have a row.
func (c *Controller) forget(live map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.state {
		if !live[id] && !c.busy[id] {
			delete(c.state, id)
		}
	}
}

// work acts on one replica once, whatever the row says is next. The row is the pass's copy:
// the writes below check that the replica is not being removed. project is the status of the
// replica's project, as the pass read it ("" when the project is gone).
func (c *Controller) work(ctx context.Context, r *registry.Replica, project registry.Status) {
	switch {
	case r.Status == statusGoingDown:
		c.removeStep(ctx, r)
	case r.Origin == registry.ReplicaSystem:
		c.watchSystem(ctx, r)
	case settingUp(r):
		c.setupStep(ctx, r)
	case active(r):
		c.monitor(ctx, r, project)
	}
}

// HandleReport implements ReportSink: it takes the instances a node reports as observations, for
// the replicas that really are on that node.
func (c *Controller) HandleReport(ctx context.Context, rep peerapi.Report) {
	if len(rep.Instances) == 0 {
		c.kick()
		return
	}
	// The intake runs this in its own goroutine and must not wait long: one query for the node's
	// replicas, however many instances the report holds.
	rows, err := c.reg.ListReplicasOn(ctx, rep.Node)
	if err != nil {
		c.log.Debug("replicas: report from a node whose replicas cannot be read", "node", rep.Node, "error", err)
		return
	}
	byID := make(map[string]*registry.Replica, len(rows))
	for i := range rows {
		byID[rows[i].Identifier] = &rows[i]
	}
	for i := range rep.Instances {
		st := rep.Instances[i]
		// A node reports only its own instances, and an instance only under the project its row
		// names: a report that puts it under another would skew that project's WAL position
		// estimate. A status that names no project takes the row's.
		r := byID[st.Identifier]
		if r == nil || (st.Ref != "" && st.Ref != r.Ref) {
			continue
		}
		st.Ref = r.Ref
		c.observed(r.Identifier, st)
	}
	c.kick()
}

// nodeLabel is how a message names a node: the operator's name for it.
func nodeLabel(n registry.Node) string {
	if n.Name != "" {
		return n.Name
	}
	return n.ID
}

// errNoOps is returned by the calls that need the node operations on a Controller built without them.
var errNoOps = errors.New("replicas: this node cannot reach the replica nodes")

// errStopping is returned by a call that would start background work while the controller shuts down.
var errStopping = errors.New("replicas: the controller is shutting down")
