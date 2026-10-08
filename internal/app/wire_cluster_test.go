package app

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
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

// A node that belongs to a cluster has every port the design requires connected, or switched off with
// a reason a person can act on. A hook that stops providing a port, or a port added to clusterPorts
// that nobody provides, fails here and not in a cluster's first failover.
func TestEveryClusterPortIsProvidedOrOff(t *testing.T) {
	w := clusterNodeWire(t)
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
	// What the Management API needs to answer for a cluster.
	if w.API.Replicas == nil || w.API.Placement == nil || !w.API.LoadBalancers || w.API.Failover == nil {
		t.Fatalf("Deps: replicas %v, placement %v, load balancers %v, failover %v", w.API.Replicas, w.API.Placement, w.API.LoadBalancers, w.API.Failover)
	}
	// The proxy has its cluster, and the one certificate endpoint is the proxy's.
	if w.Proxy.Cluster == nil {
		t.Fatal("the proxy was not given its cluster")
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
