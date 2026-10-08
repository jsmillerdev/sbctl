package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/registry"
)

// fakeFailover is the daemon's side of the control socket.
type fakeFailover struct {
	plan      *failover.Plan
	planErr   error
	steps     []registry.MoveStep
	move      *registry.Move
	runErr    error
	readiness failover.Readiness
	readyErr  error

	planned []string
	ran     []string
	gotSrv  failover.ServerOptions
	gotProj failover.ProjectOptions
}

func (f *fakeFailover) Readiness(context.Context) (failover.Readiness, error) {
	return f.readiness, f.readyErr
}

func (f *fakeFailover) PlanProject(_ context.Context, o failover.ProjectOptions) (*failover.Plan, error) {
	f.planned, f.gotProj = append(f.planned, "project"), o
	return f.plan, f.planErr
}

func (f *fakeFailover) PlanServer(_ context.Context, o failover.ServerOptions) (*failover.Plan, error) {
	f.planned, f.gotSrv = append(f.planned, "server"), o
	return f.plan, f.planErr
}

func (f *fakeFailover) RunProject(_ context.Context, o failover.ProjectOptions, on func(registry.MoveStep)) (*registry.Move, error) {
	f.ran, f.gotProj = append(f.ran, "project"), o
	for _, s := range f.steps {
		on(s)
	}
	return f.move, f.runErr
}

func (f *fakeFailover) RunServer(_ context.Context, o failover.ServerOptions, on func(registry.MoveStep)) (*registry.Move, error) {
	f.ran, f.gotSrv = append(f.ran, "server"), o
	for _, s := range f.steps {
		on(s)
	}
	return f.move, f.runErr
}

func withFailover(t *testing.T, f *fakeFailover) {
	t.Helper()
	configPath, _ = opsConfig(t, "")
	old := newFailoverClient
	newFailoverClient = func(*config.Config) failoverClient { return f }
	t.Cleanup(func() { newFailoverClient = old })
}

func goodPlan() *failover.Plan {
	lag := 0.4
	return &failover.Plan{
		Kind: "switchover", From: "n1", To: "n2", FromName: "primary", ToName: "standby", Epoch: 2,
		Checks: []failover.Check{
			{Name: "target node", OK: true, Blocking: true, Detail: "standby (n2) is active and takes over from primary"},
			{Name: "same release", OK: true, Blocking: true, Detail: "v0.2.0"},
			{Name: "replicas", OK: true, Blocking: true, Detail: "2 project(s) have a healthy replica within max_lag_seconds"},
			{Name: "projects without a replica", OK: true, Detail: "1: cccc will be restored from the archive"},
		},
		Projects: []failover.ProjectPlan{
			{Ref: "aaaaaaaaaaaaaaaaaaaa", Replica: "aaaaaaaaaaaaaaaaaaaa-rr-eu-west-1-a2a2a2", Node: "n2", LagSeconds: &lag},
			{Ref: "cccccccccccccccccccc", Node: "n2", RestoreFromArchive: true},
		},
		Notes: []string{"1 branch project(s) have no replica and stay on primary: dddd"},
	}
}

func badPlan() *failover.Plan {
	p := goodPlan()
	p.Checks = append(p.Checks,
		failover.Check{Name: "replica of aaaaaaaaaaaaaaaaaaaa", Blocking: true, Detail: "lag 45s is above max_lag_seconds (30)"},
		failover.Check{Name: "storage backend", Blocking: true, Hard: true, Detail: "[fleet] storage_backend is file"})
	return p
}

func doneMove() *registry.Move {
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	return &registry.Move{ID: 4, Scope: registry.MoveServer, Kind: registry.MoveSwitchover, FromNode: "n1", ToNode: "n2", Epoch: 2, State: registry.MoveDone,
		Steps: []registry.MoveStep{{Name: "begin", At: at}, {Name: "dns", At: at}}}
}

func TestFailoverDryRunPrintsEveryPrecondition(t *testing.T) {
	f := &fakeFailover{plan: goodPlan()}
	withFailover(t, f)
	out, err := run(t, "failover", "--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"Server switchover: primary -> standby (epoch 2)", "Preconditions", "target node", "same release", "replicas", "projects without a replica",
		"Projects", "aaaaaaaaaaaaaaaaaaaa", "lag 0.4s", "no replica: restored from the archive", "1 branch project(s)", "Dry run: nothing was changed.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if len(f.ran) != 0 {
		t.Fatalf("a dry run ran %v", f.ran)
	}
}

