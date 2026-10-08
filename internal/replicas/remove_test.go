package replicas

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/registry"
)

// Remove marks the replica GOING_DOWN at once; the controller then drops the tenant, removes the
// instance on its node and deletes the row.
func TestRemove(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	if err := e.ctrl.Remove(e.ctx, refA, r.Identifier); err != nil {
		t.Fatal(err)
	}
	got := e.replica(refA, "n2")
	if got.Status != statusGoingDown {
		t.Fatalf("status %s", got.Status)
	}
	if st, _ := e.ctrl.Statuses(e.ctx, refA); st[0].Status != statusGoingDown {
		t.Fatalf("statuses: %+v", st)
	}
	// Removing twice is fine.
	if err := e.ctrl.Remove(e.ctx, refA, r.Identifier); err != nil {
		t.Fatal(err)
	}
	e.tick(1)
	if e.hasReplica(refA, "n2") {
		t.Fatal("the row is still there")
	}
	if e.nodes.get("n2", r.Identifier) != nil {
		t.Fatal("the instance is still on the node")
	}
	if len(e.pool.removed) != 1 || e.pool.removed[0] != r.Identifier {
		t.Fatalf("removed tenants: %v", e.pool.removed)
	}
	if os, _ := e.reg.ListReplicaOptouts(e.ctx); len(os) != 0 {
		t.Fatalf("a manual replica left an opt-out: %+v", os)
	}
	// Nothing more to do: no further node calls.
	calls := len(e.nodes.calls)
	e.tick(2)
	if len(e.nodes.calls) != calls {
		t.Fatalf("calls after the removal: %v", e.nodes.calls[calls:])
	}
}

// An identifier that is not a replica of the project is not found, whoever's replica it is.
func TestRemoveUnknownIdentifier(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	for _, tt := range []struct{ ref, id string }{
		{refA, "aaaaaaaaaaaaaaaaaaaa-rr-eu-west-1-zzzzzz"},
		{refB, r.Identifier}, // a replica of another project
		{refA, "nonsense"},
	} {
		if err := e.ctrl.Remove(e.ctx, tt.ref, tt.id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Remove(%s, %s) = %v", tt.ref, tt.id, err)
		}
		if err := e.ctrl.Restart(e.ctx, tt.ref, tt.id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Restart(%s, %s) = %v", tt.ref, tt.id, err)
		}
	}
	if got := e.replica(refA, "n2").Status; got != statusHealthy {
		t.Fatalf("status %s", got)
	}
}

// A replica that is still setting up can be dropped, and the setup worker does not write over GOING_DOWN.
func TestRemoveDuringSetup(t *testing.T) {
	e := newEnv(t)
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	e.tick(2)
	r := e.replica(refA, "n2")
	if err := e.ctrl.Remove(e.ctx, refA, r.Identifier); err != nil {
		t.Fatal(err)
	}
	// A write of the setup after the removal began loses.
	if e.ctrl.setStatus(e.ctx, r.Identifier, registry.ReplicaInit, StepDownloaded, "") {
		t.Fatal("the setup wrote over GOING_DOWN")
	}
	e.tick(1)
	if e.hasReplica(refA, "n2") || e.nodes.get("n2", r.Identifier) != nil {
		t.Fatal("not removed")
	}
}

// The node does not answer: the row stays GOING_DOWN, the controller tries again with a pause, and
// removes it when the node is back. After a long time the operator hears of it.
func TestRemoveRetriesWhenTheNodeIsUnreachable(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	e.nodes.down["n2"] = true
	if err := e.ctrl.Remove(e.ctx, refA, r.Identifier); err != nil {
		t.Fatal(err)
	}
	e.tick(1)
	if got := e.replica(refA, "n2").Status; got != statusGoingDown {
		t.Fatalf("status %s", got)
	}
	n := e.nodes.callsMatching("remove n2")
	e.tick(1) // inside the pause
	if got := e.nodes.callsMatching("remove n2"); got != n {
		t.Fatalf("tried again at once: %d -> %d", n, got)
	}
	e.clock.Advance(time.Minute)
	e.tick(1)
	if got := e.nodes.callsMatching("remove n2"); got != n+1 {
		t.Fatalf("no retry after the pause: %d -> %d", n, got)
	}
	for range 10 {
		e.clock.Advance(2 * time.Minute)
		e.tick(1)
	}
	if e.alerts.count(alerts.KindReplicaUnhealthy, false) != 1 {
		t.Fatalf("alerts: %+v", e.alerts.evs)
	}
	e.nodes.down["n2"] = false
	e.clock.Advance(3 * time.Minute)
	e.tick(1)
	if e.hasReplica(refA, "n2") {
		t.Fatal("not removed after the node came back")
	}
	if e.alerts.count(alerts.KindReplicaUnhealthy, true) != 1 {
		t.Fatalf("the alert did not resolve: %+v", e.alerts.evs)
	}
}

