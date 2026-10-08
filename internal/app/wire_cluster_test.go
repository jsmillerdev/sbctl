package app

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
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
// standbys start from, the leader marker, and the replica controller with the Management API's way to it.
func TestEveryClusterPortIsProvidedOrOff(t *testing.T) {
	w := clusterNodeWire(t)
	runClusterWire(t, w)
	wantOff(t, w, "api.Deps.Replicas", "cluster.BaseBackup", "failover.Marker", "replica controller")
	// What the Management API needs to answer for a cluster. Replicas is off: a controller that does not
	// run would take a request and leave its row for nobody.
	if w.API.Replicas != nil || w.API.Placement == nil || !w.API.LoadBalancers || w.API.Failover == nil {
		t.Fatalf("Deps: replicas %v, placement %v, load balancers %v, failover %v", w.API.Replicas, w.API.Placement, w.API.LoadBalancers, w.API.Failover)
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
	if w.API.Replicas == nil || w.API.Placement == nil || !w.API.LoadBalancers || w.API.Failover == nil {
		t.Fatalf("Deps: replicas %v, placement %v, load balancers %v, failover %v", w.API.Replicas, w.API.Placement, w.API.LoadBalancers, w.API.Failover)
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
// replica controller and no failover monitor. The Management API is not given the controller either, so
// that a setup request is refused and does not leave a row for nobody.
func TestAClusterNodeWhoseHostIsBehindTakesNoReplicaRequests(t *testing.T) {
	w := clusterNodeWire(t)
	withBackups(t, w)
	Provide(w, hostsetup.Status{Have: 1, Want: 2, Known: true})
	runClusterWire(t, w)
	wantOff(t, w, "api.Deps.Replicas", "failover monitor", "replica controller")
	if r, _ := w.offReason("api.Deps.Replicas"); !strings.Contains(r, "supavise system converge") {
		t.Errorf("the reason does not say how to turn it on: %q", r)
	}
	if w.API.Replicas != nil {
		t.Fatal("the Management API was given a replica controller that does not run")
	}
	if w.API.Placement == nil || w.API.Failover == nil || !w.API.LoadBalancers {
		t.Fatalf("Deps: placement %v, load balancers %v, failover %v", w.API.Placement, w.API.LoadBalancers, w.API.Failover)
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
	if w.API.Replicas != nil || w.API.Placement != nil || w.API.LoadBalancers || w.API.Failover != nil {
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
