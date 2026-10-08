package failover

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/failover/fenced"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/registry"
)

func stepNames(mv *registry.Move) []string {
	var out []string
	for _, s := range mv.Steps {
		out = append(out, s.Name)
	}
	return out
}

func hasStep(mv *registry.Move, name string) bool {
	for _, s := range mv.Steps {
		if s.Name == name {
			return true
		}
	}
	return false
}

func projectOf(t *testing.T, w *world, ref string) *registry.Project {
	t.Helper()
	p, err := w.reg.GetProject(w.ctx, ref)
	must(t, err)
	return p
}

func replicaOn(t *testing.T, w *world, ref, node string) *registry.Replica {
	t.Helper()
	reps, err := w.reg.ListReplicas(w.ctx, ref)
	must(t, err)
	for _, r := range reps {
		if r.NodeID == node {
			r := r
			return &r
		}
	}
	return nil
}

func TestProjectSwitchover(t *testing.T) {
	w := newWorld(t)
	o := w.orch()
	var reported []string
	ctx := WithProgress(context.Background(), func(s registry.MoveStep) { reported = append(reported, s.Name) })
	mv, err := o.FailoverProject(ctx, ProjectOptions{Ref: refA})
	if err != nil {
		t.Fatal(err)
	}
	if mv.State != registry.MoveDone || mv.Kind != registry.MoveSwitchover || mv.Scope != registry.MoveProject || mv.FromNode != "n1" || mv.ToNode != "n2" || mv.Epoch != 1 {
		t.Fatalf("move: %+v", mv)
	}
	want := []string{"begin", "quiesce", "stop-old", "promote", "homed", "start-new", "tenant", "demote-old", "base-backup"}
	if got := stepNames(mv); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps %v, want %v", got, want)
	}
	if strings.Join(reported, ",") != strings.Join(want, ",") {
		t.Fatalf("progress %v", reported)
	}

	// The order that keeps one writer: the old primary has stopped, with its last position
	// handed to the replica, before the registry says anything else.
	lsn := w.lsn[refA]
	w.assertOrder("fleet.quiesce "+refA, "stop n1/"+refA, "promote n2/"+idAN2+" epoch=1 wait="+lsn,
		"registry.SetProjectNode "+refA+" n2 1", "start n2/"+refA, "fleet.ensure "+refA, "demote n1/", "basebackup n2/"+refA)
	w.assertNever("fence")
	w.assertNever("provider.")

	if p := projectOf(t, w, refA); p.NodeID != "n2" || p.Status != registry.StatusActiveHealthy {
		t.Fatalf("project: %+v", p)
	}
	if r := replicaOn(t, w, refA, "n2"); r != nil {
		t.Fatalf("the promoted replica's row is still there: %+v", r)
	}
	old := replicaOn(t, w, refA, "n1")
	if old == nil || old.Origin != registry.ReplicaManual || old.InitStep != registry.ReplicaStepDone || !registry.ValidReplicaIdentifier(old.Identifier) {
		t.Fatalf("old home's replica: %+v", old)
	}
	if !strings.Contains(old.Identifier, "-rr-eu-west-1-") {
		t.Fatalf("identifier %q should name the old home's region", old.Identifier)
	}
	if !w.has("demote n1/" + old.Identifier) {
		t.Fatalf("the old home was not demoted as %s:\n%v", old.Identifier, w.snapshot())
	}
	// Project B is untouched.
	if p := projectOf(t, w, refB); p.NodeID != "n1" {
		t.Fatalf("project B moved: %+v", p)
	}
	if k := w.alertKinds(); strings.Join(k, ",") != alerts.KindFailoverStarted+","+alerts.KindFailoverCompleted {
		t.Fatalf("alerts %v", k)
	}
	// It is in the registry's log too.
	logged, err := w.reg.GetMove(w.ctx, mv.ID)
	must(t, err)
	if logged.State != registry.MoveDone || len(logged.Steps) != len(want) {
		t.Fatalf("logged move: %+v", logged)
	}
}

func TestSwitchoverKeepsTheOriginOfTheReplica(t *testing.T) {
	w := newWorld(t)
	o := w.orch()
	if _, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refB}); err != nil {
		t.Fatal(err)
	}
	if r := replicaOn(t, w, refB, "n1"); r == nil || r.Origin != registry.ReplicaDefault {
		t.Fatalf("the replica of a default project must stay a default one: %+v", r)
	}
}

