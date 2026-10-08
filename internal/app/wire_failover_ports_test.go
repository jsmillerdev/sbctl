package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/registry"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func memberOf(role cluster.Role, epoch int64) cluster.Membership {
	return cluster.NewStatic(cluster.Snapshot{
		Self: registry.Node{ID: "n2", Name: "second"}, Nodes: []registry.Node{{ID: "n1"}, {ID: "n2"}},
		Leader: map[bool]string{true: "n2", false: "n1"}[role == cluster.RoleLeader], Epoch: epoch, Role: role,
	})
}

// The process that started after the promotion leads at the move's epoch and answers at once. The one
// that started as a follower cannot go on: it says the daemon restarts, holds back the failure the
// orchestrator will announce for that, and returns when its context ends or the wait is over.
func TestHandoffTakeover(t *testing.T) {
	var sent []alerts.Event
	tk := &handoffTakeover{m: memberOf(cluster.RoleLeader, 6), log: quiet(), wait: time.Hour,
		deliver: func(_ context.Context, ev alerts.Event) error { sent = append(sent, ev); return nil }}
	if err := tk.BecomeLeader(context.Background(), 6); err != nil {
		t.Fatalf("a leader at the move's epoch: %v", err)
	}
	if tk.handing.Load() {
		t.Fatal("a process that leads handed its move over")
	}

	// The follower process: the promotion has not reached its registry copy, so its epoch is the old one.
	tk = &handoffTakeover{m: memberOf(cluster.RoleLeader, 5), log: quiet(), wait: time.Hour, deliver: tk.deliver}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tk.BecomeLeader(ctx, 6) }()
	for !tk.handing.Load() {
		time.Sleep(time.Millisecond)
	}
	cancel() // the daemon is stopping
	err := <-done
	if !errors.Is(err, ErrRoleChanged) {
		t.Fatalf("BecomeLeader = %v, want ErrRoleChanged", err)
	}
	handed := "The failover of server from n1 to n2 stopped after promote-system: waiting for n2 to run as the leader: " + err.Error()
	tk.notify(context.Background(), alerts.Event{Kind: alerts.KindFailoverFailed, Detail: handed})
	tk.notify(context.Background(), alerts.Event{Kind: alerts.KindFailoverStarted})
	tk.notify(context.Background(), alerts.Event{Kind: alerts.KindFenced})
	// The same move failing for another reason is a failure.
	tk.notify(context.Background(), alerts.Event{Kind: alerts.KindFailoverFailed, Detail: "demoting n1: the node does not answer"})
	if len(sent) != 3 || sent[0].Kind != alerts.KindFailoverStarted || sent[1].Kind != alerts.KindFenced || sent[2].Kind != alerts.KindFailoverFailed {
		t.Fatalf("alerts sent: %+v; the failure of a handed-over move must not go out, and nothing else is held back", sent)
	}

	// A process that never handed anything over passes the failure on.
	sent = nil
	plain := &handoffTakeover{m: memberOf(cluster.RoleLeader, 6), log: quiet(), deliver: tk.deliver}
	plain.notify(context.Background(), alerts.Event{Kind: alerts.KindFailoverFailed, Detail: handed})
	if len(sent) != 1 {
		t.Fatalf("alerts sent: %+v", sent)
	}

	// The wait ends by itself when nothing restarts the daemon.
	tk = &handoffTakeover{m: memberOf(cluster.RoleLeader, 5), log: quiet(), wait: 20 * time.Millisecond}
	if err := tk.BecomeLeader(context.Background(), 6); !errors.Is(err, ErrRoleChanged) {
		t.Fatalf("BecomeLeader without a restart = %v", err)
	}
}

func TestProjectLockerSerializesAProjectInTheProcess(t *testing.T) {
	reg := registry.NewMemory()
	l := &projectLocker{reg: func() registry.Registry { return reg }}
	ctx := context.Background()
	unlock, err := l.Lock(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	// Another project is free.
	other, err := l.Lock(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	other()
	// The same project waits for the unlock, and gives up when its context ends.
	cctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := l.Lock(cctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second lock on the same project: %v", err)
	}
	var got atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		u, err := l.Lock(ctx, "a")
		if err == nil {
			got.Store(true)
			u()
		}
	}()
	time.Sleep(30 * time.Millisecond)
	if got.Load() {
		t.Fatal("the lock was taken while it was held")
	}
	unlock()
	wg.Wait()
	if !got.Load() {
		t.Fatal("the lock was never given after the unlock")
	}
}

// fakeService is a failover.Service that records a resume.
type fakeService struct {
	failover.Service
	plan    *failover.Plan
	planErr error
	mu      sync.Mutex
	resumed int
	resumeC chan struct{}
}

func (f *fakeService) PlanServer(_ context.Context, o failover.ServerOptions) (*failover.Plan, error) {
	if !o.Resume {
		return nil, errors.New("not a resume")
	}
	return f.plan, f.planErr
}

func (f *fakeService) FailoverServer(_ context.Context, o failover.ServerOptions) (*registry.Move, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o.Resume {
		f.resumed++
		select {
		case f.resumeC <- struct{}{}:
		default:
		}
	}
	return &registry.Move{}, nil
}

func (f *fakeService) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.resumed }

