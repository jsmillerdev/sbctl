package replicas

import (
	"testing"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/registry"
)

func defaultAll(o *Options) { o.Config.Replicas.Default = "all" }

// default = "all": every project that can be copied gets a replica on every active node other
// than its home, with origin "default"; nothing happens with the default off.
func TestDefaultAllMakesAReplicaOnEveryOtherNode(t *testing.T) {
	e := newEnv(t)
	e.tick(2)
	if rs, _ := e.reg.ListReplicas(e.ctx, ""); len(rs) != 0 {
		t.Fatalf("the default is off, yet: %+v", rs)
	}
	e.cfg.Replicas.Default = "all"
	e.tick(1)
	for _, ref := range []string{refA, refB} {
		for _, node := range []string{"n2", "n3"} {
			r := e.replica(ref, node)
			if r.Origin != registry.ReplicaDefault {
				t.Fatalf("%s on %s: origin %q", ref, node, r.Origin)
			}
		}
		if e.hasReplica(ref, "n1") {
			t.Fatalf("a replica on the home of %s", ref)
		}
	}
	// It is level triggered: a second pass makes nothing more.
	e.tick(1)
	if rs, _ := e.reg.ListReplicas(e.ctx, ""); len(rs) != 4 {
		t.Fatalf("%d rows", len(rs))
	}
}

// The default skips the system project, branches, projects that do not run, and nodes that are
// not active; it bypasses the size and replica-count limits of a manual request.
func TestDefaultAllSkips(t *testing.T) {
	e := newEnv(t, defaultAll)
	e.addProject(refC, "nano") // too small for a manual request, fine for the default
	paused := "dddddddddddddddddddd"
	e.addProject(paused, "small")
	e.setProject(paused, registry.StatusInactive)
	coming := "eeeeeeeeeeeeeeeeeeee"
	e.addProject(coming, "small")
	e.setProject(coming, registry.StatusComingUp)
	branch := "ffffffffffffffffffff"
	if err := e.reg.CreateProject(e.ctx, &registry.Project{Ref: branch, Name: "b", Class: "small", Status: registry.StatusActiveHealthy,
		Branch: &registry.BranchInfo{ID: "b1", ParentRef: refA, Name: "feature"}}); err != nil {
		t.Fatal(err)
	}
	e.addNode("n4", "joining", "eu-west-2")
	if err := e.reg.SetNodeState(e.ctx, "n4", registry.NodeJoining); err != nil {
		t.Fatal(err)
	}
	e.tick(1)
	for _, ref := range []string{paused, coming, branch, "system"} {
		if rs, _ := e.reg.ListReplicas(e.ctx, ref); len(rs) != 0 {
			t.Errorf("%s got replicas: %+v", ref, rs)
		}
	}
	if !e.hasReplica(refC, "n2") || !e.hasReplica(refC, "n3") {
		t.Fatal("the nano project must get replicas by default")
	}
	if rs, _ := e.reg.ListReplicas(e.ctx, ""); len(rs) != 6 {
		t.Fatalf("%d rows, want 3 projects x 2 nodes: %+v", len(rs), rs)
	}
	// The paused project gets its replicas when it runs again.
	e.setProject(paused, registry.StatusActiveHealthy)
	e.tick(1)
	if !e.hasReplica(paused, "n2") {
		t.Fatal("no replica after the project resumed")
	}
}

