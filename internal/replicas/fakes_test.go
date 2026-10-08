package replicas

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas/replicaid"
)

const (
	refA = "aaaaaaaaaaaaaaaaaaaa"
	refB = "bbbbbbbbbbbbbbbbbbbb"
	refC = "cccccccccccccccccccc"
)

// fakeClock is a clock tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeInstance is one replica instance on a fake node. Each Observe moves it one step along the
// way a real agent reports: initiated, downloaded, replayed, completed.
type fakeInstance struct {
	spec peerapi.InstanceSpec
	step string
	// stopAt holds the instance at that step (a stuck download).
	stopAt string
	// failAt makes the agent report errCode when the instance reaches that step.
	failAt  string
	errCode string
	lag     float64
	// receiver is the receiver status once the standby streams.
	receiver  string
	postgrest bool
	restarts  int
}

// fakeNodes is placement.InstanceOps over a map of nodes.
type fakeNodes struct {
	mu        sync.Mutex
	inst      map[string]map[string]*fakeInstance // node -> identifier -> instance
	down      map[string]bool                     // unreachable nodes
	absent    map[string]bool                     // nodes that answer Ensure but report every instance absent
	ensureErr error
	removeErr map[string]error
	calls     []string
	ensured   []peerapi.InstanceSpec
	// script customizes a new instance.
	script func(*fakeInstance)
	// gate, when set, holds every Observe until it is closed.
	gate chan struct{}
}

func newFakeNodes() *fakeNodes {
	return &fakeNodes{inst: map[string]map[string]*fakeInstance{}, down: map[string]bool{}, absent: map[string]bool{}, removeErr: map[string]error{}}
}

var _ interface {
	Ensure(context.Context, string, peerapi.InstanceSpec) (peerapi.InstanceStatus, error)
} = (*fakeNodes)(nil)

func (f *fakeNodes) status(node string, in *fakeInstance) peerapi.InstanceStatus {
	st := peerapi.InstanceStatus{Identifier: in.spec.Identifier, Ref: in.spec.Ref, Role: "replica", Step: in.step, At: time.Now()}
	if stepIndex(in.step) >= stepIndex(StepLaunched) {
		st.PostgresUp = true
	}
	if stepIndex(in.step) >= stepIndex(StepReplayed) {
		st.InRecovery, st.ReceiverStatus = true, in.receiver
		l := in.lag
		st.LagSeconds = &l
		st.ReceiveLSN, st.ReplayLSN = "0/5000000", "0/5000000"
	}
	if stepIndex(in.step) >= stepIndex(StepDone) {
		st.PostgRESTReady = in.postgrest
	}
	if in.failAt == in.step && in.errCode != "" {
		st.Error, st.Detail = in.errCode, "the agent says so"
	}
	return st
}

func (f *fakeNodes) record(s string) { f.calls = append(f.calls, s) }

func (f *fakeNodes) Ensure(_ context.Context, node string, spec peerapi.InstanceSpec) (peerapi.InstanceStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ensure " + node + " " + spec.Identifier)
	f.ensured = append(f.ensured, spec)
	if f.down[node] {
		return peerapi.InstanceStatus{}, fmt.Errorf("mesh: no session to node %s", node)
	}
	if f.ensureErr != nil {
		return peerapi.InstanceStatus{}, f.ensureErr
	}
	m := f.inst[node]
	if m == nil {
		m = map[string]*fakeInstance{}
		f.inst[node] = m
	}
	in := m[spec.Identifier]
	if in == nil {
		in = &fakeInstance{spec: spec, step: StepLaunched, receiver: "streaming", postgrest: true}
		if f.script != nil {
			f.script(in)
		}
		m[spec.Identifier] = in
	}
	return f.status(node, in), nil
}

