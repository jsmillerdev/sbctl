package failover

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover/fenced"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// world is a cluster of fake nodes for the tests of the orchestrator: it implements every port,
// logs what is asked of it in order (events), and fails where a test tells it to. The registry is
// the real in-memory one, behind a gate that refuses writes while the node the orchestrator runs
// on has not promoted its system cluster.
type world struct {
	t   *testing.T
	ctx context.Context

	mu     sync.Mutex
	events []string

	cfg     *config.Config
	reg     registry.Registry
	members *cluster.Static
	snap    cluster.Snapshot
	self    string

	// writable: the orchestrator's registry accepts writes.
	writable bool
	// frozen: the registry handle of this process stays read-only although the system cluster is
	// promoted, as a daemon's does until it restarts in the role.
	frozen bool
	// down: nodes that do not answer anything.
	down map[string]bool
	// partitioned: nodes that do not answer pings and peer calls but still run (a partition).
	partitioned map[string]bool
	// failures: events (by prefix) that return an error, and how many times; -1 is always.
	failures map[string]*failure

	after   map[string]func()     // events (by prefix) that run a function once they are logged
	prim    map[string]*primState // node/ref
	inst    map[string]*instState // identifier
	lsn     map[string]string     // ref: the old primary's final position
	replay  map[string]string     // identifier: where the standby has replayed to
	alerts  []alerts.Event
	marker  *backup.LeaderMarker
	markErr error
	// markerRace, when set, replaces the marker right after the next write.
	markerRace *backup.LeaderMarker
	// held: the holds of a planned stop on the nodes other than this one (node/ref); this node's is a
	// file in cfg.StateDir, read with fenced.Project.
	held map[string]bool
	// heldAtStop: for each stop (node/ref), whether the primary was held when it stopped.
	heldAtStop map[string]bool
	// pingAs, when set, is what every reachable peer answers to a ping.
	pingAs *peerapi.Ping

	clock    int
	provider *fakeProvider
	steps    []registry.MoveStep // progress reported through the context
}

type failure struct {
	err   error
	times int
}

type primState struct {
	running bool
	healthy bool
	asides  int
}

type instState struct {
	node      string
	ref       string
	role      string // replica or primary
	lag       *float64
	noUp      bool
	postgres  bool
	fenced    bool
	demotedAt int
}

const (
	refA = "aaaaaaaaaaaaaaaaaaaa"
	refB = "bbbbbbbbbbbbbbbbbbbb"
	refC = "cccccccccccccccccccc"

	idSysN2 = "system-rr-eu-west-1-s2s2s2"
	idAN2   = refA + "-rr-eu-west-1-a2a2a2"
	idBN2   = refB + "-rr-eu-west-1-b2b2b2"
	idCN2   = refC + "-rr-eu-west-1-c2c2c2"
)

func f64(v float64) *float64 { return &v }

// caughtUp is the replay position of a standby that has replayed everything up to a cluster's stop
// position: the stop position is where the shutdown checkpoint record starts, replay is where the
// last record it replayed ends.
func caughtUp(stop string) string {
	n, err := ParseLSN(stop)
	if err != nil {
		return stop
	}
	n += 0x70
	return fmt.Sprintf("%X/%X", n>>32, uint32(n))
}

// newWorld builds the standard cluster: n1 leads with the system project and two projects
// (refA, refB), n2 follows and holds a healthy standby of each, Storage is on S3 and the epoch is 1.
func newWorld(t *testing.T) *world { return newWorldFunc(t) }

// newWorldFunc builds the standard world; the tests that also run on Postgres swap it (pg_test.go).
var newWorldFunc = func(t *testing.T) *world { return newWorldOn(t, registry.NewMemory()) }