// A node that left the cluster has nothing to reach: its replicas' rows just go.
func TestRemoveOnALeftNodeDeletesTheRow(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	e.nodes.down["n2"] = true
	if err := e.reg.SetNodeState(e.ctx, "n2", registry.NodeLeft); err != nil {
		t.Fatal(err)
	}
	if err := e.ctrl.Remove(e.ctx, refA, r.Identifier); err != nil {
		t.Fatal(err)
	}
	e.tick(1)
	if e.hasReplica(refA, "n2") {
		t.Fatal("the row of a replica on a node that left is still there")
	}
}

// The system standby goes with its node.
func TestRemoveRefusesTheSystemStandby(t *testing.T) {
	e := newEnv(t)
	r := e.systemReplica("n2")
	if got := e.user(e.ctrl.Remove(e.ctx, "system", r.Identifier)); got != "The standby of the system cluster goes away with its server: use `supavise node rm`." {
		t.Fatalf("refused with %q", got)
	}
}

// RemoveAll (a project delete, a restore) and RemoveOn (node rm) leave no opt-out, report the
// removals that could not finish, and reach the system standby of a node.
func TestRemoveAllAndRemoveOn(t *testing.T) {
	e := newEnv(t, defaultAll)
	e.settle(refA, "n2")
	e.settle(refA, "n3")
	if err := e.ctrl.RemoveAll(e.ctx, refA); err != nil {
		t.Fatal(err)
	}
	if rs, _ := e.reg.ListReplicas(e.ctx, refA); len(rs) != 0 {
		t.Fatalf("rows left: %+v", rs)
	}
	if os, _ := e.reg.ListReplicaOptouts(e.ctx); len(os) != 0 {
		t.Fatalf("opt-outs: %+v", os)
	}
	// The default makes them again, as the restore that removed them wants.
	e.tick(1)
	if !e.hasReplica(refA, "n2") {
		t.Fatal("the default did not come back")
	}

	// One node does not answer: RemoveAll removes what it can and reports the rest.
	e.cfg.Replicas.Default = "off"
	e.settle(refA, "n2")
	e.settle(refA, "n3")
	e.nodes.down["n3"] = true
	err := e.ctrl.RemoveAll(e.ctx, refA)
	var pe *PendingError
	if !errors.As(err, &pe) || len(pe.Identifiers) != 1 || pe.Identifiers[0] != e.replica(refA, "n3").Identifier {
		t.Fatalf("RemoveAll = %v", err)
	}
	if e.hasReplica(refA, "n2") || e.replica(refA, "n3").Status != statusGoingDown {
		t.Fatal("the reachable replica must go and the other must wait")
	}
	e.nodes.down["n3"] = false
	e.clock.Advance(time.Minute)
	e.tick(1)
	if e.hasReplica(refA, "n3") {
		t.Fatal("the controller did not finish the removal")
	}

	// RemoveOn: node rm. The node's system standby goes too.
	sys := e.systemReplica("n2")
	e.settle(refB, "n2")
	if err := e.ctrl.RemoveOn(e.ctx, "n2"); err != nil {
		t.Fatal(err)
	}
	if rs, _ := e.reg.ListReplicasOn(e.ctx, "n2"); len(rs) != 0 {
		t.Fatalf("rows left on n2 (system %s): %+v", sys.Identifier, rs)
	}
}