// A replica the operator removed, and an opt-out someone wrote, keep the default away; a new
// node gets its replicas; asking for one by hand lifts the opt-out.
func TestDefaultAllOptOuts(t *testing.T) {
	e := newEnv(t, defaultAll)
	e.tick(1)
	id := e.replica(refA, "n2").Identifier
	if err := e.ctrl.Remove(e.ctx, refA, id); err != nil {
		t.Fatal(err)
	}
	if os, _ := e.reg.ListReplicaOptouts(e.ctx); len(os) != 1 || os[0] != (registry.ReplicaOptout{Ref: refA, NodeID: "n2"}) {
		t.Fatalf("opt-outs: %+v", os)
	}
	e.tick(2) // removed for good, and not made again
	if e.hasReplica(refA, "n2") {
		t.Fatal("the default made the removed replica again")
	}
	if !e.hasReplica(refA, "n3") || !e.hasReplica(refB, "n2") {
		t.Fatal("the opt-out reached beyond its project and node")
	}
	// A node that joins later gets replicas of every project.
	e.addNode("n4", "late", "eu-west-2")
	e.tick(1)
	if !e.hasReplica(refA, "n4") || !e.hasReplica(refB, "n4") {
		t.Fatal("no replicas on the new node")
	}
	// Asking for the removed one by hand clears the opt-out.
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	if os, _ := e.reg.ListReplicaOptouts(e.ctx); len(os) != 0 {
		t.Fatalf("opt-outs: %+v", os)
	}
	if r := e.replica(refA, "n2"); r.Origin != registry.ReplicaManual {
		t.Fatalf("origin %s", r.Origin)
	}
}

// A file:// backend or a replica range that does not fit stops the default from creating anything.
func TestDefaultAllNeedsSharedBackupsAndPorts(t *testing.T) {
	e := newEnv(t, defaultAll)
	e.cfg.Backup.Backend = "file:///var/lib/supavise/backups"
	e.tick(1)
	if rs, _ := e.reg.ListReplicas(e.ctx, ""); len(rs) != 0 {
		t.Fatalf("rows with a file backend: %+v", rs)
	}
	e.cfg.Backup.Backend = "s3://bucket/prefix"
	e.cfg.Ports.ReplicaBase = 80
	e.tick(1)
	if rs, _ := e.reg.ListReplicas(e.ctx, ""); len(rs) != 0 {
		t.Fatalf("rows with a bad replica range: %+v", rs)
	}
}

// Capacity shortage with the default: the rows exist, wait at 0_requested, and the alert names the node.
func TestDefaultAllWaitsForCapacity(t *testing.T) {
	e := newEnv(t, defaultAll)
	e.admit.full["n2"] = true
	e.tick(3)
	for _, ref := range []string{refA, refB} {
		if r := e.replica(ref, "n2"); r.InitStep != StepRequested || r.Status != registry.ReplicaInit {
			t.Fatalf("%s on n2: %+v", ref, r)
		}
		e.settle(ref, "n3")
	}
	if e.alerts.count(alerts.KindReplicaCapacity, false) != 1 {
		t.Fatalf("alerts: %+v", e.alerts.evs)
	}
	e.admit.full["n2"] = false
	e.settle(refA, "n2")
	e.settle(refB, "n2")
}

// With default = "all" the setup runs two at a time, oldest first, across all the rows.
func TestDefaultAllRespectsTheConcurrencyLimit(t *testing.T) {
	e := newEnv(t, defaultAll)
	e.tick(1)
	inFlight := func() int {
		n := 0
		rs, _ := e.reg.ListReplicas(e.ctx, "")
		for i := range rs {
			if settingUp(&rs[i]) && rs[i].InitStep != StepRequested {
				n++
			}
		}
		return n
	}
	if got := inFlight(); got != 2 {
		t.Fatalf("%d setups in flight, want the limit of 2", got)
	}
	for range 40 {
		if got := inFlight(); got > 2 {
			t.Fatalf("%d setups in flight", got)
		}
		e.tick(1)
	}
	rs, _ := e.reg.ListReplicas(e.ctx, "")
	for _, r := range rs {
		if r.Status != statusHealthy {
			t.Fatalf("not all rows finished: %+v", r)
		}
	}
}

// A follower leaves the replicas alone.
func TestOnlyTheLeaderDrivesReplicas(t *testing.T) {
	e := newEnv(t, defaultAll)
	e.members.Set(followerSnapshot())
	e.tick(2)
	if rs, _ := e.reg.ListReplicas(e.ctx, ""); len(rs) != 0 {
		t.Fatalf("a follower created rows: %+v", rs)
	}
	e.members.Set(leaderSnapshot())
	e.tick(1)
	if rs, _ := e.reg.ListReplicas(e.ctx, ""); len(rs) == 0 {
		t.Fatal("the leader made no rows")
	}
}
