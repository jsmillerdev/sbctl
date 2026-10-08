package api

// Integration tests against a real Postgres and a real supavise-pgmeta, both unpacked
// from the slim-services darwin-arm64 artifacts. They are gated: they start two
// processes and cost a few hundred MB.
//
//	SUPAVISE_API_INTEGRATION=1 \
//	SUPAVISE_PG_BIN=$HOME/.cache/sbctl/unpacked/postgres-17.11.0.004-r1-darwin-arm64/bin \
//	SUPAVISE_PGMETA_BIN=$HOME/.cache/sbctl/unpacked/pgmeta-v0.100.0-r0-darwin-arm64/bin/pgmeta \
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

type stack struct {
	t testing.TB
	// PATs by role ("owner", "admin", "dev", "ro"): personal access tokens of users with
	// that organization-wide role.
	PATs map[string]string
	// JWTs are the dashboard sessions of the same users (the /platform routes take no PAT).
	JWTs    map[string]string
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

// itLogger discards the server's logs unless SUPAVISE_API_IT_LOG is set.
func itLogger() *slog.Logger {
	if os.Getenv("SUPAVISE_API_IT_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// portBase is the first port of the 900 the stack picks from: 32100 unless
// SUPAVISE_API_IT_PORT_BASE says otherwise (a machine that reserves another range).
func portBase() int {
	if v, err := strconv.Atoi(os.Getenv("SUPAVISE_API_IT_PORT_BASE")); err == nil && v > 1024 && v < 64000 {
		return v
	}
	return 32100
}

// freePort returns a free TCP port in portBase()..portBase()+899.
func freePort(t testing.TB) int {
	t.Helper()
	base := portBase()
	for p := base + int(time.Now().UnixNano()%400); p < base+900; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			l.Close()
			return p
		}
	}
	t.Fatal("no free port in the integration range")
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

// startStack starts Postgres (one instance, small), supavise-pgmeta and the API server.
func startStack(t testing.TB) *stack {
	t.Helper()
	if os.Getenv("SUPAVISE_API_INTEGRATION") == "" {
		t.Skip("set SUPAVISE_API_INTEGRATION=1 (see integration_test.go)")
	}
	pgBin, pgmetaBin := os.Getenv("SUPAVISE_PG_BIN"), os.Getenv("SUPAVISE_PGMETA_BIN")
	if pgBin == "" || pgmetaBin == "" {
		t.Skip("set SUPAVISE_PG_BIN and SUPAVISE_PGMETA_BIN to the unpacked artifacts")
	}
	dir, err := os.MkdirTemp("", "supavise-api-it-")
	if err != nil {
		t.Fatal(err)
	}
	pgPort, metaPort, apiPort := freePort(t), 0, 0
	data := filepath.Join(dir, "pgdata")
	run(t, filepath.Join(pgBin, "initdb"), "-D", data, "-U", "postgres", "--auth=trust", "-E", "UTF8", "--locale=C")
	// Password logins of the roles the API creates must really authenticate (SCRAM),
	// while the test's own connections stay trust: prepend rules to pg_hba.conf.
	hba := filepath.Join(data, "pg_hba.conf")
	old, err := os.ReadFile(hba)
	if err != nil {
		t.Fatal(err)
	}
	rules := "host all supavise_read_only 127.0.0.1/32 scram-sha-256\nhost all /^(cli_login_|supavise_cli_ro_).*$ 127.0.0.1/32 scram-sha-256\n"
	if err := os.WriteFile(hba, append([]byte(rules), old...), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if _, err := conn.Exec(ctx, `create database supavise`); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)

	reg, err := registry.Open(ctx, fmt.Sprintf("postgres://postgres@127.0.0.1:%d/supavise?sslmode=disable&pool_max_conns=4", pgPort))
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
	cfg.Domain = "supavise.test"
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
	srv, err := NewServer(Deps{Registry: reg, Secrets: sec, Manager: mgr, Config: cfg, Logger: itLogger()})
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
	// The signed-in user owns the organization, like the claimed first user; one user per
	// other role joins it, each with a PAT created the way the dashboard does.
	st.PATs, st.JWTs = map[string]string{}, map[string]string{}
	mkUser := func(name, id string, role int) string {
		if err := srv.members.EnsureOwner(ctx, members.OrgRef{ID: org.ID, Slug: org.Slug}, id); err != nil {
			t.Fatal(err)
		}
		if role != members.RoleOwner {
			if err := srv.members.SetOrgRole(ctx, nil, members.OrgRef{ID: org.ID, Slug: org.Slug}, id, role); err != nil {
				// A node needs an Owner: the first user is one, so demoting it is refused only for the last.
				t.Fatal(err)
			}
		}
		jwt := f.signJWT(map[string]any{"sub": id, "email": name + "@example.test", "role": "authenticated"})
		req, _ := http.NewRequest("POST", st.APIURL+"/platform/profile/access-tokens", strings.NewReader(`{"name":"it-`+name+`"}`))
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var tok struct{ Token string }
		_ = json.NewDecoder(resp.Body).Decode(&tok)
		resp.Body.Close()
		if tok.Token == "" {
			t.Fatalf("could not create a PAT for %s", name)
		}
		st.PATs[name], st.JWTs[name] = tok.Token, jwt
		return jwt
	}
	st.JWT = mkUser("owner", "11111111-2222-4333-8444-555555555555", members.RoleOwner)
	mkUser("admin", "11111111-2222-4333-8444-555555555502", members.RoleAdministrator)
	mkUser("dev", "11111111-2222-4333-8444-555555555503", members.RoleDeveloper)
	mkUser("ro", "11111111-2222-4333-8444-555555555504", members.RoleReadOnly)
	st.PAT = st.PATs["owner"]
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

	// Read-only cannot be undone by the SQL itself: the role has no write privilege,
	// so `begin read write` and `reset role` get nowhere. Both routes, both paths.
	roBody := func(q string) map[string]any { return map[string]any{"query": q, "read_only": true} }
	for _, route := range []string{"/database/query", "/database/query/read-only"} {
		for _, q := range []string{
			"begin read write; insert into public.widgets values (42, 'escaped'); commit;",
			"set transaction read write; insert into public.widgets values (42, 'escaped')",
			"reset role; set session authorization postgres; insert into public.widgets values (42, 'escaped')",
			"set default_transaction_read_only = off; insert into public.widgets values (42, 'escaped')",
			"begin read write; create table public.escaped (id int); commit;",
			"begin read write; update public.widgets set name = 'x'; commit;",
			"begin read write; delete from public.widgets; commit;",
		} {
			code, b := s.call("POST", p+route, roBody(q), false)
			if code != 400 {
				t.Fatalf("read-only %s escaped with %q: %d %s", route, q, code, b)
			}
		}
		if b := mustStatus(201, "POST", p+route, roBody("select current_user as u, (select count(*) from public.widgets) as n")); !strings.Contains(string(b), `"u":"`+roleReadOnly+`"`) || !strings.Contains(string(b), `"n":3`) {
			t.Fatalf("read-only %s identity or visibility: %s", route, b)
		}
	}
	// Row level security: pg_read_all_data does not bypass it, so the read-only role
	// must be BYPASSRLS or RLS tables read as empty. Same count both ways.
	mustStatus(201, "POST", p+"/database/query", map[string]any{"query": "create table public.notes (id int primary key, body text); alter table public.notes enable row level security; insert into public.notes values (1, 'a'), (2, 'b')"})
	for _, route := range []string{"/database/query", "/database/query/read-only"} {
		if b := mustStatus(201, "POST", p+route, roBody("select count(*) as n from public.notes")); !strings.Contains(string(b), `"n":2`) {
			t.Fatalf("read-only %s sees an RLS table as empty: %s", route, b)
		}
	}
	if b := mustStatus(201, "POST", p+"/database/query", map[string]any{"query": "select count(*) as n from public.notes where id > $1", "parameters": []any{0}, "read_only": true}); !strings.Contains(string(b), `"n":2`) {
		t.Fatalf("parameterized read-only sees an RLS table as empty: %s", b)
	}
	if b := mustStatus(201, "POST", p+"/database/query", map[string]any{"query": "select count(*) as n from public.widgets where id = 42 or name = 'x'"}); !strings.Contains(string(b), `"n":0`) || mustCount(t, s, p, "pg_class where relname = 'escaped'") != 0 {
		t.Fatalf("a read-only query changed data: %s", b)
	}

	// A trailing semicolon or comment does not turn a parameterized SELECT into [].
	for _, q := range []string{
		"select id from public.widgets where id >= $1;",
		"select id from public.widgets where id >= $1 ;  \n",
		"select id from public.widgets where id >= $1; -- trailing comment",
		"select id from public.widgets where id >= $1 /* c */;",
	} {
		b = mustStatus(201, "POST", p+"/database/query", map[string]any{"query": q, "parameters": []any{1}})
		if !strings.Contains(string(b), `"id":1`) || !strings.Contains(string(b), `"id":3`) {
			t.Fatalf("parameterized %q: %s", q, b)
		}
	}
	b = mustStatus(201, "POST", p+"/database/query", map[string]any{"query": "explain select 1 where $1::int = 1", "parameters": []any{1}})
	if !strings.Contains(string(b), "Result") {
		t.Fatalf("parameterized EXPLAIN: %s", b)
	}

	// Migrations: list is empty before the first, then shows what was applied.
	if b := mustStatus(200, "GET", p+"/database/migrations", nil); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("migrations before: %s", b)
	}
	mustStatus(200, "POST", p+"/database/migrations", map[string]any{"query": "create table public.gadgets (id int)", "name": "create_gadgets"})
	var migs []map[string]any
	if err := json.Unmarshal(mustStatus(200, "GET", p+"/database/migrations", nil), &migs); err != nil || len(migs) != 1 || migs[0]["name"] != "create_gadgets" {
		t.Fatalf("migrations after: %v %v", migs, err)
	}
	// Migrations in the same second (agents apply them back to back) get distinct,
	// increasing versions, also when they race.
	mustStatus(200, "POST", p+"/database/migrations", map[string]any{"query": "create table public.gadgets2 (id int)", "name": "second"})
	mustStatus(200, "POST", p+"/database/migrations", map[string]any{"query": "create table public.gadgets3 (id int)", "name": "third"})
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = s.call("POST", p+"/database/migrations", map[string]any{"query": fmt.Sprintf("create table public.race%d (id int)", i), "name": fmt.Sprintf("race%d", i)}, false)
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("concurrent migration %d: %d", i, c)
		}
	}
	migs = nil
	if err := json.Unmarshal(mustStatus(200, "GET", p+"/database/migrations", nil), &migs); err != nil || len(migs) != 9 {
		t.Fatalf("migrations after burst: %d %v", len(migs), err)
	}
	for i := 1; i < len(migs); i++ {
		if migs[i]["version"].(string) <= migs[i-1]["version"].(string) {
			t.Fatalf("versions not increasing: %v", migs)
		}
	}
	if migs[1]["name"] != "second" || migs[2]["name"] != "third" {
		t.Fatalf("order of back-to-back migrations: %v", migs[:3])
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
	b = mustStatus(201, "POST", p+"/database/query", map[string]any{"query": "select count(*) as n from pg_roles where rolname like 'cli\\_login\\_%' or rolname like 'supavise\\_cli\\_ro\\_%'"})
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
	// The CLI steps down to postgres only for cli_login_* users, so the read-only
	// role must not carry that prefix; it connects as itself and reads.
	if strings.HasPrefix(lr.Role, "cli_login_") {
		t.Fatalf("read-only role %q would get SET SESSION ROLE postgres from the CLI", lr.Role)
	}
	var n int
	if err := conn.QueryRow(ctx, `select count(*) from public.widgets`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("read-only login role cannot read: %d %v", n, err)
	}
	for _, q := range []string{`create table public.ro_nope (id int)`, `begin read write; insert into public.widgets values (77, 'x'); commit`, `set role postgres`} {
		if _, err := conn.Exec(ctx, q); err == nil {
			t.Fatalf("read-only login role could run %q", q)
		}
	}
	conn.Close(ctx)
}

