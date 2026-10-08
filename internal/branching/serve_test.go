package branching_test

// TestServe starts a node on real clusters, a parent project with a table, rows and
// migrations, the Management API with the branching service, the sweeper and supavise-pgmeta,
// and keeps them up so the real Supabase CLI and MCP server can be pointed at it:
//
//	SUPAVISE_BRANCH_SERVE_FILE=$PWD/stack.json SUPAVISE_TEST_UNPACKED=~/.cache/sbctl/unpacked \
//	  go test ./internal/branching -run TestServe -timeout 60m
//
// The file gets {api_url, pat, parent_ref, state_dir}; delete it to stop everything.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pelletier/go-toml/v2"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

type dirArts map[string]string

func (d dirArts) Dir(svc string) (string, error) {
	if dir, ok := d[svc]; ok {
		return dir, nil
	}
	return "", fmt.Errorf("no artifact for %s", svc)
}
func (d dirArts) Tag(svc string) (string, error) { return filepath.Base(d[svc]), nil }

func TestServe(t *testing.T) {
	out := os.Getenv("SUPAVISE_BRANCH_SERVE_FILE")
	root := os.Getenv("SUPAVISE_TEST_UNPACKED")
	if out == "" || root == "" {
		t.Skip("set SUPAVISE_BRANCH_SERVE_FILE and SUPAVISE_TEST_UNPACKED to serve the stack for manual client runs")
	}
	arts := dirArts{}
	for svc, glob := range map[string]string{config.SvcPostgres: "postgres-17*", config.SvcGoTrue: "auth-*", config.SvcPostgREST: "postgrest-*"} {
		m, _ := filepath.Glob(filepath.Join(root, glob))
		if len(m) == 0 {
			t.Fatalf("no %s artifact under %s", svc, root)
		}
		arts[svc] = m[len(m)-1]
	}
	pgmeta, _ := filepath.Glob(filepath.Join(root, "pgmeta-*", "bin", "pgmeta"))
	if len(pgmeta) == 0 {
		t.Fatal("no pgmeta artifact")
	}
	state, err := os.MkdirTemp("/tmp", "sbts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })

	bin := filepath.Join(state, "supavise")
	build := exec.Command("go", "build", "-o", bin, "./cmd/supavise")
	_, thisFile, _, _ := runtime.Caller(0)
	build.Dir = filepath.Join(filepath.Dir(thisFile), "..", "..")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}

	base := freeBase(t, 40)
	cfg := config.Default()
	cfg.StateDir = filepath.Join(state, "s")
	cfg.KeyPath = filepath.Join(state, "master.key")
	cfg.Supervisor = config.SupervisorExec
	cfg.Domain = "supavise.test"
	cfg.TLS.Mode = "off"
	cfg.BinPath = bin
	cfg.Backup.Backend = "file://" + filepath.Join(state, "backups")
	cfg.Backup.ArchiveTimeoutSeconds = 30
	cfg.Ports.SystemPostgres, cfg.Ports.SystemGoTrue, cfg.Ports.ProjectBase = base, base+1, base+2
	cfg.Ports.PGMeta = base + 36 // and +1 for its admin app
	cfg.API.PGMetaCryptoKey = "serve-test-key"
	cfg.Branching.SweepIntervalSeconds = 5
	cfgPath := filepath.Join(state, "config.toml")
	b, _ := toml.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUPAVISE_CONFIG", cfgPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oo := lifecycle.OpenOptions{Artifacts: arts, ConfigPath: cfgPath}
	t.Cleanup(func() { _ = lifecycle.StopAll(context.Background(), cfg, oo) })
	n, err := lifecycle.InitSystem(ctx, cfg, oo, false)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	meta := exec.Command(pgmeta[0])
	meta.Env = append(os.Environ(), fmt.Sprintf("PG_META_PORT=%d", cfg.Ports.PGMeta), "PG_META_HOST=127.0.0.1", "CRYPTO_KEY="+cfg.API.PGMetaCryptoKey,
		fmt.Sprintf("PG_META_ADMIN_PORT=%d", cfg.Ports.PGMeta+1), "NODE_OPTIONS=--max-old-space-size=192")
	meta.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := meta.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-meta.Process.Pid, syscall.SIGKILL); _, _ = meta.Process.Wait() })
	for i := 0; ; i++ {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Ports.PGMeta))
		if err == nil {
			resp.Body.Close()
			break
		}
		if i > 100 {
			t.Fatal("pgmeta did not start")
		}
		time.Sleep(200 * time.Millisecond)
	}

	store, err := backup.OpenStore(ctx, cfg.Backup)
	if err != nil {
		t.Fatal(err)
	}
	bk, err := backup.New(backup.Options{Config: cfg, Registry: n.Registry, Store: store, Secrets: n.Secrets,
		Access: backup.AccessFromRegistry(cfg, n.Registry, n.Secrets), ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	bk.SetManager(n.Engine)
	svc, err := branching.New(branching.Deps{Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets, Engine: n.Engine, Backup: bk, CreateWait: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	go svc.Run(ctx) //nolint:errcheck
	defer svc.Drain(context.Background())

	parent, err := n.Engine.Create(ctx, lifecycle.CreateRequest{Name: "Parent", Class: "micro"})
	if err != nil {
		t.Fatal(err)
	}
	dsn, _ := n.Engine.ConnString(ctx, parent.Ref, "postgres")
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `
		create table public.items (id int primary key, name text);
		insert into public.items select g, 'row ' || g from generate_series(1, 500) g;
		create schema supabase_migrations;
		create table supabase_migrations.schema_migrations (version text not null primary key, statements text[], name text);
		insert into supabase_migrations.schema_migrations values
		  ('20260101000000', array['create table if not exists public.items (id int primary key, name text)'], 'items')`); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)

	srv := api.New(api.Deps{Registry: n.Registry, Secrets: n.Secrets, Manager: n.Engine, Config: cfg, Branching: svc, CreateWait: 30 * time.Second})
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+34))
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go hs.Serve(ln) //nolint:errcheck
	defer hs.Close()

	pat := secrets.NewPAT()
	if err := n.Registry.CreateAccessToken(ctx, &registry.AccessToken{UserID: "11111111-2222-4333-8444-555555555555", Name: "serve", Hash: secrets.HashToken(pat), Prefix: pat[:8]}); err != nil {
		t.Fatal(err)
	}
	info, _ := json.MarshalIndent(map[string]any{
		"api_url": "http://" + ln.Addr().String(), "pat": pat, "parent_ref": parent.Ref, "state_dir": cfg.StateDir, "config": cfgPath,
	}, "", "  ")
	if err := os.WriteFile(out, info, 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(55 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if _, err := os.Stat(out); err != nil {
			return
		}
	}
}

func freeBase(t *testing.T, n int) int {
	t.Helper()
	for base := 39000; base+n < 39900; base += n + 3 {
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
