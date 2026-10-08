package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// guardFixture is a relay guard on a node (n2, named "second") whose registry holds a project homed on
// n1.
func guardFixture(t *testing.T, joined bool) (*relayGuard, *registry.Memory, string) {
	t.Helper()
	ctx := context.Background()
	reg := registry.NewMemory()
	const ref = "abcdefghijklmnopqrst"
	if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: "p", Class: "micro", NodeID: registry.FounderNodeID}); err != nil {
		t.Fatal(err)
	}
	n2 := &registry.Node{Name: "second", State: registry.NodeActive}
	if err := reg.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Node.Name = "second"
	conf := filepath.Join(t.TempDir(), "etc", "config.toml")
	if joined {
		dir := config.ClusterDir(conf)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, config.NodeCertFile), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := newRelayGuard(cfg, conf, quiet())
	g.open = func(context.Context) (registry.Registry, error) { return reg, nil }
	return g, reg, ref
}

// A server that is in no cluster is not guarded: the answer needs no look at the database.
func TestRelayGuardIsOffOnASingleServer(t *testing.T) {
	g, _, ref := guardFixture(t, false)
	g.open = func(context.Context) (registry.Registry, error) {
		t.Error("the guard opened the registry on a single server")
		return nil, errors.New("no")
	}
	if rep, err := g.replica(context.Background(), ref); rep || err != nil {
		t.Fatalf("replica = %v, %v", rep, err)
	}
}

// The registry's replica row says that this node holds a replica of the project; a node that is the
// project's home, or holds no row, archives.
func TestRelayGuardKnowsAReplicaFromTheRegistry(t *testing.T) {
	g, reg, ref := guardFixture(t, true)
	ctx := context.Background()
	if rep, err := g.replica(ctx, ref); rep || err != nil {
		t.Fatalf("no row: %v, %v", rep, err)
	}
	if err := reg.CreateReplica(ctx, &registry.Replica{Identifier: registry.ReplicaIdentifier(ref, "us-east-1", "abc123"), Ref: ref, NodeID: "n2", Status: "ACTIVE_HEALTHY"}); err != nil {
		t.Fatal(err)
	}
	// A manual promotion removes standby.signal; the row is what still says replica.
	if rep, err := g.replica(ctx, ref); !rep || err != nil {
		t.Fatalf("a replica row on this node: %v, %v", rep, err)
	}
	// A move makes this node the home: the row goes, and the project archives.
	if err := reg.SetProjectNode(ctx, ref, "n2", 1); err != nil {
		t.Fatal(err)
	}
	if rep, err := g.replica(ctx, ref); rep || err != nil {
		t.Fatalf("after the move: %v, %v", rep, err)
	}
	if e, err := g.epoch(ctx); err != nil || e != 1 {
		t.Fatalf("epoch = %d, %v", e, err)
	}
}

// The node is found by the id in its certificate: a node that joined under a name other than the one the
// config gives now (`node token --name`, a renamed host) still archives for the projects it is home to and
// is still known as the holder of the replicas it has. Looking it up by name would answer an error, which
// the push guard turns into a 503 for every project's WAL.
func TestRelayGuardFindsTheNodeByItsCertificateNotItsName(t *testing.T) {
	g, reg, ref := guardFixture(t, true)
	ctx := context.Background()
	g.cfg.Node.Name = "renamed-host"
	if _, err := reg.GetNodeByName(ctx, g.cfg.NodeName()); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("the registry knows %q: %v", g.cfg.NodeName(), err)
	}
	// Without a readable certificate there is nothing but the name, and the guard says so.
	if _, err := g.replica(ctx, ref); err == nil || !strings.Contains(err.Error(), "renamed-host") {
		t.Fatalf("an unreadable certificate and an unknown name: %v", err)
	}
	// With the certificate of n2 the name does not matter.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := cluster.NewCA(sec)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := reg.GetNodeByName(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ca.Issue(pub, n2.ID, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.clusterDir, config.NodeCertFile), issued.PEM(), 0o644); err != nil {
		t.Fatal(err)
	}
	if rep, err := g.replica(ctx, ref); rep || err != nil {
		t.Fatalf("no row yet: %v, %v", rep, err)
	}
	if err := reg.CreateReplica(ctx, &registry.Replica{Identifier: registry.ReplicaIdentifier(ref, "us-east-1", "abc123"), Ref: ref, NodeID: n2.ID, Status: "ACTIVE_HEALTHY"}); err != nil {
		t.Fatal(err)
	}
	if rep, err := g.replica(ctx, ref); !rep || err != nil {
		t.Fatalf("a replica row of the node the certificate names: %v, %v", rep, err)
	}
}

// Before the registry can be read the guard answers from the data directory, and tries again later.
func TestRelayGuardFallsBackToTheDataDirectory(t *testing.T) {
	g, _, ref := guardFixture(t, true)
	g.open = func(context.Context) (registry.Registry, error) { return nil, errors.New("connection refused") }
	ctx := context.Background()
	if rep, err := g.replica(ctx, ref); rep || err != nil {
		t.Fatalf("no data directory: %v, %v", rep, err)
	}
	data := g.cfg.Paths().PostgresData(ref)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "standby.signal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if rep, err := g.replica(ctx, ref); !rep || err != nil {
		t.Fatalf("standby.signal: %v, %v", rep, err)
	}
	if _, err := g.epoch(ctx); err == nil {
		t.Fatal("an epoch from a registry nobody can read")
	}
	// The next try is not before ten seconds have passed: the guard does not dial on every push.
	opened := 0
	g.open = func(context.Context) (registry.Registry, error) { opened++; return registry.NewMemory(), nil }
	_, _ = g.replica(ctx, ref)
	if opened != 0 {
		t.Fatal("the guard dialed again at once")
	}
	g.lastTry = time.Now().Add(-time.Minute)
	_, _ = g.replica(ctx, ref)
	if opened != 1 {
		t.Fatalf("the guard dialed %d times after the pause", opened)
	}
}

// A refused push becomes a critical alert for the project, with the reason.
func TestRelayGuardRaisesTheAlertForARefusedPush(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Alerts.Webhooks = []config.AlertWebhook{{URL: srv.URL}}
	alerts.SetDefault(alerts.New(cfg, alerts.Options{Log: quiet()}))
	t.Cleanup(func() { alerts.SetDefault(nil) })
	g := newRelayGuard(cfg, "", quiet())
	g.refused("abcdefghijklmnopqrst", errors.New("this node holds a replica of abcdefghijklmnopqrst, which archives WAL only after its promotion (no promote.ok)"))
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("webhook calls: %d", len(bodies))
	}
	for _, want := range []string{`"kind":"replica_unhealthy"`, `"severity":"critical"`, "abcdefghijklmnopqrst", "no promote.ok", "archive is intact"} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("the alert lacks %s: %s", want, bodies[0])
		}
	}
}