func newWorldOn(t *testing.T, reg registry.Registry) *world {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Domain = "example.test"
	cfg.Fleet.StorageBackend = "s3"
	w := &world{
		t: t, ctx: ctx, cfg: cfg, reg: reg, self: "n1", writable: true,
		down: map[string]bool{}, partitioned: map[string]bool{}, failures: map[string]*failure{},
		prim: map[string]*primState{}, inst: map[string]*instState{}, lsn: map[string]string{}, replay: map[string]string{},
	}
	w.provider = &fakeProvider{w: w, name: "aws"}

	n1, err := w.reg.GetNode(ctx, registry.FounderNodeID)
	must(t, err)
	n1.Region, n1.Version, n1.Provider = "eu-west-1", "v0.2.0", registry.NodeProvider{AWS: &registry.NodeAWS{InstanceID: "i-n1", Region: "eu-west-1"}}
	must(t, w.reg.UpdateNode(ctx, n1))
	n2 := &registry.Node{ID: "n2", Name: "standby", Region: "eu-west-1", Version: "v0.2.0", State: registry.NodeActive,
		Provider: registry.NodeProvider{AWS: &registry.NodeAWS{InstanceID: "i-n2", Region: "eu-west-1"}}}
	must(t, w.reg.CreateNode(ctx, n2))
	must(t, w.reg.SetServiceAddress(ctx, registry.ServiceAddress{IP: "203.0.113.9", AllocationID: "eipalloc-svc"}))

	org, err := w.reg.CreateOrganization(ctx, "acme", "Acme")
	must(t, err)
	for _, p := range []registry.Project{
		{Ref: config.SystemRef, Name: "system", Status: registry.StatusActiveHealthy},
		{Ref: refA, OrgID: org.ID, Name: "a", Status: registry.StatusActiveHealthy},
		{Ref: refB, OrgID: org.ID, Name: "b", Status: registry.StatusActiveHealthy},
	} {
		p := p
		must(t, w.reg.CreateProject(ctx, &p))
	}
	for _, r := range []struct{ id, ref, origin string }{
		{idSysN2, config.SystemRef, registry.ReplicaSystem}, {idAN2, refA, registry.ReplicaManual}, {idBN2, refB, registry.ReplicaDefault},
	} {
		must(t, w.reg.CreateReplica(ctx, &registry.Replica{Identifier: r.id, Ref: r.ref, NodeID: "n2", Origin: r.origin, Status: statusHealthy, InitStep: registry.ReplicaStepDone}))
		w.inst[r.id] = &instState{node: "n2", ref: r.ref, role: "replica", lag: f64(0.4), postgres: true}
	}
	for i, ref := range []string{config.SystemRef, refA, refB} {
		w.prim["n1/"+ref] = &primState{running: true, healthy: true}
		w.lsn[ref] = fmt.Sprintf("0/%X000060", 3+i)
	}
	for id, in := range w.inst {
		w.replay[id] = caughtUp(w.lsn[in.ref])
	}
	w.marker = nil
	w.setSelf("n1", true)
	return w
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// setSelf makes the orchestrator run on node id, as the leader or as a follower.
func (w *world) setSelf(id string, leader bool) {
	nodes, err := w.reg.ListNodes(w.ctx)
	must(w.t, err)
	var self registry.Node
	for _, n := range nodes {
		if n.ID == id {
			self = n
		}
	}
	cl, err := w.reg.GetCluster(w.ctx)
	must(w.t, err)
	role, lead := cluster.RoleFollower, cl.Leader
	if leader {
		role, lead = cluster.RoleLeader, id
	}
	w.snap = cluster.Snapshot{Self: self, Nodes: nodes, Leader: lead, Epoch: cl.Epoch, Role: role, Maintenance: cl.Maintenance}
	if w.members == nil {
		w.members = cluster.NewStatic(w.snap)
	} else {
		w.members.Set(w.snap)
	}
	w.self = id
	w.writable = leader
}

// refreshMembers re-reads the registry into the membership.
func (w *world) refreshMembers() {
	nodes, _ := w.reg.ListNodes(w.ctx)
	cl, _ := w.reg.GetCluster(w.ctx)
	w.snap.Nodes, w.snap.Epoch, w.snap.Maintenance = nodes, cl.Epoch, cl.Maintenance
	for _, n := range nodes {
		if n.ID == w.self {
			w.snap.Self = n
		}
	}
	if w.snap.Role == cluster.RoleLeader || cl.Leader == w.self {
		w.snap.Leader, w.snap.Role = cl.Leader, cluster.RoleLeader
		if cl.Leader != w.self {
			w.snap.Role = cluster.RoleFollower
		}
	} else {
		w.snap.Leader = cl.Leader
	}
	w.members.Set(w.snap)
}

// addNode3 adds a third node n3 holding a replica of refA.
func (w *world) addNode3() {
	n3 := &registry.Node{ID: "n3", Name: "third", Region: "eu-west-1", Version: "v0.2.0", State: registry.NodeActive}
	must(w.t, w.reg.CreateNode(w.ctx, n3))
	w.refreshMembers()
}

func (w *world) log(format string, args ...any) {
	w.mu.Lock()
	w.events = append(w.events, fmt.Sprintf(format, args...))
	w.mu.Unlock()
}

// fail makes the events that start with prefix return err, times times (-1: always).
// noteHold records whether the primary of ref on node is held at the moment it stops: on this node by
// the file the orchestrator wrote, on another by the hold the fake peer took.
func (w *world) noteHold(node, ref string) {
	held := false
	if node == w.self {
		r, err := fenced.Project(w.cfg.Paths(), ref)
		held = err == nil && r != nil && r.Planned
	} else {
		w.mu.Lock()
		held = w.held[node+"/"+ref]
		w.mu.Unlock()
	}
	w.mu.Lock()
	if w.heldAtStop == nil {
		w.heldAtStop = map[string]bool{}
	}
	w.heldAtStop[node+"/"+ref] = held
	w.mu.Unlock()
}

func (w *world) fail(prefix string, err error, times int) {
	w.mu.Lock()
	w.failures[prefix] = &failure{err: err, times: times}
	w.mu.Unlock()
}

func (w *world) clearFailures() {
	w.mu.Lock()
	w.failures = map[string]*failure{}
	w.mu.Unlock()
}

// failing returns the error injected for the event, if any.
func (w *world) failing(event string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for prefix, f := range w.failures {
		if strings.HasPrefix(event, prefix) && f.times != 0 {
			if f.times > 0 {
				f.times--
			}
			return f.err
		}
	}
	return nil
}

// do logs the event and returns the injected failure for it, if any.
func (w *world) do(format string, args ...any) error {
	ev := fmt.Sprintf(format, args...)
	w.log("%s", ev)
	err := w.failing(ev)
	w.mu.Lock()
	var run []func()
	for prefix, fn := range w.after {
		if strings.HasPrefix(ev, prefix) {
			run = append(run, fn)
			delete(w.after, prefix)
		}
	}
	w.mu.Unlock()
	for _, fn := range run {
		fn()
	}
	return err
}

// afterEvent runs fn, once, right after the first event that starts with prefix.
func (w *world) afterEvent(prefix string, fn func()) {
	w.mu.Lock()
	if w.after == nil {
		w.after = map[string]func(){}
	}
	w.after[prefix] = fn
	w.mu.Unlock()
}

// partition cuts a node off: it runs, and answers nobody.
func (w *world) partition(node string) {
	w.mu.Lock()
	w.partitioned[node] = true
	w.mu.Unlock()
}

func (w *world) nodeUp(node string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.down[node] || w.partitioned[node] {
		return fmt.Errorf("node %s does not answer", node)
	}
	return nil
}

// log returns a copy of the events.
func (w *world) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.events...)
}

