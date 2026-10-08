package failover

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/alerts"
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
	w.assertOrder("fence n1 epoch=1 ref="+refA, "promote n2/"+idAN2+" epoch=1 wait= drain=true",
		"registry.SetProjectNode "+refA, "start n2/"+refA, "fleet.ensure "+refA, "aside n1/"+refA, "replicas.setup "+refA+" on n1", "basebackup n2/"+refA)
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

func TestProjectFailoverWhenTheHomeDoesNotAnswerIsRefused(t *testing.T) {
	w := newWorld(t)
	w.down["n1"] = true
	o := w.orch()
	_, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA})
	var re *RefusedError
	if !errors.Is(err, ErrRefused) || !errors.As(err, &re) {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(err.Error(), "node failure") {
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
	// Nothing was promoted, the old primary runs again and the shared services have it back.
	w.assertOrder("stop n1/"+refA, "promote n2/", "start n1/"+refA, "fleet.ensure "+refA)
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
	w.replay[idAN2] = w.lsn[refA]
	if _, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refA}); err != nil {
		t.Fatalf("a new try after the abort: %v", err)
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