func (f *fakeNodes) Observe(_ context.Context, node, identifier string) (peerapi.InstanceStatus, error) {
	if g := f.gate; g != nil {
		<-g
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("observe " + node + " " + identifier)
	if f.down[node] {
		return peerapi.InstanceStatus{}, fmt.Errorf("mesh: no session to node %s", node)
	}
	in := f.inst[node][identifier]
	if in == nil || f.absent[node] {
		return peerapi.InstanceStatus{Identifier: identifier, Role: "absent", At: time.Now()}, nil
	}
	if in.stopAt != in.step && !(in.failAt == in.step && in.errCode != "") {
		if i := stepIndex(in.step); i < stepIndex(StepDone) {
			in.step = steps[i+1]
		}
	}
	return f.status(node, in), nil
}

func (f *fakeNodes) Remove(_ context.Context, node, identifier string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("remove " + node + " " + identifier)
	if f.down[node] {
		return fmt.Errorf("mesh: no session to node %s", node)
	}
	if err := f.removeErr[node]; err != nil {
		return err
	}
	delete(f.inst[node], identifier)
	return nil
}

func (f *fakeNodes) Do(_ context.Context, node, identifier string, a peerapi.Action, _ peerapi.InstanceAction) (peerapi.InstanceStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(string(a) + " " + node + " " + identifier)
	if f.down[node] {
		return peerapi.InstanceStatus{}, fmt.Errorf("mesh: no session to node %s", node)
	}
	in := f.inst[node][identifier]
	if in == nil {
		return peerapi.InstanceStatus{}, errors.New("no such instance")
	}
	if a == peerapi.ActionRestart {
		in.restarts++
	}
	return f.status(node, in), nil
}

func (f *fakeNodes) get(node, identifier string) *fakeInstance {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inst[node][identifier]
}

func (f *fakeNodes) callsMatching(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// fakeBackups is backup.BaseBackupEnsurer.
type fakeBackups struct {
	mu    sync.Mutex
	calls []time.Duration
	refs  []string
	err   error
	size  int64
}

func (f *fakeBackups) EnsureBase(_ context.Context, ref string, maxAge time.Duration) (*registry.Backup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, maxAge)
	f.refs = append(f.refs, ref)
	if f.err != nil {
		return nil, f.err
	}
	return &registry.Backup{Ref: ref, Kind: "base", Status: registry.BackupCompleted, Location: "s3://bucket/" + ref + "/base/20261008T110000Z/",
		SizeBytes: f.size, StopLSN: "0/3000000"}, nil
}

// fakePooler is Pooler.
type fakePooler struct {
	mu      sync.Mutex
	ensured []string
	removed []string
	err     error
}

func (f *fakePooler) EnsureReplicaTenant(_ context.Context, ref, identifier string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.ensured = append(f.ensured, identifier)
	return nil
}

func (f *fakePooler) RemoveReplicaTenant(_ context.Context, identifier string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, identifier)
	return nil
}

// fakeNoRoom is the error a node's Ensure returns when its own admission says it is full.
type fakeNoRoom struct{ msg string }

func (e fakeNoRoom) Error() string { return "node n2: " + e.msg }
func (e fakeNoRoom) NoRoom() bool  { return true }

// fakeAdmit refuses the nodes in full.
type fakeAdmit struct {
	mu   sync.Mutex
	full map[string]bool
	reqs []AdmitRequest
}

func (f *fakeAdmit) Admit(_ context.Context, req AdmitRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.full[req.Node.ID] {
		return fmt.Errorf("node %s is full", req.Node.Name)
	}
	return nil
}

// alertLog collects the alerts.
type alertLog struct {
	mu  sync.Mutex
	evs []alerts.Event
}

func (a *alertLog) add(_ context.Context, ev alerts.Event) {
	a.mu.Lock()
	a.evs = append(a.evs, ev)
	a.mu.Unlock()
}

// count returns how many alerts of kind (and resolved or not) were raised.
func (a *alertLog) count(kind string, resolved bool) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, e := range a.evs {
		if e.Kind == kind && e.Resolved == resolved {
			n++
		}
	}
	return n
}

// env is a leader n1 with the projects, other nodes and fakes a test needs.
type env struct {
	t       *testing.T
	ctx     context.Context
	reg     *registry.Memory
	cfg     *config.Config
	clock   *fakeClock
	nodes   *fakeNodes
	bk      *fakeBackups
	pool    *fakePooler
	admit   *fakeAdmit
	alerts  *alertLog
	ctrl    *Controller
	members *cluster.Static
	opts    Options
	ids     int
}