func TestUnplannedProjectFailover(t *testing.T) {
	w := newWorld(t)
	w.prim["n1/"+refA].healthy = false // PostgREST has not answered for the grace period
	o := w.orch()
	mv, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA})
	if err != nil {
		t.Fatal(err)
	}
	if mv.Kind != registry.MoveFailover || mv.State != registry.MoveDone {
		t.Fatalf("move: %+v", mv)
	}
	want := []string{"begin", "fence-old", "promote", "homed", "start-new", "tenant", "reseed-old", "base-backup"}
	if got := stepNames(mv); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps %v, want %v", got, want)
	}
	// The old primary is told to stop and stay stopped before anything is promoted, and the
	// replica drains the archive instead of waiting for a position nobody can give.
	// The home is the leader itself, which cannot ask itself over the mesh: it records the fence and
	// stops the primary on its own.
	w.assertOrder("local.stop "+refA, "promote n2/"+idAN2+" epoch=1 wait= drain=true",
		"registry.SetProjectNode "+refA, "start n2/"+refA, "fleet.ensure "+refA, "aside n1/"+refA, "replicas.setup "+refA+" on n1", "basebackup n2/"+refA)
	if rec, err := fenced.Project(w.cfg.Paths(), refA); err != nil || rec == nil || rec.Epoch != 1 || rec.Leader != "n1" {
		t.Fatalf("fence record: %+v, %v", rec, err)
	}
	w.assertNever("fence ")
	w.assertNever("stop n1/")
	w.assertNever("fleet.quiesce")
	w.assertNever("demote")
	if p := projectOf(t, w, refA); p.NodeID != "n2" || p.Status != registry.StatusActiveHealthy {
		t.Fatalf("project: %+v", p)
	}
	if replicaOn(t, w, refA, "n1") != nil {
		t.Fatal("a replica row was added for a node whose data diverged: the controller adds it")
	}
}

// A project homed on a follower is fenced through the mesh: the leader that runs the move is not
// its home.
func TestUnplannedFailoverOfAProjectHomedOnAFollowerFencesThroughTheMesh(t *testing.T) {
	w := newWorld(t) // n1 leads
	rehomeOn(t, w, refB, "n2", "n1")
	w.prim["n2/"+refB] = &primState{running: true, healthy: false}
	mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refB})
	if err != nil || mv.Kind != registry.MoveFailover || mv.State != registry.MoveDone || mv.FromNode != "n2" || mv.ToNode != "n1" {
		t.Fatalf("move: %+v, %v", mv, err)
	}
	w.assertOrder("fence n2 epoch=1 ref="+refB, "promote n1/", "registry.SetProjectNode "+refB+" n1 1", "aside n2/"+refB)
	w.assertNever("local.stop")
}

// rehomeOn homes ref on node with its replica on the other, as an earlier switchover leaves it.
func rehomeOn(t *testing.T, w *world, ref, node, replicaNode string) {
	t.Helper()
	must(t, w.reg.SetProjectNode(w.ctx, ref, node, 1))
	reps, err := w.reg.ListReplicas(w.ctx, ref)
	must(t, err)
	var old registry.Replica
	for _, r := range reps {
		old = r
		must(t, w.reg.DeleteReplica(w.ctx, r.Identifier))
		delete(w.inst, r.Identifier)
	}
	id := ref + "-rr-eu-west-1-r1r1r1"
	must(t, w.reg.CreateReplica(w.ctx, &registry.Replica{Identifier: id, Ref: ref, NodeID: replicaNode, Origin: old.Origin, Status: statusHealthy, InitStep: registry.ReplicaStepDone}))
	w.inst[id] = &instState{node: replicaNode, ref: ref, role: "replica", lag: f64(0.4), postgres: true}
	w.replay[id] = caughtUp(w.lsn[ref])
	w.prim[replicaNode+"/"+ref] = &primState{}
	delete(w.prim, "n1/"+ref)
	if node == "n1" {
		w.prim["n1/"+ref] = &primState{running: true, healthy: true}
	}
}

// The plan the operator confirmed is the plan that runs: a project whose primary stopped answering
// after the confirmation of a clean switchover is not fenced on its strength.
func TestAProjectMoveRefusesAPlanOtherThanTheConfirmedOne(t *testing.T) {
	w := newWorld(t)
	w.prim["n1/"+refA].healthy = false
	_, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA, ExpectKind: string(registry.MoveSwitchover)})
	if !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("error: %v", err)
	}
	w.assertNever("local.stop")
	w.assertNever("promote")
	if moves, _ := w.reg.ListMoves(w.ctx, "", 10); len(moves) != 0 {
		t.Fatalf("a refused move left %d row(s)", len(moves))
	}
	if _, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA, ExpectKind: string(registry.MoveFailover)}); err != nil {
		t.Fatalf("the confirmed kind: %v", err)
	}
}

// The project's lock is taken before the plan is read, and a move that cannot get it records nothing.
func TestAProjectMoveHoldsTheLockBeforeItPlans(t *testing.T) {
	w := newWorld(t)
	var mu sync.Mutex
	var order []string
	locks := lockerFunc(func(_ context.Context, ref string) (func(), error) {
		mu.Lock()
		order = append(order, "lock "+ref)
		mu.Unlock()
		return func() { mu.Lock(); order = append(order, "unlock "+ref); mu.Unlock() }, nil
	})
	o := w.orch(func(d *Deps) { d.Locks = locks })
	if _, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA}); err != nil {
		t.Fatal(err)
	}
	// The lock is let go of around the registration with the shared services, which takes it itself,
	// and taken again for the rest of the move.
	if strings.Join(order, ",") != "lock "+refA+",unlock "+refA+",lock "+refA+",unlock "+refA || w.index("registry.CreateMove") < 0 {
		t.Fatalf("lock calls: %v", order)
	}
	// A dry run changes nothing and needs no lock.
	order = nil
	if _, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refB, DryRun: true}); err != nil || len(order) != 0 {
		t.Fatalf("dry run: %v, locks %v", err, order)
	}
	// A lock that cannot be had leaves no moves row.
	w2 := newWorld(t)
	o2 := w2.orch(func(d *Deps) {
		d.Locks = lockerFunc(func(context.Context, string) (func(), error) { return nil, errors.New("project is locked") })
	})
	if _, err := o2.FailoverProject(w2.ctx, ProjectOptions{Ref: refA}); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("error: %v", err)
	}
	if moves, _ := w2.reg.ListMoves(w2.ctx, "", 10); len(moves) != 0 {
		t.Fatalf("a move that never got the lock left %d row(s)", len(moves))
	}
}

