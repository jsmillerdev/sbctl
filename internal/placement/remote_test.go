package placement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// remoteEnv is the leader's RemotePlane wired to the agent of node n2, in this process.
type remoteEnv struct {
	local  *fakeLocal
	reg    *registry.Memory
	rpc    *muxRPC
	remote *RemotePlane
	agent  *recordingAgent
	// checkpoints answers the final_checkpoint request.
	checkpoints *fakeCheckpoints
}

type fakeCheckpoints struct {
	info lifecycle.ControlInfo
	err  error
	refs []string
}

func (f *fakeCheckpoints) FinalCheckpoint(ref string) (lifecycle.ControlInfo, error) {
	f.refs = append(f.refs, ref)
	return f.info, f.err
}

func newRemoteEnv(t *testing.T, caller string, epoch int64) *remoteEnv {
	t.Helper()
	ctx := context.Background()
	reg := registry.NewMemory()
	mustCreate(t, reg, "second")
	if err := reg.CreateProject(ctx, &registry.Project{Ref: testRef, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetProjectNode(ctx, testRef, "n2", 1); err != nil {
		t.Fatal(err)
	}
	e := &remoteEnv{local: newFakeLocal(), reg: reg, agent: &recordingAgent{}, checkpoints: &fakeCheckpoints{info: lifecycle.ControlInfo{State: "shut down", Checkpoint: "0/3000060"}}}
	mux := mesh.NewMux()
	Register(mux.Handle, HandlerDeps{
		Agent: e.agent, Plane: e.local, Resolver: RegistryResolver{Reg: reg}, Members: members("n2", "n1", 5),
		Backups: &fakeBackups{}, Checkpoints: e.checkpoints,
	})
	e.rpc = &muxRPC{mux: mux, caller: caller}
	e.remote = &RemotePlane{Node: "n2", RPC: e.rpc, Epoch: func() int64 { return epoch }}
	return e
}

func sameJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("%s:\n got %s\nwant %s", what, g, w)
	}
}

