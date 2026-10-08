package failover

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// fakeService is a Service whose runs the test steers.
type fakeService struct {
	mu        sync.Mutex
	readiness Readiness
	readyErr  error
	plan      *Plan
	planErr   error
	run       func(ctx context.Context, report func(string, string)) (*registry.Move, error)
	gotServer ServerOptions
	gotProj   ProjectOptions
}

func (f *fakeService) PlanProject(_ context.Context, o ProjectOptions) (*Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotProj = o
	return f.plan, f.planErr
}

func (f *fakeService) PlanServer(_ context.Context, o ServerOptions) (*Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotServer = o
	return f.plan, f.planErr
}

func (f *fakeService) Readiness(context.Context) (Readiness, error) { return f.readiness, f.readyErr }

func (f *fakeService) FailoverProject(ctx context.Context, o ProjectOptions) (*registry.Move, error) {
	f.mu.Lock()
	f.gotProj = o
	f.mu.Unlock()
	return f.run(ctx, func(n, d string) { reportStep(ctx, registry.MoveStep{Name: n, Detail: d}) })
}

func (f *fakeService) FailoverServer(ctx context.Context, o ServerOptions) (*registry.Move, error) {
	f.mu.Lock()
	f.gotServer = o
	f.mu.Unlock()
	return f.run(ctx, func(n, d string) { reportStep(ctx, registry.MoveStep{Name: n, Detail: d}) })
}

// controlRig serves the control socket of a fake service on a real unix socket.
func controlRig(t *testing.T) (*fakeService, Client, context.CancelFunc) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fo")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "c.sock")
	svc := &fakeService{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (&ControlServer{Svc: svc}).Serve(ctx, path) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode %v", fi.Mode())
	}
	return svc, Client{Path: path}, cancel
}