// Restart: RESTARTING at once, the node restarts the units in the background, and the replica is
// healthy again on the next observation.
func TestRestart(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	if err := e.ctrl.Restart(e.ctx, refA, r.Identifier); err != nil {
		t.Fatal(err)
	}
	if got := e.replica(refA, "n2").Status; got != statusRestart {
		t.Fatalf("status %s", got)
	}
	e.ctrl.wg.Wait() // the restart call to the node
	if got := e.nodes.get("n2", r.Identifier).restarts; got != 1 {
		t.Fatalf("restarts %d", got)
	}
	e.clock.Advance(15 * time.Second)
	e.tick(1)
	if got := e.replica(refA, "n2").Status; got != statusHealthy {
		t.Fatalf("status after the restart: %s", got)
	}
}

// A restart that fails leaves the replica unhealthy and tells the operator.
func TestRestartFailure(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	e.nodes.down["n2"] = true
	if err := e.ctrl.Restart(e.ctx, refA, r.Identifier); err != nil {
		t.Fatal(err)
	}
	e.ctrl.wg.Wait()
	if got := e.replica(refA, "n2").Status; got != statusUnhealthy {
		t.Fatalf("status %s", got)
	}
	if e.alerts.count(alerts.KindReplicaUnhealthy, false) != 1 {
		t.Fatalf("alerts: %+v", e.alerts.evs)
	}
}

// Only a replica that finished its setup restarts.
func TestRestartRefusesAReplicaThatIsNotUp(t *testing.T) {
	e := newEnv(t)
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	id := e.replica(refA, "n2").Identifier
	if got := e.user(e.ctrl.Restart(e.ctx, refA, id)); got != "The read replica "+id+" cannot restart now: it is INIT_READ_REPLICA." {
		t.Fatalf("refused with %q", got)
	}
}

// Restart without the node operations (a controller built for the CLI) says so.
func TestRestartNeedsTheNodeOperations(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	cli := New(Options{Registry: e.reg, Config: e.cfg})
	if err := cli.Restart(context.Background(), refA, r.Identifier); !errors.Is(err, errNoOps) {
		t.Fatalf("Restart = %v", err)
	}
}

// A replica a worker is acting on is not removed under it: RemoveAll leaves it GOING_DOWN, reports
// it, and the controller removes it when the worker is done.
func TestRemoveAllWaitsForABusyWorker(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Timeouts.Busy = 50 * time.Millisecond })
	r := e.activeReplica()
	e.ctrl.mu.Lock()
	e.ctrl.busy[r.Identifier] = true
	e.ctrl.mu.Unlock()
	calls := e.nodes.callsMatching("remove")
	var pe *PendingError
	if err := e.ctrl.RemoveAll(e.ctx, refA); !errors.As(err, &pe) || len(pe.Identifiers) != 1 {
		t.Fatalf("RemoveAll = %v", err)
	}
	if e.nodes.callsMatching("remove") != calls || e.replica(refA, "n2").Status != statusGoingDown {
		t.Fatal("removed under a busy worker")
	}
	e.ctrl.mu.Lock()
	delete(e.ctrl.busy, r.Identifier)
	e.ctrl.mu.Unlock()
	e.tick(1)
	if e.hasReplica(refA, "n2") {
		t.Fatal("not removed after the worker was done")
	}
}

// A worker that finishes within the wait does not make the removal pending: the caller can delete
// the project when RemoveAll returns nil.
func TestRemoveAllWaitsUntilTheWorkerIsDone(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Timeouts.Busy = 5 * time.Second })
	r := e.activeReplica()
	e.ctrl.mu.Lock()
	e.ctrl.busy[r.Identifier] = true
	e.ctrl.mu.Unlock()
	go func() {
		time.Sleep(100 * time.Millisecond)
		e.ctrl.mu.Lock()
		delete(e.ctrl.busy, r.Identifier)
		e.ctrl.mu.Unlock()
	}()
	if err := e.ctrl.RemoveAll(e.ctx, refA); err != nil {
		t.Fatalf("RemoveAll = %v", err)
	}
	if e.hasReplica(refA, "n2") || e.nodes.get("n2", r.Identifier) != nil {
		t.Fatal("not removed")
	}
}
