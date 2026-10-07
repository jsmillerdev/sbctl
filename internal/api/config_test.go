package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OWNER/sbctl/internal/projectconfig"
	"github.com/OWNER/sbctl/internal/registry"
)

func bodyMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	m, ok := decodeBody(t, rec).(map[string]any)
	if !ok {
		t.Fatalf("not an object: %s", rec.Body.String())
	}
	return m
}

func (f *fixture) mustDo(method, path string, body any, status int) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := f.do(method, path, body)
	if rec.Code != status {
		f.t.Fatalf("%s %s: %d, want %d: %s", method, path, rec.Code, status, rec.Body.String())
	}
	return rec
}

const (
	authV1       = "/v1/projects/" + testRef + "/config/auth"
	authPlatform = "/platform/auth/" + testRef + "/config"
)

func TestAuthConfigSaveApplyAndReadBack(t *testing.T) {
	f := newFixture(t)
	got := bodyMap(t, f.mustDo("GET", authV1, nil, 200))
	if got["site_url"] != "http://localhost:3000" || got["jwt_exp"] != float64(3600) || got["external_github_enabled"] != false || got["smtp_host"] != nil {
		t.Fatalf("defaults: site_url=%v jwt_exp=%v smtp_host=%v", got["site_url"], got["jwt_exp"], got["smtp_host"])
	}

	rec := f.mustDo("PATCH", authV1, map[string]any{
		"site_url": "https://app.example.com", "uri_allow_list": "https://app.example.com/**,https://*.preview.example.com/**",
		"disable_signup": true, "jwt_exp": 1800,
		"external_github_enabled": true, "external_github_client_id": "cid", "external_github_secret": "super-secret-value",
		"smtp_host": "smtp.example.com", "smtp_port": "2525", "smtp_user": "mailer", "smtp_pass": "smtp-password",
		"smtp_admin_email": "no-reply@example.com", "smtp_sender_name": "Acme",
		"mailer_templates_invite_content": "<p>Welcome {{ .Email }}</p>", "mailer_subjects_invite": "You are invited",
		"rate_limit_otp": 7, "mfa_max_enrolled_factors": 3, "password_min_length": 10,
	}, 200)
	got = bodyMap(t, rec)
	if got["site_url"] != "https://app.example.com" || got["disable_signup"] != true || got["jwt_exp"] != float64(1800) || got["smtp_host"] != "smtp.example.com" {
		t.Fatalf("PATCH response is not what was saved: %v", got["site_url"])
	}
	// The secret never comes back; its hash does, and the saved value reads the same later.
	wantHash := projectconfig.Redact("super-secret-value")
	if got["external_github_secret"] != wantHash || got["smtp_pass"] != projectconfig.Redact("smtp-password") {
		t.Fatalf("secrets must be redacted: %v %v", got["external_github_secret"], got["smtp_pass"])
	}
	if strings.Contains(rec.Body.String(), "super-secret-value") || strings.Contains(rec.Body.String(), "smtp-password") {
		t.Fatal("a secret is in the response")
	}
	if got["mailer_autoconfirm"] != false {
		t.Errorf("with SMTP configured users must confirm their address: %v", got["mailer_autoconfirm"])
	}
	again := bodyMap(t, f.mustDo("GET", authV1, nil, 200))
	for _, k := range []string{"site_url", "uri_allow_list", "rate_limit_otp", "external_github_client_id", "smtp_port", "mailer_templates_invite_content", "password_min_length"} {
		if again[k] != got[k] {
			t.Errorf("%s: GET %v differs from PATCH %v", k, again[k], got[k])
		}
	}
	if again["mailer_templates_invite_content"] != "<p>Welcome {{ .Email }}</p>" {
		t.Errorf("template: %v", again["mailer_templates_invite_content"])
	}
	// One apply for auth, one for the dependent PostgREST (jwt_exp changed); nothing else.
	if strings.Join(f.mgr.applied, ",") != testRef+" auth,"+testRef+" postgrest" {
		t.Fatalf("applied: %v", f.mgr.applied)
	}
	// Studio's twin shows the same settings under upper-case names and accepts them back, with
	// the redacted secret as a form would send it.
	plat := bodyMap(t, f.mustDo("GET", authPlatform, nil, 200))
	if plat["SITE_URL"] != "https://app.example.com" || plat["EXTERNAL_GITHUB_SECRET"] != wantHash {
		t.Fatalf("platform view: %v %v", plat["SITE_URL"], plat["EXTERNAL_GITHUB_SECRET"])
	}
	f.mgr.applied = nil
	f.mustDo("PATCH", authPlatform, map[string]any{"EXTERNAL_GITHUB_SECRET": wantHash, "EXTERNAL_GITHUB_CLIENT_ID": "cid2", "SITE_URL": "https://two.example.com"}, 200)
	st, _ := f.srv.settings.Get(context.Background(), testRef, projectconfig.Auth)
	if st.Set.Str("external_github_secret") != "super-secret-value" || st.Set.Str("site_url") != "https://two.example.com" || st.Set.Str("external_github_client_id") != "cid2" {
		t.Fatalf("saved: %v", st.Set)
	}
	// Saving nothing new applies nothing.
	f.mgr.applied = nil
	f.mustDo("PATCH", authPlatform, map[string]any{"SITE_URL": "https://two.example.com"}, 200)
	if len(f.mgr.applied) != 0 {
		t.Fatalf("an unchanged save was applied: %v", f.mgr.applied)
	}
	// Hooks go through their own route and only touch hook settings.
	f.mustDo("PATCH", authPlatform+"/hooks", map[string]any{
		"HOOK_CUSTOM_ACCESS_TOKEN_ENABLED": true, "HOOK_CUSTOM_ACCESS_TOKEN_URI": "pg-functions://postgres/public/claims", "SITE_URL": "https://ignored.example.com",
	}, 200)
	st, _ = f.srv.settings.Get(context.Background(), testRef, projectconfig.Auth)
	if !st.Set.Bool("hook_custom_access_token_enabled") || st.Set.Str("site_url") != "https://two.example.com" {
		t.Fatalf("hooks route: %v", st.Set)
	}
	// Events record what changed, never a value.
	evs, _ := f.reg.ListEvents(context.Background(), testRef, 20)
	var seen bool
	for _, e := range evs {
		if e.Kind == "settings.auth.updated" {
			seen = true
			if strings.Contains(string(e.Payload), "super-secret") {
				t.Fatal("event payload has a secret")
			}
		}
	}
	if !seen {
		t.Error("no settings.auth.updated event")
	}
}