func TestFailoverDryRunFailsWhenTheMoveWouldBeRefused(t *testing.T) {
	f := &fakeFailover{plan: badPlan()}
	withFailover(t, f)
	out, err := run(t, "failover", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "would be refused") {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "storage_backend is file") || !strings.Contains(out, "above max_lag_seconds") {
		t.Fatalf("output:\n%s", out)
	}
	// --force overrides the lag and not the hard check.
	out, err = run(t, "failover", "--dry-run", "--force")
	if err == nil || !strings.Contains(out, "FAIL (forced)") {
		t.Fatalf("with force: %v\n%s", err, out)
	}
	f.plan = goodPlan()
	f.plan.Checks = append(f.plan.Checks, failover.Check{Name: "replica of aaaaaaaaaaaaaaaaaaaa", Blocking: true, Detail: "lag 45s is above max_lag_seconds (30)"})
	if out, err = run(t, "failover", "--dry-run", "--force"); err != nil || !strings.Contains(out, "FAIL (forced)") {
		t.Fatalf("a forced lag: %v\n%s", err, out)
	}
}

func TestFailoverRunsAndPrintsTheSteps(t *testing.T) {
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	f := &fakeFailover{plan: goodPlan(), move: doneMove(), steps: []registry.MoveStep{
		{Name: "begin", At: at, Detail: `{"projects":[]}`},
		{Name: "quiesce", At: at, Detail: "3 cluster(s) stopped"},
		{Name: "dns", At: at, Detail: "The service address 203.0.113.9 now belongs to standby; DNS needs no change."},
	}}
	withFailover(t, f)
	out, err := run(t, "failover", "--yes", "--force", "--restore-missing", "--old-primary-is-down", "--to", "n2")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Join(f.ran, ",") != "server" || !f.gotSrv.Force || !f.gotSrv.RestoreMissing || !f.gotSrv.OldPrimaryIsDown || f.gotSrv.To != "n2" || f.gotSrv.DryRun {
		t.Fatalf("ran %v with %+v", f.ran, f.gotSrv)
	}
	for _, want := range []string{"quiesce: 3 cluster(s) stopped", "DNS needs no change", "Done: the switchover of the server from n1 to n2 is finished (move 4)."} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, `{"projects"`) {
		t.Fatalf("the plan JSON of the first step is printed:\n%s", out)
	}
}

func TestFailoverAsksBeforeItActsAndWontGuess(t *testing.T) {
	f := &fakeFailover{plan: goodPlan(), move: doneMove()}
	withFailover(t, f)
	// Nobody types the name of the new leader: nothing is changed (with stdin not a terminal the
	// command says to use --yes; under test stdin is the null device, which reads as one that stays silent).
	out, err := run(t, "failover")
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Type the name of the new leader (standby)") {
		t.Fatalf("the question was not asked:\n%s", out)
	}
	if len(f.ran) != 0 {
		t.Fatalf("it ran without being asked: %v", f.ran)
	}
	if !strings.Contains(serverQuestion(f.plan), "stops primary cleanly and makes standby the leader (epoch 2)") {
		t.Fatalf("question: %s", serverQuestion(f.plan))
	}
	un := goodPlan()
	un.Kind = "failover"
	if q := serverQuestion(un); !strings.Contains(q, "fences primary") || !strings.Contains(q, "is lost") {
		t.Fatalf("question for a failover: %s", q)
	}
}

func TestFailoverRefusedByThePlanNeverRuns(t *testing.T) {
	f := &fakeFailover{plan: badPlan(), move: doneMove()}
	withFailover(t, f)
	_, err := run(t, "failover", "--yes")
	var re *failover.RefusedError
	if !errors.As(err, &re) || len(re.Checks) != 2 || !errors.Is(err, failover.ErrRefused) {
		t.Fatalf("error: %v", err)
	}
	if len(f.ran) != 0 {
		t.Fatalf("it ran: %v", f.ran)
	}
}

func TestFailoverResumeSkipsTheLagCheckLikeTheDaemon(t *testing.T) {
	p := goodPlan()
	p.Checks = append(p.Checks, failover.Check{Name: "replica of aaaaaaaaaaaaaaaaaaaa", Blocking: true, Detail: "lag 45s"})
	f := &fakeFailover{plan: p, move: doneMove()}
	withFailover(t, f)
	if _, err := run(t, "failover", "--resume", "--yes"); err != nil {
		t.Fatal(err)
	}
	if !f.gotSrv.Resume || len(f.ran) != 1 {
		t.Fatalf("ran %v with %+v", f.ran, f.gotSrv)
	}
}