type lockerFunc func(ctx context.Context, ref string) (func(), error)

func (f lockerFunc) Lock(ctx context.Context, ref string) (func(), error) { return f(ctx, ref) }

func TestProjectFailoverWhenTheHomeDoesNotAnswerIsRefused(t *testing.T) {
	w := newWorld(t)
	w.down["n1"] = true
	o := w.orch()
	_, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA})
	var re *RefusedError
	if !errors.Is(err, ErrRefused) || !errors.As(err, &re) {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(err.Error(), "node failure") || !strings.Contains(err.Error(), "supavise failover on a survivor") {
		t.Fatalf("the refusal should send the operator to the server failover: %v", err)
	}
	// Even with --force: no fence can be made without the node.
	if _, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA, Force: true}); !errors.Is(err, ErrRefused) {
		t.Fatalf("with force: %v", err)
	}
	w.assertNever("promote")
	if moves, _ := w.reg.ListMoves(w.ctx, "", 10); len(moves) != 0 {
		t.Fatalf("a refused move left %d row(s)", len(moves))
	}
}

// A project whose home is a follower that does not answer has no move: the refusal says so instead
// of sending the operator to a server failover that would leave the project where it is.
func TestAProjectHomedOnAFollowerThatIsDownIsRefusedWithTheTruth(t *testing.T) {
	w := newWorld(t)
	rehomeOn(t, w, refB, "n2", "n1")
	w.down["n2"] = true
	_, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refB, Force: true})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "does not lead") || strings.Contains(err.Error(), "run supavise failover") {
		t.Fatalf("error: %v", err)
	}
	w.assertNever("promote")
}

func TestSwitchoverThatCannotCatchUpIsUndone(t *testing.T) {
	w := newWorld(t)
	w.replay[idAN2] = "0/1000000" // the standby is far behind the old primary's last WAL
	o := w.orch()
	mv, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA})
	if !errors.Is(err, ErrReplayBehind) {
		t.Fatalf("error: %v", err)
	}
	if mv.State != registry.MoveAborted {
		t.Fatalf("move: %+v", mv)
	}
	// The wait is made before the node is asked to promote anything. Nothing was promoted, the old
	// primary runs again and the shared services have it back.
	w.assertOrder("stop n1/"+refA, "start n1/"+refA, "fleet.ensure "+refA)
	w.assertNever("promote")
	w.assertNever("registry.SetProjectNode")
	w.assertNever("demote")
	if p := projectOf(t, w, refA); p.NodeID != "n1" || p.Status != registry.StatusActiveHealthy {
		t.Fatalf("project: %+v", p)
	}
	if in := w.inst[idAN2]; in.role != "replica" {
		t.Fatalf("the replica was promoted: %+v", in)
	}
	if k := w.alertKinds(); k[len(k)-1] != alerts.KindFailoverFailed {
		t.Fatalf("alerts %v", k)
	}
	// An aborted move is over: another one can start.
	w.replay[idAN2] = caughtUp(w.lsn[refA])
	if _, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA}); err != nil {
		t.Fatalf("a new try after the abort: %v", err)
	}
}

// A standby sitting exactly at the stop position has not replayed the shutdown checkpoint: it is not
// past it, and promoting it there would fork before the old primary's end.
func TestSwitchoverWaitsUntilTheStandbyIsPastTheStopPosition(t *testing.T) {
	w := newWorld(t)
	w.replay[idAN2] = w.lsn[refA]
	mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA})
	if !errors.Is(err, ErrReplayBehind) || mv.State != registry.MoveAborted {
		t.Fatalf("error %v, move %+v", err, mv)
	}
	w.assertNever("promote")
}

// A stop that reports no position gives the promotion nothing to wait for. The replica is not
// promoted without it: the switchover undoes itself, and nothing is recorded as stopped.
func TestSwitchoverThatLearnsNoStopPositionIsUndone(t *testing.T) {
	for name, lsn := range map[string]string{"empty": "", "not an LSN": "unknown"} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.lsn[refA] = lsn
			mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA})
			if !errors.Is(err, ErrNoFinalPosition) || mv.State != registry.MoveAborted {
				t.Fatalf("error %v, move %+v", err, mv)
			}
			w.assertNever("promote")
			w.assertOrder("stop n1/"+refA, "start n1/"+refA, "fleet.ensure "+refA)
			if mv, _ := w.reg.GetMove(w.ctx, mv.ID); mv != nil {
				for _, s := range mv.Steps {
					if s.Name == "stop-old" {
						t.Fatalf("the stop was recorded without a position: %+v", s)
					}
				}
			}
			if p := projectOf(t, w, refA); p.NodeID != "n1" || p.Status != registry.StatusActiveHealthy {
				t.Fatalf("project: %+v", p)
			}
		})
	}
}