func TestControlSocketRoundTrips(t *testing.T) {
	svc, c, _ := controlRig(t)
	ctx := context.Background()

	svc.readiness = Readiness{Ready: false, Mode: "manual", Blockers: []string{"storage backend is file"}, Fencer: "aws", FencerStatus: "DryRun OK", EpochMarker: "store reachable"}
	r, err := c.Readiness(ctx)
	if err != nil || r.Mode != "manual" || len(r.Blockers) != 1 || r.FencerStatus != "DryRun OK" {
		t.Fatalf("readiness: %+v, %v", r, err)
	}
	svc.readyErr = ErrNoCluster
	if _, err := c.Readiness(ctx); !errors.Is(err, ErrNoCluster) {
		t.Fatalf("a single server: %v", err)
	}

	lag := 0.5
	svc.plan = &Plan{Kind: "switchover", From: "n1", To: "n2", FromName: "primary", ToName: "standby", Epoch: 2,
		Checks:   []Check{{Name: "same release", OK: true, Blocking: true, Detail: "v0.2.0"}, {Name: "storage backend", Blocking: true, Hard: true, Detail: "file"}},
		Projects: []ProjectPlan{{Ref: refA, Replica: idAN2, Node: "n2", LagSeconds: &lag}}, Notes: []string{"a note"}}
	pl, err := c.PlanServer(ctx, ServerOptions{To: "n2", Force: true, RestoreMissing: true, OldPrimaryIsDown: true, Resume: true})
	if err != nil || pl.ToName != "standby" || len(pl.Checks) != 2 || !pl.Checks[1].Hard || *pl.Projects[0].LagSeconds != 0.5 {
		t.Fatalf("plan: %+v, %v", pl, err)
	}
	if got := svc.gotServer; got.To != "n2" || !got.Force || !got.RestoreMissing || !got.OldPrimaryIsDown || !got.Resume {
		t.Fatalf("the daemon saw %+v", got)
	}
	if _, err := c.PlanProject(ctx, ProjectOptions{Ref: refA, To: "n2", Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := svc.gotProj; got.Ref != refA || got.To != "n2" || !got.Force {
		t.Fatalf("the daemon saw %+v", got)
	}
	svc.planErr = errors.New("registry: not found")
	if _, err := c.PlanProject(ctx, ProjectOptions{Ref: "x"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("plan error: %v", err)
	}
}

func TestRunStreamsTheStepsAndTheMove(t *testing.T) {
	svc, c, _ := controlRig(t)
	svc.run = func(ctx context.Context, report func(string, string)) (*registry.Move, error) {
		report("begin", "{}")
		report("quiesce", "2 cluster(s) stopped")
		return &registry.Move{ID: 7, Scope: registry.MoveServer, Kind: registry.MoveSwitchover, FromNode: "n1", ToNode: "n2", Epoch: 2, State: registry.MoveDone,
			Steps: []registry.MoveStep{{Name: "begin"}, {Name: "quiesce", Detail: "2 cluster(s) stopped"}}}, nil
	}
	var got []string
	mv, err := c.RunServer(context.Background(), ServerOptions{}, func(s registry.MoveStep) { got = append(got, s.Name+":"+s.Detail) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "begin:{},quiesce:2 cluster(s) stopped" {
		t.Fatalf("steps %v", got)
	}
	if mv.ID != 7 || mv.State != registry.MoveDone || mv.ToNode != "n2" || len(mv.Steps) != 2 || mv.Epoch != 2 {
		t.Fatalf("move: %+v", mv)
	}
	if !svc.gotServer.Yes {
		t.Fatal("the daemon asks nobody: the CLI did")
	}
	svc.run = func(ctx context.Context, report func(string, string)) (*registry.Move, error) {
		return &registry.Move{ID: 8, Scope: registry.MoveProject, Ref: refA, Kind: registry.MoveSwitchover, State: registry.MoveDone}, nil
	}
	if mv, err := c.RunProject(context.Background(), ProjectOptions{Ref: refA}, nil); err != nil || mv.Ref != refA {
		t.Fatalf("project: %+v, %v", mv, err)
	}
}

func TestRunErrorsKeepTheirMeaningAcrossTheSocket(t *testing.T) {
	svc, c, _ := controlRig(t)
	for name, tc := range map[string]struct {
		err  error
		is   error
		want string
	}{
		"refused": {&RefusedError{Checks: []Check{{Name: "storage backend", Detail: "file", Blocking: true, Hard: true}}}, ErrRefused, "storage backend"},
		"busy":    {ErrBusy, ErrBusy, "another move"},
		"nothing": {ErrNothingToResume, ErrNothingToResume, "unfinished"},
		"plain":   {errors.New("the registry is read-only"), nil, "read-only"},
	} {
		svc.run = func(context.Context, func(string, string)) (*registry.Move, error) {
			return &registry.Move{ID: 3, State: registry.MoveFailed}, tc.err
		}
		mv, err := c.RunServer(context.Background(), ServerOptions{}, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if tc.is != nil && !errors.Is(err, tc.is) {
			t.Errorf("%s: %v is not %v", name, err, tc.is)
		}
		if mv == nil || mv.ID != 3 {
			t.Errorf("%s: the move that was recorded is lost: %+v", name, mv)
		}
		var re *RemoteError
		if name == "refused" && (!errors.As(err, &re) || len(re.Checks) != 1 || !re.Checks[0].Hard) {
			t.Errorf("refused: the checks are lost: %+v", err)
		}
	}
}

// A move belongs to the daemon: a CLI that goes away does not stop it.
func TestARunSurvivesItsClient(t *testing.T) {
	svc, c, _ := controlRig(t)
	release := make(chan struct{})
	finished := make(chan struct{})
	svc.run = func(ctx context.Context, report func(string, string)) (*registry.Move, error) {
		report("begin", "")
		<-release
		report("late", "the client is gone") // must not block
		if ctx.Err() != nil {
			t.Error("the move's context ended with the request")
		}
		close(finished)
		return &registry.Move{ID: 1, State: registry.MoveDone}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	seen := make(chan struct{})
	go func() {
		_, _ = c.RunServer(ctx, ServerOptions{}, func(s registry.MoveStep) {
			if s.Name == "begin" {
				close(seen)
			}
		})
	}()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("no step arrived")
	}
	cancel() // the CLI goes away
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the move did not finish after its client left")
	}
}

func TestClientWithoutADaemonSaysSo(t *testing.T) {
	c := Client{Path: filepath.Join(t.TempDir(), "none.sock")}
	if _, err := c.Readiness(context.Background()); err == nil || !strings.Contains(err.Error(), "control socket") {
		t.Fatalf("error: %v", err)
	}
}

func TestSocketPathTooLong(t *testing.T) {
	err := (&ControlServer{Svc: &fakeService{}}).Serve(context.Background(), "/"+strings.Repeat("a", 120)+"/c.sock")
	if err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("error: %v", err)
	}
}