// newEnv builds the cluster of the tests: n1 (the leader, region us-east-1) homing refA and refB
// (small, healthy), n2 "eu" in eu-west-1 and n3 "ap" in ap-south-1, both active.
func newEnv(t *testing.T, mutate ...func(*Options)) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), reg: registry.NewMemory(), cfg: config.Default(), clock: newClock(),
		nodes: newFakeNodes(), bk: &fakeBackups{size: 2 << 30}, pool: &fakePooler{}, admit: &fakeAdmit{full: map[string]bool{}}, alerts: &alertLog{}}
	e.cfg.Backup.Backend = "s3://bucket/prefix"
	e.cfg.Replicas.Concurrency = 2
	if err := e.reg.UpdateNode(e.ctx, &registry.Node{ID: "n1", Name: "primary", Region: "us-east-1", PublicHost: "n1.example.com"}); err != nil {
		t.Fatal(err)
	}
	e.addNode("n2", "eu", "eu-west-1")
	e.addNode("n3", "ap", "ap-south-1")
	if err := e.reg.CreateProject(e.ctx, &registry.Project{Ref: "system", Name: "system", Class: "system", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	e.addProject(refA, "small")
	e.addProject(refB, "small")
	e.members = cluster.NewStatic(cluster.Snapshot{Self: registry.Node{ID: "n1"}, Leader: "n1", Epoch: 7, Role: cluster.RoleLeader})
	e.opts = Options{
		Registry: e.reg, Config: e.cfg, Log: slog.New(slog.DiscardHandler), Members: e.members, Ops: e.nodes, Backups: e.bk,
		Pooler: e.pool, Admit: e.admit, Alert: e.alerts.add, Now: e.clock.Now, Interval: 10 * time.Second,
		NewID: func() string { e.ids++; return fmt.Sprintf("%06d", e.ids) },
	}
	for _, m := range mutate {
		m(&e.opts)
	}
	e.ctrl = New(e.opts)
	return e
}

func (e *env) addNode(id, name, region string) {
	e.t.Helper()
	n := &registry.Node{ID: id, Name: name, Region: region, State: registry.NodeActive, PublicHost: name + ".example.com"}
	if err := e.reg.CreateNode(e.ctx, n); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) addProject(ref, class string) {
	e.t.Helper()
	p := &registry.Project{Ref: ref, Name: ref, Class: class, Status: registry.StatusActiveHealthy, Region: "us-east-1"}
	if err := e.reg.CreateProject(e.ctx, p); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) setProject(ref string, status registry.Status) {
	e.t.Helper()
	if err := e.reg.SetProjectStatus(e.ctx, ref, status); err != nil {
		e.t.Fatal(err)
	}
}

// replica returns the row for the single replica of ref on node.
func (e *env) replica(ref, node string) registry.Replica {
	e.t.Helper()
	rs, err := e.reg.ListReplicas(e.ctx, ref)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range rs {
		if r.NodeID == node {
			return r
		}
	}
	e.t.Fatalf("no replica of %s on %s (have %+v)", ref, node, rs)
	return registry.Replica{}
}

// systemReplica records the standby of the system cluster on node, as the cluster join does.
func (e *env) systemReplica(node string) *registry.Replica {
	e.t.Helper()
	n, err := e.reg.GetNode(e.ctx, node)
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := replicaid.EnsureSystem(e.ctx, e.reg, *n, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) hasReplica(ref, node string) bool {
	rs, _ := e.reg.ListReplicas(e.ctx, ref)
	for _, r := range rs {
		if r.NodeID == node {
			return true
		}
	}
	return false
}

// tick runs n passes.
func (e *env) tick(n int) {
	for range n {
		e.ctrl.Tick(e.ctx)
	}
}

// settle ticks until the replica is ACTIVE_HEALTHY, failing the test after 20 passes.
func (e *env) settle(ref, node string) registry.Replica {
	e.t.Helper()
	for range 20 {
		e.tick(1)
		if r := e.replica(ref, node); r.Status == statusHealthy {
			return r
		}
	}
	r := e.replica(ref, node)
	e.t.Fatalf("replica of %s on %s did not become healthy: %s at %s (%s)", ref, node, r.Status, r.InitStep, r.InitError)
	return r
}

func (e *env) user(err error) string {
	e.t.Helper()
	var ue *UserError
	if !errors.As(err, &ue) {
		e.t.Fatalf("want a *UserError, got %T %v", err, err)
	}
	return ue.Msg
}

// fakeStatus is a healthy, streaming observation of a replica.
func fakeStatus(id, ref string) peerapi.InstanceStatus {
	zero := 0.0
	return peerapi.InstanceStatus{Identifier: id, Ref: ref, Role: "replica", Step: StepDone, PostgresUp: true, PostgRESTReady: true,
		InRecovery: true, ReceiverStatus: "streaming", ReceiveLSN: "0/5000000", ReplayLSN: "0/5000000", LagSeconds: &zero}
}

func reportOf(node string, sts ...peerapi.InstanceStatus) peerapi.Report {
	return peerapi.Report{Node: node, Instances: sts}
}

func leaderSnapshot() cluster.Snapshot {
	return cluster.Snapshot{Self: registry.Node{ID: "n1"}, Leader: "n1", Epoch: 7, Role: cluster.RoleLeader}
}

func followerSnapshot() cluster.Snapshot {
	return cluster.Snapshot{Self: registry.Node{ID: "n2"}, Leader: "n1", Epoch: 7, Role: cluster.RoleFollower}
}
