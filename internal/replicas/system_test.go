package replicas

import (
	"testing"

	"github.com/supavise/supavise/internal/registry"
)

// The controller never launches the system standby (the joining node seeds it before its daemon
// runs); it completes the row once the node is active and reports the standby streaming, and then
// watches it like any other.
func TestSystemStandbyIsWatchedNotLaunched(t *testing.T) {
	e := newEnv(t)
	if err := e.reg.SetNodeState(e.ctx, "n2", registry.NodeJoining); err != nil {
		t.Fatal(err)
	}
	r := e.systemReplica("n2")
	e.tick(2)
	if got := e.nodes.callsMatching(""); got != 0 {
		t.Fatalf("the controller called a joining node: %v", e.nodes.calls)
	}
	// The standby streams on the node; the node is confirmed.
	e.nodes.mu.Lock()
	e.nodes.inst["n2"] = map[string]*fakeInstance{r.Identifier: {step: StepDone, receiver: "streaming"}}
	e.nodes.inst["n2"][r.Identifier].spec.Identifier, e.nodes.inst["n2"][r.Identifier].spec.Ref = r.Identifier, "system"
	e.nodes.mu.Unlock()
	if err := e.reg.SetNodeState(e.ctx, "n2", registry.NodeActive); err != nil {
		t.Fatal(err)
	}
	e.tick(1)
	got := e.replica("system", "n2")
	if got.Status != statusHealthy || got.InitStep != StepDone {
		t.Fatalf("row: %+v", got)
	}
	if n := e.nodes.callsMatching("ensure"); n != 0 {
		t.Fatalf("the controller created the system standby: %v", e.nodes.calls)
	}
	// Its receiver stops: unhealthy after two minutes, like any replica.
	e.nodes.mu.Lock()
	e.nodes.inst["n2"][r.Identifier].receiver = ""
	e.nodes.mu.Unlock()
	e.tick(1)
	if got := e.replica("system", "n2").Status; got != statusHealthy {
		t.Fatalf("status %s", got)
	}
	e.clock.Advance(121 * 1e9)
	e.tick(1)
	if got := e.replica("system", "n2").Status; got != statusUnhealthy {
		t.Fatalf("status %s", got)
	}
}

// The default never copies the system project.
func TestSystemStandbyIsHiddenFromTheProjectListings(t *testing.T) {
	e := newEnv(t)
	e.systemReplica("n2")
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	l, _ := e.ctrl.List(e.ctx, refA)
	if len(l) != 1 || l[0].Ref != refA {
		t.Fatalf("list of a project: %+v", l)
	}
	all, _ := e.ctrl.List(e.ctx, "")
	if len(all) != 2 {
		t.Fatalf("list of everything: %+v", all)
	}
}
