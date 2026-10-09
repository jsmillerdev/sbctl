package app

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	ffenced "github.com/supavise/supavise/internal/failover/fenced"
	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
)

// clusterNodeWire is the wire of a leader that belongs to a cluster, with everything the hooks read: a
// cluster identity on disk (so that the mesh hook starts the mesh), a second node in the registry, and
// an Engine and plane over units that do nothing.
func clusterNodeWire(t *testing.T) *Wire {
	t.Helper()
	w, _, _, reg := meshWire(t)
	mesh.ResetDefaultMux()
	t.Cleanup(mesh.ResetDefaultMux) // the peer API is process-wide: the next test registers its own
	if err := reg.CreateNode(context.Background(), &registry.Node{Name: "second", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	cfg := w.Cfg
	cfg.Supervisor = config.SupervisorSystemd
	plane := lifecycle.NewPostgresPlane(cfg, nopSup{}, nopArts{}, reg, lifecycle.PlaneOptions{})
	eng := lifecycle.NewEngine(cfg, reg, w.Node.Secrets, nopArts{}, plane, lifecycle.Options{NodeID: "n1"})
	eng.SetTimers(&recTimers{})
	w.Node = &lifecycle.Node{Cfg: cfg, Secrets: w.Node.Secrets, Supervisor: nopSup{}, Artifacts: nopArts{}, Registry: reg, Plane: plane, Engine: eng}
	Provide(w, cluster.BootDecision{Role: cluster.RoleLeader, Joined: true, SelfID: "n1", Epoch: 1, Leader: "n1"})
	return w
}

// runClusterWire runs the hooks of a cluster node and checks what every such test checks: the node has
// its mesh, and every port is provided or switched off with a reason a person can act on.
func runClusterWire(t *testing.T, w *Wire) {
	t.Helper()
	t.Cleanup(w.stop)
	if err := w.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !w.clustered() {
		t.Fatal("the node has no mesh: the test does not look at a cluster node")
	}
	if missing := w.unaccounted(); len(missing) > 0 {
		t.Fatalf("ports neither provided nor switched off: %v", missing)
	}
	for name, reason := range w.off {
		t.Logf("off: %s: %s", name, reason)
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is switched off with no reason", name)
		}
	}
}

// wantOff fails unless the features that are off are exactly these: a hook that switches a feature off by
// mistake changes the list, and so does one that stops switching off what it cannot run.
func wantOff(t *testing.T, w *Wire, features ...string) {
	t.Helper()
	got := slices.Sorted(maps.Keys(w.off))
	if !slices.Equal(got, features) {
		t.Fatalf("features off: %v, want %v", got, features)
	}
}

func hasRunner(w *Wire, name string) bool {
	return slices.ContainsFunc(w.runners, func(r namedRunner) bool { return r.name == name })
}

// withBackups gives the wire a backup service over a file store, as Serve does when the store opens: as
// the service the hooks Get and as the Management API's Backups.
func withBackups(t *testing.T, w *Wire) {
	t.Helper()
	store, err := backup.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bs, err := backup.New(backup.Options{Config: w.Cfg, Registry: w.Node.Registry, Store: store, Secrets: w.Node.Secrets})
	if err != nil {
		t.Fatal(err)
	}
	Provide(w, bs)
	w.API.Backups = bs
}

// A node that belongs to a cluster has every port the design requires connected, or switched off with
// a reason a person can act on. A hook that stops providing a port, or a port added to clusterPorts
// that nobody provides, fails here and not in a cluster's first failover.
//
// This node has no backup service (the store did not open). What that switches off is pinned, so that a
// feature that goes off by mistake does not pass as "off is accepted": the base backup the system cluster's
// standbys start from, the leader marker, and the replica controller.
func TestEveryClusterPortIsProvidedOrOff(t *testing.T) {
	w := clusterNodeWire(t)
	runClusterWire(t, w)
	wantOff(t, w, "cluster.BaseBackup", "failover.Marker", "placement.RoutedBackups", "replica controller")
	// What the Management API needs to answer for a cluster. The controller does not run, so the API has it
	// behind replicasOff: replicas are listed and removed (a project delete asks the Remover), and a setup
	// is refused, because a controller that does not run would take a request and leave its row for nobody.
	if _, off := w.API.Replicas.(replicasOff); !off || !w.API.LoadBalancers || w.API.Failover == nil {
		t.Fatalf("Deps: replicas %T, load balancers %v, failover %v", w.API.Replicas, w.API.LoadBalancers, w.API.Failover)
	}
	if _, ok := w.API.Replicas.(replicas.Remover); !ok {
		t.Fatal("the Management API cannot remove the replicas of a project it deletes")
	}
	if hasRunner(w, "replicas") {
		t.Fatal("the replica controller runs on a node that cannot take base backups")
	}
	// The proxy has its cluster, and the one certificate endpoint is the proxy's.
	if w.Proxy.Cluster == nil {
		t.Fatal("the proxy was not given its cluster")
	}
}

// The same node with the backup service that Serve provides: nothing is off, the Management API has the
// replica controller, and the controller runs.
func TestAClusterNodeWithBackupsHasNothingOff(t *testing.T) {
	w := clusterNodeWire(t)
	withBackups(t, w)
	runClusterWire(t, w)
	wantOff(t, w)
	if _, isController := w.API.Replicas.(*replicas.Controller); !isController || !w.API.LoadBalancers || w.API.Failover == nil {
		t.Fatalf("Deps: replicas %T, load balancers %v, failover %v", w.API.Replicas, w.API.LoadBalancers, w.API.Failover)
	}
	for _, name := range []string{"replicas", "replica report intake", "failover monitor"} {
		if !hasRunner(w, name) {
			t.Errorf("%s does not run", name)
		}
	}
	if d, ok := Get[failover.Service](w); !ok || d == nil {
		t.Fatal("no failover service")
	}
}

// A node that has joined a cluster while its host is behind this release keeps its mesh and runs no
// replica controller and no failover monitor. The Management API is given the controller behind
// replicasOff, so that a setup request is refused and does not leave a row for nobody, while a project
// delete still removes the replicas that exist.
func TestAClusterNodeWhoseHostIsBehindTakesNoReplicaRequests(t *testing.T) {
	w := clusterNodeWire(t)
	withBackups(t, w)
	Provide(w, hostsetup.Status{Have: 1, Want: 2, Known: true})
	runClusterWire(t, w)
	wantOff(t, w, "failover monitor", "replica controller")
	if r, _ := w.offReason("replica controller"); !strings.Contains(r, "supavise system converge") {
		t.Errorf("the reason does not say how to turn it on: %q", r)
	}
	if _, off := w.API.Replicas.(replicasOff); !off {
		t.Fatalf("the Management API was given %T, which is not the controller behind replicasOff", w.API.Replicas)
	}
	if w.API.Failover == nil || !w.API.LoadBalancers {
		t.Fatalf("Deps: load balancers %v, failover %v", w.API.LoadBalancers, w.API.Failover)
	}
	for _, name := range []string{"replicas", "failover monitor"} {
		if hasRunner(w, name) {
			t.Errorf("%s runs while the host is behind", name)
		}
	}
}

// The check bites: a hook left out of the daemon leaves its ports unprovided, and the test names them.
func TestAPortNobodyProvidesIsNamed(t *testing.T) {
	old := wireHooks
	defer func() { wireHooks = old }()
	var without []struct {
		name string
		fn   func(ctx context.Context, w *Wire) error
	}
	for _, h := range wireHooks {
		if h.name != "fleet" {
			without = append(without, h)
		}
	}
	wireHooks = without
	w := clusterNodeWire(t)
	t.Cleanup(w.stop)
	if err := w.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	missing := strings.Join(w.unaccounted(), ",")
	for _, want := range []string{"fleet.Fleet", "fleet.PeerRefresher", "replicas.Pooler", "failover.Fleet"} {
		if !strings.Contains(missing, want) {
			t.Errorf("%s is missing and %q does not say so", want, missing)
		}
	}
	// A port that is switched off with a reason is accounted for; one switched off with none is not.
	w.Off("fleet.Fleet", "")
	if !strings.Contains(strings.Join(w.unaccounted(), ","), "fleet.Fleet") {
		t.Error("a feature switched off with no reason counted as accounted for")
	}
	w.Off("fleet.Fleet", "tested")
	if strings.Contains(strings.Join(w.unaccounted(), ","), "fleet.Fleet") {
		t.Error("a feature switched off with a reason is still named")
	}
}

// A single server has no cluster ports to account for, provides none of them, and registers nothing on
// the peer API.
func TestASingleServerHasNoClusterPorts(t *testing.T) {
	w := testWire(t)
	if err := w.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.clustered() || len(w.unaccounted()) != 0 {
		t.Fatalf("clustered %v, unaccounted %v", w.clustered(), w.unaccounted())
	}
	if w.API.Replicas != nil || w.API.LoadBalancers || w.API.Failover != nil {
		t.Fatal("a single server's Management API was given cluster collaborators")
	}
	if w.Proxy.Cluster != nil {
		t.Fatal("a single server's proxy has a cluster")
	}
	for _, p := range []string{"failover.Service", "replicas.Pooler"} {
		if _, off := w.offReason(p); off {
			t.Errorf("a single server switched %s off", p)
		}
	}
}

// Every peer endpoint is registered once, and the certificate store is the proxy's: mesh.Handle panics
// on a pattern registered twice, which would stop every cluster node at start.
func TestNoPeerEndpointIsRegisteredTwice(t *testing.T) {
	w := clusterNodeWire(t)
	t.Cleanup(w.stop)
	if err := w.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, p := range mesh.DefaultMux.Patterns() {
		seen[p]++
	}
	for p, n := range seen {
		if n != 1 {
			t.Errorf("%s is registered %d times", p, n)
		}
	}
	certs := "GET " + peerapi.PathCerts
	if seen[certs] != 1 {
		t.Fatalf("%s is registered %d times", certs, seen[certs])
	}
	// The handler behind it answers the etag query with a 304, which only the proxy's does.
	req := httptest.NewRequest(http.MethodGet, peerapi.PathCerts, nil).WithContext(mesh.WithPeer(context.Background(), mesh.Peer{Node: "n2"}))
	rec := httptest.NewRecorder()
	mesh.DefaultMux.ServeHTTP(rec, req)
	tag := rec.Header().Get("ETag")
	if rec.Code != http.StatusOK || tag == "" {
		t.Fatalf("certs: %d %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest(http.MethodGet, peerapi.PathCerts+"?etag="+strings.Trim(tag, `"`), nil).WithContext(mesh.WithPeer(context.Background(), mesh.Peer{Node: "n2"}))
	rec = httptest.NewRecorder()
	mesh.DefaultMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("certs with the current tag: %d, want 304 (the membership endpoint ignores the query)", rec.Code)
	}
}

// The checks that name what other packages must implement, at compile time.
var (
	_ failover.Takeover = (*handoffTakeover)(nil)
	_ failover.Locker   = (*projectLocker)(nil)
)

// A feature that is off says so in the log, with the reason, at a level an operator sees.
func TestOffIsLoggedWithItsReason(t *testing.T) {
	var buf bytes.Buffer
	w := testWire(t)
	w.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	w.Off("failover.LocalServices", "the shared services run under systemd; this node's supervisor is exec")
	for _, want := range []string{"level=WARN", "cluster feature off", "failover.LocalServices", "the shared services run under systemd"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the log lacks %q: %s", want, buf.String())
		}
	}
	if r, ok := w.offReason("failover.LocalServices"); !ok || r == "" {
		t.Fatal("the reason was not recorded")
	}
}

// A cluster node's membership has the orchestrator's verdict and the backup store's leader marker, which
// a running leader needs to learn that it was replaced. The verdict is the orchestrator's, built after
// the membership: a higher epoch makes it write the node's record and stop every cluster of the
// supervisor, the ones no project row names too (FenceNode), and say so.
func TestAClusterNodeFencesWhenARunningLeaderLearnsOfAHigherEpoch(t *testing.T) {
	w := clusterNodeWire(t)
	withBackups(t, w)
	runClusterWire(t, w)
	live, ok := Get[*cluster.Live](w)
	if !ok {
		t.Fatal("no membership")
	}
	if fence, marker := live.Wired(); !fence || !marker {
		t.Fatalf("the membership was given a fencer: %v, the leader marker: %v", fence, marker)
	}
	lf, ok := Get[*lateFencer](w)
	if !ok || lf.f == nil {
		t.Fatal("the failover hook did not give the membership its orchestrator")
	}

	// A cluster directory that no registry row names: only a fence of the whole node reaches it.
	stray := filepath.Join(w.Cfg.Paths().Project("stray"), "postgres.run")
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fenced, err := lf.fence(context.Background(), "node n2", 5, "n2")
	if err != nil || !fenced {
		t.Fatalf("fence = %v, %v", fenced, err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("the launcher of a cluster no project names is still there (%v): the fence stopped the primaries one by one", err)
	}
	rec, err := ffenced.Node(w.Cfg.Paths())
	if err != nil || rec == nil || rec.Epoch != 5 || rec.Leader != "n2" {
		t.Fatalf("the node's fence record = %+v, %v", rec, err)
	}
}

// With the backup service the leader backs up the projects homed on other nodes: the base backup of a
// replica's seed goes to the project's home (TakeBase), the home's backup is recorded here (Ops.Recorder),
// and the nightly round runs. Without the wiring EnsureBase asks this node's own data directory, which
// a project homed elsewhere does not have.
func TestAClusterLeaderRoutesTheBaseBackupsOfProjectsHomedOnOtherNodes(t *testing.T) {
	w := clusterNodeWire(t)
	withBackups(t, w)
	ctx := context.Background()
	nodes, err := w.Node.Registry.ListNodes(ctx)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("nodes = %v, %v", nodes, err)
	}
	other := nodes[1].ID
	if nodes[0].ID != "n1" {
		other = nodes[0].ID
	}
	const ref = "abcdefghijklmnopqrst"
	if err := w.Node.Registry.CreateProject(ctx, &registry.Project{Ref: ref, Name: "demo", Region: "local"}); err != nil {
		t.Fatal(err)
	}
	cl, err := w.Node.Registry.GetCluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Node.Registry.SetProjectNode(ctx, ref, other, cl.Epoch); err != nil {
		t.Fatal(err)
	}
	runClusterWire(t, w)
	if !hasRunner(w, "scheduled backups") {
		t.Fatal("the leader takes no nightly backup of the projects homed elsewhere")
	}
	ops, _ := Get[placement.BackupOps](w)
	if o, ok := ops.(*placement.Ops); !ok || o.Recorder == nil {
		t.Fatalf("the ops record nothing a node took for them: %T", ops)
	}
	bs, _ := Get[*backup.Service](w)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = bs.EnsureBase(cctx, ref, 0)
	if err == nil || !strings.Contains(err.Error(), "on node "+other) {
		t.Fatalf("EnsureBase of a project homed on %s = %v; it must ask that node for the backup", other, err)
	}
}
