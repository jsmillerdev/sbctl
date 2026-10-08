package placement

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/registry"
)

// recTimers is a lifecycle.Timers that records the refs it was asked about.
type recTimers struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (r *recTimers) StartTimer(_ context.Context, ref string) error { return r.rec("start " + ref) }
func (r *recTimers) StopTimer(_ context.Context, ref string) error  { return r.rec("stop " + ref) }
func (r *recTimers) rec(call string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
	return r.err
}
func (r *recTimers) got() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.calls, ",")
}

// timersEnv is the leader (n1) with a router, and the agent endpoints of n2 behind a mux, with timers
// and node resources of its own.
type timersEnv struct {
	r      *Router
	rpc    *muxRPC
	local  *recTimers // the leader's
	remote *recTimers // n2's
	a, b   string     // refs homed on n1 and on n2
}

func newTimersEnv(t *testing.T, epoch int64) *timersEnv {
	t.Helper()
	ctx := context.Background()
	reg := registry.NewMemory()
	mustCreate(t, reg, "second")
	e := &timersEnv{local: &recTimers{}, remote: &recTimers{}, a: testRef, b: "bcdefghijklmnopqrstu"}
	for _, ref := range []string{e.a, e.b} {
		if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: ref}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.SetProjectNode(ctx, e.b, "n2", 1); err != nil {
		t.Fatal(err)
	}
	mux := mesh.NewMux()
	if err := Register(mux.Handle, HandlerDeps{
		Agent: &recordingAgent{}, Plane: newFakeLocal(), Resolver: RegistryResolver{Reg: reg}, Members: members("n2", "n1", 5),
		Timers: e.remote, Resources: func() lifecycle.NodeResources { return lifecycle.NodeResources{MemoryBytes: 8 << 30, CPUs: 4} },
	}); err != nil {
		t.Fatal(err)
	}
	e.rpc = &muxRPC{mux: mux, caller: "n1"}
	e.r = NewRouter(RouterOptions{Local: newFakeLocal(), Resolver: RegistryResolver{Reg: reg}, Self: func() string { return "n1" }, RPC: e.rpc, Epoch: func() int64 { return epoch }})
	return e
}

// A project's backup timer starts and stops on the node that is its home, where its data is.
func TestRouterTimersFollowTheHome(t *testing.T) {
	ctx := context.Background()
	e := newTimersEnv(t, 5)
	tm := e.r.Timers(e.local)
	for _, ref := range []string{e.a, e.b} {
		if err := tm.StartTimer(ctx, ref); err != nil {
			t.Fatal(err)
		}
		if err := tm.StopTimer(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := e.local.got(), "start "+e.a+",stop "+e.a; got != want {
		t.Fatalf("the leader's timers: %s, want %s", got, want)
	}
	if got, want := e.remote.got(), "start "+e.b+",stop "+e.b; got != want {
		t.Fatalf("n2's timers: %s, want %s", got, want)
	}
	if got, want := e.rpc.callLog(), "POST /peer/v1/projects/"+e.b+"/plane/start_timer\nPOST /peer/v1/projects/"+e.b+"/plane/stop_timer"; got != want {
		t.Fatalf("mesh calls:\n%s\nwant\n%s", got, want)
	}

	// A ref the registry no longer knows is a leftover of this node's.
	if err := tm.StopTimer(ctx, "zzzzzzzzzzzzzzzzzzzz"); err != nil || !strings.HasSuffix(e.local.got(), "stop zzzzzzzzzzzzzzzzzzzz") {
		t.Fatalf("a deleted project: %v, %s", err, e.local.got())
	}

	// A node with no timers (the exec backend) has nothing to start for a project homed there.
	if err := e.r.Timers(nil).StartTimer(ctx, e.a); err != nil {
		t.Fatalf("no timers: %v", err)
	}

	// What the home answers comes back: a failure, and the sentinel of a refusal.
	e.remote.err = errors.New("unit not found")
	if err := tm.StartTimer(ctx, e.b); err == nil || !strings.Contains(err.Error(), "unit not found") {
		t.Fatalf("a timer that did not start: %v", err)
	}
	e.remote.err = nil
	stale := newTimersEnv(t, 4) // a leader that has been replaced
	if err := stale.r.Timers(stale.local).StopTimer(ctx, stale.b); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("a stale leader: %v", err)
	}
	if stale.remote.got() != "" {
		t.Fatalf("a stale leader stopped a timer: %s", stale.remote.got())
	}
}

// The resources of a project's home are read on that node; a project homed here has none to read.
func TestRouterReadsTheResourcesOfTheHome(t *testing.T) {
	ctx := context.Background()
	e := newTimersEnv(t, 5)
	res, err := e.r.NodeResources(ctx, &registry.Project{Ref: e.b, NodeID: "n2"})
	if err != nil || res.MemoryBytes != 8<<30 || res.CPUs != 4 {
		t.Fatalf("resources of n2 = %+v, %v", res, err)
	}
	if res, err = e.r.NodeResources(ctx, &registry.Project{Ref: e.a, NodeID: "n1"}); err != nil || res != (lifecycle.NodeResources{}) {
		t.Fatalf("a project homed here: %+v, %v", res, err)
	}
	// The request is for the home only.
	if _, err = e.r.NodeResources(ctx, &registry.Project{Ref: e.a, NodeID: "n2"}); !errors.Is(err, ErrNotHome) {
		t.Fatalf("a project that is not homed on the node asked: %v", err)
	}
}

// A node that has no timers or does not know its machine answers the requests with nothing to do and
// unknown resources.
func TestAgentAnswersTimerAndResourceRequestsWithoutTheirSources(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMemory()
	mustCreate(t, reg, "second")
	if err := reg.CreateProject(ctx, &registry.Project{Ref: testRef, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetProjectNode(ctx, testRef, "n2", 1); err != nil {
		t.Fatal(err)
	}
	mux := mesh.NewMux()
	if err := Register(mux.Handle, HandlerDeps{Agent: &recordingAgent{}, Plane: newFakeLocal(), Resolver: RegistryResolver{Reg: reg}, Members: members("n2", "n1", 5)}); err != nil {
		t.Fatal(err)
	}
	r := NewRouter(RouterOptions{Local: newFakeLocal(), Resolver: RegistryResolver{Reg: reg}, Self: func() string { return "n1" }, RPC: &muxRPC{mux: mux, caller: "n1"}, Epoch: func() int64 { return 5 }})
	if err := r.Timers(nil).StartTimer(ctx, testRef); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := r.Timers(nil).StopTimer(ctx, testRef); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if res, err := r.NodeResources(ctx, &registry.Project{Ref: testRef, NodeID: "n2"}); err != nil || res != (lifecycle.NodeResources{}) {
		t.Fatalf("resources: %+v, %v", res, err)
	}
}