// A promotion call that fails after the standby caught up may be a promotion in flight (the node is
// inside pg_promote or its restart when the connection drops). The old primary is not started again:
// two primaries are worse than a move that waits for --resume, and the resume promotes again.
func TestAFailedPromotionCallDoesNotRestartTheOldPrimaryEvenIfTheStandbyStillReplays(t *testing.T) {
	w := newWorld(t)
	w.fail("promote n2/", errors.New("context deadline exceeded"), 1) // the standby is still in recovery afterwards
	o := w.orch()
	mv, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA})
	if err == nil || errors.Is(err, ErrReplayBehind) || mv.State != registry.MoveFailed {
		t.Fatalf("error %v, move %+v", err, mv)
	}
	w.assertNever("start n1/")
	if in := w.inst[idAN2]; in.role != "replica" {
		t.Fatalf("the fake promoted: %+v", in)
	}
	if _, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA, Resume: true}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if p := projectOf(t, w, refA); p.NodeID != "n2" || p.Status != registry.StatusActiveHealthy {
		t.Fatalf("project: %+v", p)
	}
	if in := w.inst[idAN2]; in == nil || in.role != "primary" {
		t.Fatalf("the resume did not promote: %+v", in)
	}
}

func TestSwitchoverThatCannotRestartTheOldPrimaryIsFailedNotAborted(t *testing.T) {
	w := newWorld(t)
	w.replay[idAN2] = "0/1000000"
	w.fail("start n1/", errors.New("disk full"), -1)
	o := w.orch()
	mv, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA})
	if err == nil || !strings.Contains(err.Error(), "also failed") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error: %v", err)
	}
	if mv.State != registry.MoveFailed {
		t.Fatalf("move: %+v", mv)
	}
	if p := projectOf(t, w, refA); p.Status != registry.StatusActiveUnhealthy {
		t.Fatalf("a project nobody runs must say so: %+v", p)
	}
}

func TestPromotionWithAnUnknownOutcomeKeepsTheOldPrimaryDown(t *testing.T) {
	w := newWorld(t)
	w.fail("promote n2/", errors.New("connection reset"), 1)
	w.afterEvent("promote n2/", func() { w.partition("n2") }) // and the node cannot be asked afterwards
	o := w.orch()
	mv, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA})
	if err == nil || errors.Is(err, ErrReplayBehind) {
		t.Fatalf("error: %v", err)
	}
	if mv.State != registry.MoveFailed {
		t.Fatalf("move: %+v", mv)
	}
	w.assertNever("start n1/") // the new primary may exist: starting the old one could make two
	if p := projectOf(t, w, refA); p.NodeID != "n1" {
		t.Fatalf("project: %+v", p)
	}
	if p := projectOf(t, w, refA); p.Status == registry.StatusRestarting {
		t.Fatalf("nothing is restarting the project any more: %+v", p)
	}
}

// A project move can stop after any step. Each failure is injected in turn; the move is resumed by
// a fresh orchestrator and must end as an uninterrupted one does, with nothing irreversible done twice.
func TestProjectResumeAtEveryStep(t *testing.T) {
	type tc struct {
		name      string
		unhealthy bool
		fail      string
	}
	cases := []tc{
		{name: "planned/stop", fail: "stop n1/" + refA},
		{name: "planned/promote", fail: "promote n2/"},
		{name: "planned/promote answered", fail: "promote-after n2/"},
		{name: "planned/home", fail: "registry.SetProjectNode"},
		{name: "planned/old home's row", fail: "registry.CreateReplica"},
		{name: "planned/start", fail: "start n2/"},
		{name: "planned/tenant", fail: "fleet.ensure"},
		{name: "planned/demote", fail: "demote n1/"},
		{name: "unplanned/fence", unhealthy: true, fail: "local.stop " + refA},
		{name: "unplanned/promote", unhealthy: true, fail: "promote n2/"},
		{name: "unplanned/promote answered", unhealthy: true, fail: "promote-after n2/"},
		{name: "unplanned/home", unhealthy: true, fail: "registry.SetProjectNode"},
		{name: "unplanned/start", unhealthy: true, fail: "start n2/"},
		{name: "unplanned/tenant", unhealthy: true, fail: "fleet.ensure"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			if c.unhealthy {
				w.prim["n1/"+refA].healthy = false
			}
			w.fail(c.fail, errors.New("injected"), -1)
			mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA})
			if err == nil {
				t.Fatalf("no error although %q fails", c.fail)
			}
			if mv.State != registry.MoveFailed {
				t.Fatalf("move: %+v", mv)
			}
			if p := projectOf(t, w, refA); p.Status == registry.StatusRestarting {
				t.Fatalf("nothing is restarting the project any more: %+v", p)
			}
			w.clearFailures()
			// A new process: nothing is remembered but the registry and the world.
			mv, err = w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA, Resume: true})
			if err != nil {
				t.Fatalf("resume: %v\n%v", err, w.snapshot())
			}
			if mv.State != registry.MoveDone {
				t.Fatalf("resumed move: %+v", mv)
			}
			p := projectOf(t, w, refA)
			if p.NodeID != "n2" || p.Status != registry.StatusActiveHealthy {
				t.Fatalf("project: %+v", p)
			}
			// The stop ran once (twice when the stop itself was what failed). The node is asked to promote
			// again after a promotion that failed, and after one that was answered but not recorded: its
			// promotion is repeatable, finds the cluster promoted and starts nothing twice, and it is
			// what finishes a promotion that an earlier try left halfway.
			if n := w.count("stop n1/" + refA); n > 1 && !strings.HasPrefix(c.fail, "stop ") {
				t.Errorf("the old primary was stopped %d times", n)
			}
			if n := w.count("promote n2/"); n > 2 {
				t.Errorf("%d promotions", n)
			}
			reps, _ := w.reg.ListReplicas(w.ctx, refA)
			if !c.unhealthy && (len(reps) != 1 || reps[0].NodeID != "n1") {
				t.Errorf("replica rows after a switchover: %+v", reps)
			}
			if mvs, _ := w.reg.ListMoves(w.ctx, "", 10); len(mvs) != 1 {
				t.Errorf("%d moves rows, want the one that was resumed", len(mvs))
			}
		})
	}
}