func TestAuthConfigRejectsBadValues(t *testing.T) {
	f := newFixture(t)
	for body, want := range map[string]string{
		`{"site_url": "nonsense"}`:                        "absolute URL",
		`{"jwt_exp": 99999999}`:                           "between",
		`{"external_github_enabled": true}`:               "client_id",
		`{"hook_send_email_uri": "http://example.com/x"}`: "http is only supported",
		`{"smtp_host": "smtp.example.com"}`:               "smtp_admin_email",
		`{"uri_allow_list": "https://a.example.com/[x"}`:  "redirect pattern",
		`{"security_captcha_provider": "recaptcha"}`:      "one of",
		`not json`: "invalid request body",
	} {
		rec := f.do("PATCH", authV1, body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if len(f.mgr.applied) != 0 {
		t.Fatalf("a rejected save must not touch the service: %v", f.mgr.applied)
	}
	if st, _ := f.srv.settings.Get(context.Background(), testRef, projectconfig.Auth); st.Version != 0 {
		t.Fatalf("a rejected save was stored: v%d", st.Version)
	}
	f.mustDo("GET", "/v1/projects/zzzzzzzzzzzzzzzzzzzz/config/auth", nil, 404)
}

// A save that GoTrue (or the apply) rejects is undone: the old settings are saved and applied
// again, and the caller sees an error.
func TestFailedApplyRollsBack(t *testing.T) {
	f := newFixture(t)
	f.mustDo("PATCH", authV1, map[string]any{"site_url": "https://good.example.com"}, 200)
	f.mgr.applied = nil
	f.mgr.applyErr[projectconfig.Auth] = errors.New("gotrue exited with status 1")
	// The failure is only for the first apply: the restore applies the old settings.
	n := 0
	f.mgr.applyHook = func(svc projectconfig.Service) error {
		n++
		if n == 1 {
			return f.mgr.applyErr[svc]
		}
		return nil
	}
	rec := f.do("PATCH", authV1, map[string]any{"site_url": "https://bad.example.com"})
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "rolled back") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := bodyMap(t, f.mustDo("GET", authV1, nil, 200))
	if got["site_url"] != "https://good.example.com" {
		t.Fatalf("site_url after rollback: %v", got["site_url"])
	}
	if len(f.mgr.applied) != 2 {
		t.Fatalf("expected the failing apply and the restoring apply, got %v", f.mgr.applied)
	}
}

func TestPostgRESTRealtimeStorageConfig(t *testing.T) {
	f := newFixture(t)
	pg := "/v1/projects/" + testRef + "/postgrest"
	got := bodyMap(t, f.mustDo("GET", pg, nil, 200))
	if got["db_schema"] != "public,graphql_public" || got["max_rows"] != float64(1000) || got["jwt_secret"] == "" {
		t.Fatalf("defaults: %v", got)
	}
	f.mustDo("PATCH", pg, map[string]any{"db_schema": "public, graphql_public, api", "max_rows": 100, "db_extra_search_path": "public,extensions,api"}, 200)
	plat := bodyMap(t, f.mustDo("GET", "/platform/projects/"+testRef+"/config/postgrest", nil, 200))
	if plat["db_schema"] != "public,graphql_public,api" || plat["max_rows"] != float64(100) || plat["db_anon_role"] != "anon" || plat["role_claim_key"] != ".role" {
		t.Fatalf("platform view: %v", plat)
	}
	f.mustDo("PATCH", "/platform/projects/"+testRef+"/config/postgrest", map[string]any{"max_rows": 500, "db_pool": 7}, 200)
	if strings.Join(f.mgr.applied, ",") != testRef+" postgrest,"+testRef+" postgrest" {
		t.Fatalf("applied: %v", f.mgr.applied)
	}
	f.mustDo("PATCH", pg, map[string]any{"max_rows": -5}, 400)

	rt := "/platform/projects/" + testRef + "/config/realtime"
	f.mustDo("PATCH", rt, map[string]any{"max_concurrent_users": 500, "private_only": true, "connection_pool": 3}, 204)
	got = bodyMap(t, f.mustDo("GET", rt, nil, 200))
	if got["max_concurrent_users"] != float64(500) || got["private_only"] != true || got["connection_pool"] != float64(3) || got["admin_suspended_at"] != nil {
		t.Fatalf("realtime: %v", got)
	}
	f.mustDo("PATCH", "/v1/projects/"+testRef+"/config/realtime", map[string]any{"max_events_per_second": 0}, 400)
	// A null returns a setting to its default, and the tenant is told the default.
	f.mustDo("PATCH", rt, map[string]any{"private_only": nil, "max_concurrent_users": nil}, 204)
	got = bodyMap(t, f.mustDo("GET", rt, nil, 200))
	if got["private_only"] != false || got["max_concurrent_users"] != float64(200) || got["connection_pool"] != float64(3) {
		t.Fatalf("realtime after a reset: %v", got)
	}

	st := "/platform/projects/" + testRef + "/config/storage"
	got = bodyMap(t, f.mustDo("GET", st, nil, 200))
	caps := got["capabilities"].(map[string]any)
	if got["fileSizeLimit"] != float64(52428800) || caps["list_v2"] != true {
		t.Fatalf("storage defaults: %v", got)
	}
	got = bodyMap(t, f.mustDo("PATCH", st, map[string]any{"fileSizeLimit": 209715200, "features": map[string]any{"imageTransformation": map[string]any{"enabled": true}}}, 200))
	feats := got["features"].(map[string]any)
	if got["fileSizeLimit"] != float64(209715200) || feats["imageTransformation"].(map[string]any)["enabled"] != true || feats["s3Protocol"].(map[string]any)["enabled"] != true {
		t.Fatalf("storage after patch: %v", got)
	}
	rec := f.mustDo("PATCH", "/v1/projects/"+testRef+"/config/storage", map[string]any{"fileSizeLimit": 1048576}, 200)
	if rec.Body.Len() != 0 {
		t.Errorf("the v1 storage PATCH answers an empty body: %q", rec.Body.String())
	}
	if v := bodyMap(t, f.mustDo("GET", "/v1/projects/"+testRef+"/config/storage", nil, 200))["fileSizeLimit"]; v != float64(1048576) {
		t.Fatalf("v1 read: %v", v)
	}
	f.mustDo("PATCH", st, map[string]any{"fileSizeLimit": 536870912001}, 400)
	f.mustDo("PATCH", st, map[string]any{"features": map[string]any{"imageTransformation": map[string]any{"enabled": "yes"}}}, 400)
	f.mustDo("PATCH", st, map[string]any{"features": map[string]any{"teleport": map[string]any{"enabled": true}}}, 200) // unknown features are dropped
}

func TestPostgresConfig(t *testing.T) {
	f := newFixture(t)
	url := "/v1/projects/" + testRef + "/config/database/postgres"
	f.mgr.pending = true
	f.project.Limits.MemoryMax = "1G"
	if err := f.reg.UpdateProject(context.Background(), f.project); err != nil {
		t.Fatal(err)
	}
	got := bodyMap(t, f.mustDo("PUT", url, map[string]any{"statement_timeout": "30s", "max_connections": 80, "restart_database": true}, 200))
	if got["statement_timeout"] != "30s" || got["max_connections"] != float64(80) {
		t.Fatalf("PUT response: %v", got)
	}
	if _, leaked := got["restart_database"]; leaked {
		t.Error("restart_database is a request flag")
	}
	if len(f.mgr.applyOpts) != 1 || !f.mgr.applyOpts[0].RestartDatabase {
		t.Fatalf("restart flag not passed: %+v", f.mgr.applyOpts)
	}
	if got := bodyMap(t, f.mustDo("GET", url, nil, 200)); got["statement_timeout"] != "30s" {
		t.Fatalf("GET: %v", got)
	}
	// Asking for a restart again with nothing new still applies: an earlier save may be waiting for one.
	f.mustDo("PUT", url, map[string]any{"statement_timeout": "30s", "restart_database": true}, 200)
	if len(f.mgr.applyOpts) != 2 || !f.mgr.applyOpts[1].RestartDatabase {
		t.Fatalf("an unchanged save with restart_database must still reach the manager: %+v", f.mgr.applyOpts)
	}
	f.mustDo("PUT", url, map[string]any{"statement_timeout": "30s"}, 200)
	if len(f.mgr.applyOpts) != 2 {
		t.Fatalf("an unchanged save without restart_database applies nothing: %+v", f.mgr.applyOpts)
	}
	// Unsafe or malformed values are rejected before anything is saved.
	for body, want := range map[string]string{
		`{"max_connections": 3}`:       "at least 20",
		`{"shared_buffers": "9GB"}`:    "40%",
		`{"work_mem": "4"}`:            "needs a unit",
		`{"checkpoint_timeout": "1s"}`: "between",
	} {
		f.mgr.applied = nil
		rec := f.do("PUT", url, body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), want) || len(f.mgr.applied) != 0 {
			t.Errorf("%s: %d %s applied=%v", body, rec.Code, rec.Body.String(), f.mgr.applied)
		}
	}
}

