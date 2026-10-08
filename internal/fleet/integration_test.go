package fleet_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

type globArts map[string]string

func (g globArts) Dir(svc string) (string, error) {
	if d, ok := g[svc]; ok {
		return d, nil
	}
	return "", fmt.Errorf("no artifact for %s", svc)
}
func (g globArts) Tag(svc string) (string, error) { return filepath.Base(g[svc]), nil }

// freeRange finds n consecutive free loopback ports in 37200-37890 such that extra(base)
// ports are free too.
func freeRange(t *testing.T, n int, extra func(base int) []int) int {
	t.Helper()
	free := func(p int) bool {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			return false
		}
		l.Close()
		return true
	}
	for base := 37200; base+n+110 < 37990; base += n + 7 {
		ok := true
		ps := []int{}
		for i := 0; i < n; i++ {
			ps = append(ps, base+i)
		}
		if extra != nil {
			ps = append(ps, extra(base)...)
		}
		for _, p := range ps {
			if !free(p) {
				ok = false
				break
			}
		}
		if ok {
			return base
		}
	}
	t.Fatal("no free port range")
	return 0
}

// TestIntegrationFleetTenants runs the real artifacts under the exec backend: the system
// project, Supavisor, Realtime, Storage and postgres-meta, then one project that the
// Engine registers with all three services on create, re-registers on key rotation and
// removes on delete. It needs SUPAVISE_TEST_UNPACKED to name a directory with unpacked
// slim-services artifacts for this platform (postgres-17*, auth-*, postgrest-*, pooler-*,
// realtime-*, storage-*, pgmeta-*) and about 1 GB of RAM.
func TestIntegrationFleetTenants(t *testing.T) {
	root := os.Getenv("SUPAVISE_TEST_UNPACKED")
	if root == "" {
		t.Skip("SUPAVISE_TEST_UNPACKED not set")
	}
	arts := globArts{}
	for svc, glob := range map[string]string{
		config.SvcPostgres: "postgres-17*", config.SvcGoTrue: "auth-*", config.SvcPostgREST: "postgrest-*",
		config.SvcSupavisor: "pooler-*", config.SvcRealtime: "realtime-*", config.SvcStorage: "storage-*", config.SvcPGMeta: "pgmeta-*",
	} {
		m, _ := filepath.Glob(filepath.Join(root, glob))
		if len(m) == 0 {
			t.Skipf("no %s artifact under %s", svc, root)
		}
		arts[svc] = m[len(m)-1]
	}

	// Ports: base..base+14 for the system cluster and the shared services, base+100..102
	// for the first project, base+6+1369 for Realtime's gen_rpc.
	base := freeRange(t, 15, func(b int) []int { return []int{b + 100, b + 101, b + 102, b + 6 + 1369} })
	cfg := config.Default()
	d, err := os.MkdirTemp("/tmp", "sbfi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	cfg.StateDir = d
	cfg.KeyPath = filepath.Join(d, "master.key")
	cfg.Supervisor = config.SupervisorExec
	cfg.Domain = "supavise.test"
	cfg.TLS.Mode = "off"
	cfg.BinPath = "/usr/bin/true"
	cfg.Ports.SystemPostgres, cfg.Ports.SystemGoTrue, cfg.Ports.ProjectBase = base, base+1, base+97 // project 1: base+100..102
	cfg.Ports.SupavisorSession, cfg.Ports.SupavisorTransaction = base+8, base+9
	cfg.Ports.Realtime, cfg.Ports.Storage, cfg.Ports.StorageAdmin = base+6, base+10, base+11
	cfg.Ports.PGMeta, cfg.Fleet.SupavisorAPIPort = base+12, base+14 // pgmeta's admin port is base+13

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	oo := lifecycle.OpenOptions{Artifacts: arts}
	n, err := lifecycle.InitSystem(ctx, cfg, oo, false)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	deps := fleet.Deps{Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets, Supervisor: n.Supervisor, Artifacts: arts,
		Skip: []string{config.SvcStudio}, ReadyTimeout: 4 * time.Minute, Start: true}
	mgr, err := fleet.NewManager(deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mgr.Stop(context.Background()); err != nil {
			t.Errorf("stop fleet: %v", err)
		}
		if err := lifecycle.StopAll(context.Background(), cfg, oo); err != nil {
			t.Errorf("StopAll: %v", err)
		}
	})
	fl, err := fleet.Setup(ctx, deps)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range mgr.Status(ctx) {
		if !h.Healthy {
			t.Fatalf("service not healthy: %+v", h)
		}
	}
	// Setup with Start leaves the services running; asking again changes nothing.
	if _, err := fleet.Setup(ctx, deps); err != nil {
		t.Fatalf("second Setup: %v", err)
	}

	eng := lifecycle.NewEngine(cfg, n.Registry, n.Secrets, arts, n.Plane, lifecycle.Options{Fleet: fl})
	p, err := eng.Create(ctx, lifecycle.CreateRequest{Class: "micro"})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := eng.Keys(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	host := cfg.ProjectHost(p.Ref)

	// Supavisor: postgres.<ref> logs in on both ports, in the clear and over TLS (the
	// Supabase CLI refuses a database that answers without TLS).
	for _, port := range []int{cfg.Ports.SupavisorSession, cfg.Ports.SupavisorTransaction} {
		for _, mode := range []string{"disable", "require"} {
			dsn := fmt.Sprintf("postgres://postgres.%s:%s@127.0.0.1:%d/postgres?sslmode=%s&default_query_exec_mode=simple_protocol", p.Ref, keys.DBPassword, port, mode)
			c, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatalf("pooler port %d, sslmode=%s: %v", port, mode, err)
			}
			if _, isTLS := c.PgConn().Conn().(*tls.Conn); isTLS != (mode == "require") {
				t.Errorf("pooler port %d, sslmode=%s: TLS = %v", port, mode, isTLS)
			}
			var user string
			if err := c.QueryRow(ctx, "select current_user").Scan(&user); err != nil || user != "postgres" {
				t.Fatalf("pooler port %d, sslmode=%s: user %q, %v", port, mode, user, err)
			}
			c.Close(ctx)
		}
	}
	if _, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres.%s:wrong@127.0.0.1:%d/postgres?sslmode=disable", p.Ref, cfg.Ports.SupavisorSession)); err == nil {
		t.Fatal("pooler accepted a wrong password")
	}

	// Storage: the tenant resolves from x-forwarded-host, the service key authorizes.
	storage := func(key, method, path string, body any) (int, string) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", cfg.Ports.Storage, path), rd)
		req.Header.Set("X-Forwarded-Host", host)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := storage(keys.ServiceRoleKey, "POST", "/bucket", map[string]any{"name": "files"}); code != 200 {
		t.Fatalf("create bucket: %d %s", code, body)
	}
	if code, body := storage(keys.ServiceRoleKey, "GET", "/bucket", nil); code != 200 || !strings.Contains(body, `"files"`) {
		t.Fatalf("list buckets: %d %s", code, body)
	}

	// Realtime: the tenant reaches its database.
	secret := func(name string) string {
		sealed, err := n.Registry.GetSecret(ctx, config.SystemRef, name)
		if err != nil {
			t.Fatal(err)
		}
		pt, err := n.Secrets.Open(sealed)
		if err != nil {
			t.Fatal(err)
		}
		return string(pt)
	}
	realtime := func(method, path string) int {
		tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "test", "exp": time.Now().Add(time.Minute).Unix()}).SignedString([]byte(secret(fleet.SecretRealtimeAPIJWT)))
		req, _ := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", cfg.Ports.Realtime, path), nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := realtime("GET", "/api/tenants/"+p.Ref+"/health"); code != 200 {
		t.Fatalf("realtime tenant health: %d", code)
	}

	// Rotation: the services learn the new keys.
	nk, err := eng.RotateKeys(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := storage(keys.ServiceRoleKey, "GET", "/bucket", nil); code == 200 {
		t.Fatal("storage still accepts the old service key after rotation")
	}
	if code, body := storage(nk.ServiceRoleKey, "GET", "/bucket", nil); code != 200 {
		t.Fatalf("new service key: %d %s", code, body)
	}

	// Delete removes the tenants.
	if err := eng.Delete(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Registry.GetProject(ctx, p.Ref); err == nil || err != registry.ErrNotFound {
		t.Fatalf("project survived: %v", err)
	}
	if code := realtime("GET", "/api/tenants/"+p.Ref); code != 404 {
		t.Fatalf("realtime tenant after delete: %d", code)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/tenants/%s", cfg.Ports.StorageAdmin, p.Ref), nil)
	req.Header.Set("apikey", secret(fleet.SecretStorageAdminAPIKey))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("storage tenant after delete: %d", resp.StatusCode)
	}
	if _, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres.%s:%s@127.0.0.1:%d/postgres?sslmode=disable", p.Ref, keys.DBPassword, cfg.Ports.SupavisorSession)); err == nil {
		t.Fatal("pooler still knows the deleted tenant")
	}
}