// A paused project has no answering primary on purpose. It is switched over, not failed over: nothing
// is fenced or set aside, the replica waits for the stopped cluster's last checkpoint, and the project
// is paused on its new home like it was.
func TestAPausedProjectIsSwitchedOverAndStaysPaused(t *testing.T) {
	w := newWorld(t)
	must(t, w.reg.SetProjectStatus(w.ctx, refA, registry.StatusInactive))
	w.prim["n1/"+refA] = &primState{} // stopped by the pause: not running, not healthy
	o := w.orch()

	pl, err := o.PlanProject(w.ctx, ProjectOptions{Ref: refA})
	if err != nil {
		t.Fatal(err)
	}
	if pl.Kind != string(registry.MoveSwitchover) || len(pl.Refused(false)) != 0 {
		t.Fatalf("plan: %+v", pl)
	}
	if c := findCheck(t, pl, "primary"); !strings.Contains(c.Detail, "paused") {
		t.Fatalf("check: %+v", c)
	}

	mv, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA, ExpectKind: string(registry.MoveSwitchover)})
	if err != nil || mv.State != registry.MoveDone || mv.Kind != registry.MoveSwitchover {
		t.Fatalf("move %+v, error %v\n%v", mv, err, w.snapshot())
	}
	w.assertNever("fence ")
	w.assertNever("local.stop")
	w.assertNever("aside ")
	w.assertOrder("stop n1/"+refA, "promote n2/"+idAN2+" epoch=1 wait="+w.lsn[refA], "registry.SetProjectNode "+refA+" n2 1", "stop n2/"+refA, "demote n1/")
	w.assertNever("start n2/" + refA)
	w.assertNever("fleet.ensure " + refA)
	if p := projectOf(t, w, refA); p.NodeID != "n2" || p.Status != registry.StatusInactive {
		t.Fatalf("project: %+v", p)
	}
	if r := replicaOn(t, w, refA, "n1"); r == nil {
		t.Fatal("the old home is a replica of the new one")
	}
	// A project that is not paused and does not answer is still failed over.
	w2 := newWorld(t)
	w2.prim["n1/"+refA].healthy = false
	if pl, _ := w2.orch().PlanProject(w2.ctx, ProjectOptions{Ref: refA}); pl.Kind != string(registry.MoveFailover) {
		t.Fatalf("plan: %+v", pl)
	}
}

// The old home refuses to set its data aside while its registry copy still homes the project there; the
// copy follows the leader by a moment, so the move asks again.
func TestTheOldHomeIsAskedAgainWhileItsRegistryTrails(t *testing.T) {
	trailing := &mesh.RemoteError{Node: "n1", Status: http.StatusConflict, Code: CodeHomedHere, Message: "the registry homes it on this node"}
	run := func(t *testing.T, refusals int) (*world, string) {
		w := newWorld(t)
		w.prim["n1/"+refA].healthy = false
		w.fail("aside n1/"+refA, trailing, refusals)
		mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA})
		if err != nil || mv.State != registry.MoveDone {
			t.Fatalf("move %+v, error %v", mv, err)
		}
		return w, w.stepDetail(mv, "reseed-old")
	}
	w, detail := run(t, 3)
	if n := w.count("aside n1/" + refA); n != 4 || strings.HasPrefix(detail, "warning:") {
		t.Fatalf("%d tries, step %q", n, detail)
	}
	w, detail = run(t, -1)
	if n := w.count("aside n1/" + refA); n != asideAttempts || !strings.HasPrefix(detail, "warning: the old primary's data") || !strings.Contains(detail, "supavise replicas add") {
		t.Fatalf("%d tries, step %q", n, detail)
	}
	// Another refusal is not waited for.
	w = newWorld(t)
	w.prim["n1/"+refA].healthy = false
	w.fail("aside n1/"+refA, errors.New("permission denied"), -1)
	mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA})
	if err != nil || w.count("aside n1/"+refA) != 1 || !strings.HasPrefix(w.stepDetail(mv, "reseed-old"), "warning:") {
		t.Fatalf("move %+v, error %v, %d tries", mv, err, w.count("aside n1/"+refA))
	}
}

