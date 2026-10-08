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
	e := &remoteEnv{local: newFakeLocal(), reg: reg, agent: &recordingAgent{}}
	mux := mesh.NewMux()
	Register(mux.Handle, HandlerDeps{
		Agent: e.agent, Plane: e.local, Resolver: RegistryResolver{Reg: reg}, Members: members("n2", "n1", 5),
		Backups: &fakeBackups{},
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
		{lifecycle.ErrNotStandby, lifecycle.ErrInvalidState},
		{lifecycle.ErrNotCleanShutdown, lifecycle.ErrInvalidState},
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
	if err := e.remote.Stop(ctx, testRef); !errors.Is(err, cluster.ErrNotLeader) {
		t.Fatalf("a caller that is not the leader: %v", err)
	}
	// A request that carries no node certificate (the join endpoints' kind) is refused too.
	e = newRemoteEnv(t, "", 5)
	if err := e.remote.Stop(ctx, testRef); !errors.Is(err, cluster.ErrNotLeader) {
		t.Fatalf("no certificate: %v", err)
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
