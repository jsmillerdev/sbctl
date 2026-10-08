package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/units"
)

// fakeMesh is a mesh.Mesh that records the peer API calls made through it.
type fakeMesh struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeMesh) Call(_ context.Context, node, method, path string, _, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, node+" "+method+" "+path)
	return nil
}
func (f *fakeMesh) Dial(context.Context, string, mesh.Header) (net.Conn, error) {
	return nil, mesh.ErrNoSession
}
func (f *fakeMesh) Connected(string) bool            { return true }
func (f *fakeMesh) RTT(string) (time.Duration, bool) { return 0, false }
func (f *fakeMesh) Peers() []string                  { return nil }

type nopSup struct{}

func (nopSup) Render(context.Context, units.Spec) error { return nil }
func (nopSup) Start(context.Context, string) error      { return nil }
func (nopSup) Stop(context.Context, string) error       { return nil }
func (nopSup) Remove(context.Context, string) error     { return nil }
func (nopSup) Status(context.Context, string) (units.Status, error) {
	return units.Status{State: units.StateInactive}, nil
}

type nopArts struct{}

func (nopArts) Dir(svc string) (string, error) { return "/art/" + svc, nil }
func (nopArts) Tag(svc string) (string, error) { return svc + "-tag", nil }

// clusterWire is a wire for a leader (n1) in a cluster of two, with a real Engine and plane over a
// Memory registry and a mesh that records.
func clusterWire(t *testing.T) (*Wire, *fakeMesh, *registry.Memory) {
	t.Helper()
	ctx := context.Background()
	reg := registry.NewMemory()
	if err := reg.CreateNode(ctx, &registry.Node{Name: "second", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Domain = "example.test"
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	plane := lifecycle.NewPostgresPlane(cfg, nopSup{}, nopArts{}, reg, lifecycle.PlaneOptions{})
	eng := lifecycle.NewEngine(cfg, reg, sec, nopArts{}, plane, lifecycle.Options{NodeID: "n1"})
	node := &lifecycle.Node{Cfg: cfg, Secrets: sec, Registry: reg, Plane: plane, Engine: eng}
	w := testWire(t)
	w.Cfg, w.Node = cfg, node
	fm := &fakeMesh{}
	Provide[mesh.Mesh](w, fm)
	Provide[cluster.Membership](w, cluster.NewStatic(cluster.Snapshot{
		Self: registry.Node{ID: "n1", Name: "primary"}, Nodes: []registry.Node{{ID: "n1"}, {ID: "n2", Name: "second"}},
		Leader: "n1", Epoch: 3, Role: cluster.RoleLeader,
	}))
	return w, fm, reg
}

func TestPlacementWiringDoesNothingWithoutACluster(t *testing.T) {
	w := testWire(t)
	mux := mesh.NewMux()
	if err := placementWiring(context.Background(), w, mux.Handle); err != nil {
		t.Fatal(err)
	}
	if len(mux.Patterns()) != 0 || len(w.runners) != 0 || len(w.stops) != 0 {
		t.Fatalf("patterns %v, runners %d", mux.Patterns(), len(w.runners))
	}
	if _, ok := Get[placement.InstanceOps](w); ok {
		t.Fatal("instance ops provided on a node with no cluster")
	}
	// The stub in the hook table is the real one.
	if err := wirePlacement(context.Background(), w); err != nil {
		t.Fatal(err)
	}
}

func TestPlacementWiringPutsTheRouterInFrontOfTheEngineAndServesThePeerAPI(t *testing.T) {
	ctx := context.Background()
	w, fm, reg := clusterWire(t)
	mux := mesh.NewMux()
	if err := placementWiring(ctx, w, mux.Handle); err != nil {
		t.Fatal(err)
	}

	// What it registers and provides.
	got := strings.Join(mux.Patterns(), "\n")
	for _, want := range []string{
		"PUT /peer/v1/instances/{identifier}", "GET /peer/v1/instances/{identifier}", "DELETE /peer/v1/instances/{identifier}",
		"POST /peer/v1/instances/{identifier}/{action}", "POST /peer/v1/projects/{ref}/plane/{method}", "POST /peer/v1/projects/{ref}/backup/{op}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no handler for %q in\n%s", want, got)
		}
	}
	if _, ok := Get[placement.Resolver](w); !ok {
		t.Error("no Resolver")
	}
	if _, ok := Get[placement.PlaneRouter](w); !ok {
		t.Error("no PlaneRouter")
	}
	ops, ok := Get[placement.InstanceOps](w)
	if !ok {
		t.Fatal("no InstanceOps")
	}
	if _, ok := Get[placement.BackupOps](w); !ok {
		t.Error("no BackupOps")
	}
	var names []string
	for _, r := range w.runners {
		names = append(names, r.name)
	}
	if strings.Join(names, ",") != "replicas start,replica report cache,replica report,replica schema reload" {
		t.Errorf("runners = %v", names)
	}

	// The Engine now sends a project homed on n2 to n2: a pause stops it through the mesh.
	p := &registry.Project{Ref: "abcdefghijklmnopqrst", Name: "demo", Class: "micro", Engine: registry.EnginePostgres, Status: registry.StatusActiveHealthy}
	if err := reg.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetProjectNode(ctx, p.Ref, "n2", 1); err != nil {
		t.Fatal(err)
	}
	if err := w.Node.Engine.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if len(fm.calls) != 1 || fm.calls[0] != "n2 POST /peer/v1/projects/"+p.Ref+"/plane/stop" {
		t.Fatalf("mesh calls = %v", fm.calls)
	}
	// A project homed here stays local.
	q := &registry.Project{Ref: "bcdefghijklmnopqrstu", Name: "other", Class: "micro", Engine: registry.EnginePostgres, Status: registry.StatusActiveHealthy}
	if err := reg.CreateProject(ctx, q); err != nil {
		t.Fatal(err)
	}
	if err := w.Node.Engine.Pause(ctx, q.Ref); err != nil || len(fm.calls) != 1 {
		t.Fatalf("local pause: %v, calls %v", err, fm.calls)
	}

	// The instance endpoints answer the leader and nobody else; an unknown replica is absent.
	id := registry.ReplicaIdentifier(p.Ref, "us-east-1", "abc123")
	serve := func(caller string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, peerapi.InstancePath(id), bytes.NewReader(nil)).WithContext(mesh.WithPeer(ctx, mesh.Peer{Node: caller}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := serve("n2"); rec.Code != http.StatusForbidden {
		t.Fatalf("a node that is not the leader: %d %s", rec.Code, rec.Body)
	}
	rec := serve("n1")
	var st peerapi.InstanceStatus
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &st) != nil || st.Role != "absent" || st.Identifier != id {
		t.Fatalf("observe: %d %s", rec.Code, rec.Body)
	}
	// InstanceOps for this node reaches the agent without the mesh.
	if _, err := ops.Observe(ctx, "n1", id); err != nil || len(fm.calls) != 1 {
		t.Fatalf("local observe: %v, calls %v", err, fm.calls)
	}
}

// fakeForwarders stands for *mesh.Forwarders, which the mesh hook provides under its own type.
type fakeForwarders struct {
	mu       sync.Mutex
	held     []string
	released []string
}

func (f *fakeForwarders) Suspend(ref string) func() {
	f.mu.Lock()
	f.held = append(f.held, ref)
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.released = append(f.released, ref)
		f.mu.Unlock()
	}
}