// What a node answers to a promotion says what happened to it. The placement layer gives the
// sentinels back through errors.Is: a node that did not reach the position promoted nothing, so the
// old primary starts again; a node at a higher epoch refused a leader that has been replaced, and
// repeating the call changes nothing.
func TestAPromotionThatTheNodeAnswersWithASentinelIsClassified(t *testing.T) {
	t.Run("the replica did not reach the position: nothing was promoted", func(t *testing.T) {
		w := newWorld(t)
		w.fail("promote n2/", fmt.Errorf("%w: waiting for %s: not reached within 2m0s", lifecycle.ErrReplayBehind, w.lsn[refA]), -1)
		mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA})
		if !errors.Is(err, ErrReplayBehind) || mv.State != registry.MoveAborted {
			t.Fatalf("move %+v, error %v", mv, err)
		}
		w.assertOrder("stop n1/"+refA, "promote n2/", "start n1/"+refA)
		if p := projectOf(t, w, refA); p.NodeID != "n1" || p.Status != registry.StatusActiveHealthy {
			t.Fatalf("project: %+v", p)
		}
	})
	t.Run("a node that refuses the promotion changed nothing", func(t *testing.T) {
		for name, refusal := range map[string]error{
			"the node is fenced":          fmt.Errorf("%w: the node is fenced", lifecycle.ErrInvalidState),
			"the cluster is no standby":   fmt.Errorf("%w: no standby.signal", lifecycle.ErrNotStandby),
			"the state does not allow it": fmt.Errorf("%w: still being set up", lifecycle.ErrInvalidState),
			"the project is not homed":    fmt.Errorf("%w: refused", placement.ErrNotHome),
			"the replica is not there":    fmt.Errorf("%w: no such replica", registry.ErrNotFound),
		} {
			w := newWorld(t)
			w.fail("promote n2/", refusal, -1)
			mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA})
			if err == nil || mv.State != registry.MoveAborted || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("%s: move %+v, error %v", name, mv, err)
			}
			w.assertOrder("stop n1/"+refA, "promote n2/", "start n1/"+refA)
			if p := projectOf(t, w, refA); p.NodeID != "n1" || p.Status != registry.StatusActiveHealthy {
				t.Fatalf("%s: project %+v", name, p)
			}
		}
	})
	t.Run("a node that does not take the leader for the leader is not asked to settle it", func(t *testing.T) {
		w := newWorld(t)
		w.fail("promote n2/", fmt.Errorf("%w: the caller is not the leader", cluster.ErrNotLeader), -1)
		mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA})
		if err == nil || mv.State != registry.MoveFailed {
			t.Fatalf("move %+v, error %v", mv, err)
		}
		w.assertNever("start n1/")
	})
	t.Run("a failure in the middle may have promoted", func(t *testing.T) {
		w := newWorld(t)
		w.fail("promote n2/", errors.New("node n2: pg_promote: connection reset"), -1)
		mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA})
		if err == nil || mv.State != registry.MoveFailed {
			t.Fatalf("move %+v, error %v", mv, err)
		}
		w.assertNever("start n1/")
	})
	t.Run("an unplanned failover has no position to wait for", func(t *testing.T) {
		w := newWorld(t)
		w.prim["n1/"+refA].healthy = false
		w.fail("promote n2/", fmt.Errorf("%w: late", lifecycle.ErrReplayBehind), -1)
		mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA})
		if errors.Is(err, ErrReplayBehind) || mv.State != registry.MoveFailed {
			t.Fatalf("move %+v, error %v", mv, err)
		}
		w.assertNever("start n1/")
	})
	t.Run("the node is at a higher epoch", func(t *testing.T) {
		w := newWorld(t)
		w.fail("promote n2/", fmt.Errorf("%w: the request is under epoch 1 and this node is at 3", placement.ErrStaleEpoch), -1)
		mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA})
		if !errors.Is(err, ErrEpochLost) || mv.State != registry.MoveFailed || !strings.Contains(err.Error(), "epoch 1") {
			t.Fatalf("move %+v, error %v", mv, err)
		}
		if n := w.count("promote n2/"); n != 1 {
			t.Fatalf("a refusal for the epoch was repeated %d times", n)
		}
		w.assertNever("start n1/") // the node may be a primary under the other leader: the old one stays down
	})
}

// engineLike plays the lifecycle engine as the orchestrator meets it: its project lock is not
// reentrant, and its registration of a project with the shared services takes that lock and does
// nothing for a project that is not active.
type engineLike struct {
	w       *world
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	held    map[string]bool
	skipped []string
	locked  []string // refs registered while the move held their lock: a deadlock in the real engine
	ensured []string
}

func newEngineLike(w *world) *engineLike {
	return &engineLike{w: w, locks: map[string]*sync.Mutex{}, held: map[string]bool{}}
}

func (e *engineLike) lockOf(ref string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.locks[ref] == nil {
		e.locks[ref] = &sync.Mutex{}
	}
	return e.locks[ref]
}

func (e *engineLike) Lock(_ context.Context, ref string) (func(), error) {
	m := e.lockOf(ref)
	m.Lock()
	e.mu.Lock()
	e.held[ref] = true
	e.mu.Unlock()
	return func() {
		e.mu.Lock()
		e.held[ref] = false
		e.mu.Unlock()
		m.Unlock()
	}, nil
}