func TestFailoverReportsAMoveThatStopped(t *testing.T) {
	mv := doneMove()
	mv.State = registry.MoveFailed
	f := &fakeFailover{plan: goodPlan(), move: mv, runErr: errors.New("1 step(s) did not finish: project aaaa: boom")}
	withFailover(t, f)
	out, err := run(t, "failover", "--yes")
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(out, "The move (id 4) is failed after dns.") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestFailoverOnAServerThatIsNotInAClusterSaysSo(t *testing.T) {
	f := &fakeFailover{planErr: failover.ErrNoCluster}
	withFailover(t, f)
	_, err := run(t, "failover", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "not part of a cluster") {
		t.Fatalf("error: %v", err)
	}
}

func TestProjectFailover(t *testing.T) {
	p := &failover.Plan{Kind: "switchover", Ref: "aaaaaaaaaaaaaaaaaaaa", From: "n1", To: "n2", FromName: "primary", ToName: "standby", Epoch: 1,
		Checks: []failover.Check{{Name: "replica", OK: true, Blocking: true, Detail: "ok"}}}
	mv := &registry.Move{ID: 9, Scope: registry.MoveProject, Ref: "aaaaaaaaaaaaaaaaaaaa", Kind: registry.MoveSwitchover, FromNode: "n1", ToNode: "n2", State: registry.MoveDone}
	f := &fakeFailover{plan: p, move: mv, steps: []registry.MoveStep{{Name: "promote", Detail: "primary on standby", At: time.Now()}}}
	withFailover(t, f)

	out, err := run(t, "projects", "failover", "aaaaaaaaaaaaaaaaaaaa", "--to", "n2", "--dry-run")
	if err != nil || !strings.Contains(out, "Project aaaaaaaaaaaaaaaaaaaa switchover: primary -> standby") || len(f.ran) != 0 || f.gotProj.To != "n2" {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	// A project move asks nothing: it stops one project for seconds.
	out, err = run(t, "projects", "failover", "aaaaaaaaaaaaaaaaaaaa", "--force", "--resume")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Join(f.ran, ",") != "project" || f.gotProj.Ref != "aaaaaaaaaaaaaaaaaaaa" || !f.gotProj.Force || !f.gotProj.Resume {
		t.Fatalf("ran %v with %+v", f.ran, f.gotProj)
	}
	if !strings.Contains(out, "promote: primary on standby") || !strings.Contains(out, "Done: the switchover of project aaaaaaaaaaaaaaaaaaaa from n1 to n2 is finished (move 9).") {
		t.Fatalf("output:\n%s", out)
	}
	f.plan = badPlan()
	f.plan.Ref = "aaaaaaaaaaaaaaaaaaaa"
	f.ran = nil
	if _, err := run(t, "projects", "failover", "aaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, failover.ErrRefused) || len(f.ran) != 0 {
		t.Fatalf("refused: %v, ran %v", err, f.ran)
	}
}

func TestFailoverStatusBlock(t *testing.T) {
	f := &fakeFailover{readiness: failover.Readiness{Ready: true, Mode: "manual", Fencer: "aws", FencerStatus: "DryRun OK", EpochMarker: "store reachable"}}
	withFailover(t, f)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// No daemon, no socket: there is no block, and no error either.
	if r, err := failoverStatus(context.Background(), cfg); r != nil || err != nil {
		t.Fatalf("without a socket: %+v, %v", r, err)
	}
	sock := failover.ControlSocket(cfg)
	if err := os.MkdirAll(filepath.Dir(sock), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := failoverStatus(context.Background(), cfg)
	if err != nil || r == nil || !r.Ready || r.Fencer != "aws" {
		t.Fatalf("with a daemon: %+v, %v", r, err)
	}
	f.readyErr = failover.ErrNoCluster
	if r, err := failoverStatus(context.Background(), cfg); r != nil || err != nil {
		t.Fatalf("a single server: %+v, %v", r, err)
	}
	f.readyErr = errors.New("the daemon timed out")
	if _, err := failoverStatus(context.Background(), cfg); err == nil {
		t.Fatal("an error was swallowed")
	}
}