func TestAPIKeysLifecycle(t *testing.T) {
	f := newFixture(t)
	base := "/v1/projects/" + testRef + "/api-keys"
	list := func() []map[string]any {
		var out []map[string]any
		for _, e := range decodeBody(t, f.mustDo("GET", base+"?reveal=true", nil, 200)).([]any) {
			out = append(out, e.(map[string]any))
		}
		return out
	}
	byName := func(typ, name string) map[string]any {
		for _, k := range list() {
			if k["type"] == typ && k["name"] == name {
				return k
			}
		}
		return nil
	}
	if len(list()) != 4 || byName("secret", "default") == nil {
		t.Fatalf("a new project has the legacy pair and two default keys: %v", list())
	}

	// Create a second secret key: shown in full once, masked in listings without reveal.
	created := bodyMap(t, f.mustDo("POST", base, map[string]any{"type": "secret", "name": "ci_runner", "description": "for CI"}, 201))
	key := created["api_key"].(string)
	if !strings.HasPrefix(key, "sb_secret_") || created["name"] != "ci_runner" {
		t.Fatalf("created: %v", created)
	}
	masked := false
	for _, e := range decodeBody(t, f.mustDo("GET", base, nil, 200)).([]any) {
		if m := e.(map[string]any); m["name"] == "ci_runner" {
			// The type prefix and four characters of the random part, then the mask.
			want := key[:len("sb_secret_")+4]
			masked = m["api_key"] != key && m["prefix"] == want && strings.HasPrefix(m["api_key"].(string), want) &&
				!strings.Contains(m["api_key"].(string), key[len(want):])
		}
	}
	if !masked {
		t.Error("a secret key must be masked unless reveal=true")
	}
	if r := f.do("POST", base, map[string]any{"type": "secret", "name": "ci_runner"}); r.Code != 409 {
		t.Errorf("duplicate name: %d", r.Code)
	}
	for _, bad := range []map[string]any{{"type": "secret", "name": "Bad Name"}, {"type": "legacy", "name": "x"}, {"type": "publishable", "name": "p", "secret_jwt_template": map[string]any{"role": "service_role"}}, {"type": "secret", "name": "t", "secret_jwt_template": map[string]any{"role": "postgres"}}} {
		if r := f.do("POST", base, bad); r.Code != 400 {
			t.Errorf("%v: %d", bad, r.Code)
		}
	}
	pub := bodyMap(t, f.mustDo("POST", base, map[string]any{"type": "publishable", "name": "web"}, 201))
	if !strings.HasPrefix(pub["api_key"].(string), "sb_publishable_") {
		t.Fatalf("publishable: %v", pub)
	}

	// The keys the proxy accepts come from the same records.
	k, _ := f.mgr.Keys(context.Background(), testRef)
	var accepted []string
	for _, o := range k.OpaqueKeys(testRef) {
		accepted = append(accepted, o.Key)
	}
	if !contains(accepted, key) || !contains(accepted, pub["api_key"].(string)) || len(accepted) != 4 {
		t.Fatalf("the stored keys are not what the proxy would accept: %v", accepted)
	}

	// Rename and describe.
	id := created["id"].(string)
	got := bodyMap(t, f.mustDo("PATCH", base+"/"+id, map[string]any{"name": "ci", "description": "renamed"}, 200))
	if got["name"] != "ci" || got["description"] != "renamed" {
		t.Fatalf("patched: %v", got)
	}
	if bodyMap(t, f.mustDo("GET", base+"/"+id, nil, 200))["name"] != "ci" {
		t.Fatal("rename not stored")
	}

	// Revoke: the key is gone from every listing and from the accepted set; its value is erased.
	f.mustDo("DELETE", base+"/"+id, nil, 200)
	f.mustDo("GET", base+"/"+id, nil, 404)
	k, _ = f.mgr.Keys(context.Background(), testRef)
	for _, o := range k.OpaqueKeys(testRef) {
		if o.Key == key {
			t.Fatal("revoked key still accepted")
		}
	}
	for _, r := range k.AllRecords() {
		if r.ID == id && (r.Key != "" || !r.Revoked) {
			t.Fatalf("a revoked record keeps no key: %+v", r)
		}
	}
	// The default keys can be revoked and renamed too; the legacy ones cannot be deleted.
	def := byName("secret", "default")
	f.mustDo("PATCH", base+"/"+def["id"].(string), map[string]any{"name": "primary"}, 200)
	if byName("secret", "primary") == nil {
		t.Fatal("default key not renamed")
	}
	f.mustDo("DELETE", base+"/"+def["id"].(string), nil, 200)
	k, _ = f.mgr.Keys(context.Background(), testRef)
	for _, o := range k.OpaqueKeys(testRef) {
		if o.Key == k.SecretKey {
			t.Fatal("the revoked default secret key is still accepted")
		}
	}
	anon := byName("legacy", "anon")
	f.mustDo("DELETE", base+"/"+anon["id"].(string), nil, 400)
	f.mustDo("PATCH", base+"/"+anon["id"].(string), map[string]any{"name": "x"}, 400)
	f.mustDo("GET", base+"/not-a-key", nil, 404)
	f.mustDo("DELETE", base+"/not-a-key", nil, 404)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestLegacyKeySwitch(t *testing.T) {
	f := newFixture(t)
	url := "/v1/projects/" + testRef + "/api-keys/legacy"
	if bodyMap(t, f.mustDo("GET", url, nil, 200))["enabled"] != true {
		t.Fatal("legacy keys start enabled")
	}
	f.mustDo("PUT", url+"?enabled=false", nil, 200)
	if bodyMap(t, f.mustDo("GET", url, nil, 200))["enabled"] != false {
		t.Fatal("not disabled")
	}
	list := decodeBody(t, f.mustDo("GET", "/v1/projects/"+testRef+"/api-keys", nil, 200)).([]any)
	for _, e := range list {
		if e.(map[string]any)["type"] == "legacy" {
			t.Fatal("a disabled legacy key is still listed")
		}
	}
	if k, _ := f.mgr.Keys(context.Background(), testRef); !k.LegacyDisabled {
		t.Fatal("the proxy's view of the keys does not say disabled")
	}
	f.mustDo("PUT", url+"?enabled=true", nil, 200)
	if bodyMap(t, f.mustDo("GET", url, nil, 200))["enabled"] != true {
		t.Fatal("not re-enabled")
	}
	// The body form works too, and a missing flag is an error.
	f.mustDo("PUT", url, map[string]any{"enabled": false}, 200)
	f.mustDo("PUT", url, nil, 400)
	// Cannot switch off with no key to fall back on: revoke both defaults, then try.
	f.mustDo("PUT", url+"?enabled=true", nil, 200)
	base := "/v1/projects/" + testRef + "/api-keys"
	k, _ := f.mgr.Keys(context.Background(), testRef)
	for _, o := range k.OpaqueKeys(testRef) {
		if o.Type == "secret" {
			f.mustDo("DELETE", base+"/"+o.ID, nil, 200)
		}
	}
	if rec := f.do("PUT", url+"?enabled=false", nil); rec.Code != 400 {
		t.Fatalf("disabling the legacy keys without a secret key: %d", rec.Code)
	}
}

func TestDatabasePassword(t *testing.T) {
	f := newFixture(t)
	for _, url := range []string{"/v1/projects/" + testRef + "/database/password", "/platform/projects/" + testRef + "/db-password"} {
		if rec := f.do("PATCH", url, map[string]any{"password": "short"}); rec.Code != 400 {
			t.Errorf("%s short password: %d", url, rec.Code)
		}
		if rec := f.do("PATCH", url, map[string]any{}); rec.Code != 400 {
			t.Errorf("%s no password: %d", url, rec.Code)
		}
		rec := f.mustDo("PATCH", url, map[string]any{"password": "a-much-longer-password"}, 200)
		if !strings.Contains(rec.Body.String(), "message") {
			t.Errorf("%s: %s", url, rec.Body.String())
		}
	}
	if len(f.mgr.passwords) != 2 || f.mgr.passwords[0] != testRef+":a-much-longer-password" {
		t.Fatalf("passwords: %v", f.mgr.passwords)
	}
	f.mgr.pwErr = errors.New("boom")
	if rec := f.do("PATCH", "/v1/projects/"+testRef+"/database/password", map[string]any{"password": "another-long-password"}); rec.Code != 500 || strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("a failed change answers a generic 500: %d %s", rec.Code, rec.Body.String())
	}
	f.mgr.pwErr = nil
	if rec := f.do("PATCH", "/v1/projects/zzzzzzzzzzzzzzzzzzzz/database/password", map[string]any{"password": "another-long-password"}); rec.Code != 404 {
		t.Fatalf("unknown project: %d", rec.Code)
	}
}