// Every method of lifecycle.Plane runs on the node's own plane with the arguments it was given, and
// its results come back.
func TestRemotePlaneRoundTripsEveryMethod(t *testing.T) {
	ctx := context.Background()
	e := newRemoteEnv(t, "n1", 5)
	p, k := testProject("n2"), testKeys()

	if err := e.remote.Create(ctx, p, k, nil); err != nil {
		t.Fatal(err)
	}
	sameJSON(t, "Create project", e.local.projects[testRef], p)
	sameJSON(t, "Create keys", e.local.keys[testRef], k)
	if !e.local.has("Create " + testRef + " seed=false") {
		t.Fatalf("calls = %v", e.local.calls)
	}
	if err := e.remote.Start(ctx, p, k); err != nil {
		t.Fatal(err)
	}
	if err := e.remote.StartDatabase(ctx, p, k); err != nil {
		t.Fatal(err)
	}
	if err := e.remote.Reconfigure(ctx, p, k); err != nil {
		t.Fatal(err)
	}
	if err := e.remote.Stop(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	if err := e.remote.Delete(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	b, err := e.remote.Snapshot(ctx, testRef)
	if err != nil || b == nil || b.ID != 7 || b.Timeline != 3 || b.SizeBytes != 99 || b.Status != registry.BackupCompleted {
		t.Fatalf("Snapshot = %+v, %v", b, err)
	}
	u, err := e.remote.Route(ctx, testRef)
	if err != nil || u.Postgres != "127.0.0.1:20006" || u.PostgREST != "127.0.0.1:20008" {
		t.Fatalf("Route = %+v, %v", u, err)
	}
	us, err := e.remote.Usage(ctx, testRef)
	if err != nil || us.DiskBytes != 1<<20 || us.MemoryBytes != 5<<20 {
		t.Fatalf("Usage = %+v, %v", us, err)
	}
	e.local.health = []lifecycle.ServiceHealth{{Name: "postgres", Healthy: true, Status: "ACTIVE_HEALTHY"}, {Name: "postgrest", Status: "UNHEALTHY", Error: "down"}}
	hs := e.remote.Health(ctx, p, nil)
	sameJSON(t, "Health", hs, e.local.health)

	want := []string{"Create", "Start", "StartDatabase", "Reconfigure", "Stop", "Delete", "Snapshot", "Route", "Usage", "Health"}
	var got []string
	for _, c := range e.local.calls {
		got = append(got, strings.Fields(c)[0])
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the node's plane was called %v, want %v", got, want)
	}
	for _, c := range strings.Split(e.rpc.callLog(), "\n") {
		if !strings.HasPrefix(c, "POST /peer/v1/projects/"+testRef+"/plane/") {
			t.Fatalf("call %q is not on the plane path", c)
		}
	}
	if !strings.Contains(e.rpc.callLog(), "/plane/start_database") {
		t.Fatalf("calls:\n%s", e.rpc.callLog())
	}
}

func TestRemoteCreateRefusesASeed(t *testing.T) {
	e := newRemoteEnv(t, "n1", 5)
	err := e.remote.Create(context.Background(), testProject("n2"), testKeys(), func(context.Context, *registry.Project, string) error { return nil })
	if !errors.Is(err, ErrRemoteSeed) || len(e.local.calls) != 0 {
		t.Fatalf("Create with a seed: %v (calls %v)", err, e.local.calls)
	}
}

func TestSentinelErrorsSurviveTheWire(t *testing.T) {
	ctx := context.Background()
	e := newRemoteEnv(t, "n1", 5)
	for _, tc := range []struct {
		err  error
		want error
	}{
		{fmt.Errorf("project: %w", registry.ErrNotFound), registry.ErrNotFound},
		{fmt.Errorf("%w: busy", lifecycle.ErrInvalidState), lifecycle.ErrInvalidState},
		{fmt.Errorf("%w: /x", lifecycle.ErrClusterExists), lifecycle.ErrClusterExists},
		{lifecycle.ErrNoSnapshot, lifecycle.ErrNoSnapshot},
		{fmt.Errorf("%w: new", lifecycle.ErrNoRestorableState), lifecycle.ErrNoRestorableState},
		{lifecycle.ErrReplayBehind, lifecycle.ErrReplayBehind},
		{lifecycle.ErrNotStandby, lifecycle.ErrNotStandby},
		{lifecycle.ErrNotCleanShutdown, lifecycle.ErrNotCleanShutdown},
		{fmt.Errorf("%w: n2 leads", lifecycle.ErrFenced), lifecycle.ErrFenced},
		{fmt.Errorf("%w: %w", ErrNoRoom, &lifecycle.CapacityError{Message: "no room"}), ErrNoRoom},
	} {
		e.local.err["Delete"] = tc.err
		err := e.remote.Delete(ctx, testRef)
		if !errors.Is(err, tc.want) {
			t.Errorf("%v came back as %v, want errors.Is %v", tc.err, err, tc.want)
		}
		if !strings.Contains(fmt.Sprint(err), "node n2") {
			t.Errorf("the error does not name the node: %v", err)
		}
	}
	// An error without a sentinel keeps its text and is not mistaken for one.
	e.local.err["Delete"] = errors.New("disk on fire")
	err := e.remote.Delete(ctx, testRef)
	var re *mesh.RemoteError
	if err == nil || !errors.As(err, &re) || re.Status != http.StatusInternalServerError || !strings.Contains(re.Message, "disk on fire") {
		t.Fatalf("plain error: %v", err)
	}
	for _, s := range []error{registry.ErrNotFound, lifecycle.ErrInvalidState, lifecycle.ErrClusterExists} {
		if errors.Is(err, s) {
			t.Fatalf("a plain error is %v", s)
		}
	}
}

func TestPlaneEndpointsAuthorizeTheLeaderAtItsEpoch(t *testing.T) {
	ctx := context.Background()

	// A node that is not the leader is refused.
	e := newRemoteEnv(t, "n3", 5)
	// The refusal says who asked and who leads; that text reaches the operator through the leader's error.
	if err := e.remote.Stop(ctx, testRef); !errors.Is(err, cluster.ErrNotLeader) {
		t.Fatalf("a caller that is not the leader: %v", err)
	} else if want := `node n2: cluster: this node is not the leader: n3 asked, the leader is "n1"`; err.Error() != want {
		t.Fatalf("a caller that is not the leader: error %q, want %q", err, want)
	}
	// A request that carries no node certificate (the join endpoints' kind) is refused too.
	e = newRemoteEnv(t, "", 5)
	if err := e.remote.Stop(ctx, testRef); !errors.Is(err, cluster.ErrNotLeader) {
		t.Fatalf("no certificate: %v", err)
	} else if want := "node n2: cluster: this node is not the leader: the request carries no node certificate"; err.Error() != want {
		t.Fatalf("no certificate: error %q, want %q", err, want)
	}
	// A leader that has been replaced acts under an older epoch.
	e = newRemoteEnv(t, "n1", 4)
	if err := e.remote.Stop(ctx, testRef); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale epoch: %v", err)
	}
	if len(e.local.calls) != 0 {
		t.Fatalf("a refused call reached the plane: %v", e.local.calls)
	}
	// A newer epoch than the node's is accepted: the node learns it from the leader.
	e = newRemoteEnv(t, "n1", 6)
	if err := e.remote.Stop(ctx, testRef); err != nil {
		t.Fatalf("newer epoch: %v", err)
	}
	// The project is not homed on the node.
	e = newRemoteEnv(t, "n1", 5)
	if err := e.reg.SetProjectNode(ctx, testRef, "n1", 1); err != nil {
		t.Fatal(err)
	}
	if err := e.remote.Stop(ctx, testRef); !errors.Is(err, ErrNotHome) {
		t.Fatalf("a project homed elsewhere: %v", err)
	}
	// An unknown project.
	e = newRemoteEnv(t, "n1", 5)
	if err := e.remote.Stop(ctx, "zzzzzzzzzzzzzzzzzzzz"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	// Bad requests: an unknown method, a body for another project, no keys where they are needed.
	e = newRemoteEnv(t, "n1", 5)
	var re *mesh.RemoteError
	err := e.rpc.Call(ctx, "n2", http.MethodPost, "/peer/v1/projects/"+testRef+"/plane/bogus", peerapi.PlaneCall{Epoch: 5}, nil)
	if !errors.As(err, &re) || re.Status != http.StatusBadRequest {
		t.Fatalf("unknown method: %v", err)
	}
	other := testProject("n2")
	other.Ref = "zzzzzzzzzzzzzzzzzzzz"
	b, _ := json.Marshal(projectArgs{Project: other, Keys: testKeys()})
	err = e.rpc.Call(ctx, "n2", http.MethodPost, peerapi.PlanePath(testRef, peerapi.PlaneStart), peerapi.PlaneCall{Epoch: 5, Args: b}, nil)
	if err == nil || len(e.local.calls) != 0 {
		t.Fatalf("a body for another project: %v (calls %v)", err, e.local.calls)
	}
	b, _ = json.Marshal(projectArgs{Project: testProject("n2")})
	err = e.rpc.Call(ctx, "n2", http.MethodPost, peerapi.PlanePath(testRef, peerapi.PlaneStart), peerapi.PlaneCall{Epoch: 5, Args: b}, nil)
	if err == nil || len(e.local.calls) != 0 {
		t.Fatalf("start without keys: %v (calls %v)", err, e.local.calls)
	}
}

func TestRemoteHealthOfAnUnreachableNodeIsUnhealthy(t *testing.T) {
	r := &RemotePlane{Node: "n2", RPC: failingRPC{mesh.ErrNoSession}}
	hs := r.Health(context.Background(), testProject("n2"), nil)
	if len(hs) != 3 {
		t.Fatalf("health = %+v", hs)
	}
	for _, h := range hs {
		if h.Healthy || h.Status != "UNHEALTHY" || !strings.Contains(h.Error, "no session") {
			t.Fatalf("health = %+v", h)
		}
	}
	sys := testProject("n2")
	sys.Ref = "system"
	if hs := r.Health(context.Background(), sys, nil); len(hs) != 2 {
		t.Fatalf("the system project has no PostgREST: %+v", hs)
	}
}

type failingRPC struct{ err error }

func (f failingRPC) Call(context.Context, string, string, string, any, any) error { return f.err }

// A method added to lifecycle.Plane must come with a remote counterpart: a peerapi.PlaneMethod, an
// entry in the agent's table and a method on RemotePlane. This test fails until it has all three.
func TestRemotePlaneCoversEveryPlaneMethod(t *testing.T) {
	var methods []string
	pt := reflect.TypeFor[lifecycle.Plane]()
	for i := 0; i < pt.NumMethod(); i++ {
		methods = append(methods, string(planeMethodName(pt.Method(i).Name)))
	}
	sort.Strings(methods)
	var declared []string
	for _, m := range peerapi.PlaneMethods {
		declared = append(declared, string(m))
	}
	sort.Strings(declared)
	if strings.Join(methods, ",") != strings.Join(declared, ",") {
		t.Fatalf("lifecycle.Plane has %v and peerapi.PlaneMethods lists %v", methods, declared)
	}
	var handled []string
	for m := range planeCalls {
		handled = append(handled, string(m))
	}
	sort.Strings(handled)
	if strings.Join(methods, ",") != strings.Join(handled, ",") {
		t.Fatalf("lifecycle.Plane has %v and the agent handles %v", methods, handled)
	}
	rt := reflect.TypeFor[*RemotePlane]()
	for i := 0; i < pt.NumMethod(); i++ {
		name := pt.Method(i).Name
		m, ok := rt.MethodByName(name)
		if !ok {
			t.Errorf("RemotePlane lacks %s", name)
			continue
		}
		if m.Type.NumIn()-1 != pt.Method(i).Type.NumIn() || m.Type.NumOut() != pt.Method(i).Type.NumOut() {
			t.Errorf("RemotePlane.%s has the wrong shape", name)
		}
	}
	// The constants in peerapi must be the names planeMethodName derives, so that the path a client
	// builds is the path the agent serves.
	for _, c := range []struct {
		goName string
		m      peerapi.PlaneMethod
	}{{"Create", peerapi.PlaneCreate}, {"Delete", peerapi.PlaneDelete}, {"Snapshot", peerapi.PlaneSnapshot}, {"Route", peerapi.PlaneRoute},
		{"Usage", peerapi.PlaneUsage}, {"Start", peerapi.PlaneStart}, {"StartDatabase", peerapi.PlaneStartDatabase}, {"Stop", peerapi.PlaneStop},
		{"Reconfigure", peerapi.PlaneReconfigure}, {"Health", peerapi.PlaneHealth}} {
		if planeMethodName(c.goName) != c.m {
			t.Errorf("planeMethodName(%s) = %s, want %s", c.goName, planeMethodName(c.goName), c.m)
		}
	}
}

// The control file of a stopped cluster is read on its home, for the position a switchover waits for.
func TestFinalCheckpointIsReadOnTheHome(t *testing.T) {
	ctx := context.Background()
	e := newRemoteEnv(t, "n1", 5)
	ci, err := e.remote.FinalCheckpoint(ctx, testRef)
	if err != nil || ci.State != "shut down" || ci.Checkpoint != "0/3000060" || !ci.ShutDown() {
		t.Fatalf("FinalCheckpoint = %+v, %v", ci, err)
	}
	if len(e.checkpoints.refs) != 1 || e.checkpoints.refs[0] != testRef || len(e.local.calls) != 0 {
		t.Fatalf("refs %v, plane calls %v", e.checkpoints.refs, e.local.calls)
	}
	if !strings.Contains(e.rpc.callLog(), "/plane/final_checkpoint") {
		t.Fatalf("calls:\n%s", e.rpc.callLog())
	}
	// A cluster that cannot be read says why.
	e.checkpoints.err = fmt.Errorf("open pg_control: %w", registry.ErrNotFound)
	if _, err := e.remote.FinalCheckpoint(ctx, testRef); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unreadable: %v", err)
	}
	// The router reads it where the project is homed: here through the local plane, elsewhere over the wire.
	r := newRouterEnv(t)
	local := &localWithCheckpoint{fakeLocal: r.local}
	r.r.opts.Local = local
	if ci, err := r.r.FinalCheckpoint(ctx, r.a); err != nil || ci.Checkpoint != "0/28" {
		t.Fatalf("local: %+v %v", ci, err)
	}
	if _, err := r.r.FinalCheckpoint(ctx, r.b); err != nil || len(r.rpc.calls) != 1 || !strings.HasSuffix(r.rpc.calls[0], "/plane/final_checkpoint") {
		t.Fatalf("remote: %v %v", err, r.rpc.calls)
	}
	if _, err := r.r.FinalCheckpoint(ctx, "zzzzzzzzzzzzzzzzzzzz"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	// A plane that cannot read one is not supported.
	r.r.opts.Local = r.local
	if _, err := r.r.FinalCheckpoint(ctx, r.a); !errors.Is(err, lifecycle.ErrNotSupported) {
		t.Fatalf("a plane without a reader: %v", err)
	}
	// A node with no reader answers that it has no such capability.
	e2 := newRemoteEnv(t, "n1", 5)
	e2.rpc.mux = mesh.NewMux()
	Register(e2.rpc.mux.Handle, HandlerDeps{Agent: e2.agent, Plane: e2.local, Resolver: RegistryResolver{Reg: e2.reg}, Members: members("n2", "n1", 5)})
	if _, err := e2.remote.FinalCheckpoint(ctx, testRef); !errors.Is(err, lifecycle.ErrNoSnapshot) {
		t.Fatalf("no reader: %v", err)
	}
}

// localWithCheckpoint is a local plane that can read a control file.
type localWithCheckpoint struct{ *fakeLocal }

func (localWithCheckpoint) FinalCheckpoint(string) (lifecycle.ControlInfo, error) {
	return lifecycle.ControlInfo{State: "shut down", Checkpoint: "0/28"}, nil
}

// Handlers that cannot tell the leader from another peer, or the home from another node, would serve
// every request, so Register refuses to be built without what tells them.
func TestRegisterNeedsWhatTheHandlersCheckAgainst(t *testing.T) {
	mux := mesh.NewMux()
	reg := registry.NewMemory()
	good := HandlerDeps{Agent: &recordingAgent{}, Plane: newFakeLocal(), Resolver: RegistryResolver{Reg: reg}, Members: members("n2", "n1", 5)}
	for name, mutate := range map[string]func(d *HandlerDeps){
		"no agent":      func(d *HandlerDeps) { d.Agent = nil },
		"no membership": func(d *HandlerDeps) { d.Members = nil },
		"no resolver":   func(d *HandlerDeps) { d.Resolver = nil },
	} {
		d := good
		mutate(&d)
		if err := Register(mux.Handle, d); err == nil {
			t.Errorf("%s: Register accepted it", name)
		}
	}
	if len(mux.Patterns()) != 0 {
		t.Fatalf("a refused Register registered %v", mux.Patterns())
	}
	if err := Register(mux.Handle, good); err != nil || len(mux.Patterns()) != 6 {
		t.Fatalf("Register: %v, %v", err, mux.Patterns())
	}
}

// What the node refused and what it may have done is told apart by the error, on the node's own agent
// and across the peer API: a promotion that ended in a refusal changed nothing, one that ended in
// anything else may have happened.
func TestRefusedTellsARefusalFromAFailureInTheMiddle(t *testing.T) {
	ctx := context.Background()
	e := newRemoteEnv(t, "n1", 5)
	for _, tc := range []struct {
		err     error
		refused bool
	}{
		{fmt.Errorf("project: %w", registry.ErrNotFound), true},
		{fmt.Errorf("%w: busy", lifecycle.ErrInvalidState), true},
		{fmt.Errorf("%w: not a standby", lifecycle.ErrNotStandby), true},
		{fmt.Errorf("%w: n2 leads", lifecycle.ErrFenced), true},
		{lifecycle.ErrReplayBehind, true},
		{fmt.Errorf("%w: /x", lifecycle.ErrClusterExists), true},
		{lifecycle.ErrNoSnapshot, true},
		{fmt.Errorf("%w: %w", ErrNoRoom, &lifecycle.CapacityError{Message: "no room"}), true},
		{fmt.Errorf("%w: epoch 3", ErrStaleEpoch), true},
		{fmt.Errorf("%w: homed elsewhere", ErrNotHome), true},
		{cluster.ErrNotLeader, true},
		// The cluster was stopped before the check: the node changed.
		{lifecycle.ErrNotCleanShutdown, false},
		{errors.New("checkpoint after the promotion: connection reset"), false},
		{context.DeadlineExceeded, false},
		{nil, false},
	} {
		if got := Refused(tc.err); got != tc.refused {
			t.Errorf("Refused(%v) = %v on the node's side", tc.err, got)
		}
		if tc.err == nil {
			continue
		}
		e.local.err["Delete"] = tc.err
		err := e.remote.Delete(ctx, testRef)
		if got := Refused(err); got != tc.refused {
			t.Errorf("Refused(%v) = %v across the peer API (%v)", tc.err, got, err)
		}
	}
}

// A node that has no room for a replica answers so that the replica controller waits and asks again:
// the error has a NoRoom method, on the node itself and across the peer API.
func TestNoRoomSurvivesTheWireAsARefusalForLackOfRoom(t *testing.T) {
	type roomError interface{ NoRoom() bool }
	noRoom := func(err error) bool {
		var re roomError
		return errors.As(err, &re) && re.NoRoom()
	}
	disk := fmt.Errorf("%w: %w", ErrNoRoom, &lifecycle.CapacityError{Message: "this node cannot run a Small project"})
	if !noRoom(disk) {
		t.Fatalf("the node's own refusal: %v", disk)
	}
	e := newRemoteEnv(t, "n1", 5)
	e.local.err["Delete"] = disk
	err := e.remote.Delete(context.Background(), testRef)
	var nr *NoRoomError
	if !errors.As(err, &nr) || !noRoom(err) || !errors.Is(err, ErrNoRoom) || nr.Node != "n2" {
		t.Fatalf("across the peer API: %v", err)
	}
	if noRoom(errors.New("boom")) {
		t.Fatal("an error with no NoRoom method counts as one")
	}
}