// has reports whether an event that starts with prefix happened.
func (w *world) has(prefix string) bool { return w.count(prefix) > 0 }

func (w *world) count(prefix string) int {
	n := 0
	for _, e := range w.snapshot() {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

func (w *world) index(prefix string) int {
	for i, e := range w.snapshot() {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

// assertOrder fails unless events starting with each prefix happened in the given order.
func (w *world) assertOrder(prefixes ...string) {
	w.t.Helper()
	evs := w.snapshot()
	pos := 0
	for _, p := range prefixes {
		found := false
		for ; pos < len(evs); pos++ {
			if strings.HasPrefix(evs[pos], p) {
				found = true
				pos++
				break
			}
		}
		if !found {
			w.t.Fatalf("event %q not found in order; events:\n%s", p, strings.Join(evs, "\n"))
		}
	}
}

func (w *world) assertNever(prefix string) {
	w.t.Helper()
	if w.has(prefix) {
		w.t.Fatalf("event %q happened; events:\n%s", prefix, strings.Join(w.snapshot(), "\n"))
	}
}

// deps builds the dependencies of an orchestrator over the world.
func (w *world) deps() Deps {
	return Deps{
		Cfg: w.cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store: func() Store { return &gatedStore{w: w, Registry: w.reg} }, Members: w.members,
		Instances: (*worldInstances)(w), Primaries: (*worldPrimaries)(w), Backups: (*worldBackups)(w), Fleet: (*worldFleet)(w),
		Peers: (*worldPeers)(w), Leader: (*worldLeader)(w), Takeover: (*worldTakeover)(w), Replicas: (*worldReplicas)(w),
		Marker: (*worldMarker)(w), Provider: w.provider, LocalPrimaries: (*worldLocal)(w), LocalServices: (*worldServices)(w),
		Notify: func(_ context.Context, ev alerts.Event) { w.mu.Lock(); w.alerts = append(w.alerts, ev); w.mu.Unlock() },
		Now:    w.now,
		Sleep:  func(ctx context.Context, d time.Duration) error { return ctx.Err() },
	}
}

// now is the world's clock: it moves one second at every look, so a loop that waits for a
// deadline ends even though Sleep returns at once.
func (w *world) now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.clock++
	return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC).Add(time.Duration(w.clock) * time.Second)
}

func (w *world) orch(mut ...func(*Deps)) *Orchestrator {
	d := w.deps()
	for _, m := range mut {
		m(&d)
	}
	o, err := New(d)
	must(w.t, err)
	return o
}

func (w *world) alertKinds() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, a := range w.alerts {
		out = append(out, a.Kind)
	}
	return out
}

// gatedStore is the in-memory registry with its writes refused while the node is a standby.
type gatedStore struct {
	w *world
	registry.Registry
}

func (g *gatedStore) gate(what string) error {
	if !g.w.writable || g.w.frozen {
		return fmt.Errorf("%s: %w", what, registry.ErrReadOnly)
	}
	return g.w.do("registry.%s", what)
}

func (g *gatedStore) SetProjectStatus(ctx context.Context, ref string, s registry.Status) error {
	if err := g.gate("SetProjectStatus " + ref + " " + string(s)); err != nil {
		return err
	}
	return g.Registry.SetProjectStatus(ctx, ref, s)
}

func (g *gatedStore) SetNodeState(ctx context.Context, id string, s registry.NodeState) error {
	if err := g.gate("SetNodeState " + id + " " + string(s)); err != nil {
		return err
	}
	return g.Registry.SetNodeState(ctx, id, s)
}

func (g *gatedStore) SetLeader(ctx context.Context, node string, epoch int64) error {
	if err := g.gate(fmt.Sprintf("SetLeader %s %d", node, epoch)); err != nil {
		return err
	}
	if err := g.Registry.SetLeader(ctx, node, epoch); err != nil {
		return err
	}
	g.w.refreshMembers()
	return nil
}

func (g *gatedStore) SetMaintenance(ctx context.Context, m registry.Maintenance) error {
	if err := g.gate("SetMaintenance " + m.Node); err != nil {
		return err
	}
	return g.Registry.SetMaintenance(ctx, m)
}

func (g *gatedStore) SetProjectNode(ctx context.Context, ref, node string, epoch int64) error {
	if err := g.gate(fmt.Sprintf("SetProjectNode %s %s %d", ref, node, epoch)); err != nil {
		return err
	}
	return g.Registry.SetProjectNode(ctx, ref, node, epoch)
}

func (g *gatedStore) CreateReplica(ctx context.Context, r *registry.Replica) error {
	if err := g.gate("CreateReplica " + r.Ref + " " + r.NodeID); err != nil {
		return err
	}
	return g.Registry.CreateReplica(ctx, r)
}

func (g *gatedStore) CreateMove(ctx context.Context, m *registry.Move) error {
	if err := g.gate("CreateMove " + string(m.Scope)); err != nil {
		return err
	}
	return g.Registry.CreateMove(ctx, m)
}

func (g *gatedStore) AppendMoveStep(ctx context.Context, id int64, s registry.MoveStep) error {
	if !g.w.writable || g.w.frozen {
		return registry.ErrReadOnly
	}
	return g.Registry.AppendMoveStep(ctx, id, s)
}

func (g *gatedStore) FinishMove(ctx context.Context, id int64, st registry.MoveState, e string) error {
	if err := g.gate("FinishMove " + string(st)); err != nil {
		return err
	}
	return g.Registry.FinishMove(ctx, id, st, e)
}

// worldPrimaries implements Primaries.
type worldPrimaries world

func (p *worldPrimaries) w() *world { return (*world)(p) }

func (p *worldPrimaries) state(node, ref string) *primState {
	w := p.w()
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.prim[node+"/"+ref]
	if s == nil {
		s = &primState{}
		w.prim[node+"/"+ref] = s
	}
	return s
}

func (p *worldPrimaries) Stop(_ context.Context, node, ref string) (string, error) {
	w := p.w()
	if err := w.nodeUp(node); err != nil {
		return "", err
	}
	if err := w.do("stop %s/%s", node, ref); err != nil {
		return "", err
	}
	w.noteHold(node, ref)
	s := p.state(node, ref)
	w.mu.Lock()
	s.running = false
	w.mu.Unlock()
	return w.lsn[ref], nil
}

func (p *worldPrimaries) Start(_ context.Context, node, ref string) error {
	w := p.w()
	if err := w.nodeUp(node); err != nil {
		return err
	}
	if err := w.do("start %s/%s", node, ref); err != nil {
		return err
	}
	s := p.state(node, ref)
	w.mu.Lock()
	s.running, s.healthy = true, true
	w.mu.Unlock()
	return nil
}

func (p *worldPrimaries) Healthy(_ context.Context, node, ref string) (bool, string, error) {
	w := p.w()
	if err := w.nodeUp(node); err != nil {
		return false, "", err
	}
	s := p.state(node, ref)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !s.healthy {
		return false, "PostgREST does not answer", nil
	}
	return s.running, "", nil
}

func (p *worldPrimaries) SetAside(_ context.Context, node, ref string, epoch int64) error {
	w := p.w()
	if err := w.do("aside %s/%s epoch=%d", node, ref, epoch); err != nil {
		return err
	}
	s := p.state(node, ref)
	w.mu.Lock()
	s.asides++
	w.mu.Unlock()
	return nil
}

// worldInstances implements Instances.
type worldInstances world

func (i *worldInstances) w() *world { return (*world)(i) }

func (i *worldInstances) status(id string) peerapi.InstanceStatus {
	w := i.w()
	w.mu.Lock()
	defer w.mu.Unlock()
	in := w.inst[id]
	if in == nil {
		return peerapi.InstanceStatus{Identifier: id, Role: "absent"}
	}
	return peerapi.InstanceStatus{
		Identifier: id, Ref: in.ref, Role: in.role, PostgresUp: in.postgres, InRecovery: in.role == "replica",
		LagSeconds: in.lag, ReplayLSN: w.replay[id], ReceiveLSN: w.replay[id], Step: registry.ReplicaStepDone,
	}
}

func (i *worldInstances) Observe(_ context.Context, node, identifier string) (peerapi.InstanceStatus, error) {
	w := i.w()
	if err := w.nodeUp(node); err != nil {
		return peerapi.InstanceStatus{}, err
	}
	w.mu.Lock()
	in := w.inst[identifier]
	w.mu.Unlock()
	if in == nil || in.node != node {
		return peerapi.InstanceStatus{Identifier: identifier, Role: "absent"}, nil
	}
	return i.status(identifier), nil
}

func (i *worldInstances) Ensure(_ context.Context, node string, spec peerapi.InstanceSpec) (peerapi.InstanceStatus, error) {
	w := i.w()
	if err := w.nodeUp(node); err != nil {
		return peerapi.InstanceStatus{}, err
	}
	if err := w.do("ensure %s/%s noupstream=%v", node, spec.Identifier, spec.NoUpstream); err != nil {
		return peerapi.InstanceStatus{}, err
	}
	w.mu.Lock()
	if w.inst[spec.Identifier] == nil {
		w.inst[spec.Identifier] = &instState{node: node, ref: spec.Ref, role: "replica", noUp: spec.NoUpstream, postgres: true}
		w.replay[spec.Identifier] = caughtUp(w.lsn[spec.Ref])
	}
	w.mu.Unlock()
	return i.status(spec.Identifier), nil
}

func (i *worldInstances) Do(_ context.Context, node, identifier string, a peerapi.Action, req peerapi.InstanceAction) (peerapi.InstanceStatus, error) {
	w := i.w()
	if err := w.nodeUp(node); err != nil {
		return peerapi.InstanceStatus{}, err
	}
	switch a {
	case peerapi.ActionPromote:
		if err := w.do("promote %s/%s epoch=%d wait=%s drain=%v", node, identifier, req.Epoch, req.WaitLSN, req.DrainArchive); err != nil {
			return peerapi.InstanceStatus{}, err
		}
		w.mu.Lock()
		in := w.inst[identifier]
		if in == nil || in.node != node {
			w.mu.Unlock()
			return peerapi.InstanceStatus{}, fmt.Errorf("no instance %s on %s", identifier, node)
		}
		if req.WaitLSN != "" && !lsnPast(w.replay[identifier], req.WaitLSN) {
			w.mu.Unlock()
			return peerapi.InstanceStatus{}, fmt.Errorf("replay %s did not reach %s", w.replay[identifier], req.WaitLSN)
		}
		in.role = "primary"
		w.prim[node+"/"+in.ref] = &primState{running: true, healthy: true}
		if in.ref == config.SystemRef {
			w.writable = true // the node's system cluster is a primary: its registry accepts writes
		}
		w.mu.Unlock()
		// A call that promoted and then failed to answer: the promotion happened.
		if err := w.failing("promote-after " + node + "/" + identifier); err != nil {
			return peerapi.InstanceStatus{}, err
		}
	case peerapi.ActionDemote:
		if err := w.do("demote %s/%s epoch=%d", node, identifier, req.Epoch); err != nil {
			return peerapi.InstanceStatus{}, err
		}
		ref, _, _, ok := registry.ParseReplicaIdentifier(identifier)
		if !ok {
			return peerapi.InstanceStatus{}, fmt.Errorf("bad identifier %s", identifier)
		}
		w.mu.Lock()
		w.inst[identifier] = &instState{node: node, ref: ref, role: "replica", lag: f64(0), postgres: true}
		w.replay[identifier] = caughtUp(w.lsn[ref])
		w.mu.Unlock()
	default:
		return peerapi.InstanceStatus{}, fmt.Errorf("unexpected action %s", a)
	}
	return i.status(identifier), nil
}

// worldFleet implements Fleet.
type worldFleet world

func (f *worldFleet) QuiesceTenant(_ context.Context, ref string) error {
	return (*world)(f).do("fleet.quiesce %s", ref)
}

func (f *worldFleet) EnsureTenant(_ context.Context, ref string) error {
	return (*world)(f).do("fleet.ensure %s", ref)
}

// worldBackups implements BaseBackups.
type worldBackups world

func (b *worldBackups) BaseBackup(_ context.Context, node, ref string, req peerapi.BackupRequest) (peerapi.BackupResult, error) {
	if err := (*world)(b).do("basebackup %s/%s", node, ref); err != nil {
		return peerapi.BackupResult{}, err
	}
	return peerapi.BackupResult{ID: "b1"}, nil
}

// worldReplicas implements ReplicaSetup.
type worldReplicas world

func (r *worldReplicas) SetupOn(_ context.Context, ref, node string) error {
	return (*world)(r).do("replicas.setup %s on %s", ref, node)
}

// worldPeers implements Peers.
type worldPeers world

// errSelf is what the mesh answers for a call to this very node (mesh.ErrNoSession): the orchestrator
// has to handle itself without the peer API.
func (w *world) errSelf(node string) error {
	if node == w.self {
		return fmt.Errorf("node %s is this node", node)
	}
	return nil
}

func (p *worldPeers) Ping(_ context.Context, node string) (peerapi.Ping, error) {
	w := (*world)(p)
	if err := w.errSelf(node); err != nil {
		return peerapi.Ping{}, err
	}
	if err := w.nodeUp(node); err != nil {
		return peerapi.Ping{}, err
	}
	w.mu.Lock()
	as := w.pingAs
	w.mu.Unlock()
	if as != nil {
		p := *as
		p.Node = node
		return p, nil
	}
	cl, _ := w.reg.GetCluster(w.ctx)
	return peerapi.Ping{Node: node, Epoch: cl.Epoch, Leader: cl.Leader, Health: "healthy"}, nil
}

func (p *worldPeers) Fence(_ context.Context, node string, req FenceCall) (peerapi.FenceResponse, error) {
	w := (*world)(p)
	if err := w.errSelf(node); err != nil {
		return peerapi.FenceResponse{}, err
	}
	if err := w.nodeUp(node); err != nil {
		w.log("fence %s unreachable ref=%s", node, req.Ref)
		return peerapi.FenceResponse{}, err
	}
	if err := w.do("fence %s epoch=%d ref=%s", node, req.Epoch, req.Ref); err != nil {
		return peerapi.FenceResponse{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if req.Ref != "" {
		w.prim[node+"/"+req.Ref] = &primState{}
		return peerapi.FenceResponse{Epoch: req.Epoch, Fenced: true, Stopped: []string{req.Ref}}, nil
	}
	var stopped []string
	for k := range w.prim {
		if n, ref, _ := strings.Cut(k, "/"); n == node {
			w.prim[k].running = false
			stopped = append(stopped, ref)
		}
	}
	sort.Strings(stopped)
	return peerapi.FenceResponse{Epoch: req.Epoch, Fenced: true, Stopped: stopped}, nil
}

// Hold and Release of a node other than this one: the node's disk is not in the test, so what it
// holds is in the world, and the order of the events is what the tests read.
func (p *worldPeers) Hold(_ context.Context, node, ref string, epoch int64, _ string) error {
	w := (*world)(p)
	if err := w.errSelf(node); err != nil {
		return err
	}
	if err := w.nodeUp(node); err != nil {
		return err
	}
	if err := w.do("hold %s/%s epoch=%d", node, ref, epoch); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.held == nil {
		w.held = map[string]bool{}
	}
	w.held[node+"/"+ref] = true
	return nil
}

func (p *worldPeers) Release(_ context.Context, node, ref string, epoch int64) error {
	w := (*world)(p)
	if err := w.errSelf(node); err != nil {
		return err
	}
	if err := w.nodeUp(node); err != nil {
		return err
	}
	if err := w.do("release %s/%s epoch=%d", node, ref, epoch); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.held, node+"/"+ref)
	return nil
}

// worldLeader implements Leader.
type worldLeader world

func (l *worldLeader) Quiesce(_ context.Context, node string, req QuiesceRequest) (QuiesceResult, error) {
	w := (*world)(l)
	if err := w.nodeUp(node); err != nil {
		return QuiesceResult{}, err
	}
	if err := w.do("quiesce %s to=%s epoch=%d", node, req.To, req.Epoch); err != nil {
		return QuiesceResult{}, err
	}
	res := QuiesceResult{LSNs: map[string]string{}}
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, s := range w.prim {
		if n, ref, _ := strings.Cut(k, "/"); n == node {
			s.running = false
			res.LSNs[ref] = w.lsn[ref]
		}
	}
	return res, nil
}

func (l *worldLeader) Resume(_ context.Context, node string) error {
	w := (*world)(l)
	if err := w.do("resume-leader %s", node); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, s := range w.prim {
		if n, _, _ := strings.Cut(k, "/"); n == node {
			s.running = true
		}
	}
	return nil
}

// worldTakeover implements Takeover.
type worldTakeover world

func (tk *worldTakeover) BecomeLeader(_ context.Context, epoch int64) error {
	w := (*world)(tk)
	if err := w.do("become-leader epoch=%d", epoch); err != nil {
		return err
	}
	if !w.writable {
		return errors.New("the system cluster is not promoted")
	}
	return nil
}

// worldMarker implements backup.EpochMarkerStore.
type worldMarker world

func (m *worldMarker) ReadLeaderMarker(context.Context) (*backup.LeaderMarker, error) {
	w := (*world)(m)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.markErr != nil {
		return nil, w.markErr
	}
	if w.marker == nil {
		return nil, nil
	}
	c := *w.marker
	return &c, nil
}

func (m *worldMarker) WriteLeaderMarker(_ context.Context, lm backup.LeaderMarker) error {
	w := (*world)(m)
	if err := w.do("marker epoch=%d leader=%s", lm.Epoch, lm.Leader); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.markErr != nil {
		return w.markErr
	}
	if w.marker != nil && w.marker.Epoch > lm.Epoch {
		return backup.ErrMarkerNewer
	}
	w.marker = &lm
	if w.markerRace != nil { // another survivor's write lands after this one: the store ignores conditions
		w.marker = w.markerRace
	}
	return nil
}

// fakeProvider implements Provider and Cloud.
type fakeProvider struct {
	w     *world
	name  string
	state PeerState
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Fence(_ context.Context, req Request) error {
	if err := p.w.do("provider.fence %s planned=%v epoch=%d", req.Old.ID, req.Planned, req.Epoch); err != nil {
		return err
	}
	p.w.mu.Lock()
	p.w.down[req.Old.ID] = true
	p.w.mu.Unlock()
	return nil
}

func (p *fakeProvider) Probe(context.Context) error { return p.w.do("provider.probe") }

func (p *fakeProvider) TakeOver(_ context.Context, req Request) error {
	return p.w.do("provider.takeover %s", req.New.ID)
}

func (p *fakeProvider) PeerState(context.Context, registry.Node) (PeerState, error) {
	if err := p.w.do("provider.peerstate"); err != nil {
		return PeerState{}, err
	}
	return p.state, nil
}

func (p *fakeProvider) ProbeTakeover(context.Context) error { return p.w.do("provider.probetakeover") }

var (
	_ Cloud         = (*fakeProvider)(nil)
	_ AddressProber = (*fakeProvider)(nil)
)

// check finds the named check of a plan.
func findCheck(t *testing.T, pl *Plan, name string) Check {
	t.Helper()
	for _, c := range pl.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", name, pl.Checks)
	return Check{}
}

func hasCheck(pl *Plan, name string) bool {
	for _, c := range pl.Checks {
		if c.Name == name {
			return true
		}
	}
	return false
}

// worldLocal implements LocalPrimaries: this node's own primaries.
type worldLocal world

func (l *worldLocal) Stop(ctx context.Context, ref string) (string, error) {
	w := (*world)(l)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := w.do("local.stop %s", ref); err != nil {
		return "", err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.prim[w.self+"/"+ref]; s != nil {
		s.running = false
	}
	return w.lsn[ref], nil
}

func (l *worldLocal) Start(ctx context.Context, ref string) error {
	w := (*world)(l)
	if err := w.do("local.start %s", ref); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prim[w.self+"/"+ref] = &primState{running: true, healthy: true}
	return nil
}

func (l *worldLocal) Healthy(ctx context.Context, ref string) (bool, string, error) {
	w := (*world)(l)
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.prim[w.self+"/"+ref]
	return s != nil && s.running && s.healthy, "", nil
}

func (l *worldLocal) SetAside(ctx context.Context, ref string, epoch int64) error {
	return (*world)(l).do("local.aside %s epoch=%d", ref, epoch)
}

// worldServices implements LocalServices.
type worldServices world

func (s *worldServices) Stop(context.Context) error  { return (*world)(s).do("services.stop") }
func (s *worldServices) Start(context.Context) error { return (*world)(s).do("services.start") }

type backupMarker = backup.LeaderMarker