// A leader that a server move promoted finishes the move once its services and projects are up, and
// only that move: not one that stopped before the promotion, not one to another node, not as a follower.
func TestResumeServerMove(t *testing.T) {
	pass := func(to string) *failover.Plan {
		return &failover.Plan{To: to, Checks: []failover.Check{{Name: "unfinished move", OK: true}}}
	}
	// run starts the resume with the node in role and the plan given, marks the services up when ready
	// is set, and reports how many times the move was resumed. A resume that is expected is waited for;
	// one that is not gets a short time to show up.
	run := func(t *testing.T, role cluster.Role, plan *failover.Plan, ready, want bool) int {
		t.Helper()
		w := testWire(t)
		svc := &fakeService{plan: plan, resumeC: make(chan struct{}, 1)}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { resumeServerMove(ctx, w, svc, memberOf(role, 6)); close(done) }()
		time.Sleep(20 * time.Millisecond)
		if svc.count() != 0 {
			t.Fatal("the move resumed before the services and projects were up")
		}
		if ready {
			w.markReady()
		}
		wait := 100 * time.Millisecond
		if want {
			wait = 10 * time.Second
		}
		select {
		case <-svc.resumeC:
		case <-done:
		case <-time.After(wait):
		}
		cancel()
		<-done
		return svc.count()
	}
	if n := run(t, cluster.RoleLeader, pass("n2"), true, true); n != 1 {
		t.Errorf("the move that promoted this node was resumed %d times", n)
	}
	if n := run(t, cluster.RoleLeader, pass("n3"), true, false); n != 0 {
		t.Errorf("a move to another node was resumed here")
	}
	if n := run(t, cluster.RoleLeader, &failover.Plan{To: "n2", Checks: []failover.Check{{Name: "unfinished move", OK: false}}}, true, false); n != 0 {
		t.Errorf("there was nothing to resume and it was resumed")
	}
	if n := run(t, cluster.RoleFollower, pass("n2"), true, false); n != 0 {
		t.Errorf("a follower finished a move")
	}
	if n := run(t, cluster.RoleLeader, pass("n2"), false, false); n != 0 {
		t.Errorf("the move resumed with the services not up")
	}
}

func TestJoinServerChecksAsksEveryPart(t *testing.T) {
	a := func(context.Context, registry.Node) []failover.Check { return []failover.Check{{Name: "a", OK: true}} }
	b := func(context.Context, registry.Node) []failover.Check {
		return []failover.Check{{Name: "b1"}, {Name: "b2", OK: true}}
	}
	var names []string
	for _, c := range joinServerChecks([]failover.ExtraChecks{a, b})(context.Background(), registry.Node{ID: "n2"}) {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "a,b1,b2" {
		t.Fatalf("checks: %v", names)
	}
	if got := joinServerChecks(nil)(context.Background(), registry.Node{}); len(got) != 0 {
		t.Fatalf("no parts gave %v", got)
	}
}

type recFleet struct {
	mu   sync.Mutex
	refs []string
	fail map[string]error
}

func (r *recFleet) QuiesceTenant(context.Context, string) error { return nil }
func (r *recFleet) EnsureTenant(_ context.Context, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
	return r.fail[ref]
}

// The leader registers the projects that run on other nodes with the shared services it runs, once
// they are up; a follower, whose shared services are parked, does not.
func TestEnsureRemoteTenantsRegistersWhatRunsElsewhere(t *testing.T) {
	ctx := context.Background()
	w := testWire(t)
	reg := w.Node.Registry
	if err := reg.CreateNode(ctx, &registry.Node{Name: "second", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []registry.Project{
		{Ref: "aaaaaaaaaaaaaaaaaaaa", Name: "here", Status: registry.StatusActiveHealthy},
		{Ref: "bbbbbbbbbbbbbbbbbbbb", Name: "there", Status: registry.StatusActiveHealthy},
		{Ref: "cccccccccccccccccccc", Name: "paused there", Status: registry.StatusInactive},
		{Ref: "dddddddddddddddddddd", Name: "unhealthy there", Status: registry.StatusActiveUnhealthy},
	} {
		p := p
		if err := reg.CreateProject(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range []string{"bbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccc", "dddddddddddddddddddd"} {
		if err := reg.SetProjectNode(ctx, ref, "n2", 1); err != nil {
			t.Fatal(err)
		}
	}
	f := &recFleet{fail: map[string]error{"dddddddddddddddddddd": errors.New("supavisor: connection refused")}}
	Provide[failover.Fleet](w, f)
	Provide[failover.Locker](w, &projectLocker{reg: func() registry.Registry { return reg }})
	Provide[cluster.Membership](w, cluster.NewStatic(cluster.Snapshot{Self: registry.Node{ID: "n1"}, Leader: "n1", Epoch: 1, Role: cluster.RoleLeader}))
	ensureRemoteTenants(ctx, w, quiet())
	if strings.Join(f.refs, ",") != "bbbbbbbbbbbbbbbbbbbb,dddddddddddddddddddd" {
		t.Fatalf("registered %v: the active projects homed on n2, and neither the one homed here nor the paused one", f.refs)
	}

	f.refs = nil
	Provide[cluster.Membership](w, cluster.NewStatic(cluster.Snapshot{Self: registry.Node{ID: "n2"}, Leader: "n1", Epoch: 1, Role: cluster.RoleFollower}))
	ensureRemoteTenants(ctx, w, quiet())
	if len(f.refs) != 0 {
		t.Fatalf("a follower registered %v", f.refs)
	}
}
