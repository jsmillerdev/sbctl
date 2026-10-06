package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// TestPostgresRegistry runs the proxy over the real Postgres registry and real
// LISTEN/NOTIFY, with keys opened from sealed project_secrets (RegistryKeys). It
// needs SBCTL_TEST_DATABASE_URL, an empty database in a throwaway cluster.
func TestPostgresRegistry(t *testing.T) {
	dsn := os.Getenv("SBCTL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SBCTL_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	reg, err := registry.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if _, err := reg.Pool().Exec(ctx, `truncate sbctl.projects, sbctl.organizations cascade`); err != nil {
		t.Fatal(err)
	}
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
		where pid <> pg_backend_pid() and datname = current_database() and query ilike 'listen sbctl_changes%'`); err != nil {
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
