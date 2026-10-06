package api

// Integration tests against a real Postgres and a real sb-pgmeta, both unpacked
// from the slim-services darwin-arm64 artifacts. They are gated: they start two
// processes and cost a few hundred MB.
//
//	SBCTL_API_INTEGRATION=1 \
//	SBCTL_PG_BIN=$HOME/.cache/sbctl/unpacked/postgres-17.11.0.004-r1-darwin-arm64/bin \
//	SBCTL_PGMETA_BIN=$HOME/.cache/sbctl/unpacked/pgmeta-v0.100.0-r0-darwin-arm64/bin/pgmeta \
//	scripts/guard.sh -- go test ./internal/api -run Integration -v
//
// Everything listens on 127.0.0.1 in 32100-32999 and is stopped by test cleanup.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

type stack struct {
	t       testing.TB
	APIURL  string
	DSN     string // postgres superuser DSN of the project database
	PGPort  int
	Ref     string
	JWT     string // dashboard session
	PAT     string
	Manager *fakeManager
	Cfg     *config.Config
	Server  *Server
}

// freePort returns a free TCP port in the private range 32100-32999.
func freePort(t testing.TB) int {
	t.Helper()
	for p := 32100 + int(time.Now().UnixNano()%400); p < 33000; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			l.Close()
			return p
		}
	}
	t.Fatal("no free port in 32100-32999")
	return 0
}

func run(t testing.TB, name string, args ...string) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func waitHTTP(t testing.TB, url string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s did not come up", url)
}

