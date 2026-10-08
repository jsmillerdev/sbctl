package placement

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

func testReplicaID() string { return registry.ReplicaIdentifier(testRef, "us-east-1", "abc123") }

// opsEnv is the leader (n1) with its Ops, and the agent of n2 behind a mux.
type opsEnv struct {
	leaderAgent *recordingAgent // the leader's own agent
	nodeAgent   *recordingAgent // n2's
	rpc         *muxRPC
	ops         *Ops
	backups     *fakeBackups // n2's
	own         *fakeBackups // the leader's
}

func newOpsEnv(t *testing.T) *opsEnv {
	t.Helper()
	reg := registry.NewMemory()
	mustCreate(t, reg, "second")
	if err := reg.CreateProject(context.Background(), &registry.Project{Ref: testRef, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	e := &opsEnv{leaderAgent: &recordingAgent{}, nodeAgent: &recordingAgent{}, backups: &fakeBackups{}, own: &fakeBackups{}}
	mux := mesh.NewMux()
	// n2 serves the home-only endpoints for a project homed on n2.
	nodeReg := registry.NewMemory()
	mustCreate(t, nodeReg, "second")
	if err := nodeReg.CreateProject(context.Background(), &registry.Project{Ref: testRef, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := nodeReg.SetProjectNode(context.Background(), testRef, "n2", 1); err != nil {
		t.Fatal(err)
	}
	Register(mux.Handle, HandlerDeps{Agent: e.nodeAgent, Plane: newFakeLocal(), Resolver: RegistryResolver{Reg: nodeReg}, Members: members("n2", "n1", 5), Backups: e.backups})
	e.rpc = &muxRPC{mux: mux, caller: "n1"}
	e.ops = &Ops{Self: func() string { return "n1" }, Agent: e.leaderAgent, Backups: e.own, RPC: e.rpc, Epoch: func() int64 { return 5 }}
	return e
}

func TestOpsRunLocallyOnThisNodeAndOverThePeerAPIElsewhere(t *testing.T) {
	ctx := context.Background()
	e := newOpsEnv(t)
	id := testReplicaID()

	// This node: the agent directly, no call over the wire.
	if _, err := e.ops.Ensure(ctx, "n1", peerapi.InstanceSpec{Identifier: id, Ref: testRef}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ops.Observe(ctx, "n1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ops.Do(ctx, "n1", id, peerapi.ActionStop, peerapi.InstanceAction{}); err != nil {
		t.Fatal(err)
	}
	if err := e.ops.Remove(ctx, "n1", id); err != nil {
		t.Fatal(err)
	}
	if got := e.leaderAgent.all(); got != "ensure "+id+",observe "+id+",stop "+id+",remove "+id {
		t.Fatalf("the leader's agent: %s", got)
	}
	if e.rpc.callLog() != "" {
		t.Fatalf("local operations went over the wire: %s", e.rpc.callLog())
	}
	if e.leaderAgent.spec.Epoch != 5 || e.leaderAgent.req.Epoch != 5 {
		t.Fatalf("the epoch is not filled in: %+v %+v", e.leaderAgent.spec, e.leaderAgent.req)
	}

	// Another node: the peer API, with the epoch.
	st, err := e.ops.Ensure(ctx, "n2", peerapi.InstanceSpec{Identifier: id, Ref: testRef, BackupID: "b1", NoUpstream: true})
	if err != nil || st.Identifier != id || st.Step != StepCompleted || !st.PostgRESTReady {
		t.Fatalf("Ensure = %+v, %v", st, err)
	}
	if e.nodeAgent.spec.BackupID != "b1" || !e.nodeAgent.spec.NoUpstream || e.nodeAgent.spec.Epoch != 5 {
		t.Fatalf("spec at the node: %+v", e.nodeAgent.spec)
	}
	if _, err := e.ops.Observe(ctx, "n2", id); err != nil {
		t.Fatal(err)
	}
	for _, a := range []peerapi.Action{peerapi.ActionRestart, peerapi.ActionStop, peerapi.ActionStart, peerapi.ActionPromote, peerapi.ActionDemote} {
		if _, err := e.ops.Do(ctx, "n2", id, a, peerapi.InstanceAction{WaitLSN: "0/3000100", DrainArchive: true, TimeoutSeconds: 30, Class: "small"}); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	}
	if e.nodeAgent.req.Epoch != 5 || e.nodeAgent.req.WaitLSN != "0/3000100" || !e.nodeAgent.req.DrainArchive || e.nodeAgent.req.TimeoutSeconds != 30 || e.nodeAgent.req.Class != "small" {
		t.Fatalf("action at the node: %+v", e.nodeAgent.req)
	}
	if err := e.ops.Remove(ctx, "n2", id); err != nil {
		t.Fatal(err)
	}
	if got := e.nodeAgent.all(); got != "ensure "+id+",observe "+id+",restart "+id+",stop "+id+",start "+id+",promote "+id+",demote "+id+",remove "+id {
		t.Fatalf("n2's agent: %s", got)
	}
	log := e.rpc.callLog()
	for _, want := range []string{"PUT /peer/v1/instances/" + id, "GET /peer/v1/instances/" + id, "POST /peer/v1/instances/" + id + "/promote", "DELETE /peer/v1/instances/" + id} {
		if !strings.Contains(log, want) {
			t.Errorf("calls lack %q:\n%s", want, log)
		}
	}
}

func TestInstanceEndpointsAuthorizeAndMapErrors(t *testing.T) {
	ctx := context.Background()
	e := newOpsEnv(t)
	id := testReplicaID()
	// Only the leader may ask.
	e.rpc.caller = "n3"
	if _, err := e.ops.Observe(ctx, "n2", id); !errors.Is(err, cluster.ErrNotLeader) {
		t.Fatalf("a node that is not the leader: %v", err)
	}
	e.rpc.caller = "n1"
	// An action under an older epoch is refused by the agent's own check, and a request for another
	// identifier than the path's by the handler.
	e.nodeAgent.err = lifecycle.ErrNotStandby
	if _, err := e.ops.Do(ctx, "n2", id, peerapi.ActionPromote, peerapi.InstanceAction{}); !errors.Is(err, lifecycle.ErrInvalidState) {
		t.Fatalf("promote of a non-standby: %v", err)
	}
	e.nodeAgent.err = nil
	var re *mesh.RemoteError
	err := e.rpc.Call(ctx, "n2", http.MethodPut, peerapi.InstancePath(id), peerapi.InstanceSpec{Identifier: registry.ReplicaIdentifier(testRef, "us-east-1", "zzz999"), Ref: testRef}, nil)
	if !errors.As(err, &re) || re.Status != http.StatusBadRequest {
		t.Fatalf("a body for another identifier: %v", err)
	}
	// No body fields at all is fine: the identifier comes from the path.
	if err := e.rpc.Call(ctx, "n2", http.MethodPut, peerapi.InstancePath(id), nil, nil); err != nil {
		t.Fatalf("spec from the path: %v", err)
	}
	if e.nodeAgent.spec.Identifier != id {
		t.Fatalf("spec = %+v", e.nodeAgent.spec)
	}
}

func TestBackupOperations(t *testing.T) {
	ctx := context.Background()
	e := newOpsEnv(t)
	when := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	// On the home (n2 here): the node's backup service, and the result describes the backup.
	res, err := e.ops.BaseBackup(ctx, "n2", testRef, peerapi.BackupRequest{Reason: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	want := peerapi.BackupResult{ID: "20261001T000000Z-ab12", Timeline: 2, StartLSN: "0/3000028", StopLSN: "0/3000100", SizeBytes: 1234}
	if res != want || len(e.backups.reasons) != 1 || e.backups.reasons[0] != "manual" {
		t.Fatalf("BaseBackup = %+v, reasons %v", res, e.backups.reasons)
	}
	if _, err := e.ops.Restore(ctx, "n2", testRef, peerapi.BackupRequest{BackupID: "b1", Target: &when}); err != nil {
		t.Fatal(err)
	}
	if len(e.backups.restored) != 1 || e.backups.restored[0].BackupID != "b1" || !e.backups.restored[0].Target.Equal(when) {
		t.Fatalf("restored = %+v", e.backups.restored)
	}
	// On this node: the leader's own service, no wire.
	if _, err := e.ops.BaseBackup(ctx, "n1", testRef, peerapi.BackupRequest{Reason: "pre-upgrade"}); err != nil {
		t.Fatal(err)
	}
	if len(e.own.reasons) != 1 || e.own.reasons[0] != "pre-upgrade" || len(e.backups.reasons) != 1 {
		t.Fatalf("own %v, node %v", e.own.reasons, e.backups.reasons)
	}
	// A node with no backup service says so.
	e.ops.Backups = nil
	if _, err := e.ops.Restore(ctx, "n1", testRef, peerapi.BackupRequest{}); !errors.Is(err, lifecycle.ErrNoSnapshot) {
		t.Fatalf("no service: %v", err)
	}
	// A failure on the node comes back with its text.
	e.backups.err = errors.New("archive is not reachable")
	if _, err := e.ops.BaseBackup(ctx, "n2", testRef, peerapi.BackupRequest{}); err == nil || !strings.Contains(err.Error(), "archive is not reachable") {
		t.Fatalf("remote failure: %v", err)
	}
	// An unknown operation is a bad request.
	e.backups.err = nil
	var re *mesh.RemoteError
	if err := e.rpc.Call(ctx, "n2", http.MethodPost, "/peer/v1/projects/"+testRef+"/backup/bogus", peerapi.BackupRequest{Epoch: 5}, nil); !errors.As(err, &re) || re.Status != http.StatusBadRequest {
		t.Fatalf("unknown op: %v", err)
	}
	// A project that is not homed there.
	if _, err := e.ops.BaseBackup(ctx, "n2", "zzzzzzzzzzzzzzzzzzzz", peerapi.BackupRequest{}); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
}

func TestFleetRestartsAReplicaOnItsNodeOnTheNewSize(t *testing.T) {
	ctx := context.Background()
	e := newOpsEnv(t)
	reg := registry.NewMemory()
	f := &Fleet{Registry: reg, Ops: e.ops, Epoch: func() int64 { return 5 }}
	r := registry.Replica{Identifier: testReplicaID(), Ref: testRef, NodeID: "n2"}
	if err := f.Restart(ctx, r, "large"); err != nil {
		t.Fatal(err)
	}
	if e.nodeAgent.req.Class != "large" || e.nodeAgent.req.Epoch != 5 || e.nodeAgent.all() != "restart "+testReplicaID() {
		t.Fatalf("agent: %s %+v", e.nodeAgent.all(), e.nodeAgent.req)
	}
	// A node that answers but whose replica does not come up is a failure.
	e.nodeAgent.err = errors.New("unit failed")
	if err := f.Restart(ctx, r, "large"); err == nil {
		t.Fatal("a restart that failed was reported as done")
	}
	var told []string
	f.OnFailure = func(_ context.Context, r registry.Replica, cause error) {
		told = append(told, r.Identifier+": "+cause.Error())
	}
	f.Failed(ctx, r, errors.New("x"))
	if len(told) != 1 {
		t.Fatalf("told = %v", told)
	}
	f.OnFailure = nil
	f.Failed(ctx, r, errors.New("x")) // no hook: nothing happens
	if rs, err := f.Replicas(ctx, testRef); err != nil || len(rs) != 0 {
		t.Fatalf("replicas: %v %v", rs, err)
	}
}

type fixedObserver []peerapi.InstanceStatus

func (o fixedObserver) ObserveAll(context.Context) []peerapi.InstanceStatus { return o }

func TestReporterTellsTheLeaderAndOnlyThe(t *testing.T) {
	ctx := context.Background()
	rpc := &recordRPC{}
	m := members("n2", "n1", 5)
	st := peerapi.InstanceStatus{Identifier: testReplicaID(), Ref: testRef, Role: "replica"}
	r := &Reporter{Members: m, RPC: rpc, Agent: fixedObserver{st}, Projects: func(context.Context) []peerapi.ProjectHealth {
		return []peerapi.ProjectHealth{{Ref: testRef, Healthy: true}}
	}}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rep := r.Report(ctx, now)
	if rep.Node != "n2" || rep.Epoch != 5 || !rep.At.Equal(now) || len(rep.Instances) != 1 || len(rep.Projects) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if err := r.Once(ctx, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	if len(rpc.calls) != 1 || rpc.nodes[0] != "n1" || rpc.calls[0] != "POST "+peerapi.PathReport {
		t.Fatalf("calls %v on %v", rpc.calls, rpc.nodes)
	}
	// The leader reports to nobody; a node that knows no leader has no one to tell.
	for _, mem := range []cluster.Membership{
		cluster.Solo(registry.Node{ID: "n1", Name: "primary"}),
		cluster.NewStatic(cluster.Snapshot{Self: registry.Node{ID: "n2"}, Epoch: 1, Role: cluster.RoleFollower}),
	} {
		rpc2 := &recordRPC{}
		r2 := &Reporter{Members: mem, RPC: rpc2, Agent: fixedObserver{}}
		if err := r2.Once(ctx, time.Now); err != nil || len(rpc2.calls) != 0 {
			t.Fatalf("reported to %v: %v", rpc2.calls, err)
		}
	}
	// A failure is returned (and Run logs it).
	r.RPC = failingRPC{mesh.ErrNoSession}
	if err := r.Once(ctx, time.Now); !errors.Is(err, mesh.ErrNoSession) {
		t.Fatalf("Once: %v", err)
	}
	ctx2, cancel := context.WithCancel(ctx)
	var seen int
	done := make(chan struct{})
	go func() {
		r.Run(ctx2, 5*time.Millisecond, func(error) {
			seen++
			if seen == 2 {
				cancel()
			}
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestReporterIsQuietAboutALeaderWithoutAReportEndpoint(t *testing.T) {
	ctx := context.Background()
	m := members("n2", "n1", 5)
	r := &Reporter{Members: m, RPC: failingRPC{&mesh.RemoteError{Node: "n1", Status: http.StatusNotFound, Message: "page not found"}}, Agent: fixedObserver{}}
	if err := r.Once(ctx, time.Now); !errors.Is(err, ErrNoReportEndpoint) {
		t.Fatalf("Once: %v", err)
	}
	// Run says so once, and again only after the leader answered in between.
	ctx2, cancel := context.WithCancel(ctx)
	var logged []string
	var calls int
	rpc := funcRPC(func() error {
		calls++
		switch {
		case calls <= 3:
			return &mesh.RemoteError{Node: "n1", Status: http.StatusNotFound, Message: "page not found"}
		case calls == 4:
			return nil
		case calls == 5:
			return &mesh.RemoteError{Node: "n1", Status: http.StatusNotFound, Message: "page not found"}
		case calls == 6:
			return errors.New("connection reset")
		}
		cancel()
		return nil
	})
	r.RPC = rpc
	done := make(chan struct{})
	go func() {
		r.Run(ctx2, time.Millisecond, func(err error) { logged = append(logged, err.Error()) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if len(logged) != 3 || !strings.Contains(logged[0], "no report endpoint") || !strings.Contains(logged[1], "no report endpoint") || !strings.Contains(logged[2], "connection reset") {
		t.Fatalf("logged = %q", logged)
	}
}

type funcRPC func() error

func (f funcRPC) Call(context.Context, string, string, string, any, any) error { return f() }

// countingObserver counts the observations of the node and answers with the n-th.
type countingObserver struct {
	mu sync.Mutex
	n  int
}

func (o *countingObserver) ObserveAll(context.Context) []peerapi.InstanceStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.n++
	return []peerapi.InstanceStatus{{Identifier: testReplicaID(), Ref: testRef, ReplayLSN: fmt.Sprintf("0/%d", o.n)}}
}

func (o *countingObserver) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.n
}

func TestReportCacheAnswersFromMemory(t *testing.T) {
	ctx := context.Background()
	obs := &countingObserver{}
	c := &ReportCache{Agent: obs, Projects: func(context.Context) []peerapi.ProjectHealth {
		return []peerapi.ProjectHealth{{Ref: testRef, Healthy: true}}
	}}
	// Before the first refresh there is nothing, and nothing is probed to say so.
	if in, pr := c.Contribute(ctx); len(in) != 0 || len(pr) != 0 || obs.count() != 0 {
		t.Fatalf("before a refresh: %v %v (%d probes)", in, pr, obs.count())
	}
	c.Refresh(ctx)
	for i := 0; i < 3; i++ {
		in, pr := c.Contribute(ctx)
		if len(in) != 1 || in[0].ReplayLSN != "0/1" || len(pr) != 1 || !pr[0].Healthy {
			t.Fatalf("contribution = %v %v", in, pr)
		}
	}
	// A caller that changes the answer does not change the cache.
	in := c.ObserveAll(ctx)
	in[0].ReplayLSN = "changed"
	if got := c.ObserveAll(ctx); got[0].ReplayLSN != "0/1" {
		t.Fatalf("the cache was changed through its answer: %v", got)
	}
	if obs.count() != 1 {
		t.Fatalf("reading the cache probed the node: %d", obs.count())
	}
	// The reporter reads it as an Observer and a Projects function.
	r := &Reporter{Members: members("n2", "n1", 5), RPC: &recordRPC{}, Agent: c, Projects: c.ProjectHealth}
	if rep := r.Report(ctx, time.Now()); len(rep.Instances) != 1 || len(rep.Projects) != 1 || obs.count() != 1 {
		t.Fatalf("report = %+v (%d probes)", rep, obs.count())
	}

	// Run refreshes at once and on every tick, and stops with its context.
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { c.Run(rctx, 5*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for obs.count() < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("Run refreshed %d times", obs.count())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	// Without a projects function the report has none.
	c2 := &ReportCache{Agent: fixedObserver{}}
	c2.Refresh(ctx)
	if _, pr := c2.Contribute(ctx); len(pr) != 0 {
		t.Fatalf("projects = %v", pr)
	}
}
