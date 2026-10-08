package app

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/notimpl"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/registry"
)

type stubMesh struct{}

func (stubMesh) Dial(context.Context, string, mesh.Header) (net.Conn, error) {
	return nil, mesh.ErrNoSession
}
func (stubMesh) Call(context.Context, string, string, string, any, any) error {
	return mesh.ErrNoSession
}
func (stubMesh) Connected(string) bool            { return false }
func (stubMesh) RTT(string) (time.Duration, bool) { return 0, false }
func (stubMesh) Peers() []string                  { return nil }

type stubOps struct{}

func (stubOps) Ensure(context.Context, string, peerapi.InstanceSpec) (peerapi.InstanceStatus, error) {
	return peerapi.InstanceStatus{}, mesh.ErrNoSession
}
func (stubOps) Observe(context.Context, string, string) (peerapi.InstanceStatus, error) {
	return peerapi.InstanceStatus{}, mesh.ErrNoSession
}
func (stubOps) Remove(context.Context, string, string) error { return nil }
func (stubOps) Do(context.Context, string, string, peerapi.Action, peerapi.InstanceAction) (peerapi.InstanceStatus, error) {
	return peerapi.InstanceStatus{}, mesh.ErrNoSession
}

type stubLocal struct{}

func (stubLocal) Stop(context.Context, string) (string, error)          { return "0/3000060", nil }
func (stubLocal) Start(context.Context, string) error                   { return nil }
func (stubLocal) Healthy(context.Context, string) (bool, string, error) { return true, "", nil }
func (stubLocal) SetAside(context.Context, string, int64) error         { return nil }

var (
	_ placement.InstanceOps = stubOps{}
	_ mesh.Mesh             = stubMesh{}
)

// A node with no cluster behind it: the hook does nothing, whatever the other hooks provided.
func TestWireFailoverDoesNothingWithoutACluster(t *testing.T) {
	w := testWire(t)
	if err := wireFailover(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if len(w.runners) != 0 || len(w.stops) != 0 {
		t.Fatalf("started %d runners, %d stops on a single node", len(w.runners), len(w.stops))
	}
	if _, ok := Get[failover.Service](w); ok {
		t.Fatal("a failover service on a single node")
	}
}

// A cluster whose other hooks are not there yet: skipped as not implemented, like the other hooks.
func TestWireFailoverNamesWhatItIsMissing(t *testing.T) {
	w := testWire(t)
	Provide[cluster.Membership](w, twoNodes(t, w))
	err := wireFailover(context.Background(), w)
	if !errors.Is(err, notimpl.Err) || !strings.Contains(err.Error(), "mesh") {
		t.Fatalf("error: %v", err)
	}
	Provide[mesh.Mesh](w, stubMesh{})
	if err := wireFailover(context.Background(), w); !errors.Is(err, notimpl.Err) || !strings.Contains(err.Error(), "InstanceOps") {
		t.Fatalf("error: %v", err)
	}
	Provide[placement.InstanceOps](w, stubOps{})
	if err := wireFailover(context.Background(), w); !errors.Is(err, notimpl.Err) || !strings.Contains(err.Error(), "LocalPrimaries") {
		t.Fatalf("error: %v", err)
	}
	if len(w.runners) != 0 {
		t.Fatal("a hook that was skipped started something")
	}
}

func twoNodes(t *testing.T, w *Wire) *cluster.Static {
	t.Helper()
	ctx := context.Background()
	n1, err := w.Node.Registry.GetNode(ctx, registry.FounderNodeID)
	if err != nil {
		t.Fatal(err)
	}
	n2 := &registry.Node{ID: "n2", Name: "standby", State: registry.NodeActive}
	if err := w.Node.Registry.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	n2.ID = "n2"
	return cluster.NewStatic(cluster.Snapshot{Self: *n2, Nodes: []registry.Node{*n1, *n2}, Leader: "n1", Epoch: 1, Role: cluster.RoleFollower})
}

func TestWireFailoverBuildsTheOrchestratorAndServesTheCLI(t *testing.T) {
	// A short state directory: a unix socket path must fit in 100 bytes.
	dir, err := os.MkdirTemp("", "wf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	w := testWire(t)
	w.Cfg.StateDir = dir
	w.Cfg.Fleet.StorageBackend = "s3"
	Provide[cluster.Membership](w, twoNodes(t, w))
	Provide[mesh.Mesh](w, stubMesh{})
	Provide[placement.InstanceOps](w, stubOps{})
	Provide[failover.LocalPrimaries](w, stubLocal{})

	if err := wireFailover(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	svc, ok := Get[failover.Service](w)
	if !ok {
		t.Fatal("no failover.Service was provided")
	}
	if len(w.runners) != 3 {
		var names []string
		for _, r := range w.runners {
			names = append(names, r.name)
		}
		t.Fatalf("runners: %v, want the monitor, the control socket and the janitor", names)
	}
	// The peer endpoints are on the mesh.
	have := map[string]bool{}
	for _, p := range mesh.DefaultMux.Patterns() {
		have[p] = true
	}
	for _, p := range []string{"POST /peer/v1/fence", "POST /peer/v1/failover/quiesce", "POST /peer/v1/failover/resume", "POST /peer/v1/failover/primary/{ref}/{op}"} {
		if !have[p] {
			t.Errorf("%s is not registered on the mesh", p)
		}
	}
	// With no way to ask the peers, a plan says so instead of guessing.
	pl, err := svc.PlanServer(context.Background(), failover.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Blocked()) == 0 {
		t.Fatalf("a plan with no replicas, no marker store and no fencer passed: %+v", pl.Checks)
	}

	// The control socket answers the CLI.
	ctx, cancel := context.WithCancel(context.Background())
	g, gctx := errgroup.WithContext(ctx)
	w.start(g, gctx)
	sock := failover.ControlSocket(w.Cfg)
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, err := failover.Client{Path: sock}.Readiness(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Ready || r.Mode != config.FailoverManual || r.Fencer != "none" {
		t.Fatalf("readiness over the socket: %+v", r)
	}
	cancel()
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "system", "failover", "control.sock")); err == nil {
		t.Fatal("the socket outlives the daemon")
	}
	w.stop()
}