// startStack starts Postgres (one instance, small), sb-pgmeta and the API server.
func startStack(t testing.TB) *stack {
	t.Helper()
	if os.Getenv("SBCTL_API_INTEGRATION") == "" {
		t.Skip("set SBCTL_API_INTEGRATION=1 (see integration_test.go)")
	}
	pgBin, pgmetaBin := os.Getenv("SBCTL_PG_BIN"), os.Getenv("SBCTL_PGMETA_BIN")
	if pgBin == "" || pgmetaBin == "" {
		t.Skip("set SBCTL_PG_BIN and SBCTL_PGMETA_BIN to the unpacked artifacts")
	}
	dir, err := os.MkdirTemp("", "sbctl-api-it-")
	if err != nil {
		t.Fatal(err)
	}
	pgPort, metaPort, apiPort := freePort(t), 0, 0
	data := filepath.Join(dir, "pgdata")
	run(t, filepath.Join(pgBin, "initdb"), "-D", data, "-U", "postgres", "--auth=trust", "-E", "UTF8", "--locale=C")
	pgctl := filepath.Join(pgBin, "pg_ctl")
	run(t, pgctl, "-D", data, "-w", "-l", filepath.Join(dir, "pg.log"), "-o",
		fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1 -c unix_socket_directories=%s -c shared_buffers=16MB -c max_connections=30", pgPort, dir), "start")
	t.Cleanup(func() {
		_ = exec.Command(pgctl, "-D", data, "-m", "immediate", "stop").Run()
		os.RemoveAll(dir)
	})
	admin := fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", pgPort)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `create database sbctl`); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)

	reg, err := registry.Open(ctx, fmt.Sprintf("postgres://postgres@127.0.0.1:%d/sbctl?sslmode=disable&pool_max_conns=4", pgPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)

	for metaPort = pgPort + 3; ; metaPort += 2 { // pg-meta also takes port+1 for its admin app
		if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", metaPort)); err == nil {
			l.Close()
			if l2, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", metaPort+1)); err == nil {
				l2.Close()
				break
			}
		}
	}
	const cryptoKey = "integration-test-key"
	meta := exec.Command(pgmetaBin)
	meta.Env = append(os.Environ(), fmt.Sprintf("PG_META_PORT=%d", metaPort), "PG_META_HOST=127.0.0.1", "CRYPTO_KEY="+cryptoKey,
		"NODE_OPTIONS=--max-old-space-size=192")
	meta.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logf, _ := os.Create(filepath.Join(dir, "pgmeta.log"))
	meta.Stdout, meta.Stderr = logf, logf
	if err := meta.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-meta.Process.Pid, syscall.SIGKILL)
		_, _ = meta.Process.Wait()
	})
	waitHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", metaPort))

	sec, _ := secrets.New(make([]byte, 32))
	cfg := config.Default()
	cfg.Domain = "sbctl.test"
	cfg.TLS.Mode = "off"
	cfg.Ports.PGMeta = metaPort
	cfg.API.PGMetaCryptoKey = cryptoKey
	mgr := newFakeManager(reg, sec)
	mgr.dsn = admin
	org, err := reg.CreateOrganization(ctx, "default", "Default")
	if err != nil {
		t.Fatal(err)
	}
	mgr.addProject(t, config.SystemRef, "system", 0, registry.StatusActiveHealthy)
	ref := "abcdefghijklmnopqrst"
	mgr.addProject(t, ref, "Integration", org.ID, registry.StatusActiveHealthy)
	srv, err := NewServer(Deps{Registry: reg, Secrets: sec, Manager: mgr, Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	apiPort = freePort(t)
	for apiPort == metaPort || apiPort == metaPort+1 {
		apiPort = freePort(t)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", apiPort))
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go hs.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { _ = hs.Close() })

	st := &stack{t: t, APIURL: "http://" + ln.Addr().String(), DSN: fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", pgPort),
		PGPort: pgPort, Ref: ref, Manager: mgr, Cfg: cfg, Server: srv}
	sysKeys, _ := mgr.Keys(ctx, config.SystemRef)
	f := &fixture{t: t, system: sysKeys}
	st.JWT = f.signJWT(map[string]any{"sub": "11111111-2222-4333-8444-555555555555", "email": "dev@example.test", "role": "authenticated"})
	// A PAT, created the way the dashboard does.
	req, _ := http.NewRequest("POST", st.APIURL+"/platform/profile/access-tokens", strings.NewReader(`{"name":"it"}`))
	req.Header.Set("Authorization", "Bearer "+st.JWT)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var tok struct{ Token string }
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	st.PAT = tok.Token
	if st.PAT == "" {
		t.Fatal("could not create a PAT")
	}
	return st
}

// call does an HTTP request with the PAT (or the JWT when jwt is true).
func (s *stack) call(method, path string, body any, jwt bool) (int, []byte) {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, s.APIURL+path, rd)
	tok := s.PAT
	if jwt {
		tok = s.JWT
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestIntegrationDatabase(t *testing.T) {
	s := startStack(t)
	p := "/v1/projects/" + s.Ref
	mustStatus := func(want int, method, path string, body any) []byte {
		t.Helper()
		code, b := s.call(method, path, body, false)
		if code != want {
			t.Fatalf("%s %s: %d %s", method, path, code, b)
		}
		return b
	}

	b := mustStatus(201, "POST", p+"/database/query", map[string]any{"query": "create table public.widgets (id bigint primary key, name text, meta jsonb); insert into public.widgets values (1, 'a', '{\"k\":1}'), (2, null, null); select * from public.widgets order by id"})
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil || len(rows) != 2 || rows[0]["name"] != "a" || rows[1]["name"] != nil {
		t.Fatalf("rows: %s (%v)", b, err)
	}
	if m, _ := rows[0]["meta"].(map[string]any); m["k"] != float64(1) {
		t.Fatalf("jsonb column: %s", b)
	}
	// Errors come back in the envelope with a client error status.
	b = mustStatus(400, "POST", p+"/database/query", map[string]any{"query": "select * from nope"})
	if !strings.Contains(string(b), `"message"`) || !strings.Contains(string(b), "nope") {
		t.Fatalf("error body: %s", b)
	}
	// Parameters bind over the extended protocol (the MCP server's list_tables does this).
	b = mustStatus(201, "POST", p+"/database/query", map[string]any{"query": "select $1::int as n, $2::text as s, name from public.widgets where id = $3", "parameters": []any{5, "x", 1}})
	if strings.TrimSpace(string(b)) != `[{"n":5,"s":"x","name":"a"}]` {
		t.Fatalf("parameterized select: %s", b)
	}
	mustStatus(201, "POST", p+"/database/query", map[string]any{"query": "insert into public.widgets values ($1, $2)", "parameters": []any{3, "c"}})
	b = mustStatus(201, "POST", p+"/database/query", map[string]any{"query": "update public.widgets set name = $1 where id = $2 returning id, name", "parameters": []any{"d", 3}})
	if strings.TrimSpace(string(b)) != `[{"id":3,"name":"d"}]` {
		t.Fatalf("returning: %s", b)
	}
	b = mustStatus(400, "POST", p+"/database/query/read-only", map[string]any{"query": "insert into public.widgets values ($1, $2)", "parameters": []any{4, "e"}})
	if !strings.Contains(string(b), "read-only") {
		t.Fatalf("read-only with parameters: %s", b)
	}
	// Read-only is enforced by Postgres.
	b = mustStatus(400, "POST", p+"/database/query/read-only", map[string]any{"query": "create table public.nope (id int)"})
	if !strings.Contains(string(b), "read-only") {
		t.Fatalf("read-only error: %s", b)
	}
	mustStatus(201, "POST", p+"/database/query", map[string]any{"query": "select 1", "read_only": true})

	// Migrations: list is empty before the first, then shows what was applied.
	if b := mustStatus(200, "GET", p+"/database/migrations", nil); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("migrations before: %s", b)
	}
	mustStatus(200, "POST", p+"/database/migrations", map[string]any{"query": "create table public.gadgets (id int)", "name": "create_gadgets"})
	var migs []map[string]any
	if err := json.Unmarshal(mustStatus(200, "GET", p+"/database/migrations", nil), &migs); err != nil || len(migs) != 1 || migs[0]["name"] != "create_gadgets" {
		t.Fatalf("migrations after: %v %v", migs, err)
	}
	validateAgainstSpec(t, "GET /v1/projects/{ref}/database/migrations", mustJSON2(migs))
	// A failing migration leaves nothing behind.
	mustStatus(400, "POST", p+"/database/migrations", map[string]any{"query": "create table public.half (id int); select * from missing_table", "name": "broken"})
	b = mustStatus(201, "POST", p+"/database/query", map[string]any{"query": "select to_regclass('public.half') as t"})
	if !strings.Contains(string(b), `"t":null`) {
		t.Fatalf("failed migration was not rolled back: %s", b)
	}

	// Types.
	b = mustStatus(200, "GET", p+"/types/typescript?included_schemas=public", nil)
	var ts struct{ Types string }
	_ = json.Unmarshal(b, &ts)
	if !strings.Contains(ts.Types, "widgets") || !strings.Contains(ts.Types, "export type Database") {
		t.Fatalf("types: %s", truncate(string(b), 400))
	}
	validateAgainstSpec(t, "GET /v1/projects/{ref}/types/typescript", b)

	// pg-meta through the platform proxy, with the JWT like Studio.
	code, b := s.call("GET", "/platform/pg-meta/"+s.Ref+"/tables?included_schemas=public", nil, true)
	if code != 200 || !strings.Contains(string(b), `"name":"widgets"`) {
		t.Fatalf("pg-meta tables: %d %s", code, truncate(string(b), 300))
	}
	code, b = s.call("POST", "/platform/pg-meta/"+s.Ref+"/query", map[string]any{"query": "select current_user as u"}, true)
	if code != 200 || !strings.Contains(string(b), `"u":"postgres"`) {
		t.Fatalf("pg-meta query: %d %s", code, b)
	}

	// CLI login role: usable, valid-until set, member of postgres, removable.
	b = mustStatus(201, "POST", p+"/cli/login-role", map[string]any{"read_only": false})
	var lr struct {
		Role, Password string
		TTLSeconds     int `json:"ttl_seconds"`
	}
	_ = json.Unmarshal(b, &lr)
	validateAgainstSpec(t, "POST /v1/projects/{ref}/cli/login-role", b)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/postgres?sslmode=disable", lr.Role, lr.Password, s.PGPort))
	if err != nil {
		t.Fatalf("login role cannot connect: %v", err)
	}
	var cur, role string
	var until *time.Time
	if err := conn.QueryRow(ctx, `select session_user, current_user`).Scan(&cur, &role); err != nil || cur != lr.Role || role != "postgres" {
		t.Fatalf("session_user %q current_user %q: %v", cur, role, err)
	}
	if err := conn.QueryRow(ctx, `select rolvaliduntil from pg_roles where rolname = $1`, lr.Role).Scan(&until); err != nil || until == nil || time.Until(*until) < 50*time.Minute {
		t.Fatalf("valid until %v: %v", until, err)
	}
	conn.Close(ctx)
	mustStatus(200, "DELETE", p+"/cli/login-role", nil)
	b = mustStatus(201, "POST", p+"/database/query", map[string]any{"query": "select count(*) as n from pg_roles where rolname like 'cli\\_login\\_%'"})
	if !strings.Contains(string(b), `"n":0`) {
		t.Fatalf("login roles not removed: %s", b)
	}
	// Read-only role cannot write.
	b = mustStatus(201, "POST", p+"/cli/login-role", map[string]any{"read_only": true})
	_ = json.Unmarshal(b, &lr)
	conn, err = pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/postgres?sslmode=disable", lr.Role, lr.Password, s.PGPort))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `create table public.ro_nope (id int)`); err == nil {
		t.Fatal("read-only login role could write")
	}
	conn.Close(ctx)
}