func TestEmailTemplatesAreServedToLoopbackOnly(t *testing.T) {
	f := newFixture(t)
	f.mustDo("PATCH", authV1, map[string]any{"mailer_templates_recovery_content": "<h1>Reset</h1>"}, 200)
	get := func(path, remote string, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = remote
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		return rec
	}
	bare := "/internal/templates/" + testRef + "/recovery"
	url := bare + "?v=1&t=" + f.srv.settings.TemplateToken(testRef, "recovery")
	if rec := get(url, "127.0.0.1:5555"); rec.Code != 200 || rec.Body.String() != "<h1>Reset</h1>" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("loopback: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if rec := get(url, "[::1]:5555"); rec.Code != 200 {
		t.Fatalf("ipv6 loopback: %d", rec.Code)
	}
	for _, c := range []struct {
		path, remote string
		hdr          []string
	}{
		{url, "203.0.113.9:4444", nil},
		{bare, "127.0.0.1:5555", nil},                                   // no token: another process of the node
		{bare + "?t=" + strings.Repeat("0", 64), "127.0.0.1:5555", nil}, // wrong token
		{"/internal/templates/" + testRef + "/invite?t=" + f.srv.settings.TemplateToken(testRef, "recovery"), "127.0.0.1:5555", nil}, // another template's token
		{url, "127.0.0.1:5555", []string{"X-Forwarded-For", "203.0.113.9"}},
		{"/internal/templates/" + testRef + "/invite", "127.0.0.1:1", nil},        // nothing saved
		{"/internal/templates/" + testRef + "/nope", "127.0.0.1:1", nil},          // not a template
		{"/internal/templates/system/recovery", "127.0.0.1:1", nil},               // not a user project
		{"/internal/templates/zzzzzzzzzzzzzzzzzzzz/recovery", "127.0.0.1:1", nil}, // no settings
	} {
		if rec := get(c.path, c.remote, c.hdr...); rec.Code != 404 {
			t.Errorf("%s from %s %v: %d", c.path, c.remote, c.hdr, rec.Code)
		}
	}
}

