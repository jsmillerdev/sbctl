package failover

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	svc := &fakeService{}
	c, cancel := rigFor(t, svc)
	return svc, c, cancel
}

// rigFor serves svc on a real unix socket.
func rigFor(t *testing.T, svc Service) (Client, context.CancelFunc) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fo")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "c.sock")
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
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", fi.Mode())
	}
	if di, err := os.Stat(dir); err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("socket directory: %v, %v", di, err)
	}
	return Client{Path: path}, cancel
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
		"changed": {ErrPlanChanged, ErrPlanChanged, "plan changed"},
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
	// No socket: the server is not in a cluster, or the daemon does not run.
	if _, err := c.Readiness(context.Background()); err == nil || !strings.Contains(err.Error(), "not part of a cluster, or supavise is not running") {
		t.Fatalf("error: %v", err)
	}
	// A socket nobody listens on: the daemon is there and does not answer.
	dead := filepath.Join(t.TempDir(), "dead.sock")
	ln, err := net.Listen("unix", dead)
	must(t, err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false) // the file stays, as a daemon that died leaves it
	ln.Close()
	if _, err := (Client{Path: dead}).Readiness(context.Background()); err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("error: %v", err)
	}
}

// What the operator confirmed travels to the daemon, which holds the run to it.
func TestTheConfirmedPlanTravelsWithTheRun(t *testing.T) {
	svc, c, _ := controlRig(t)
	svc.run = func(context.Context, func(string, string)) (*registry.Move, error) {
		return &registry.Move{ID: 1, State: registry.MoveDone}, nil
	}
	if _, err := c.RunServer(context.Background(), ServerOptions{ExpectKind: "switchover", ExpectEpoch: 4}, nil); err != nil {
		t.Fatal(err)
	}
	if svc.gotServer.ExpectKind != "switchover" || svc.gotServer.ExpectEpoch != 4 {
		t.Fatalf("server options: %+v", svc.gotServer)
	}
	if _, err := c.RunProject(context.Background(), ProjectOptions{Ref: refA, ExpectKind: "failover"}, nil); err != nil {
		t.Fatal(err)
	}
	if svc.gotProj.ExpectKind != "failover" {
		t.Fatalf("project options: %+v", svc.gotProj)
	}
}

// A client that is connected and does not read stalls nothing: the steps of the move do not wait
// for it, and the end of the move still reaches it with every step it has not read yet.
func TestAClientThatDoesNotReadDoesNotStallTheMove(t *testing.T) {
	svc, c, _ := controlRig(t)
	finished := make(chan struct{})
	svc.run = func(ctx context.Context, report func(string, string)) (*registry.Move, error) {
		for i := 0; i < 600; i++ { // more than the buffer holds
			report(fmt.Sprintf("step-%d", i), "")
		}
		close(finished)
		return &registry.Move{ID: 1, State: registry.MoveDone}, nil
	}
	got := make(chan int, 1)
	block := make(chan struct{})
	go func() {
		n := 0
		_, _ = c.RunServer(context.Background(), ServerOptions{}, func(registry.MoveStep) {
			if n == 0 {
				<-block // the terminal is paused
			}
			n++
		})
		got <- n
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the move waited for a client that does not read")
	}
	close(block)
	select {
	case n := <-got:
		if n == 0 {
			t.Fatal("the client saw no step")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client never got the end of the move")
	}
}

func TestSocketPathTooLong(t *testing.T) {
	err := (&ControlServer{Svc: &fakeService{}}).Serve(context.Background(), "/"+strings.Repeat("a", 120)+"/c.sock")
	if err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("error: %v", err)
	}
}

// followingService is a Service that also keeps the run of a daemon that restarted.
type followingService struct {
	*fakeService
	mu       sync.Mutex
	answers  []ServerStatus
	asked    []string
	askedFor int64
}

func (f *followingService) Follow(_ context.Context, from int, epoch int64) ServerStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, fmt.Sprintf("%d@%d", from, epoch))
	f.askedFor = epoch
	st := f.answers[0]
	if len(f.answers) > 1 {
		f.answers = f.answers[1:]
	}
	return st
}

// The CLI whose connection the restart cut asks the daemon that starts for the move, from where it
// stopped, and waits out the time the daemon needs to come up and pick the move up.
func TestFollowShowsTheMoveTheDaemonContinuedAfterItsRestart(t *testing.T) {
	followPollWas, followPatienceWas := followPoll, followPatience
	followPoll, followPatience = 5*time.Millisecond, 5*time.Second
	defer func() { followPoll, followPatience = followPollWas, followPatienceWas }()
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc := &followingService{fakeService: &fakeService{}, answers: []ServerStatus{
		{State: "idle"}, // up, and the move is not picked up yet
		{State: "running", Steps: []stepJSON{{Name: "leader", At: at, Detail: "standby leads at epoch 2"}}, Next: 1},
		{State: "running", Next: 1},
		{State: "done", Steps: []stepJSON{{Name: "dns", At: at}}, Next: 2, Move: &moveJSON{ID: 5, Scope: "server", Kind: "switchover", From: "n1", To: "n2", Epoch: 2, State: "done"}},
	}}
	c, _ := rigFor(t, svc)
	var seen []string
	mv, err := c.Follow(context.Background(), 2, func(s registry.MoveStep) { seen = append(seen, s.Name) })
	if err != nil || mv == nil || mv.State != registry.MoveDone || mv.Epoch != 2 {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	if strings.Join(seen, ",") != "leader,dns" {
		t.Fatalf("steps: %v", seen)
	}
	if svc.askedFor != 2 || svc.asked[0] != "0@2" || svc.asked[len(svc.asked)-1] != "1@2" {
		t.Fatalf("asked %v", svc.asked)
	}

	t.Run("a move that failed", func(t *testing.T) {
		svc.answers = []ServerStatus{{State: "failed", Error: "1 step(s) did not finish", Move: &moveJSON{ID: 5, State: "failed"}}}
		mv, err := c.Follow(context.Background(), 2, nil)
		var re *RemoteError
		if !errors.As(err, &re) || !strings.Contains(err.Error(), "did not finish") || mv == nil || mv.State != registry.MoveFailed {
			t.Fatalf("move %+v, error %v", mv, err)
		}
	})
	t.Run("a daemon that has no move", func(t *testing.T) {
		idleWas := followIdleGrace
		followIdleGrace = 0
		defer func() { followIdleGrace = idleWas }()
		svc.answers = []ServerStatus{{State: "idle"}}
		if _, err := c.Follow(context.Background(), 2, nil); !errors.Is(err, ErrNothingRunning) {
			t.Fatalf("error: %v", err)
		}
	})
	t.Run("a daemon that never comes back", func(t *testing.T) {
		followPatience = 50 * time.Millisecond
		_, err := Client{Path: filepath.Join(os.TempDir(), "no-such-socket-failover")}.Follow(context.Background(), 2, nil)
		if err == nil || !strings.Contains(err.Error(), "did not answer again") {
			t.Fatalf("error: %v", err)
		}
	})
}

// A Service without a run to follow (a test double, or a daemon without an orchestrator) says idle.
func TestFollowOfAServiceThatKeepsNoRunIsIdle(t *testing.T) {
	c, _ := rigFor(t, &fakeService{})
	idleWas := followIdleGrace
	followIdleGrace = 0
	defer func() { followIdleGrace = idleWas }()
	followPollWas := followPoll
	followPoll = time.Millisecond
	defer func() { followPoll = followPollWas }()
	if _, err := c.Follow(context.Background(), 2, nil); !errors.Is(err, ErrNothingRunning) {
		t.Fatalf("error: %v", err)
	}
}