func (e *engineLike) isHeld(ref string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.held[ref]
}

func (e *engineLike) QuiesceTenant(_ context.Context, ref string) error {
	return e.w.do("fleet.quiesce %s", ref)
}

func (e *engineLike) EnsureTenant(_ context.Context, ref string) error {
	if err := e.w.do("fleet.ensure %s", ref); err != nil {
		return err
	}
	m := e.lockOf(ref)
	if !m.TryLock() {
		e.mu.Lock()
		e.locked = append(e.locked, ref)
		e.mu.Unlock()
		return errors.New("the project's lock is held: the real engine would wait for ever")
	}
	defer m.Unlock()
	p, err := e.w.reg.GetProject(e.w.ctx, ref)
	if err != nil {
		return err
	}
	if p.Status != registry.StatusActiveHealthy && p.Status != registry.StatusActiveUnhealthy {
		e.mu.Lock()
		e.skipped = append(e.skipped, ref+" "+string(p.Status))
		e.mu.Unlock()
		return nil // its own operation registers it
	}
	e.mu.Lock()
	e.ensured = append(e.ensured, ref)
	e.mu.Unlock()
	return nil
}

// The engine's registration takes the project's lock and skips a project that is not active, so a move
// that held the lock across it would wait for itself, and one that called it while the project was
// RESTARTING or COMING_UP would register nothing and say nothing.
func TestTheMoveRegistersTheProjectWithTheEngineWhileItIsActiveAndNotUnderItsOwnLock(t *testing.T) {
	t.Run("a project switchover", func(t *testing.T) {
		w := newWorld(t)
		eng := newEngineLike(w)
		var heldAtDemote, heldAtEnsure bool
		w.afterEvent("demote n1/", func() { heldAtDemote = eng.isHeld(refA) })
		w.afterEvent("fleet.ensure "+refA, func() { heldAtEnsure = eng.isHeld(refA) })
		o := w.orch(func(d *Deps) { d.Fleet, d.Locks = eng, eng })
		mv, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA})
		if err != nil || mv.State != registry.MoveDone {
			t.Fatalf("move %+v, error %v", mv, err)
		}
		if len(eng.locked) != 0 || heldAtEnsure {
			t.Fatalf("registered under the move's lock: %v (held %v)", eng.locked, heldAtEnsure)
		}
		if len(eng.skipped) != 0 || len(eng.ensured) != 1 || eng.ensured[0] != refA {
			t.Fatalf("registered %v, skipped %v", eng.ensured, eng.skipped)
		}
		if !heldAtDemote || eng.isHeld(refA) {
			t.Fatalf("the lock is taken again after the registration (%v) and let go at the end (%v)", heldAtDemote, eng.isHeld(refA))
		}
		if p := projectOf(t, w, refA); p.Status != registry.StatusActiveHealthy || p.NodeID != "n2" {
			t.Fatalf("project: %+v", p)
		}
	})
	t.Run("a project failover", func(t *testing.T) {
		w := newWorld(t)
		w.prim["n1/"+refA].healthy = false
		eng := newEngineLike(w)
		o := w.orch(func(d *Deps) { d.Fleet, d.Locks = eng, eng })
		if mv, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA}); err != nil || mv.State != registry.MoveDone {
			t.Fatalf("move %+v, error %v", mv, err)
		}
		if len(eng.locked) != 0 || len(eng.skipped) != 0 || len(eng.ensured) != 1 {
			t.Fatalf("registered %v, skipped %v, under the lock %v", eng.ensured, eng.skipped, eng.locked)
		}
	})
	t.Run("a switchover that is undone", func(t *testing.T) {
		w := newWorld(t)
		eng := newEngineLike(w)
		w.fail("promote n2/", fmt.Errorf("%w: late", lifecycle.ErrReplayBehind), -1)
		o := w.orch(func(d *Deps) { d.Fleet, d.Locks = eng, eng })
		mv, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA})
		if !errors.Is(err, ErrReplayBehind) || mv.State != registry.MoveAborted {
			t.Fatalf("move %+v, error %v", mv, err)
		}
		if len(eng.locked) != 0 || len(eng.skipped) != 0 || len(eng.ensured) != 1 {
			t.Fatalf("registered %v, skipped %v, under the lock %v", eng.ensured, eng.skipped, eng.locked)
		}
		if eng.isHeld(refA) {
			t.Fatal("the lock outlives the move")
		}
	})
	t.Run("a paused project is not registered", func(t *testing.T) {
		w := newWorld(t)
		must(t, w.reg.SetProjectStatus(w.ctx, refA, registry.StatusInactive))
		w.prim["n1/"+refA] = &primState{}
		eng := newEngineLike(w)
		o := w.orch(func(d *Deps) { d.Fleet, d.Locks = eng, eng })
		if mv, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA}); err != nil || mv.State != registry.MoveDone {
			t.Fatalf("move %+v, error %v", mv, err)
		}
		if len(eng.ensured)+len(eng.skipped)+len(eng.locked) != 0 {
			t.Fatalf("registered %v, skipped %v", eng.ensured, eng.skipped)
		}
		if p := projectOf(t, w, refA); p.Status != registry.StatusInactive {
			t.Fatalf("project: %+v", p)
		}
	})
	t.Run("a pause that got in while the move let go of the lock is kept", func(t *testing.T) {
		w := newWorld(t)
		eng := newEngineLike(w)
		w.afterEvent("fleet.ensure "+refA, func() { must(t, w.reg.SetProjectStatus(w.ctx, refA, registry.StatusInactive)) })
		o := w.orch(func(d *Deps) { d.Fleet, d.Locks = eng, eng })
		if mv, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA}); err != nil || mv.State != registry.MoveDone {
			t.Fatalf("move %+v, error %v", mv, err)
		}
		if p := projectOf(t, w, refA); p.Status != registry.StatusInactive {
			t.Fatalf("the move overwrote a pause: %+v", p)
		}
	})
}