// A promotion and a demotion take the project's ports from the forwarders the mesh hook provided,
// whatever type it provided them as.
func TestPlacementWiringBindsThePlaneToTheForwarders(t *testing.T) {
	ctx := context.Background()
	w, _, _ := clusterWire(t)
	fwd := &fakeForwarders{}
	Provide(w, fwd)
	if err := placementWiring(ctx, w, mesh.NewMux().Handle); err != nil {
		t.Fatal(err)
	}
	ref := "abcdefghijklmnopqrst"
	target := lifecycle.ReplicaTarget{
		Identifier: registry.ReplicaIdentifier(ref, "us-east-1", "abc123"),
		Project:    &registry.Project{Ref: ref, Seq: 3, Class: "micro", Engine: registry.EnginePostgres},
		Keys:       &secrets.ProjectKeys{ReplicationPassword: "pw"},
	}
	// There is no cluster to demote: the call fails after it took the ports and gives them back.
	if err := w.Node.Plane.DemoteToReplica(ctx, target); err == nil {
		t.Fatal("a demotion of nothing succeeded")
	}
	if len(fwd.held) != 1 || fwd.held[0] != ref || len(fwd.released) != 1 {
		t.Fatalf("held %v, released %v", fwd.held, fwd.released)
	}
}

func TestPlacementWiringWithoutForwardersHoldsNothing(t *testing.T) {
	ctx := context.Background()
	w, _, _ := clusterWire(t)
	if err := placementWiring(ctx, w, mesh.NewMux().Handle); err != nil {
		t.Fatal(err)
	}
	if _, ok := providedAs[portHolder](w); ok {
		t.Fatal("a port holder from nowhere")
	}
	// The contribution to the leader's report is provided for the cluster package to take.
	contrib, ok := Get[placement.Contribution](w)
	if !ok {
		t.Fatal("no contribution")
	}
	if in, pr := contrib(ctx); len(in) != 0 || len(pr) != 0 {
		t.Fatalf("a contribution before the first refresh: %v %v", in, pr)
	}
}
