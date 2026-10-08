package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// TestPostgresRegistry runs the proxy over the real Postgres registry and real
// LISTEN/NOTIFY, with keys opened from sealed project_secrets (RegistryKeys). It
// needs SUPAVISE_TEST_DATABASE_URL, a role that may create databases in a throwaway
// cluster; it runs in a database of its own, so it cannot collide with other
// packages' tests that use the same DSN.
func TestPostgresRegistry(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	reg, err := registry.Open(ctx, privateDatabase(t, dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	sec, err := secrets.New(secrets.RandomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	seal := func(ref string, k *secrets.ProjectKeys) {
		t.Helper()
		for name, v := range k.Map() {
			blob, err := sec.Seal([]byte(v))
			if err != nil {
				t.Fatal(err)
			}
			if err := reg.PutSecret(ctx, ref, name, blob); err != nil {
				t.Fatal(err)
			}
		}
	}

	up := newUpstream(t, "postgrest")
	cfg := config.Default()
	cfg.Domain, cfg.TLS.Mode, cfg.StateDir = testDomain, "off", t.TempDir()
	srv, err := New(Options{Config: cfg, Registry: reg, Keys: RegistryKeys{Registry: reg, Secrets: sec}, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	srv.upstreamFn = func(service, project) string { return up.addr() }
	srv.table.retry = 50 * time.Millisecond
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { srv.Sync(sctx); close(done) }()
	defer func() { cancel(); <-done }()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(ref, apikey string) int {
		t.Helper()
		r, _ := http.NewRequest("GET", ts.URL+"/rest/v1/x", nil)
		r.Host = ref + ".api." + testDomain
		r.Header.Set("apikey", apikey)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	k := testKeys(t, testRef)
	p := &registry.Project{Ref: testRef, Name: "p", Status: registry.StatusActiveHealthy, Engine: registry.EnginePostgres}
	if err := reg.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	seal(testRef, k)
	eventually(t, "project and keys visible through NOTIFY", func() bool { return get(testRef, k.PublishableKey) == 200 })

	// Rotation: sealed secrets change, the NOTIFY drops the cached keys.
	k2 := testKeys(t, testRef)
	seal(testRef, k2)
	eventually(t, "rotated keys take effect", func() bool { return get(testRef, k.PublishableKey) == 401 && get(testRef, k2.PublishableKey) == 200 })

	// Kill the LISTEN connection: the proxy resubscribes and reloads.
	if _, err := reg.Pool().Exec(ctx, `select pg_terminate_backend(pid) from pg_stat_activity
		where pid <> pg_backend_pid() and datname = current_database() and query ilike 'listen supavise_changes%'`); err != nil {
		t.Fatal(err)
	}
	const ref2 = "mmmmmmmmmmnnnnnnnnnn"
	k3 := testKeys(t, ref2)
	if err := reg.CreateProject(ctx, &registry.Project{Ref: ref2, Name: "p2", Status: registry.StatusActiveHealthy, Engine: registry.EnginePostgres}); err != nil {
		t.Fatal(err)
	}
	seal(ref2, k3)
	eventually(t, "project created around a dropped LISTEN connection", func() bool { return get(ref2, k3.PublishableKey) == 200 })

	if err := reg.DeleteProject(ctx, ref2); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deleted project gone", func() bool { return get(ref2, k3.PublishableKey) == 404 })
}

// privateDatabase creates a database that only this test uses, drops it when the
// test ends, and returns its DSN. Other packages' tests truncate tables in the
// database named by SUPAVISE_TEST_DATABASE_URL while their binaries run in parallel.
func privateDatabase(t *testing.T, dsn string) string {
	t.Helper()
	ctx := context.Background()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "supavise_proxy_test_" + hex.EncodeToString(b[:])
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Errorf("dropping %s: %v", name, err)
			return
		}
		defer c.Close(context.Background())
		// Connections close in earlier cleanups; force covers a straggler.
		if _, err := c.Exec(context.Background(), "drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)"); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	})
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		return u.String()
	}
	return dsn + " dbname=" + name // keyword/value DSN: the last dbname wins
}

// TestPostgresReadOnlyRegistry runs the proxy of a follower: its registry is opened read-only, cannot
// LISTEN, and tells the table to read each table again whenever the cluster's change counter moves.
// Nodes, replicas and projects written through the leader's registry reach the routes of the follower.
func TestPostgresReadOnlyRegistry(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	private := privateDatabase(t, dsn)
	w, err := registry.Open(ctx, private)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ro, err := registry.OpenReadOnly(ctx, private)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	sec, err := secrets.New(secrets.RandomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	k := testKeys(t, testRef)
	if err := w.CreateProject(ctx, &registry.Project{Ref: testRef, Name: "p", Status: registry.StatusActiveHealthy, Engine: registry.EnginePostgres}); err != nil {
		t.Fatal(err)
	}
	for name, v := range k.Map() {
		blob, err := sec.Seal([]byte(v))
		if err != nil {
			t.Fatal(err)
		}
		if err := w.PutSecret(ctx, testRef, name, blob); err != nil {
			t.Fatal(err)
		}
	}

	primary, standby := newUpstream(t, "primary"), newUpstream(t, "replica")
	cfg := config.Default()
	cfg.Domain, cfg.TLS.Mode, cfg.StateDir = testDomain, "off", t.TempDir()
	srv, err := New(Options{Config: cfg, Registry: ro, Keys: RegistryKeys{Registry: ro, Secrets: sec}, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	srv.upstreamFn = func(service, project) string { return primary.addr() }
	srv.replicaUpstreamFn = func(project, replica) string { return standby.addr() }
	srv.table.retry = 50 * time.Millisecond
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { srv.Sync(sctx); close(done) }()
	defer func() { cancel(); <-done }()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status := func(host string) int {
		t.Helper()
		r, _ := http.NewRequest("GET", ts.URL+"/rest/v1/x", nil)
		r.Host = host
		r.Header.Set("apikey", k.PublishableKey)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// The poll runs once a second.
	until := func(what string, f func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if f() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	if status(testRef+".api."+testDomain) != 200 {
		t.Fatal("the project is not served")
	}

	// A second node, a replica on it, and a status change: all written on the leader's side.
	node := &registry.Node{Name: "eu-1", Region: "eu", State: registry.NodeActive}
	if err := w.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := w.CreateReplica(ctx, &registry.Replica{Identifier: repEU, Ref: testRef, NodeID: node.ID}); err != nil {
		t.Fatal(err)
	}
	repHost, lbHost := repEU+".api."+testDomain, testRef+"-lb.api."+testDomain
	until("the replica to reach the follower's routes", func() bool { return srv.table.routeKind(repHost) == kindReplica })
	if srv.table.routeKind(lbHost) != kindBalancer {
		t.Error("the balancer does not exist while the project has a replica")
	}
	if got := status(repHost); got != 503 {
		t.Errorf("a replica still setting up answers %d, want 503", got)
	}
	if err := w.SetReplicaStatus(ctx, repEU, string(registry.StatusActiveHealthy), registry.ReplicaStepDone, ""); err != nil {
		t.Fatal(err)
	}
	until("the replica to be served", func() bool { return status(repHost) == 200 })
	if standby.count() == 0 || primary.count() != 1 {
		t.Errorf("replica asked %d times, primary %d", standby.count(), primary.count())
	}
	if !srv.table.nodeActive(node.ID) {
		t.Error("the node's state did not reach the table")
	}
	if err := w.SetNodeState(ctx, node.ID, registry.NodeFenced); err != nil {
		t.Fatal(err)
	}
	until("the node state to change", func() bool { return !srv.table.nodeActive(node.ID) })

	// The project's own status moves with the poll as well.
	if err := w.SetProjectStatus(ctx, testRef, registry.StatusInactive); err != nil {
		t.Fatal(err)
	}
	until("the project status to change", func() bool { return status(testRef+".api."+testDomain) == 503 })
	if err := w.SetProjectStatus(ctx, testRef, registry.StatusActiveHealthy); err != nil {
		t.Fatal(err)
	}
	until("the project to be active again", func() bool { return status(testRef+".api."+testDomain) == 200 })

	if err := w.DeleteReplica(ctx, repEU); err != nil {
		t.Fatal(err)
	}
	until("the replica to leave the follower's routes", func() bool {
		return srv.table.routeKind(repHost) == "" && srv.table.routeKind(lbHost) == ""
	})
}
