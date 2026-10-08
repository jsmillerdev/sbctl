package failover

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// serverWorld is the standard cluster seen from n2, the follower that takes over.
func serverWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.setSelf("n2", false)
	return w
}

func stateFileExists(w *world) bool {
	_, err := os.Stat(w.cfg.Paths().FailoverState())
	return err == nil
}

func serverMove(t *testing.T, w *world) *registry.Move {
	t.Helper()
	ms, err := w.reg.ListMoves(w.ctx, "", 10)
	must(t, err)
	for _, m := range ms {
		if m.Scope == registry.MoveServer {
			return &m
		}
	}
	return nil
}

func clusterOf(t *testing.T, w *world) *registry.Cluster {
	t.Helper()
	cl, err := w.reg.GetCluster(w.ctx)
	must(t, err)
	return cl
}

func nodeState(t *testing.T, w *world, id string) registry.NodeState {
	t.Helper()
	n, err := w.reg.GetNode(w.ctx, id)
	must(t, err)
	return n.State
}

func TestServerSwitchover(t *testing.T) {
	w := serverWorld(t)
	o := w.orch()
	var inFile, fileMode string
	w.afterEvent("promote n2/"+idSysN2, func() {
		if b, err := os.ReadFile(w.cfg.Paths().FailoverState()); err == nil {
			inFile = string(b)
		}
		if fi, err := os.Stat(w.cfg.Paths().FailoverState()); err == nil {
			fileMode = fi.Mode().Perm().String()
		}
	})
	mv, err := o.FailoverServer(w.ctx, ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mv.State != registry.MoveDone || mv.Kind != registry.MoveSwitchover || mv.Scope != registry.MoveServer || mv.FromNode != "n1" || mv.ToNode != "n2" || mv.Epoch != 2 {
		t.Fatalf("move: %+v", mv)
	}

	// Until the system cluster is promoted the log is failover.json (0600), with the steps so far.
	if !strings.Contains(inFile, `"quiesce"`) || !strings.Contains(inFile, `"marker"`) || fileMode != "-rw-------" {
		t.Fatalf("failover.json at the promotion (%s):\n%s", fileMode, inFile)
	}
	if stateFileExists(w) {
		t.Fatal("failover.json outlives the promotion")
	}
	// Afterwards it is the moves row, with every step the file had.
	logged := serverMove(t, w)
	if logged == nil || logged.State != registry.MoveDone || !hasStep(logged, "begin") || !hasStep(logged, "quiesce") || !hasStep(logged, "dns") {
		t.Fatalf("logged move: %+v", logged)
	}

	// The order that keeps one writer.
	lsys := w.lsn[config.SystemRef]
	w.assertOrder(
		"quiesce n1 to=n2 epoch=2",
		"marker epoch=2 leader=n2",
		"provider.takeover n2",
		"promote n2/"+idSysN2+" epoch=2 wait="+lsys,
		"become-leader epoch=2",
		"registry.CreateMove",
		"registry.SetLeader n2 2",
		"registry.SetProjectNode system n2 2",
		"promote n2/"+idAN2+" epoch=2 wait="+w.lsn[refA],
		"registry.SetProjectNode "+refA+" n2 2",
		"start n2/"+refA,
		"fleet.ensure "+refA,
		"demote n1/",
		"basebackup n2/system",
	)
	w.assertNever("provider.fence") // a switchover stops the old leader cleanly instead
	w.assertNever("fence ")
	// No registry write happened while the node was a standby (the gate would have refused it),
	// and the first one is the move being created.
	if i, j := w.index("registry."), w.index("promote n2/"+idSysN2); i < j {
		t.Fatalf("the registry was written at event %d, before the promotion at %d:\n%v", i, j, w.snapshot())
	}

	cl := clusterOf(t, w)
	if cl.Leader != "n2" || cl.Epoch != 2 || cl.Maintenance.Active(w.deps().Now()) {
		t.Fatalf("cluster: %+v", cl)
	}
	if nodeState(t, w, "n1") != registry.NodeActive {
		t.Fatalf("a switchover leaves the old leader active, not %s", nodeState(t, w, "n1"))
	}
	for _, ref := range []string{config.SystemRef, refA, refB} {
		p := projectOf(t, w, ref)
		if p.NodeID != "n2" || p.Status != registry.StatusActiveHealthy {
			t.Fatalf("project %s: %+v", ref, p)
		}
		old := replicaOn(t, w, ref, "n1")
		if old == nil || !w.has("demote n1/"+old.Identifier) {
			t.Fatalf("project %s: the old leader has %+v and was not demoted into it:\n%v", ref, old, w.snapshot())
		}
		if replicaOn(t, w, ref, "n2") != nil {
			t.Fatalf("project %s keeps a replica row on its own home", ref)
		}
	}
	if r := replicaOn(t, w, config.SystemRef, "n1"); r.Origin != registry.ReplicaSystem {
		t.Fatalf("the old leader's system replica: %+v", r)
	}
	if r := replicaOn(t, w, refB, "n1"); r.Origin != registry.ReplicaDefault {
		t.Fatalf("the origin of a default replica is kept: %+v", r)
	}
	if !strings.Contains(w.stepDetail(logged, "dns"), "DNS needs no change") {
		t.Fatalf("dns step: %q", w.stepDetail(logged, "dns"))
	}
	if k := w.alertKinds(); strings.Join(k, ",") != alerts.KindFailoverStarted+","+alerts.KindFailoverCompleted {
		t.Fatalf("alerts %v", k)
	}
}

func (w *world) stepDetail(mv *registry.Move, name string) string {
	for _, s := range mv.Steps {
		if s.Name == name {
			return s.Detail
		}
	}
	return ""
}

func TestServerFailover(t *testing.T) {
	w := serverWorld(t)
	w.down["n1"] = true
	o := w.orch()
	mv, err := o.FailoverServer(w.ctx, ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mv.Kind != registry.MoveFailover || mv.State != registry.MoveDone || mv.Epoch != 2 {
		t.Fatalf("move: %+v", mv)
	}
	// Fence first (the cooperative try, then the provider's), the address, the marker, and only then the promotion.
	w.assertOrder(
		"fence n1 unreachable",
		"provider.fence n1 planned=false epoch=2",
		"marker epoch=2 leader=n2",
		"provider.takeover n2",
		"promote n2/"+idSysN2+" epoch=2 wait= drain=true",
		"registry.CreateMove",
		"registry.SetLeader n2 2",
		"registry.SetNodeState n1 fenced",
		"promote n2/"+idAN2+" epoch=2 wait= drain=true",
		"registry.SetProjectNode "+refA+" n2 2",
		"start n2/"+refA,
		"fleet.ensure "+refA,
		"basebackup n2/"+refA,
	)
	w.assertNever("quiesce")
	w.assertNever("demote") // the old leader is gone: it is rebuilt when it returns
	if nodeState(t, w, "n1") != registry.NodeFenced {
		t.Fatalf("n1 is %s", nodeState(t, w, "n1"))
	}
	for _, ref := range []string{refA, refB} {
		if replicaOn(t, w, ref, "n1") != nil {
			t.Fatalf("a replica row for the dead node on %s", ref)
		}
	}
	if cl := clusterOf(t, w); cl.Leader != "n2" || cl.Epoch != 2 {
		t.Fatalf("cluster: %+v", cl)
	}
}

func TestFailoverIsNotPromotedWhenTheFenceFails(t *testing.T) {
	w := serverWorld(t)
	w.down["n1"] = true
	w.fail("provider.fence", errors.New("StopInstances: throttled"), -1)
	o := w.orch()
	mv, err := o.FailoverServer(w.ctx, ServerOptions{})
	if !errors.Is(err, ErrFence) {
		t.Fatalf("error: %v", err)
	}
	if mv.State != registry.MoveFailed {
		t.Fatalf("move: %+v", mv)
	}
	for _, e := range []string{"provider.takeover", "marker", "promote", "registry."} {
		w.assertNever(e)
	}
	if !stateFileExists(w) {
		t.Fatal("the state is lost: failover.json is gone and nothing is in the registry")
	}
	// The problem is fixed; the same move continues and the fence is tried again.
	w.clearFailures()
	mv, err = o.FailoverServer(w.ctx, ServerOptions{Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if mv.State != registry.MoveDone || w.count("provider.fence") != 2 || w.count("promote n2/"+idSysN2) != 1 {
		t.Fatalf("resumed move: %+v\n%v", mv, w.snapshot())
	}
	if stateFileExists(w) {
		t.Fatal("failover.json is left behind")
	}
}

func TestFailoverWithoutAFencingMethodNeedsTheOperatorsWord(t *testing.T) {
	w := serverWorld(t)
	w.down["n1"] = true
	o := w.orch(func(d *Deps) { d.Provider = Manual{} })
	_, err := o.FailoverServer(w.ctx, ServerOptions{})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "--old-primary-is-down") {
		t.Fatalf("error: %v", err)
	}
	// --force does not stand in for the assertion.
	if _, err := o.FailoverServer(w.ctx, ServerOptions{Force: true}); !errors.Is(err, ErrRefused) {
		t.Fatalf("with force: %v", err)
	}
	w.assertNever("promote")
	mv, err := o.FailoverServer(w.ctx, ServerOptions{OldPrimaryIsDown: true})
	if err != nil {
		t.Fatal(err)
	}
	logged := serverMove(t, w)
	if !strings.Contains(w.stepDetail(logged, "fence"), "asserted down by the operator") {
		t.Fatalf("fence step: %q", w.stepDetail(logged, "fence"))
	}
	if !strings.Contains(w.stepDetail(logged, "address"), "not moved") || !strings.Contains(w.stepDetail(logged, "dns"), "Point DNS at") {
		t.Fatalf("address %q, dns %q", w.stepDetail(logged, "address"), w.stepDetail(logged, "dns"))
	}
	if mv.State != registry.MoveDone {
		t.Fatalf("move: %+v", mv)
	}
}

func TestAnAssertionIsRefusedWhileTheLeaderAnswersAndNothingFences(t *testing.T) {
	w := serverWorld(t)
	o := w.orch(func(d *Deps) { d.Provider = Manual{} })
	_, err := o.FailoverServer(w.ctx, ServerOptions{OldPrimaryIsDown: true})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "still answers") {
		t.Fatalf("error: %v", err)
	}
}

func TestEpochRaceStopsBeforeThePromotion(t *testing.T) {
	w := serverWorld(t)
	w.down["n1"] = true
	// Between the preflight and the marker another node is promoted and writes its marker.
	w.afterEvent("provider.fence", func() {
		w.mu.Lock()
		w.marker = &backup.LeaderMarker{Epoch: 9, Leader: "n3"}
		w.mu.Unlock()
	})
	o := w.orch()
	mv, err := o.FailoverServer(w.ctx, ServerOptions{})
	if !errors.Is(err, ErrEpochLost) {
		t.Fatalf("error: %v", err)
	}
	if mv.State != registry.MoveAborted {
		t.Fatalf("move: %+v", mv)
	}
	w.assertNever("promote")
	w.assertNever("registry.")
	// The loser has not touched the service address, which the winner moves.
	w.assertNever("provider.takeover")
	if stateFileExists(w) {
		t.Fatal("an aborted move leaves nothing to resume")
	}
	// The marker the other node wrote is untouched.
	if w.marker.Epoch != 9 {
		t.Fatalf("marker: %+v", w.marker)
	}
}

// Two survivors that act at once ask for the same epoch: the store takes one, and the other stops
// before it moves the address, whether the store refuses the write or only shows the other's marker.
func TestTwoSurvivorsOnTheSameEpochOnlyOneGoesOn(t *testing.T) {
	w := serverWorld(t)
	w.down["n1"] = true
	w.afterEvent("provider.fence", func() {
		w.mu.Lock()
		w.marker = &backup.LeaderMarker{Epoch: 2, Leader: "n3"} // the other survivor got there first, at the same epoch
		w.mu.Unlock()
	})
	mv, err := w.orch().FailoverServer(w.ctx, ServerOptions{})
	if !errors.Is(err, ErrEpochLost) || mv.State != registry.MoveAborted {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	w.assertNever("provider.takeover")
	w.assertNever("promote")
}

func TestAMarkerAlreadyAtTheEpochRefusesThePlan(t *testing.T) {
	w := serverWorld(t)
	w.marker = &backup.LeaderMarker{Epoch: 2, Leader: "n3"}
	pl, err := w.orch().PlanServer(w.ctx, ServerOptions{})
	must(t, err)
	c := findCheck(t, pl, "epoch marker")
	if c.OK || !c.Hard || !strings.Contains(c.Detail, "another node was promoted") {
		t.Fatalf("check: %+v", c)
	}
}

// The promoted node's daemon restarts and finds its role in the cluster row, from its peers or in the
// marker. A marker that cannot be written leaves a survivor that starts fenced, so --force does not
// skip it, unless the Takeover records the leadership in the promoted registry itself.
func TestMarkerStoreDownRefusesTheMoveAndForceDoesNotChangeIt(t *testing.T) {
	w := serverWorld(t)
	w.markErr = errors.New("connection refused")
	o := w.orch()
	if _, err := o.FailoverServer(w.ctx, ServerOptions{}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("error: %v", err)
	}
	if _, err := o.FailoverServer(w.ctx, ServerOptions{Force: true}); !errors.Is(err, ErrRefused) {
		t.Fatalf("with force: %v", err)
	}
	w.assertNever("promote")
	// A marker that fails to be written after the plan (the store went down in between) stops the move before the promotion.
	w.markErr = nil
	w.fail("marker epoch=", errors.New("connection reset"), -1)
	mv, err := o.FailoverServer(w.ctx, ServerOptions{Force: true})
	if err == nil || !strings.Contains(err.Error(), "starts fenced") || mv.State != registry.MoveFailed {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	w.assertNever("promote")
	w.assertNever("provider.takeover")
}

// recordingTakeover is a Takeover that writes the cluster row of the promoted registry itself.
type recordingTakeover struct{ Takeover }

func (recordingTakeover) RecordsLeadership() bool { return true }

func TestMarkerStoreDownWithATakeoverThatRecordsTheLeadershipGoesOnWhenForced(t *testing.T) {
	w := serverWorld(t)
	w.markErr = errors.New("connection refused")
	o := w.orch(func(d *Deps) { d.Takeover = recordingTakeover{d.Takeover} })
	if _, err := o.FailoverServer(w.ctx, ServerOptions{}); !errors.Is(err, ErrRefused) {
		t.Fatalf("without force: %v", err)
	}
	mv, err := o.FailoverServer(w.ctx, ServerOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.stepDetail(serverMove(t, w), "marker"), "warning: the leader marker was not written") {
		t.Fatalf("marker step: %q", w.stepDetail(serverMove(t, w), "marker"))
	}
	if mv.State != registry.MoveDone {
		t.Fatalf("move: %+v", mv)
	}
}

func TestPartialProjectFailureIsReportedAndResumed(t *testing.T) {
	w := serverWorld(t)
	w.fail("promote n2/"+idBN2, errors.New("pg_promote timed out"), -1)
	o := w.orch()
	mv, err := o.FailoverServer(w.ctx, ServerOptions{})
	if err == nil || !strings.Contains(err.Error(), "project "+refB) || strings.Contains(err.Error(), "project "+refA) {
		t.Fatalf("error: %v", err)
	}
	if mv.State != registry.MoveFailed {
		t.Fatalf("move: %+v", mv)
	}
	// The cluster serves from the new leader; A moved, B did not, and B says so.
	if cl := clusterOf(t, w); cl.Leader != "n2" {
		t.Fatalf("cluster: %+v", cl)
	}
	if projectOf(t, w, refA).NodeID != "n2" || projectOf(t, w, refB).NodeID != "n1" {
		t.Fatalf("homes: %s, %s", projectOf(t, w, refA).NodeID, projectOf(t, w, refB).NodeID)
	}
	if projectOf(t, w, refB).Status != registry.StatusActiveUnhealthy {
		t.Fatalf("B: %+v", projectOf(t, w, refB))
	}
	// The old leader was demoted for what moved: the system cluster and A.
	if !w.has("demote n1/") || w.count("demote n1/") != 2 {
		t.Fatalf("demotions: %d\n%v", w.count("demote n1/"), w.snapshot())
	}

	w.clearFailures()
	mv, err = o.FailoverServer(w.ctx, ServerOptions{Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if mv.State != registry.MoveDone {
		t.Fatalf("resumed: %+v", mv)
	}
	if w.count("promote n2/"+idAN2) != 1 || w.count("promote n2/"+idSysN2) != 1 || w.count("marker") != 1 || w.count("quiesce") != 1 {
		t.Fatalf("a finished step ran again:\n%v", w.snapshot())
	}
	if projectOf(t, w, refB).NodeID != "n2" || projectOf(t, w, refB).Status != registry.StatusActiveHealthy {
		t.Fatalf("B: %+v", projectOf(t, w, refB))
	}
	if w.count("demote n1/") != 3 {
		t.Fatalf("demotions after the resume: %d", w.count("demote n1/"))
	}
}

// Every step of a move can be the last one before a crash. The table fails each in turn, builds a
// fresh orchestrator (the process died), resumes, and checks that the result is the same as an
// uninterrupted run and that nothing irreversible ran twice.
func TestResumeAtEveryStep(t *testing.T) {
	type tc struct {
		name    string
		planned bool
		fail    string
		// once: the step may run twice (it is repeated after the crash because the crash fell before its record).
		repeats map[string]int
	}
	cases := []tc{
		{name: "unplanned/fence", fail: "provider.fence"},
		{name: "unplanned/address", fail: "provider.takeover"},
		{name: "unplanned/marker", fail: "marker"},
		{name: "unplanned/promote system", fail: "promote n2/" + idSysN2},
		{name: "unplanned/promote system answered", fail: "promote-after n2/" + idSysN2},
		{name: "unplanned/become leader", fail: "become-leader"},
		{name: "unplanned/create move", fail: "registry.CreateMove"},
		{name: "unplanned/set leader", fail: "registry.SetLeader"},
		{name: "unplanned/node state", fail: "registry.SetNodeState"},
		{name: "unplanned/system home", fail: "registry.SetProjectNode system"},
		{name: "unplanned/promote project", fail: "promote n2/" + idAN2},
		{name: "unplanned/project answered", fail: "promote-after n2/" + idAN2},
		{name: "unplanned/project home", fail: "registry.SetProjectNode " + refA},
		{name: "unplanned/start", fail: "start n2/" + refA},
		{name: "unplanned/tenant", fail: "fleet.ensure " + refA},
		{name: "planned/quiesce", planned: true, fail: "quiesce"},
		{name: "planned/address", planned: true, fail: "provider.takeover"},
		{name: "planned/marker", planned: true, fail: "marker"},
		{name: "planned/promote system", planned: true, fail: "promote n2/" + idSysN2},
		{name: "planned/become leader", planned: true, fail: "become-leader"},
		{name: "planned/project home", planned: true, fail: "registry.SetProjectNode " + refB},
		{name: "planned/project answered", planned: true, fail: "promote-after n2/" + idBN2},
		{name: "planned/start", planned: true, fail: "start n2/" + refB},
		{name: "planned/demote system", planned: true, fail: "demote n1/system"},
		{name: "planned/demote project", planned: true, fail: "demote n1/" + refA},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := serverWorld(t)
			if !c.planned {
				w.down["n1"] = true
			}
			w.fail(c.fail, errors.New("injected"), -1)
			_, err := w.orch().FailoverServer(w.ctx, ServerOptions{})
			if err == nil {
				t.Fatalf("no error although %q fails", c.fail)
			}
			if c.fail == "quiesce" {
				// A switchover that cannot stop the leader undoes itself: nothing to resume.
				if !errors.Is(err, ErrRefused) && !strings.Contains(err.Error(), "stopping") {
					t.Fatalf("error: %v", err)
				}
				if !w.has("resume-leader n1") || stateFileExists(w) {
					t.Fatalf("the leader was not told to start again, or state is left:\n%v", w.snapshot())
				}
				return
			}
			w.clearFailures()
			// A new process: nothing is remembered but the state file, the registry and the world.
			mv, err := w.orch().FailoverServer(w.ctx, ServerOptions{Resume: true})
			if err != nil {
				t.Fatalf("resume: %v\n%v", err, w.snapshot())
			}
			if mv.State != registry.MoveDone {
				t.Fatalf("resumed move: %+v", mv)
			}
			if stateFileExists(w) {
				t.Fatal("failover.json is left behind")
			}
			cl := clusterOf(t, w)
			if cl.Leader != "n2" || cl.Epoch != 2 {
				t.Fatalf("cluster: %+v", cl)
			}
			for _, ref := range []string{config.SystemRef, refA, refB} {
				if p := projectOf(t, w, ref); p.NodeID != "n2" || p.Status != registry.StatusActiveHealthy {
					t.Fatalf("project %s: %+v", ref, p)
				}
			}
			// The irreversible steps ran once, or once more only where the crash fell inside them.
			for prefix, max := range map[string]int{"marker": 2, "quiesce": 2, "provider.takeover": 2} {
				if got := w.count(prefix); got > max {
					t.Errorf("%s ran %d times", prefix, got)
				}
			}
			if w.count("registry.SetLeader") > 2 {
				t.Errorf("SetLeader ran %d times", w.count("registry.SetLeader"))
			}
			if mvs, _ := w.reg.ListMoves(w.ctx, "", 10); len(mvs) != 1 {
				t.Errorf("%d moves rows, want 1 (the file's steps were adopted once)", len(mvs))
			}
			if c.planned {
				for _, ref := range []string{config.SystemRef, refA, refB} {
					if replicaOn(t, w, ref, "n1") == nil {
						t.Errorf("project %s has no replica row for the old leader", ref)
					}
				}
			}
		})
	}
}

func TestPlannedSwitchoverUndoneWhenTheLeaderCannotBeStopped(t *testing.T) {
	w := serverWorld(t)
	w.fail("quiesce n1", errors.New("stopping p1 timed out"), -1)
	mv, err := w.orch().FailoverServer(w.ctx, ServerOptions{})
	if err == nil || mv.State != registry.MoveAborted {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	w.assertOrder("quiesce n1", "resume-leader n1")
	w.assertNever("provider.")
	w.assertNever("marker")
	if stateFileExists(w) {
		t.Fatal("the aborted move left failover.json")
	}
}

func TestPlannedSwitchoverUndoneWhenTheStandbyCannotCatchUp(t *testing.T) {
	w := serverWorld(t)
	w.replay[idSysN2] = "0/1000000"
	mv, err := w.orch().FailoverServer(w.ctx, ServerOptions{})
	if !errors.Is(err, ErrReplayBehind) || mv.State != registry.MoveAborted {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	// Nothing irreversible happened before the standby caught up, so the leader starts again.
	w.assertOrder("quiesce n1", "resume-leader n1")
	w.assertNever("provider.takeover")
	w.assertNever("marker")
	w.assertNever("promote")
}

func TestRestoreMissingBuildsAStandbyFromTheArchive(t *testing.T) {
	w := serverWorld(t)
	org, _ := w.reg.GetOrganization(w.ctx, "acme")
	must(t, w.reg.CreateProject(w.ctx, &registry.Project{Ref: refC, OrgID: org.ID, Name: "c", Status: registry.StatusActiveHealthy}))
	w.prim["n1/"+refC] = &primState{running: true, healthy: true}
	w.lsn[refC] = "0/9000060"
	o := w.orch()

	pl, err := o.PlanServer(w.ctx, ServerOptions{})
	must(t, err)
	if c := findCheck(t, pl, "projects without a replica"); c.OK || !strings.Contains(c.Detail, refC) || !c.Blocking {
		t.Fatalf("check: %+v", c)
	}
	if _, err := o.FailoverServer(w.ctx, ServerOptions{}); !errors.Is(err, ErrRefused) {
		t.Fatalf("without --restore-missing: %v", err)
	}
	w.assertNever("quiesce")

	pl, err = o.PlanServer(w.ctx, ServerOptions{RestoreMissing: true})
	must(t, err)
	if c := findCheck(t, pl, "projects without a replica"); !c.OK || !strings.Contains(c.Detail, "archive_timeout") {
		t.Fatalf("check with the flag: %+v", c)
	}
	var restore bool
	for _, p := range pl.Projects {
		if p.Ref == refC {
			restore = p.RestoreFromArchive
		}
	}
	if !restore {
		t.Fatalf("plan: %+v", pl.Projects)
	}

	mv, err := o.FailoverServer(w.ctx, ServerOptions{RestoreMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	if mv.State != registry.MoveDone {
		t.Fatalf("move: %+v", mv)
	}
	// Seeded from the archive with no upstream, drained instead of waited for, and the old
	// primary is rebuilt rather than turned into a replica of a timeline it may have outrun.
	w.assertOrder("ensure n2/"+refC+"-rr-eu-west-1-", "promote n2/"+refC+"-rr-eu-west-1-", "registry.SetProjectNode "+refC+" n2 2", "start n2/"+refC, "aside n1/"+refC, "replicas.setup "+refC+" on n1")
	if !w.has("ensure n2/" + refC + "-rr-eu-west-1-") {
		t.Fatal("no standby was seeded")
	}
	if !strings.Contains(strings.Join(w.snapshot(), "\n"), "noupstream=true") {
		t.Fatal("the standby was seeded with an upstream")
	}
	for _, e := range w.snapshot() {
		if strings.HasPrefix(e, "promote n2/"+refC) && !strings.Contains(e, "wait= drain=true") {
			t.Fatalf("promote of the restored project: %s", e)
		}
	}
	w.assertNever("demote n1/" + refC)
}

func TestAProjectWithItsReplicaOnAThirdNodeMovesThere(t *testing.T) {
	w := serverWorld(t)
	w.addNode3()
	// refA's only replica is on n3 now.
	must(t, w.reg.DeleteReplica(w.ctx, idAN2))
	delete(w.inst, idAN2)
	idAN3 := refA + "-rr-eu-west-1-a3a3a3"
	must(t, w.reg.CreateReplica(w.ctx, &registry.Replica{Identifier: idAN3, Ref: refA, NodeID: "n3", Status: statusHealthy, InitStep: registry.ReplicaStepDone}))
	w.inst[idAN3] = &instState{node: "n3", ref: refA, role: "replica", lag: f64(1), postgres: true}
	w.replay[idAN3] = caughtUp(w.lsn[refA])
	mv, err := w.orch().FailoverServer(w.ctx, ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mv.State != registry.MoveDone {
		t.Fatalf("move: %+v", mv)
	}
	if p := projectOf(t, w, refA); p.NodeID != "n3" {
		t.Fatalf("refA is homed on %s, want n3", p.NodeID)
	}
	w.assertOrder("promote n3/"+idAN3, "registry.SetProjectNode "+refA+" n3 2", "start n3/"+refA)
}

func TestAPausedProjectIsPromotedAndPausedAgain(t *testing.T) {
	w := serverWorld(t)
	must(t, w.reg.SetProjectStatus(w.ctx, refB, registry.StatusInactive))
	w.prim["n1/"+refB].running = false
	if _, err := w.orch().FailoverServer(w.ctx, ServerOptions{}); err != nil {
		t.Fatal(err)
	}
	p := projectOf(t, w, refB)
	if p.NodeID != "n2" || p.Status != registry.StatusInactive {
		t.Fatalf("B: %+v", p)
	}
	w.assertOrder("promote n2/"+idBN2, "registry.SetProjectNode "+refB+" n2 2", "stop n2/"+refB)
	w.assertNever("start n2/" + refB)
	w.assertNever("fleet.ensure " + refB)
	w.assertNever("basebackup n2/" + refB)
}

func TestBranchesAreLeftWhereTheyAre(t *testing.T) {
	w := serverWorld(t)
	org, _ := w.reg.GetOrganization(w.ctx, "acme")
	br := &registry.Project{Ref: refC, OrgID: org.ID, Name: "branch", Status: registry.StatusActiveHealthy, Branch: &registry.BranchInfo{ID: "b1", ParentRef: refA, Name: "feature"}}
	must(t, w.reg.CreateProject(w.ctx, br))
	pl, err := w.orch().PlanServer(w.ctx, ServerOptions{})
	must(t, err)
	if len(pl.Notes) == 0 || !strings.Contains(pl.Notes[0], refC) {
		t.Fatalf("notes: %v", pl.Notes)
	}
	if hasCheck(pl, "projects without a replica") {
		t.Fatalf("a branch is not a project without a replica: %+v", pl.Checks)
	}
}

func TestPreflightRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mut    func(w *world)
		opts   ServerOptions
		check  string
		hard   bool
		forced bool // a forced run goes ahead
	}{
		"file storage":        {mut: func(w *world) { w.cfg.Fleet.StorageBackend = "file" }, check: "storage backend", hard: true},
		"replica lagging":     {mut: func(w *world) { w.inst[idAN2].lag = f64(45) }, check: "replica of " + refA, forced: true},
		"replica lag unknown": {mut: func(w *world) { w.inst[idBN2].lag = nil }, check: "replica of " + refB, forced: true},
		"system lagging":      {mut: func(w *world) { w.inst[idSysN2].lag = f64(300) }, check: "system lag", forced: true},
		"replica not healthy": {mut: func(w *world) {
			must(w.t, w.reg.SetReplicaStatus(w.ctx, idAN2, "ACTIVE_UNHEALTHY", registry.ReplicaStepDone, ""))
		}, check: "replica of " + refA, forced: true},
		"standby down": {mut: func(w *world) { w.inst[idAN2].postgres = false }, check: "replica of " + refA, forced: true},
		"release skew": {mut: func(w *world) {
			n, _ := w.reg.GetNode(w.ctx, "n2")
			n.Version = "v0.3.0"
			must(w.t, w.reg.UpdateNode(w.ctx, n))
			w.refreshMembers()
		}, check: "same release", forced: true},
		"a project without a replica": {mut: func(w *world) {
			org, _ := w.reg.GetOrganization(w.ctx, "acme")
			must(w.t, w.reg.CreateProject(w.ctx, &registry.Project{Ref: refC, OrgID: org.ID, Name: "c", Status: registry.StatusActiveHealthy}))
			w.prim["n1/"+refC] = &primState{running: true, healthy: true}
			w.lsn[refC] = "0/9000060"
		}, check: "projects without a replica", hard: true},
		"no system replica": {mut: func(w *world) { must(w.t, w.reg.DeleteReplica(w.ctx, idSysN2)) }, check: "system replica", hard: true},
		"target left":       {mut: func(w *world) { must(w.t, w.reg.SetNodeState(w.ctx, "n2", registry.NodeLeft)) }, check: "target node", hard: true},
	} {
		t.Run(name, func(t *testing.T) {
			w := serverWorld(t)
			tc.mut(w)
			o := w.orch()
			pl, err := o.PlanServer(w.ctx, tc.opts)
			must(t, err)
			c := findCheck(t, pl, tc.check)
			if c.OK || !c.Blocking || c.Hard != tc.hard {
				t.Fatalf("check %q: %+v", tc.check, c)
			}
			_, err = o.FailoverServer(w.ctx, tc.opts)
			var re *RefusedError
			if !errors.As(err, &re) {
				t.Fatalf("error: %v", err)
			}
			w.assertNever("quiesce")
			opts := tc.opts
			opts.Force = true
			if tc.forced {
				if _, err := o.FailoverServer(w.ctx, opts); err != nil {
					t.Fatalf("forced: %v", err)
				}
				return
			}
			if _, err := o.FailoverServer(w.ctx, opts); !errors.Is(err, ErrRefused) {
				t.Fatalf("forced, but a hard check failed: %v", err)
			}
			w.assertNever("quiesce")
		})
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	w := serverWorld(t)
	o := w.orch()
	before := len(w.snapshot())
	mv, err := o.FailoverServer(w.ctx, ServerOptions{DryRun: true})
	if err != nil || mv != nil {
		t.Fatalf("dry run: %+v, %v", mv, err)
	}
	for _, e := range w.snapshot()[before:] {
		for _, bad := range []string{"quiesce", "stop ", "promote", "marker", "registry.", "fence", "provider.takeover", "provider.fence", "start "} {
			if strings.HasPrefix(e, bad) {
				t.Fatalf("dry run did %s", e)
			}
		}
	}
	if stateFileExists(w) {
		t.Fatal("dry run wrote failover.json")
	}
	if ms, _ := w.reg.ListMoves(w.ctx, "", 10); len(ms) != 0 {
		t.Fatalf("dry run left moves: %+v", ms)
	}
	pl, err := o.PlanServer(w.ctx, ServerOptions{})
	must(t, err)
	if pl.Kind != "switchover" || pl.From != "n1" || pl.To != "n2" || pl.Epoch != 2 || len(pl.Blocked()) != 0 {
		t.Fatalf("plan: %+v", pl)
	}
	for _, name := range []string{"target node", "same release", "leader", "storage backend", "epoch marker", "system replica", "replicas"} {
		if !hasCheck(pl, name) {
			t.Errorf("no %q check among %+v", name, pl.Checks)
		}
	}
	if len(pl.Projects) != 2 {
		t.Fatalf("projects: %+v", pl.Projects)
	}
}

func TestOneMoveRunsAtATime(t *testing.T) {
	w := serverWorld(t)
	o := w.orch()
	release, err := o.acquire()
	must(t, err)
	if _, err := o.FailoverServer(w.ctx, ServerOptions{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("server: %v", err)
	}
	if _, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA}); !errors.Is(err, ErrBusy) {
		t.Fatalf("project: %v", err)
	}
	release()
}

func TestStateFileBlocksANewMoveUntilItIsResumed(t *testing.T) {
	w := serverWorld(t)
	w.down["n1"] = true
	w.fail("provider.fence", errors.New("injected"), -1)
	o := w.orch()
	_, _ = o.FailoverServer(w.ctx, ServerOptions{})
	w.clearFailures()
	_, err := o.FailoverServer(w.ctx, ServerOptions{})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "--resume") {
		t.Fatalf("a second start: %v", err)
	}
	if _, err := w.orch().FailoverServer(w.ctx, ServerOptions{Resume: true}); err != nil {
		t.Fatal(err)
	}
	// Nothing left to resume.
	if _, err := w.orch().FailoverServer(w.ctx, ServerOptions{Resume: true}); !errors.Is(err, ErrRefused) {
		t.Fatalf("resume of nothing: %v", err)
	}
}

func TestAProjectMoveWaitsForAnUnfinishedServerMove(t *testing.T) {
	w := serverWorld(t)
	w.down["n1"] = true
	w.fail("provider.fence", errors.New("injected"), -1)
	_, _ = w.orch().FailoverServer(w.ctx, ServerOptions{})
	// The node is not the leader, and a server move is unfinished: both stop a project move.
	pl, err := w.orch().PlanProject(w.ctx, ProjectOptions{Ref: refA})
	must(t, err)
	if c := findCheck(t, pl, "server move"); c.OK || !c.Hard {
		t.Fatalf("check: %+v", c)
	}
}

// Right after a server move the other nodes may not know yet that the survivor leads: their first
// refusals are repeated.
func TestACallTurnedAwayWhileTheNodeLearnsWhoLeadsIsRepeated(t *testing.T) {
	w := serverWorld(t)
	w.addNode3()
	must(t, w.reg.DeleteReplica(w.ctx, idAN2))
	delete(w.inst, idAN2)
	idAN3 := refA + "-rr-eu-west-1-a3a3a3"
	must(t, w.reg.CreateReplica(w.ctx, &registry.Replica{Identifier: idAN3, Ref: refA, NodeID: "n3", Status: statusHealthy, InitStep: registry.ReplicaStepDone}))
	w.inst[idAN3] = &instState{node: "n3", ref: refA, role: "replica", lag: f64(1), postgres: true}
	w.replay[idAN3] = caughtUp(w.lsn[refA])
	w.fail("promote n3/"+idAN3, &mesh.RemoteError{Node: "n3", Status: 403, Message: "only the leader may ask for this"}, 2)
	w.fail("start n3/"+refA, &mesh.RemoteError{Node: "n3", Status: 403, Message: "only the leader may ask for this"}, 1)
	mv, err := w.orch().FailoverServer(w.ctx, ServerOptions{})
	if err != nil || mv.State != registry.MoveDone {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	if got := w.count("promote n3/" + idAN3); got != 3 {
		t.Fatalf("%d promote calls, want 3", got)
	}
	if got := w.count("start n3/" + refA); got != 2 {
		t.Fatalf("%d start calls, want 2", got)
	}
	// Another kind of error is not repeated, and neither is a 409.
	for _, err := range []error{errors.New("pg_promote failed"), &mesh.RemoteError{Node: "n2", Status: 409, Message: "the standby did not catch up"}} {
		w2 := serverWorld(t)
		w2.fail("promote n2/"+idBN2, err, -1)
		_, _ = w2.orch().FailoverServer(w2.ctx, ServerOptions{})
		if got := w2.count("promote n2/" + idBN2); got != 1 {
			t.Fatalf("%d promote calls for %v", got, err)
		}
	}
}

// A store that ignores conditional writes (Garage does) lets both survivors write. The one whose
// marker was overwritten sees the other's when it reads back, and stops before the address.
func TestASurvivorWhoseMarkerIsOverwrittenAfterItsWriteStopsBeforeTheAddress(t *testing.T) {
	w := serverWorld(t)
	w.down["n1"] = true
	w.markerRace = &backup.LeaderMarker{Epoch: 2, Leader: "n3"}
	mv, err := w.orch().FailoverServer(w.ctx, ServerOptions{})
	if !errors.Is(err, ErrEpochLost) || mv.State != registry.MoveAborted {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	w.assertNever("provider.takeover")
	w.assertNever("promote")
}

func TestAServerMoveRefusesAPlanOtherThanTheConfirmedOne(t *testing.T) {
	w := serverWorld(t)
	o := w.orch()
	// The operator confirmed a clean stop of the leader; the leader stops answering before the run.
	pl, err := o.PlanServer(w.ctx, ServerOptions{})
	must(t, err)
	if pl.Kind != string(registry.MoveSwitchover) {
		t.Fatalf("plan: %+v", pl)
	}
	w.down["n1"] = true
	_, err = o.FailoverServer(w.ctx, ServerOptions{ExpectKind: pl.Kind, ExpectEpoch: pl.Epoch})
	if !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("error: %v", err)
	}
	for _, e := range []string{"provider.fence", "fence ", "marker", "promote"} {
		w.assertNever(e)
	}
	if stateFileExists(w) {
		t.Fatal("a refused run left a state file")
	}
	// The epoch is part of the plan too.
	w.down["n1"] = false
	if _, err := o.FailoverServer(w.ctx, ServerOptions{ExpectKind: pl.Kind, ExpectEpoch: pl.Epoch + 1}); !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("another epoch: %v", err)
	}
	if _, err := o.FailoverServer(w.ctx, ServerOptions{ExpectKind: pl.Kind, ExpectEpoch: pl.Epoch}); err != nil {
		t.Fatalf("the confirmed plan: %v", err)
	}
}

// A project that finishes on a later --resume gets a base backup then, not never.
func TestAProjectThatFinishesOnAResumeGetsItsBaseBackup(t *testing.T) {
	w := serverWorld(t)
	w.fail("start n2/"+refB, errors.New("PostgREST did not start"), -1)
	o := w.orch()
	mv, err := o.FailoverServer(w.ctx, ServerOptions{})
	if err == nil || mv == nil || mv.State != registry.MoveFailed {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	if !w.has("basebackup n2/"+refA) || w.has("basebackup n2/"+refB) {
		t.Fatalf("base backups after the first run:\n%v", w.snapshot())
	}
	if hasStep(serverMove(t, w), "base-backups") {
		t.Fatal("the base backup step is over although a project has not finished")
	}
	w.clearFailures()
	if _, err := w.orch().FailoverServer(w.ctx, ServerOptions{Resume: true}); err != nil {
		t.Fatal(err)
	}
	if w.count("basebackup n2/"+refB) != 1 || w.count("basebackup n2/"+refA) != 1 || w.count("basebackup n2/system") != 1 {
		t.Fatalf("each project gets one base backup:\n%v", w.snapshot())
	}
	if !hasStep(serverMove(t, w), "base-backups") {
		t.Fatal("the step did not end")
	}
}

// A project restored from the archive says so in the announcement: its data loss is the owner's to know.
func TestTheAnnouncementNamesTheProjectsRestoredFromTheArchive(t *testing.T) {
	w := serverWorld(t)
	org, _ := w.reg.GetOrganization(w.ctx, "acme")
	must(t, w.reg.CreateProject(w.ctx, &registry.Project{Ref: refC, OrgID: org.ID, Name: "c", Status: registry.StatusActiveHealthy}))
	w.prim["n1/"+refC] = &primState{running: true, healthy: true}
	w.lsn[refC] = "0/9000060"
	if _, err := w.orch().FailoverServer(w.ctx, ServerOptions{RestoreMissing: true}); err != nil {
		t.Fatal(err)
	}
	if d := w.alerts[0].Detail; w.alerts[0].Kind != alerts.KindFailoverStarted || !strings.Contains(d, refC) || !strings.Contains(d, "archive_timeout") {
		t.Fatalf("announcement: %+v", w.alerts[0])
	}
}

// countingInstances records how many Observe calls are in flight at once.
type countingInstances struct {
	Instances
	cur, peak atomic.Int32
}

func (c *countingInstances) Observe(ctx context.Context, node, identifier string) (peerapi.InstanceStatus, error) {
	n := c.cur.Add(1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond) // long enough for the others to arrive
	defer c.cur.Add(-1)
	return c.Instances.Observe(ctx, node, identifier)
}

// A plan reads the replicas of every project it would move from the nodes that hold them, in
// parallel and no more than viewParallel at a time, so that dozens of projects do not make the
// plan outlast the timeout of `supavise status`.
func TestAPlanViewsTheReplicasInParallelButBounded(t *testing.T) {
	w := serverWorld(t)
	org, _ := w.reg.GetOrganization(w.ctx, "acme")
	for i := 0; i < 24; i++ {
		ref := "q" + strings.Repeat("a", 17) + string(rune('a'+i/26)) + string(rune('a'+i%26)) // a valid ref
		must(t, w.reg.CreateProject(w.ctx, &registry.Project{Ref: ref, OrgID: org.ID, Name: ref[:4], Status: registry.StatusActiveHealthy}))
		id := ref + "-rr-eu-west-1-aaaaaa"
		must(t, w.reg.CreateReplica(w.ctx, &registry.Replica{Identifier: id, Ref: ref, NodeID: "n2", Origin: registry.ReplicaDefault, Status: statusHealthy, InitStep: registry.ReplicaStepDone}))
		w.inst[id] = &instState{node: "n2", ref: ref, role: "replica", lag: f64(0.4), postgres: true}
		w.lsn[ref] = "0/9000060"
		w.replay[id] = caughtUp(w.lsn[ref])
		w.prim["n1/"+ref] = &primState{running: true, healthy: true}
	}
	ci := &countingInstances{}
	o := w.orch(func(d *Deps) { ci.Instances = d.Instances; d.Instances = ci })
	pl, err := o.PlanServer(w.ctx, ServerOptions{To: "n2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Projects) != 26 || len(pl.Blocked()) != 0 {
		t.Fatalf("%d projects, blocked %+v", len(pl.Projects), pl.Blocked())
	}
	if peak := ci.peak.Load(); peak < 2 || peak > viewParallel {
		t.Fatalf("%d nodes were asked at once; want between 2 and %d", peak, viewParallel)
	}
}
