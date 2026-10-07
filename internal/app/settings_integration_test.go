package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/fleet"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
	"github.com/OWNER/sbctl/internal/units"
)

// freeBase finds a base port in 42000-42899 where base..base+40 and the project's ports
// (base+100..102) are free, for the settings integration test.
func freeBase(t *testing.T) int {
	t.Helper()
	free := func(p int) bool {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			return false
		}
		l.Close()
		return true
	}
	for base := 42000; base < 42890; base += 50 {
		ok := true
		for _, p := range append(seq(base, 41), base+100, base+101, base+102) {
			if !free(p) {
				ok = false
				break
			}
		}
		if ok {
			return base
		}
	}
	t.Fatal("no free port range in 42000-42999")
	return 0
}

func seq(from, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = from + i
	}
	return out
}

// TestSettingsIntegration saves every kind of project setting through the Management API of
// a real daemon and observes the services change behavior: GoTrue accepts a new redirect URL
// and refuses sign-ups, PostgREST serves a newly exposed schema, Storage takes a bigger
// upload, a revoked or disabled API key is refused by the proxy at once, and after a database
// password reset the new password works (directly and through the pooler) and the old one
// does not. It starts the system cluster, one micro project, Supavisor and Storage (no
// Realtime, no pg-meta, no Studio): under 1 GB.
//
// Needs SBCTL_TEST_UNPACKED (unpacked slim-services artifacts) and SBCTL_SETTINGS_E2E=1.
// With SBCTL_SETTINGS_E2E_HOLD=<file> the stack stays up afterwards and the file receives
// the connection details of the stack (api_url, proxy_url, pat, ref, keys, ports) as JSON for
// real clients (the Supabase CLI); delete the file to stop everything.
func TestSettingsIntegration(t *testing.T) {
	root := os.Getenv("SBCTL_TEST_UNPACKED")
	if root == "" || os.Getenv("SBCTL_SETTINGS_E2E") == "" {
		t.Skip("SBCTL_TEST_UNPACKED and SBCTL_SETTINGS_E2E not set")
	}
	arts := dirArts{}
	for svc, glob := range map[string]string{
		config.SvcPostgres: "postgres-17*", config.SvcGoTrue: "auth-*", config.SvcPostgREST: "postgrest-*",
		config.SvcSupavisor: "pooler-*", config.SvcStorage: "storage-*",
	} {
		m, _ := filepath.Glob(filepath.Join(root, glob))
		if len(m) == 0 {
			t.Skipf("no %s artifact under %s", svc, root)
		}
		arts[svc] = m[len(m)-1]
	}
	base := freeBase(t)
	state, err := os.MkdirTemp("/tmp", "sbk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	cfg := config.Default()
	cfg.StateDir = state
	cfg.KeyPath = filepath.Join(state, "master.key")
	cfg.Supervisor = config.SupervisorExec
	cfg.Domain = "sbctl.test"
	cfg.TLS.Mode = "off"
	cfg.BinPath = "/usr/bin/true"
	cfg.Backup.Backend = "file://" + filepath.Join(state, "backups")
	cfg.Ports.SystemPostgres, cfg.Ports.SystemGoTrue, cfg.Ports.ProjectBase = base, base+1, base+97
	cfg.Ports.SupavisorSession, cfg.Ports.SupavisorTransaction = base+8, base+9
	cfg.Ports.Realtime, cfg.Ports.Storage, cfg.Ports.StorageAdmin = base+6, base+10, base+11
	cfg.Ports.PGMeta, cfg.Fleet.SupavisorAPIPort = base+12, base+14
	cfg.Listen = config.Listen{HTTP: fmt.Sprintf("127.0.0.1:%d", base+30), HTTPS: fmt.Sprintf("127.0.0.1:%d", base+31), Admin: fmt.Sprintf("127.0.0.1:%d", base+32)}
	o := Options{Artifacts: arts}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	lo := LifecycleOptions(cfg, o)
	n, err := lifecycle.InitSystem(ctx, cfg, lo, false)
	if err != nil {
		t.Fatal(err)
	}
	deps := fleet.Deps{Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets, Supervisor: n.Supervisor, Artifacts: arts,
		Skip: []string{config.SvcStudio, config.SvcRealtime, config.SvcPGMeta}, ReadyTimeout: 4 * time.Minute, Start: true}
	mgr, err := fleet.NewManager(deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = mgr.Stop(context.Background())
		if err := lifecycle.StopAll(context.Background(), cfg, lo); err != nil {
			t.Errorf("StopAll: %v", err)
		}
	})
	fl, err := fleet.Setup(ctx, deps)
	if err != nil {
		t.Fatal(err)
	}
	var tenants fleet.Fleet // no Realtime in this stack
	for _, tn := range fl {
		if tn.Service() != config.SvcRealtime {
			tenants = append(tenants, tn)
		}
	}
	eng := lifecycle.NewEngine(cfg, n.Registry, n.Secrets, arts, n.Plane, lifecycle.Options{Fleet: tenants, Settings: n.Settings})
	p, err := eng.Create(ctx, lifecycle.CreateRequest{Name: "settings", Class: "micro"})
	if err != nil {
		n.Close()
		t.Fatal(err)
	}
	keys, err := eng.Keys(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	pat := secrets.NewPAT()
	if err := n.Registry.CreateAccessToken(ctx, &registry.AccessToken{UserID: "11111111-2222-4333-8444-555555555555", Name: "e2e", Hash: secrets.HashToken(pat), Prefix: pat[:8]}); err != nil {
		t.Fatal(err)
	}
	n.Close()

	sctx, stop := context.WithCancel(ctx)
	served := make(chan struct{})
	go func() { _ = Serve(sctx, cfg, o); close(served) }()
	t.Cleanup(func() { stop(); <-served })

	host := cfg.ProjectHost(p.Ref)
	hc := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(target, hostHdr, method, path string, body []byte, hdr ...string) (int, http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, target+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if hostHdr != "" {
			req.Host = hostHdr
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, b
	}
	admin := "http://" + cfg.Listen.Admin
	proxyURL := "http://" + cfg.Listen.HTTP
	api := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		code, _, out := do(admin, "", method, path, b, "Authorization", "Bearer "+pat, "Content-Type", "application/json")
		return code, out
	}
	mustAPI := func(method, path string, body any) []byte {
		t.Helper()
		code, out := api(method, path, body)
		if code >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, code, out)
		}
		return out
	}
	project := func(method, path string, body []byte, hdr ...string) (int, http.Header, []byte) {
		t.Helper()
		return do(proxyURL, host, method, path, body, hdr...)
	}
	waitFor := func(what string, f func() (bool, string)) {
		t.Helper()
		var last string
		for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			var ok bool
			if ok, last = f(); ok {
				return
			}
		}
		t.Fatalf("%s: %s", what, last)
	}
	waitFor("project API after boot", func() (bool, string) {
		req, _ := http.NewRequest("GET", proxyURL+"/auth/v1/settings", nil)
		req.Host = host
		req.Header.Set("apikey", keys.PublishableKey)
		resp, err := hc.Do(req)
		if err != nil {
			return false, err.Error() // the daemon is still binding its listeners
		}
		resp.Body.Close()
		return resp.StatusCode == 200, fmt.Sprint(resp.StatusCode)
	})
	cfgPath := "/v1/projects/" + p.Ref
	// SQL straight to the project's cluster as supabase_admin (the API's database/query would
	// need pg-meta, which this stack does not run).
	sql := func(q string) []map[string]any {
		t.Helper()
		dsn := fmt.Sprintf("postgres://supabase_admin:%s@127.0.0.1:%d/postgres?sslmode=disable", url.QueryEscape(keys.AdminPassword), cfg.PortsFor(p.Ref, p.Seq).Postgres)
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(ctx)
		rows, err := c.Query(ctx, q, pgx.QueryExecModeSimpleProtocol)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowToMap)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return out
	}

	t.Run("auth redirect allow list", func(t *testing.T) {
		mustAPI("PATCH", cfgPath+"/config/auth", map[string]any{
			"site_url": "https://site.example.com", "external_github_enabled": true,
			"external_github_client_id": "client-id", "external_github_secret": "client-secret",
		})
		// GoTrue sends an invalid token's holder to the redirect URL when the allow list accepts
		// it and to the site URL when it does not.
		referrer := func(redirectTo string) string {
			t.Helper()
			code, h, b := project("GET", "/auth/v1/verify?type=magiclink&token=nope&redirect_to="+url.QueryEscape(redirectTo), nil)
			if code != 303 && code != 302 {
				t.Fatalf("verify: %d %s", code, b)
			}
			loc, err := url.Parse(h.Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			loc.Fragment, loc.RawQuery = "", ""
			return loc.String()
		}
		// The provider settings reached GoTrue: it sends the browser to GitHub with the client id
		// and the project's callback.
		code, h, b := project("GET", "/auth/v1/authorize?provider=github", nil)
		loc, _ := url.Parse(h.Get("Location"))
		if code != 302 || loc.Host != "github.com" || loc.Query().Get("client_id") != "client-id" || loc.Query().Get("redirect_uri") != "http://"+host+"/auth/v1/callback" {
			t.Fatalf("authorize: %d %s %s", code, h.Get("Location"), b)
		}
		if got := referrer("https://new.example.com/cb"); got != "https://site.example.com" && got != "https://site.example.com/" {
			t.Fatalf("a redirect URL that is not allowed must fall back to the site URL, got %q", got)
		}
		mustAPI("PATCH", cfgPath+"/config/auth", map[string]any{"uri_allow_list": "https://new.example.com/**"})
		if got := referrer("https://new.example.com/cb"); got != "https://new.example.com/cb" {
			t.Fatalf("the new redirect URL is not accepted: %q", got)
		}
		var got map[string]any
		_ = json.Unmarshal(mustAPI("GET", cfgPath+"/config/auth", nil), &got)
		if got["uri_allow_list"] != "https://new.example.com/**" || got["external_github_secret"] == "client-secret" || got["external_github_secret"] == nil {
			t.Fatalf("GET after save: %v %v", got["uri_allow_list"], got["external_github_secret"])
		}
	})

	t.Run("sign-up toggle", func(t *testing.T) {
		signup := func(email string) (int, string) {
			body, _ := json.Marshal(map[string]string{"email": email, "password": "correct-horse-battery"})
			code, _, b := project("POST", "/auth/v1/signup", body, "apikey", keys.PublishableKey, "Content-Type", "application/json")
			return code, string(b)
		}
		if code, b := signup("a@example.com"); code != 200 {
			t.Fatalf("signup with sign-ups on: %d %s", code, b)
		}
		mustAPI("PATCH", cfgPath+"/config/auth", map[string]any{"disable_signup": true})
		if code, b := signup("b@example.com"); code == 200 || !strings.Contains(strings.ToLower(b), "signup") {
			t.Fatalf("a disabled sign-up was accepted: %d %s", code, b)
		}
		mustAPI("PATCH", cfgPath+"/config/auth", map[string]any{"disable_signup": false})
		if code, b := signup("c@example.com"); code != 200 {
			t.Fatalf("signup after re-enabling: %d %s", code, b)
		}
	})

	t.Run("email template", func(t *testing.T) {
		mustAPI("PATCH", cfgPath+"/config/auth", map[string]any{"mailer_templates_recovery_content": "<h2>Reset for {{ .Email }}</h2>"})
		// The daemon serves the template to the loopback, and GoTrue's unit points at it.
		code, _, b := do(admin, "", "GET", "/internal/templates/"+p.Ref+"/recovery", nil)
		if code != 200 || !strings.Contains(string(b), "Reset for") {
			t.Fatalf("template endpoint: %d %s", code, b)
		}
		unitEnv := authUnitEnvironment(t, cfg, p.Ref)
		want := `GOTRUE_MAILER_TEMPLATES_RECOVERY="http://` + cfg.Listen.Admin + "/internal/templates/" + p.Ref + "/recovery?v="
		if !strings.Contains(unitEnv, want) {
			t.Fatalf("GoTrue's environment does not point at the template (want %s...)", want)
		}
	})

	t.Run("smtp and mail templates reach GoTrue", func(t *testing.T) {
		// A minimal SMTP server that keeps the messages it is given.
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+40))
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		mails := make(chan string, 4)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					rd := bufio.NewReader(c)
					fmt.Fprint(c, "220 fake ESMTP\r\n")
					var data strings.Builder
					inData := false
					for {
						line, err := rd.ReadString('\n')
						if err != nil {
							return
						}
						switch {
						case inData && line == ".\r\n":
							inData = false
							mails <- data.String()
							fmt.Fprint(c, "250 queued\r\n")
						case inData:
							data.WriteString(line)
						case strings.HasPrefix(strings.ToUpper(line), "EHLO"), strings.HasPrefix(strings.ToUpper(line), "HELO"):
							fmt.Fprint(c, "250 fake\r\n")
						case strings.HasPrefix(strings.ToUpper(line), "DATA"):
							inData = true
							fmt.Fprint(c, "354 go on\r\n")
						case strings.HasPrefix(strings.ToUpper(line), "QUIT"):
							fmt.Fprint(c, "221 bye\r\n")
							return
						default:
							fmt.Fprint(c, "250 ok\r\n")
						}
					}
				}()
			}
		}()
		mustAPI("PATCH", cfgPath+"/config/auth", map[string]any{
			"smtp_host": "127.0.0.1", "smtp_port": fmt.Sprint(base + 40), "smtp_admin_email": "no-reply@example.com", "smtp_sender_name": "Acme",
			"mailer_subjects_recovery": "Acme password reset",
		})
		body, _ := json.Marshal(map[string]string{"email": "a@example.com"})
		waitFor("recovery mail sent", func() (bool, string) {
			code, _, b := project("POST", "/auth/v1/recover", body, "apikey", keys.PublishableKey, "Content-Type", "application/json")
			return code == 200, fmt.Sprint(code, string(b))
		})
		select {
		case m := <-mails:
			flat := strings.NewReplacer("=\r\n", "", "=3D", "=").Replace(m)
			if !strings.Contains(flat, "Reset for a@example.com") || !strings.Contains(flat, "Subject: Acme password reset") || !strings.Contains(flat, "Acme") {
				t.Fatalf("the mail does not carry the saved template and subject:\n%.600s", m)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("no mail reached the SMTP server")
		}
		// With SMTP on, confirmation is on (autoconfirm off) until the setting says otherwise.
		var got map[string]any
		_ = json.Unmarshal(mustAPI("GET", cfgPath+"/config/auth", nil), &got)
		if got["mailer_autoconfirm"] != false || got["smtp_host"] != "127.0.0.1" {
			t.Fatalf("auth config: autoconfirm=%v smtp_host=%v", got["mailer_autoconfirm"], got["smtp_host"])
		}
	})

	t.Run("postgrest exposed schema", func(t *testing.T) {
		sql(`create schema if not exists api_extra;
			create table if not exists api_extra.items (id int primary key);
			insert into api_extra.items values (1) on conflict do nothing;
			grant usage on schema api_extra to anon;
			grant select on api_extra.items to anon;`)
		get := func() int {
			code, _, _ := project("GET", "/rest/v1/items", nil, "apikey", keys.PublishableKey, "Accept-Profile", "api_extra")
			return code
		}
		if code := get(); code == 200 {
			t.Fatalf("a schema that is not exposed answered %d", code)
		}
		var got map[string]any
		_ = json.Unmarshal(mustAPI("PATCH", cfgPath+"/postgrest", map[string]any{"db_schema": "public, graphql_public, api_extra", "max_rows": 7}), &got)
		if got["db_schema"] != "public,graphql_public,api_extra" {
			t.Fatalf("patch answer: %v", got)
		}
		waitFor("api_extra served", func() (bool, string) {
			code := get()
			return code == 200, fmt.Sprint(code)
		})
		_ = json.Unmarshal(mustAPI("GET", cfgPath+"/postgrest", nil), &got)
		if got["max_rows"] != float64(7) {
			t.Fatalf("GET: %v", got)
		}
	})

	t.Run("postgrest refuses an empty schema list", func(t *testing.T) {
		code, out := api("PATCH", cfgPath+"/postgrest", map[string]any{"db_schema": ""})
		if code != 400 || !strings.Contains(string(out), "at least one schema") {
			t.Fatalf("empty db_schema: %d %s", code, out)
		}
		if c, _, _ := project("GET", "/rest/v1/items", nil, "apikey", keys.PublishableKey, "Accept-Profile", "api_extra"); c != 200 {
			t.Fatalf("the exposed schema stopped being served after a rejected save: %d", c)
		}
	})

	t.Run("storage upload limit", func(t *testing.T) {
		svc := "Bearer " + keys.SecretKey
		hdr := []string{"apikey", keys.SecretKey, "Authorization", svc, "Content-Type", "application/json"}
		if code, _, b := project("POST", "/storage/v1/bucket", []byte(`{"id":"files","name":"files","public":false}`), hdr...); code != 200 {
			t.Fatalf("create bucket: %d %s", code, b)
		}
		upload := func(name string, size int) int {
			body := make([]byte, size)
			_, _ = rand.Read(body)
			code, _, b := project("POST", "/storage/v1/object/files/"+name, body, "apikey", keys.SecretKey, "Authorization", svc, "Content-Type", "application/octet-stream")
			t.Logf("upload %s (%d bytes): %d %.100s", name, size, code, b)
			return code
		}
		mustAPI("PATCH", cfgPath+"/config/storage", map[string]any{"fileSizeLimit": 1 << 20})
		waitFor("limit enforced", func() (bool, string) {
			code := upload("big1.bin", 2<<20)
			return code == 413 || code == 400, fmt.Sprint(code)
		})
		mustAPI("PATCH", cfgPath+"/config/storage", map[string]any{"fileSizeLimit": 10 << 20})
		waitFor("bigger upload accepted", func() (bool, string) {
			code := upload("big2.bin", 2<<20)
			return code == 200, fmt.Sprint(code)
		})
	})

	t.Run("api key revocation", func(t *testing.T) {
		var created map[string]any
		_ = json.Unmarshal(mustAPI("POST", cfgPath+"/api-keys", map[string]any{"type": "secret", "name": "ci"}), &created)
		key, id := created["api_key"].(string), created["id"].(string)
		status := func(k string) int {
			code, _, _ := project("GET", "/rest/v1/", nil, "apikey", k)
			return code
		}
		if code := status(key); code != 200 {
			t.Fatalf("the new secret key: %d", code)
		}
		mustAPI("DELETE", cfgPath+"/api-keys/"+id, nil)
		if code := status(key); code != 401 {
			t.Fatalf("a revoked key was accepted: %d", code) // immediately: no waiting
		}
		if code := status(keys.SecretKey); code != 200 {
			t.Fatalf("the default secret key stopped working: %d", code)
		}
		mustAPI("PUT", cfgPath+"/api-keys/legacy?enabled=false", nil)
		if code := status(keys.ServiceRoleKey); code != 401 {
			t.Fatalf("a legacy key was accepted while disabled: %d", code)
		}
		if code := status(keys.SecretKey); code != 200 {
			t.Fatalf("the secret key stopped working with the legacy keys off: %d", code)
		}
		if code, _, _ := project("GET", "/auth/v1/settings", nil, "apikey", keys.AnonKey); code != 401 {
			t.Fatalf("legacy anon accepted: %d", code)
		}
		mustAPI("PUT", cfgPath+"/api-keys/legacy?enabled=true", nil)
		if code := status(keys.ServiceRoleKey); code != 200 {
			t.Fatalf("legacy keys did not come back: %d", code)
		}
	})

	t.Run("database password", func(t *testing.T) {
		const newPW = "a-brand-new-password-42"
		oldPW := keys.DBPassword
		direct := func(pw string) error {
			dsn := fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable", url.QueryEscape(pw), cfg.PortsFor(p.Ref, p.Seq).Postgres)
			c, err := pgx.Connect(ctx, dsn)
			if err != nil {
				return err
			}
			return c.Close(ctx)
		}
		pooler := func(pw string) error {
			dsn := fmt.Sprintf("postgres://postgres.%s:%s@127.0.0.1:%d/postgres?sslmode=disable&default_query_exec_mode=simple_protocol", p.Ref, url.QueryEscape(pw), cfg.Ports.SupavisorSession)
			c, err := pgx.Connect(ctx, dsn)
			if err != nil {
				return err
			}
			return c.Close(ctx)
		}
		if err := pooler(oldPW); err != nil { // fills the pooler's credential cache with the old verifier
			t.Fatalf("pooler login before: %v", err)
		}
		mustAPI("PATCH", cfgPath+"/database/password", map[string]any{"password": newPW})
		if err := direct(newPW); err != nil {
			t.Fatalf("new password, direct: %v", err)
		}
		if err := direct(oldPW); err == nil {
			t.Fatal("old password still works, direct")
		}
		if err := pooler(newPW); err != nil {
			t.Fatalf("new password, pooler: %v", err)
		}
		if err := pooler(oldPW); err == nil {
			t.Fatal("old password still works through the pooler")
		}
		// Everything else keeps working: the API connects with the new password, the services with theirs.
		sql("select 1")
		if code, _, _ := project("GET", "/rest/v1/", nil, "apikey", keys.SecretKey); code != 200 {
			t.Fatalf("PostgREST after the password change: %d", code)
		}
		if code, _, _ := project("GET", "/auth/v1/settings", nil, "apikey", keys.PublishableKey); code != 200 {
			t.Fatalf("GoTrue after the password change: %d", code)
		}
	})

	t.Run("postgres settings", func(t *testing.T) {
		var got map[string]any
		_ = json.Unmarshal(mustAPI("PUT", cfgPath+"/config/database/postgres", map[string]any{"statement_timeout": "45s", "work_mem": "8MB", "log_connections": true}), &got)
		if got["statement_timeout"] != "45s" {
			t.Fatalf("PUT: %v", got)
		}
		rows := sql("select current_setting('statement_timeout') as t, current_setting('work_mem') as w")
		if len(rows) != 1 || rows[0]["t"] != "45s" || rows[0]["w"] != "8MB" {
			t.Fatalf("the cluster did not take the settings: %v", rows)
		}
		if code, out := api("PUT", cfgPath+"/config/database/postgres", map[string]any{"max_connections": 5}); code != 400 {
			t.Fatalf("unsafe value: %d %s", code, out)
		}
		// Resetting a setting returns the cluster to its default.
		mustAPI("PUT", cfgPath+"/config/database/postgres", map[string]any{"statement_timeout": nil})
		rows = sql("select current_setting('statement_timeout') as t")
		if len(rows) != 1 || rows[0]["t"] == "45s" {
			t.Fatalf("reset did not reach the cluster: %v", rows)
		}
	})

	t.Run("postgres setting that needs a restart", func(t *testing.T) {
		// Without restart_database the value is saved and applies at the next restart.
		mustAPI("PUT", cfgPath+"/config/database/postgres", map[string]any{"statement_timeout": "45s", "max_connections": 40})
		if rows := sql("show max_connections"); rows[0]["max_connections"] != "30" {
			t.Fatalf("max_connections changed without a restart: %v", rows)
		}
		var got map[string]any
		_ = json.Unmarshal(mustAPI("GET", cfgPath+"/config/database/postgres", nil), &got)
		if got["max_connections"] != float64(40) {
			t.Fatalf("GET must return what was saved: %v", got["max_connections"])
		}
		// With it the whole project restarts (the API units are bound to the cluster) on the saved settings.
		mustAPI("PUT", cfgPath+"/config/database/postgres", map[string]any{"max_connections": 40, "restart_database": true})
		waitFor("max_connections applied by the restart", func() (bool, string) {
			c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://supabase_admin:%s@127.0.0.1:%d/postgres?sslmode=disable", url.QueryEscape(keys.AdminPassword), cfg.PortsFor(p.Ref, p.Seq).Postgres))
			if err != nil {
				return false, err.Error()
			}
			defer c.Close(ctx)
			var v string
			if err := c.QueryRow(ctx, "show max_connections").Scan(&v); err != nil {
				return false, err.Error()
			}
			return v == "40", v
		})
		if rows := sql("show statement_timeout"); rows[0]["statement_timeout"] != "45s" {
			t.Fatalf("statement_timeout lost by the restart: %v", rows)
		}
		waitFor("PostgREST and GoTrue back after the restart", func() (bool, string) {
			a, _, _ := project("GET", "/rest/v1/", nil, "apikey", keys.SecretKey)
			b, _, _ := project("GET", "/auth/v1/settings", nil, "apikey", keys.PublishableKey)
			return a == 200 && b == 200, fmt.Sprint(a, b)
		})
		// Back to the class's value: the command-line setting goes away and the next restart restores 30.
		mustAPI("PUT", cfgPath+"/config/database/postgres", map[string]any{"max_connections": nil, "restart_database": true})
		waitFor("class value back", func() (bool, string) {
			c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://supabase_admin:%s@127.0.0.1:%d/postgres?sslmode=disable", url.QueryEscape(keys.AdminPassword), cfg.PortsFor(p.Ref, p.Seq).Postgres))
			if err != nil {
				return false, err.Error()
			}
			defer c.Close(ctx)
			var v string
			if err := c.QueryRow(ctx, "show max_connections").Scan(&v); err != nil {
				return false, err.Error()
			}
			return v == "30", v
		})
	})

	if hold := os.Getenv("SBCTL_SETTINGS_E2E_HOLD"); hold != "" {
		info, _ := json.MarshalIndent(map[string]any{
			"api_url": admin, "proxy_url": proxyURL, "project_host": host, "pat": pat, "ref": p.Ref,
			"anon_key": keys.AnonKey, "service_key": keys.ServiceRoleKey, "publishable_key": keys.PublishableKey, "secret_key": keys.SecretKey,
			"db_port": cfg.PortsFor(p.Ref, p.Seq).Postgres, "pooler_port": cfg.Ports.SupavisorSession, "db_password": keys.DBPassword,
		}, "", "  ")
		if err := os.WriteFile(hold, info, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("stack held; delete %s to stop it", hold)
		for {
			if _, err := os.Stat(hold); err != nil {
				break
			}
			time.Sleep(time.Second)
		}
	}
}

// authUnitEnvironment returns the environment file the exec backend rendered for a
// project's GoTrue.
func authUnitEnvironment(t *testing.T, cfg *config.Config, ref string) string {
	t.Helper()
	f := units.FilesFor(cfg, units.Spec{Service: config.SvcGoTrue, Ref: ref})
	b, err := os.ReadFile(f.Env)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