// mustCount counts rows of a catalog expression through the API (postgres role).
func mustCount(t *testing.T, s *stack, p, from string) int {
	t.Helper()
	code, b := s.call("POST", p+"/database/query", map[string]any{"query": "select count(*)::int as n from " + from}, false)
	var rows []struct{ N int }
	if code != 201 || json.Unmarshal(b, &rows) != nil || len(rows) != 1 {
		t.Fatalf("count %s: %d %s", from, code, b)
	}
	return rows[0].N
}

func mustJSON2(v any) []byte { b, _ := json.Marshal(v); return b }

// TestIntegrationServe starts the stack and keeps it up so real clients (the
// Supabase CLI, the MCP server) can be pointed at it from a shell:
//
//	SUPAVISE_API_SERVE_FILE=$PWD/stack.json ... go test ./internal/api -run IntegrationServe
//
// It writes connection details to the file as JSON and runs until the file is deleted.
func TestIntegrationServe(t *testing.T) {
	path := os.Getenv("SUPAVISE_API_SERVE_FILE")
	if path == "" {
		t.Skip("set SUPAVISE_API_SERVE_FILE to serve the stack for manual client runs")
	}
	s := startStack(t)
	info, _ := json.MarshalIndent(map[string]any{
		"api_url": s.APIURL, "pat": s.PAT, "pats": s.PATs, "jwt": s.JWT, "ref": s.Ref, "dsn": s.DSN, "pg_port": s.PGPort,
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

// TestIntegrationRoles checks, against a real Postgres and a real pg-meta, that a Read-only
// member cannot change anything however the request is made, and that the other roles can.
func TestIntegrationRoles(t *testing.T) {
	s := startStack(t)
	p := "/v1/projects/" + s.Ref
	callAs := func(role, method, path string, body any) (int, string) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			rd = strings.NewReader(string(mustJSON2(body)))
		}
		req, _ := http.NewRequest(method, s.APIURL+path, rd)
		tok := s.PATs[role]
		if strings.HasPrefix(path, "/platform/") {
			tok = s.JWTs[role]
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, b := callAs("owner", "POST", p+"/database/query", map[string]any{"query": "create table public.roles_t (id int primary key, v text); insert into public.roles_t values (1, 'a')"}); code != 201 {
		t.Fatalf("setup: %d %s", code, b)
	}
	count := func() string {
		code, b := callAs("owner", "POST", p+"/database/query", map[string]any{"query": "select count(*)::int as n from public.roles_t"})
		if code != 201 {
			t.Fatalf("count: %d %s", code, b)
		}
		return b
	}
	writes := []string{
		"insert into public.roles_t values (2, 'b')",
		"update public.roles_t set v = 'x'",
		"delete from public.roles_t",
		"drop table public.roles_t",
		"create table public.evil (id int)",
		"begin read write; delete from public.roles_t; commit",
		"set session characteristics as transaction read write; delete from public.roles_t",
		"reset role; delete from public.roles_t",
		"alter table public.roles_t add column z int",
	}
	for _, q := range writes {
		// The Management API's SQL route (the MCP server's execute_sql) and Studio's pg-meta route.
		if code, b := callAs("ro", "POST", p+"/database/query", map[string]any{"query": q}); code < 400 || code == 401 || code == 403 {
			t.Errorf("read-only database/query did not fail in the database for %q: %d %s", q, code, b)
		}
		if code, b := callAs("ro", "POST", "/platform/pg-meta/"+s.Ref+"/query", map[string]any{"query": q}); code < 400 || code == 401 || code == 403 {
			t.Errorf("read-only pg-meta did not fail in the database for %q: %d %s", q, code, b)
		}
	}
	if got := count(); !strings.Contains(got, `"n":1`) {
		t.Fatalf("a read-only write got through: %s", got)
	}
	// Reads work for everyone.
	for _, role := range []string{"ro", "dev", "admin", "owner"} {
		if code, b := callAs(role, "POST", p+"/database/query", map[string]any{"query": "select v from public.roles_t"}); code != 201 || !strings.Contains(b, `"v":"a"`) {
			t.Errorf("%s read: %d %s", role, code, b)
		}
		if code, b := callAs(role, "POST", "/platform/pg-meta/"+s.Ref+"/query", map[string]any{"query": "select v from public.roles_t"}); code != 200 || !strings.Contains(b, `"v":"a"`) {
			t.Errorf("%s pg-meta read: %d %s", role, code, b)
		}
	}
	// Developers, Administrators and Owners write, through both routes.
	for i, role := range []string{"dev", "admin", "owner"} {
		q := "insert into public.roles_t values (" + itoa(int64(10+i)) + ", 'w')"
		if code, b := callAs(role, "POST", p+"/database/query", map[string]any{"query": q}); code != 201 {
			t.Errorf("%s write: %d %s", role, code, b)
		}
		q = "insert into public.roles_t values (" + itoa(int64(20+i)) + ", 'w')"
		if code, b := callAs(role, "POST", "/platform/pg-meta/"+s.Ref+"/query", map[string]any{"query": q}); code != 200 {
			t.Errorf("%s pg-meta write: %d %s", role, code, b)
		}
	}
	// Migrations: refused for Read-only before anything runs; applied for a Developer.
	mig := map[string]any{"query": "create table public.mig_t (id int)", "name": "roles_migration"}
	if code, _ := callAs("ro", "POST", p+"/database/migrations", mig); code != 403 {
		t.Errorf("read-only migration: %d", code)
	}
	if code, b := callAs("dev", "POST", p+"/database/migrations", mig); code != 200 && code != 201 {
		t.Errorf("developer migration: %d %s", code, b)
	}
	if code, b := callAs("ro", "POST", p+"/database/query", map[string]any{"query": "select to_regclass('public.mig_t') is not null as e"}); code != 201 || !strings.Contains(b, `"e":true`) {
		t.Errorf("the developer's migration is visible: %d %s", code, b)
	}
	// The CLI's login role for a Read-only member cannot write either.
	code, b := callAs("ro", "POST", p+"/cli/login-role", map[string]any{"read_only": false})
	if code != 201 || !strings.Contains(b, "supavise_cli_ro_") {
		t.Fatalf("read-only login role: %d %s", code, b)
	}
	var lr struct {
		Role, Password string
	}
	_ = json.Unmarshal([]byte(b), &lr)
	roConn, err := pgx.Connect(context.Background(), fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/postgres?sslmode=disable", lr.Role, lr.Password, s.PGPort))
	if err != nil {
		t.Fatalf("login with the read-only login role: %v", err)
	}
	defer roConn.Close(context.Background())
	if _, err := roConn.Exec(context.Background(), "delete from public.roles_t"); err == nil {
		t.Error("the read-only login role deleted rows")
	}
	if _, err := roConn.Exec(context.Background(), "select * from public.roles_t"); err != nil {
		t.Errorf("the read-only login role cannot read: %v", err)
	}
	// Settings and keys.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", p + "/secrets", []any{map[string]any{"name": "A", "value": "b"}}},
		{"PATCH", p + "/config/auth", map[string]any{"site_url": "https://x.example.test"}},
		{"POST", p + "/api-keys", map[string]any{"type": "publishable", "name": "k"}},
		{"POST", p + "/pause", nil},
		{"DELETE", p, nil},
	} {
		if code, b := callAs("ro", c.method, c.path, c.body); code != 403 {
			t.Errorf("read-only %s %s: %d %s", c.method, c.path, code, b)
		}
		if code, b := callAs("dev", c.method, c.path, c.body); code != 403 {
			t.Errorf("developer %s %s: %d %s", c.method, c.path, code, b)
		}
	}
}