func mustJSON2(v any) []byte { b, _ := json.Marshal(v); return b }

// TestIntegrationServe starts the stack and keeps it up so real clients (the
// Supabase CLI, the MCP server) can be pointed at it from a shell:
//
//	SBCTL_API_SERVE_FILE=$PWD/stack.json ... go test ./internal/api -run IntegrationServe
//
// It writes connection details to the file as JSON and runs until the file is deleted.
func TestIntegrationServe(t *testing.T) {
	path := os.Getenv("SBCTL_API_SERVE_FILE")
	if path == "" {
		t.Skip("set SBCTL_API_SERVE_FILE to serve the stack for manual client runs")
	}
	s := startStack(t)
	info, _ := json.MarshalIndent(map[string]any{
		"api_url": s.APIURL, "pat": s.PAT, "jwt": s.JWT, "ref": s.Ref, "dsn": s.DSN, "pg_port": s.PGPort,
	}, "", "  ")
	if err := os.WriteFile(path, info, 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if _, err := os.Stat(path); err != nil {
			return
		}
	}
}

// TestIntegrationStore runs the Store conformance suite against the Postgres store
// and the migrations of internal/registry/migrations/0100_api.sql.
func TestIntegrationStore(t *testing.T) {
	s := startStack(t)
	pg, ok := s.Server.store.(*PGStore)
	if !ok {
		t.Fatalf("expected the Postgres store, got %T", s.Server.store)
	}
	testStore(t, pg, s.Ref)
}