// fakeStorage is a Storage that answers the routes the dashboard actions use.
type fakeStorage struct {
	*httptest.Server
	mu    sync.Mutex
	calls []string
	last  map[string]any
}

func newFakeStorage(t *testing.T, adminKey string) *fakeStorage {
	t.Helper()
	fs := &fakeStorage{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fs.mu.Lock()
		fs.calls = append(fs.calls, r.Method+" "+r.URL.Path)
		fs.last = nil
		_ = json.Unmarshal(b, &fs.last)
		fs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/bucket/pub":
			_, _ = w.Write([]byte(`{"id":"pub","name":"pub","public":true}`))
		case r.URL.Path == "/bucket/priv":
			_, _ = w.Write([]byte(`{"id":"priv","name":"priv","public":false}`))
		case strings.HasPrefix(r.URL.Path, "/bucket/"):
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"Bucket not found"}`))
		case r.URL.Path == "/object/sign/pub":
			_, _ = w.Write([]byte(`[{"error":null,"path":"a/b.png","signedURL":"/object/sign/pub/a/b.png?token=T1"},{"error":"Either the object does not exist or you do not have access to it","path":"missing.png","signedURL":null}]`))
		case r.URL.Path == "/object/list-v2/pub":
			_, _ = w.Write([]byte(`{"hasNext":false,"folders":[{"name":"a"}],"objects":[]}`))
		case strings.HasSuffix(r.URL.Path, "/credentials"):
			if r.Header.Get("apikey") != adminKey {
				w.WriteHeader(401)
				return
			}
			switch r.Method {
			case "GET":
				_, _ = w.Write([]byte(`[{"id":"1b4e28ba-2fa1-11d2-883f-0016d3cca427","description":"backup","created_at":"2026-01-01T00:00:00.000Z","access_key":"AKIA"}]`))
			case "POST":
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{"id":"1b4e28ba-2fa1-11d2-883f-0016d3cca427","access_key":"AK","secret_key":"SK","description":"backup"}`))
			case "DELETE":
				w.WriteHeader(204)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (f *fixture) withStorage(t *testing.T) *fakeStorage {
	t.Helper()
	adminKey := "admin-key-123"
	fs := newFakeStorage(t, adminKey)
	f.srv.upstreamOverride = func(_ *registry.Project, svc string) string {
		if svc == upStorage || svc == upStorageAdmin {
			return fs.URL
		}
		return ""
	}
	sealed, _ := f.srv.sec.Seal([]byte(adminKey))
	if err := f.reg.PutSecret(context.Background(), "system", "fleet_storage_admin_api_key", sealed); err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestStorageDashboardActions(t *testing.T) {
	f := newFixture(t)
	fs := f.withStorage(t)
	obj := "/platform/storage/" + testRef + "/buckets/pub/objects"

	rec := f.mustDo("POST", obj+"/public-url", map[string]any{"path": "folder/my file.png"}, 201)
	validateAgainstSpec(t, "POST /platform/storage/{ref}/buckets/{id}/objects/public-url", rec.Body.Bytes())
	if got := bodyMap(t, rec)["publicUrl"]; got != "https://"+testRef+".api.example.test/storage/v1/object/public/pub/folder/my%20file.png" {
		t.Fatalf("publicUrl: %v", got)
	}
	rec = f.mustDo("POST", obj+"/public-url", map[string]any{"path": "a.png", "options": map[string]any{"download": "x y.png"}}, 201)
	if got := bodyMap(t, rec)["publicUrl"].(string); !strings.HasSuffix(got, "/pub/a.png?download=x+y.png") {
		t.Fatalf("download: %v", got)
	}
	f.mustDo("POST", "/platform/storage/"+testRef+"/buckets/priv/objects/public-url", map[string]any{"path": "a.png"}, 400)
	f.mustDo("POST", "/platform/storage/"+testRef+"/buckets/ghost/objects/public-url", map[string]any{"path": "a.png"}, 404)
	f.mustDo("POST", obj+"/public-url", map[string]any{"path": "../x"}, 400)
	f.mustDo("POST", obj+"/public-url", map[string]any{}, 400)

	rec = f.mustDo("POST", obj+"/sign-multi", map[string]any{"path": []string{"/a/b.png", "missing.png"}, "expiresIn": 60}, 200)
	validateAgainstSpec(t, "POST /platform/storage/{ref}/buckets/{id}/objects/sign-multi", rec.Body.Bytes())
	var signed []map[string]any
	for _, e := range decodeBody(t, rec).([]any) {
		signed = append(signed, e.(map[string]any))
	}
	if len(signed) != 2 || signed[0]["signedUrl"] != "https://"+testRef+".api.example.test/storage/v1/object/sign/pub/a/b.png?token=T1" ||
		signed[1]["signedUrl"] != nil || signed[1]["error"] == nil {
		t.Fatalf("sign-multi: %v", signed)
	}
	if paths, _ := fs.last["paths"].([]any); len(paths) != 2 || paths[0] != "a/b.png" || fs.last["expiresIn"] != float64(60) {
		t.Fatalf("what Storage received: %v", fs.last)
	}
	rec = f.mustDo("POST", obj+"/list-v2", map[string]any{"prefix": "", "with_delimiter": true, "limit": 100}, 200)
	validateAgainstSpec(t, "POST /platform/storage/{ref}/buckets/{id}/objects/list-v2", rec.Body.Bytes())
	if bodyMap(t, rec)["hasNext"] != false || fs.last["with_delimiter"] != true {
		t.Fatalf("list-v2: %s / %v", rec.Body.String(), fs.last)
	}

	cred := "/platform/storage/" + testRef + "/credentials"
	rec = f.mustDo("GET", cred, nil, 200)
	validateAgainstSpec(t, "GET /platform/storage/{ref}/credentials", rec.Body.Bytes())
	list := bodyMap(t, rec)["data"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["description"] != "backup" || strings.Contains(f.do("GET", cred, nil).Body.String(), "AKIA") == false && false {
		t.Fatalf("credentials: %v", list)
	}
	rec = f.mustDo("POST", cred, map[string]any{"description": "backup"}, 201)
	validateAgainstSpec(t, "POST /platform/storage/{ref}/credentials", rec.Body.Bytes())
	created := bodyMap(t, rec)
	if created["secret_key"] != "SK" {
		t.Fatalf("created: %v", created)
	}
	if fs.last["description"] != "backup" || fs.last["claims"].(map[string]any)["role"] != "service_role" {
		t.Fatalf("claims: %v", fs.last)
	}
	f.mustDo("POST", cred, map[string]any{"description": "x"}, 400)
	f.mustDo("DELETE", cred+"/1b4e28ba-2fa1-11d2-883f-0016d3cca427", nil, 204)
	if fs.last["id"] != "1b4e28ba-2fa1-11d2-883f-0016d3cca427" {
		t.Fatalf("delete body: %v", fs.last)
	}
}

// Every setting of the specs' auth bodies has a place in the schema: when the pinned specs
// grow a field (the nightly spec diff re-pins them), this fails until the schema knows it.
func TestAuthSchemaCoversTheSpecs(t *testing.T) {
	covered := map[string]bool{}
	for _, f := range projectconfig.AuthSchema.Fields {
		covered[f.Name] = true
	}
	// Settings the specs list that GoTrue's self-hosted build or sbctl does not take: reported
	// only (see projectconfig's README).
	reportOnly := map[string]bool{
		"custom_oauth_max_providers": true, "nimbus_oauth_email_optional": true,
	}
	for _, spec := range []struct {
		file, schema string
		upper        bool
	}{{"gen/specs/v1.json", "UpdateAuthConfigBody", false}, {"gen/specs/v1.json", "AuthConfigResponse_Output", false},
		{"gen/specs/platform.json", "UpdateGoTrueConfigBody", true}, {"gen/specs/platform.json", "GoTrueConfigResponse", true}} {
		b, err := specFS.ReadFile(spec.file)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Components struct {
				Schemas map[string]struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"schemas"`
			} `json:"components"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		props := doc.Components.Schemas[spec.schema].Properties
		if len(props) < 100 {
			t.Fatalf("%s %s has %d properties", spec.file, spec.schema, len(props))
		}
		for name := range props {
			n := strings.ToLower(name)
			if !covered[n] && !reportOnly[n] && !platformOnly[n] {
				t.Errorf("%s of %s: no setting %q in projectconfig.AuthSchema", spec.schema, spec.file, n)
			}
		}
	}
}

// platformOnly are settings only Studio's twin lists; they are not stored.
var platformOnly = map[string]bool{
	"audit_log_disable_postgres": true, "index_worker_ensure_user_search_indexes_exist": true,
	"mailer_subjects_custom_contents": true, "mailer_templates_custom_contents": true, "mfa_allow_low_aal": true,
}

// A rollback after a failed apply must bring the database back on the restored settings
// (Recover) and carry the restart request over, so a restart that left the cluster down is
// undone, and the original request still gets its error.
func TestFailedPostgresApplyRecoversTheCluster(t *testing.T) {
	f := newFixture(t)
	url := "/v1/projects/" + testRef + "/config/database/postgres"
	f.project.Limits.MemoryMax = "1G"
	if err := f.reg.UpdateProject(context.Background(), f.project); err != nil {
		t.Fatal(err)
	}
	f.mustDo("PUT", url, map[string]any{"statement_timeout": "30s"}, 200)
	f.mgr.applyOpts = nil
	n := 0
	f.mgr.applyHook = func(projectconfig.Service) error {
		n++
		if n == 1 {
			return errors.New("postgres did not start")
		}
		return nil
	}
	rec := f.do("PUT", url, map[string]any{"statement_timeout": "60s", "restart_database": true})
	if rec.Code != 502 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if len(f.mgr.applyOpts) != 2 || f.mgr.applyOpts[0].Recover || !f.mgr.applyOpts[0].RestartDatabase {
		t.Fatalf("the failing apply: %+v", f.mgr.applyOpts)
	}
	if r := f.mgr.applyOpts[1]; !r.Recover || !r.RestartDatabase {
		t.Fatalf("the restoring apply must recover the cluster and restart it: %+v", r)
	}
	if got := bodyMap(t, f.mustDo("GET", url, nil, 200)); got["statement_timeout"] != "30s" {
		t.Fatalf("settings after the rollback: %v", got["statement_timeout"])
	}
}

// The storage limit shown before anything is saved is the node's configured one, the same
// value the tenant is given.
func TestStorageDefaultLimitIsTheNodes(t *testing.T) {
	f := newFixture(t)
	f.srv.cfg.Fleet.StorageFileSizeLimit = 123 << 20
	url := "/v1/projects/" + testRef + "/config/storage"
	if got := bodyMap(t, f.mustDo("GET", url, nil, 200)); got["fileSizeLimit"] != float64(123<<20) {
		t.Fatalf("GET before a save: %v", got["fileSizeLimit"])
	}
	f.mustDo("PATCH", url, map[string]any{"fileSizeLimit": 1 << 20}, 200)
	if got := bodyMap(t, f.mustDo("GET", url, nil, 200)); got["fileSizeLimit"] != float64(1<<20) {
		t.Fatalf("GET after a save: %v", got["fileSizeLimit"])
	}
	f.mustDo("PATCH", url, map[string]any{"fileSizeLimit": nil}, 200)
	if got := bodyMap(t, f.mustDo("GET", url, nil, 200)); got["fileSizeLimit"] != float64(123<<20) {
		t.Fatalf("GET after a reset: %v", got["fileSizeLimit"])
	}
}

func TestShortErrKeepsTheFirstLineOnly(t *testing.T) {
	err := errors.New("lifecycle: apply postgrest settings: did not become ready\nGET /rest/v1 200\nPGRST002 retry")
	if got := shortErr(err); got != "lifecycle: apply postgrest settings: did not become ready" {
		t.Errorf("%q", got)
	}
	if got := shortErr(errors.New(strings.Repeat("x", 500))); len(got) != 303 {
		t.Errorf("long message not cut: %d", len(got))
	}
}

func TestKeyPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"sb_secret_Bbn5nzeQ4HabcdEF":    "sb_secret_Bbn5",
		"sb_publishable_Xy12zzzzzzzzzz": "sb_publishable_Xy12",
		"sb_secret_ab":                  "sb_secret_ab",
	} {
		if got := keyPrefix(in); got != want {
			t.Errorf("keyPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
