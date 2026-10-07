package lifecycle

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// dirArts serves unpacked artifact directories picked by glob; Tag is only a label.
type dirArts map[string]string

func (d dirArts) Dir(svc string) (string, error) {
	if dir, ok := d[svc]; ok {
		return dir, nil
	}
	return "", fmt.Errorf("no artifact for %s", svc)
}
func (d dirArts) Tag(svc string) (string, error) { return filepath.Base(d[svc]), nil }

// freePortBase finds n consecutive free loopback ports in the private range 34200-34900.
func freePortBase(t *testing.T, n int) int {
	t.Helper()
	for base := 34200; base+n < 34900; base += n + 3 {
		var ls []net.Listener
		ok := true
		for i := 0; i < n; i++ {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				ok = false
				break
			}
			ls = append(ls, l)
		}
		for _, l := range ls {
			l.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatal("no free port range")
	return 0
}

// TestIntegrationSystemAndProject runs the real artifacts under the exec backend:
// system init, one project, health, SQL, pause, resume, key rotation, delete, stop.
// It needs SUPAVISE_TEST_UNPACKED to name a directory holding unpacked slim-services
// artifacts (postgres-*, auth-*, postgrest-*) for this platform, for example
// ~/.cache/sbctl/unpacked. It starts two PostgreSQL clusters (a few tens of MB each).
func TestIntegrationSystemAndProject(t *testing.T) {
	root := os.Getenv("SUPAVISE_TEST_UNPACKED")
	if root == "" {
		t.Skip("SUPAVISE_TEST_UNPACKED not set")
	}
	arts := dirArts{}
	for svc, glob := range map[string]string{config.SvcPostgres: "postgres-17*", config.SvcGoTrue: "auth-*", config.SvcPostgREST: "postgrest-*"} {
		m, _ := filepath.Glob(filepath.Join(root, glob))
		if len(m) == 0 {
			t.Skipf("no %s artifact under %s", svc, root)
		}
		arts[svc] = m[len(m)-1]
	}
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1)")
	}

	base := freePortBase(t, 8)
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.KeyPath = filepath.Join(cfg.StateDir, "master.key")
	cfg.Supervisor = config.SupervisorExec
	cfg.Domain = "supavise.test"
	cfg.TLS.Mode = "off"
	cfg.BinPath = truePath // archive_command succeeds, so WAL does not pile up
	cfg.Ports.SystemPostgres, cfg.Ports.SystemGoTrue, cfg.Ports.ProjectBase = base, base+1, base+2

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	oo := OpenOptions{Artifacts: arts}
	t.Cleanup(func() {
		if err := StopAll(context.Background(), cfg, oo); err != nil {
			t.Errorf("StopAll: %v", err)
		}
	})

	n, err := InitSystem(ctx, cfg, oo, false)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	sys, err := n.Registry.GetProject(ctx, config.SystemRef)
	if err != nil || sys.Status != registry.StatusActiveHealthy {
		t.Fatalf("system = %+v %v", sys, err)
	}
	if hs := n.Plane.Health(ctx, sys, nil); !allHealthy(hs) || len(hs) != 2 {
		t.Fatalf("system health = %+v", hs)
	}
	for _, db := range SystemDatabases {
		c, err := connect(ctx, SystemSocketDSN(cfg, db))
		if err != nil {
			t.Fatalf("database %s: %v", db, err)
		}
		c.Close(ctx)
	}
	// Running init again on the live node is a no-op that keeps the credentials.
	before, _ := n.Engine.Keys(ctx, config.SystemRef)
	n2, err := InitSystem(ctx, cfg, oo, false)
	if err != nil {
		t.Fatalf("second init: %v", err)
	}
	n2.Close()
	if after, _ := n.Engine.Keys(ctx, config.SystemRef); *after != *before {
		t.Fatal("second init changed the system credentials")
	}

	// Each fleet service has its own role: it reaches its database and nothing else.
	creds, err := n.Engine.FleetCredentials(ctx)
	if err != nil || len(creds) != len(FleetRoles) {
		t.Fatalf("fleet credentials = %+v %v", creds, err)
	}
	for _, fc := range creds {
		own, err := pgx.Connect(ctx, dsnURL(fc.Role, fc.Password, cfg.Ports.SystemPostgres, fc.Database))
		if err != nil {
			t.Fatalf("%s into %s: %v", fc.Role, fc.Database, err)
		}
		own.Close(ctx)
		for _, other := range []string{"supavise", "_supavisor", "_realtime", "_storage"} {
			if other == fc.Database {
				continue
			}
			if oc, err := pgx.Connect(ctx, dsnURL(fc.Role, fc.Password, cfg.Ports.SystemPostgres, other)); err == nil {
				oc.Close(ctx)
				t.Fatalf("%s can connect to %s", fc.Role, other)
			}
		}
	}

	// The passwords were set during init; none may appear in the Postgres log (the
	// artifact logs DDL, so a plaintext ALTER ROLE would end up there).
	var sysPasswords []string
	for _, fc := range creds {
		sysPasswords = append(sysPasswords, fc.Password)
	}
	skeys, _ := n.Engine.Keys(ctx, config.SystemRef)
	sysPasswords = append(sysPasswords, skeys.DBPassword, skeys.AdminPassword, skeys.AuthenticatorPassword, skeys.AuthAdminPassword, skeys.StorageAdminPassword, skeys.ReplicationPassword)
	assertNotInLogs(t, cfg, sysPasswords...)

	p, err := n.Engine.Create(ctx, CreateRequest{Name: "it", Class: "micro"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != registry.StatusActiveHealthy || p.Seq != 1 {
		t.Fatalf("project = %+v", p)
	}
	ports := cfg.PortsFor(p.Ref, p.Seq)
	keys, _ := n.Engine.Keys(ctx, p.Ref)
	assertNotInLogs(t, cfg, keys.DBPassword, keys.AdminPassword, keys.AuthenticatorPassword, keys.AuthAdminPassword, keys.StorageAdminPassword, keys.ReplicationPassword)

	// Services answer, with real requests.
	get := func(url, bearer string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get(fmt.Sprintf("http://127.0.0.1:%d/health", ports.GoTrue), ""); c != 200 {
		t.Fatalf("gotrue /health = %d", c)
	}
	if c := get(fmt.Sprintf("http://127.0.0.1:%d/", ports.PostgREST), keys.AnonKey); c != 200 {
		t.Fatalf("postgrest / = %d", c)
	}

	// SQL over TCP with the stored password; the micro class and archiving are in effect.
	dsn, err := n.Engine.ConnString(ctx, p.Ref, RolePostgres)
	if err != nil {
		t.Fatal(err)
	}
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var sb, mc, wl, ac string
	if err := c.QueryRow(ctx, `select current_setting('shared_buffers'), current_setting('max_connections'), current_setting('wal_level'), current_setting('archive_command')`).Scan(&sb, &mc, &wl, &ac); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
	if sb != "16MB" || mc != "30" || wl != "logical" || !strings.Contains(ac, "wal push --ref "+p.Ref) {
		t.Fatalf("settings: shared_buffers=%s max_connections=%s wal_level=%s archive_command=%s", sb, mc, wl, ac)
	}
	// Loopback TCP without a password is refused.
	if c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", ports.Postgres)); err == nil {
		c.Close(ctx)
		t.Fatal("passwordless TCP login was accepted")
	}
	// Replication role password works (base backups connect with it).
	rc, err := pgx.Connect(ctx, dsnURL(RoleReplication, keys.ReplicationPassword, ports.Postgres, "postgres"))
	if err != nil {
		t.Fatalf("replication role: %v", err)
	}
	rc.Close(ctx)

	// pg_cron runs its jobs in the cluster's background workers: over libpq it would fail with
	// "connection failed" (nothing trusts a loopback connection) and the job would never succeed.
	cc, err := connect(ctx, socketDSN(pathsFor(cfg, p.Ref, ports.Postgres), "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`create extension if not exists pg_cron`, `select cron.schedule('it-job', '1 seconds', 'select 1')`} {
		if _, err := cc.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var cronStatus, cronMsg string
	for i := 0; i < 60; i++ {
		err := cc.QueryRow(ctx, `select status, coalesce(return_message, '') from cron.job_run_details order by runid desc limit 1`).Scan(&cronStatus, &cronMsg)
		if err == nil && cronStatus == "succeeded" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if cronStatus != "succeeded" {
		t.Fatalf("pg_cron job: status %q (%s), want succeeded", cronStatus, cronMsg)
	}
	cc.Close(ctx)

	// Pause stops everything; resume brings it back.
	if err := n.Engine.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if hs, _ := n.Engine.Health(ctx, p.Ref); allHealthy(hs) {
		t.Fatalf("paused project reports healthy: %+v", hs)
	}
	if err := n.Engine.Resume(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if hs, _ := n.Engine.Health(ctx, p.Ref); !allHealthy(hs) || len(hs) != 3 {
		t.Fatalf("after resume: %+v", hs)
	}

	// Rotation: the old anon key stops working and the new one works.
	nk, err := n.Engine.RotateKeys(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if c := get(fmt.Sprintf("http://127.0.0.1:%d/", ports.PostgREST), keys.AnonKey); c != 401 {
		t.Fatalf("old key after rotation = %d, want 401", c)
	}
	if c := get(fmt.Sprintf("http://127.0.0.1:%d/", ports.PostgREST), nk.AnonKey); c != 200 {
		t.Fatalf("new key after rotation = %d, want 200", c)
	}

	// Delete removes units, data and rows.
	if err := n.Engine.Delete(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.Paths().Project(p.Ref)); !os.IsNotExist(err) {
		t.Fatalf("project directory remains: %v", err)
	}
	if _, err := n.Registry.GetProject(ctx, p.Ref); err == nil {
		t.Fatal("registry row remains")
	}
	if get := func() error {
		_, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", ports.Postgres), time.Second)
		return err
	}(); get == nil {
		t.Fatal("postgres still listens after delete")
	}

	// An init interrupted during the launcher's first boot leaves a witness file and a
	// cluster nobody can use; running init again starts over instead of failing.
	n.Close()
	if err := StopAll(ctx, cfg, oo); err != nil {
		t.Fatal(err)
	}
	witness := filepath.Join(pathsFor(cfg, config.SystemRef, cfg.Ports.SystemPostgres).Data, initPendingWitness)
	if err := os.WriteFile(witness, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	n3, err := InitSystem(ctx, cfg, oo, false)
	if err != nil {
		t.Fatalf("init after an interrupted init: %v", err)
	}
	defer n3.Close()
	if after, err := n3.Engine.Keys(ctx, config.SystemRef); err != nil || after.JWTSecret == before.JWTSecret {
		t.Fatalf("expected fresh credentials after starting over: %v", err)
	}
	if _, err := os.Stat(witness); !os.IsNotExist(err) {
		t.Fatalf("witness remains after re-init: %v", err)
	}
}

// assertNotInLogs fails if any of the secrets appears in a unit log file of the exec backend.
func assertNotInLogs(t *testing.T, cfg *config.Config, secrets ...string) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(cfg.StateDir, "logs", "*.log"))
	if len(files) == 0 {
		t.Fatal("no unit logs found to check")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range secrets {
			if s != "" && strings.Contains(string(b), s) {
				t.Errorf("a stored password appears in %s", filepath.Base(f))
			}
		}
	}
}

func allHealthy(hs []ServiceHealth) bool {
	for _, h := range hs {
		if !h.Healthy {
			return false
		}
	}
	return len(hs) > 0
}

// TestIntegrationInitFailureCleansUp makes PostgreSQL fail to bind and checks that
// Create leaves INIT_FAILED with nothing running or on disk.
func TestIntegrationInitFailureCleansUp(t *testing.T) {
	root := os.Getenv("SUPAVISE_TEST_UNPACKED")
	if root == "" {
		t.Skip("SUPAVISE_TEST_UNPACKED not set")
	}
	arts := dirArts{}
	for svc, glob := range map[string]string{config.SvcPostgres: "postgres-17*", config.SvcGoTrue: "auth-*", config.SvcPostgREST: "postgrest-*"} {
		m, _ := filepath.Glob(filepath.Join(root, glob))
		if len(m) == 0 {
			t.Skipf("no %s artifact under %s", svc, root)
		}
		arts[svc] = m[len(m)-1]
	}
	truePath, _ := exec.LookPath("true")
	base := freePortBase(t, 8)
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.KeyPath = filepath.Join(cfg.StateDir, "master.key")
	cfg.Supervisor = config.SupervisorExec
	cfg.Domain, cfg.BinPath = "supavise.test", truePath
	cfg.TLS.Mode = "off"
	cfg.Ports.SystemPostgres, cfg.Ports.SystemGoTrue, cfg.Ports.ProjectBase = base, base+1, base+2

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	oo := OpenOptions{Artifacts: arts}
	t.Cleanup(func() { StopAll(context.Background(), cfg, oo) })
	n, err := InitSystem(ctx, cfg, oo, false)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	// Occupy the port project 1 would use for PostgreSQL.
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.PortsFor("x", 1).Postgres))
	if err != nil {
		t.Skipf("cannot occupy port: %v", err)
	}
	defer l.Close()
	_, err = n.Engine.Create(ctx, CreateRequest{Ref: "abcdefghijklmnopqrst", Class: "micro"})
	if err == nil || !strings.Contains(err.Error(), "INIT_FAILED") {
		t.Fatalf("err = %v", err)
	}
	p, gerr := n.Registry.GetProject(ctx, "abcdefghijklmnopqrst")
	if gerr != nil || p.Status != registry.StatusInitFailed {
		t.Fatalf("project = %+v %v", p, gerr)
	}
	if _, err := os.Stat(cfg.Paths().Project("abcdefghijklmnopqrst")); !os.IsNotExist(err) {
		t.Fatalf("data left behind: %v", err)
	}
	if st, _ := n.Supervisor.Status(ctx, config.UnitName(config.SvcPostgres, "abcdefghijklmnopqrst")); st.State == "active" {
		t.Fatal("postgres still running")
	}
	if err := n.Engine.DeleteWith(ctx, p.Ref, DeleteOptions{SkipFinalBackup: true}); err != nil {
		t.Fatal(err)
	}
}