// A server move holds no project lock, and still registers a project only while it is active.
func TestAServerMoveRegistersEachProjectWithTheEngineWhileItIsActive(t *testing.T) {
	w := serverWorld(t)
	eng := newEngineLike(w)
	o := w.orch(func(d *Deps) { d.Fleet = eng })
	mv, err := o.FailoverServer(w.ctx, ServerOptions{})
	if err != nil || mv.State != registry.MoveDone {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	if len(eng.skipped) != 0 || len(eng.locked) != 0 || len(eng.ensured) != 2 {
		t.Fatalf("registered %v, skipped %v", eng.ensured, eng.skipped)
	}
}

// A paused project had its cluster stopped before the move. A switchover that is undone leaves it so:
// it is not started, and its status comes back.
func TestAnAbortedSwitchoverOfAPausedProjectDoesNotStartIt(t *testing.T) {
	w := newWorld(t)
	must(t, w.reg.SetProjectStatus(w.ctx, refA, registry.StatusInactive))
	w.prim["n1/"+refA] = &primState{}
	w.replay[idAN2] = "0/1000000" // the standby is far behind the cluster's last checkpoint
	o := w.orch()
	mv, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA})
	if !errors.Is(err, ErrReplayBehind) || mv.State != registry.MoveAborted {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	w.assertNever("start n1/")
	w.assertNever("fleet.ensure")
	w.assertNever("promote")
	if p := projectOf(t, w, refA); p.NodeID != "n1" || p.Status != registry.StatusInactive {
		t.Fatalf("project: %+v", p)
	}
	if !hasStep(mv, "undo") {
		t.Fatalf("steps %v", stepNames(mv))
	}
	// It moves once the standby has caught up.
	w.replay[idAN2] = caughtUp(w.lsn[refA])
	if mv, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA}); err != nil || mv.State != registry.MoveDone {
		t.Fatalf("a new try: %+v, %v", mv, err)
	}
}

// The answers that count as a refusal also come after the node has promoted (the restart on the canonical
// port fails with an invalid state, say). The old primary starts again only while the node's instance
// still is a standby that is up; otherwise it stays down, because two primaries are worse.
func TestARefusalAfterThePromotionRanDoesNotStartTheOldPrimary(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  func(w *world)
		undone bool
	}{
		"the instance is still a standby": {setup: func(w *world) { w.fail("promote n2/", fmt.Errorf("%w: refused", placement.ErrNotHome), -1) }, undone: true},
		"the instance promoted": {setup: func(w *world) {
			w.fail("promote-after n2/"+idAN2, fmt.Errorf("%w: restarting on the canonical port", lifecycle.ErrInvalidState), -1)
		}},
		"the instance cannot be looked at": {setup: func(w *world) {
			w.fail("promote n2/", fmt.Errorf("%w: refused", placement.ErrNotHome), -1)
			w.afterEvent("promote n2/", func() { w.down["n2"] = true }) // the node stops answering after the refusal
		}},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			tc.setup(w)
			mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA})
			if err == nil {
				t.Fatalf("move %+v", mv)
			}
			switch {
			case tc.undone && (mv.State != registry.MoveAborted || !w.has("start n1/"+refA)):
				t.Fatalf("move %+v, error %v\n%v", mv, err, w.snapshot())
			case !tc.undone && (mv.State != registry.MoveFailed || w.has("start n1/")):
				t.Fatalf("move %+v, error %v\n%v", mv, err, w.snapshot())
			}
		})
	}
}

// The project is in the move for its whole length except while the shared services take it: after that it
// shows RESTARTING again, so that what looks at the status and not at the lock still finds it moving.
func TestTheProjectIsMovingAgainAfterTheSharedServicesTookIt(t *testing.T) {
	w := newWorld(t)
	var during, between registry.Status
	w.afterEvent("fleet.ensure "+refA, func() { during = projectOf(t, w, refA).Status })
	w.afterEvent("demote n1/", func() { between = projectOf(t, w, refA).Status })
	if mv, err := w.orch().FailoverProject(w.ctx, ProjectOptions{Ref: refA}); err != nil || mv.State != registry.MoveDone {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	if during != registry.StatusActiveHealthy || between != registry.StatusRestarting {
		t.Fatalf("status during the registration %s, after it %s", during, between)
	}
	if p := projectOf(t, w, refA); p.Status != registry.StatusActiveHealthy {
		t.Fatalf("project: %+v", p)
	}
}
