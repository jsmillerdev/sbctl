package projectconfig

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/OWNER/sbctl/internal/secrets"
)

const ref = "abcdefghijklmnopqrst"

func newManager(t *testing.T) (*Manager, *Memory) {
	t.Helper()
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st := NewMemory()
	return NewManager(st, sec, Options{TemplateBaseURL: "http://127.0.0.1:7000/internal/templates"}), st
}

func patch(t *testing.T, m *Manager, svc Service, body map[string]any) *Change {
	t.Helper()
	c, err := m.Patch(context.Background(), ref, svc, body, CrossContext{})
	if err != nil {
		t.Fatalf("patch %v: %v", body, err)
	}
	return c
}

func wantInvalid(t *testing.T, m *Manager, svc Service, body map[string]any, contains string) {
	t.Helper()
	_, err := m.Patch(context.Background(), ref, svc, body, CrossContext{MemoryLimit: 1 << 30})
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Msg, contains) {
		t.Fatalf("patch %v: want a validation error containing %q, got %v", body, contains, err)
	}
}

// Every variable the schema renders must be one GoTrue reads: the list is generated from
// the structs of supabase/auth internal/conf at the pinned release (testdata/genenv).
func TestAuthEnvNamesExist(t *testing.T) {
	b, err := os.ReadFile("testdata/gotrue-env-names.txt")
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, n := range strings.Fields(string(b)) {
		known[n] = true
	}
	if len(known) < 300 {
		t.Fatalf("only %d known names: is the fixture empty?", len(known))
	}
	check := func(env, what string) {
		if env != "" && !known[env] {
			t.Errorf("%s: GoTrue has no variable %s", what, env)
		}
	}
	for _, f := range AuthSchema.Fields {
		check(f.Env, f.Name)
	}
	for _, p := range authProviders {
		check("GOTRUE_EXTERNAL_"+strings.ToUpper(p.name)+"_REDIRECT_URI", p.name)
	}
	check("GOTRUE_DB_CONN_PERCENTAGE", "db pool percent")
}

func TestDefaultsRenderNothing(t *testing.T) {
	m, _ := newManager(t)
	env, err := m.AuthEnv(context.Background(), ref, "https://x/auth/v1")
	if err != nil || len(env) != 0 {
		t.Fatalf("no saved settings must leave the unit as lifecycle renders it: %v %v", env, err)
	}
	if got, _ := m.PostgRESTEnv(context.Background(), ref); len(got) != 0 {
		t.Fatalf("postgrest: %v", got)
	}
	if got, _ := m.PostgresSettings(context.Background(), ref); len(got) != 0 {
		t.Fatalf("postgres: %v", got)
	}
}

func TestAuthPatchRendersAndReadsBack(t *testing.T) {
	m, _ := newManager(t)
	c := patch(t, m, Auth, map[string]any{
		"site_url":                              "https://app.example.com",
		"uri_allow_list":                        "https://app.example.com/**,https://preview-*.example.com/**",
		"jwt_exp":                               float64(7200),
		"disable_signup":                        true,
		"external_github_enabled":               true,
		"external_github_client_id":             "gh-id",
		"external_github_secret":                "gh-secret",
		"external_google_enabled":               true,
		"external_google_client_id":             "g1",
		"external_google_additional_client_ids": "g2,g3",
		"external_google_secret":                "gs",
		"rate_limit_otp":                        float64(5),
		"sessions_timebox":                      float64(24),
		"smtp_max_frequency":                    float64(90),
		"sms_test_otp":                          "15551234567=123456,15557654321=654321",
		"mailer_templates_invite_content":       "<h2>Hi {{ .Email }}</h2>",
		"unknown_setting":                       1,
	})
	if len(c.Ignored) != 1 || c.Ignored[0] != "unknown_setting" {
		t.Fatalf("ignored: %v", c.Ignored)
	}
	if c.State.Version != 1 {
		t.Fatalf("version %d", c.State.Version)
	}
	env, err := m.AuthEnv(context.Background(), ref, "https://abc.api.example.com/auth/v1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"GOTRUE_SITE_URL":                     "https://app.example.com",
		"GOTRUE_URI_ALLOW_LIST":               "https://app.example.com/**,https://preview-*.example.com/**",
		"GOTRUE_JWT_EXP":                      "7200",
		"GOTRUE_DISABLE_SIGNUP":               "true",
		"GOTRUE_EXTERNAL_GITHUB_ENABLED":      "true",
		"GOTRUE_EXTERNAL_GITHUB_CLIENT_ID":    "gh-id",
		"GOTRUE_EXTERNAL_GITHUB_SECRET":       "gh-secret",
		"GOTRUE_EXTERNAL_GITHUB_REDIRECT_URI": "https://abc.api.example.com/auth/v1/callback",
		"GOTRUE_EXTERNAL_GOOGLE_CLIENT_ID":    "g1,g2,g3",
		"GOTRUE_RATE_LIMIT_OTP":               "5",
		"GOTRUE_SESSIONS_TIMEBOX":             "24h",
		"GOTRUE_SMTP_MAX_FREQUENCY":           "90s",
		"GOTRUE_SMS_TEST_OTP":                 "15551234567:123456,15557654321:654321",
		"GOTRUE_MAILER_TEMPLATES_INVITE":      "http://127.0.0.1:7000/internal/templates/" + ref + "/invite?v=1&t=" + m.TemplateToken(ref, "invite"),
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	// A second save bumps the version, which changes the template URL.
	patch(t, m, Auth, map[string]any{"jwt_exp": float64(3600)})
	env, _ = m.AuthEnv(context.Background(), ref, "")
	if !strings.Contains(env["GOTRUE_MAILER_TEMPLATES_INVITE"], "?v=2&t=") {
		t.Errorf("template URL kept its version: %s", env["GOTRUE_MAILER_TEMPLATES_INVITE"])
	}
	body, ok, _ := m.Template(context.Background(), ref, "invite")
	if !ok || body != "<h2>Hi {{ .Email }}</h2>" {
		t.Errorf("template body: %q %v", body, ok)
	}
}

func TestSecretsAreSealedAndEchoKeepsThem(t *testing.T) {
	m, st := newManager(t)
	patch(t, m, Auth, map[string]any{"smtp_host": "mail.example.com", "smtp_admin_email": "no-reply@example.com", "smtp_pass": "hunter2hunter2"})
	rec, _ := st.Get(context.Background(), ref, Auth)
	if _, plain := rec.Values["smtp_pass"]; plain || len(rec.Sealed["smtp_pass"]) == 0 {
		t.Fatalf("smtp_pass must be sealed, not in values: %+v", rec)
	}
	for _, b := range rec.Sealed {
		if strings.Contains(string(b), "hunter2") {
			t.Fatal("sealed value contains the plaintext")
		}
	}
	got, _ := m.Get(context.Background(), ref, Auth)
	if got.Set.Str("smtp_pass") != "hunter2hunter2" {
		t.Fatal("secret did not round trip")
	}
	// A client that saves the form sends back what GET showed (the redaction): no change.
	c := patch(t, m, Auth, map[string]any{"smtp_pass": Redact("hunter2hunter2"), "smtp_sender_name": "Acme"})
	if len(c.Changed) != 1 || c.Changed[0] != "smtp_sender_name" {
		t.Fatalf("echoed secret counted as a change: %v", c.Changed)
	}
	if after, _ := m.Get(context.Background(), ref, Auth); after.Set.Str("smtp_pass") != "hunter2hunter2" {
		t.Fatal("echoed redaction replaced the secret")
	}
	// SMTP on: confirmation turns on unless saved explicitly.
	env, _ := m.AuthEnv(context.Background(), ref, "")
	if env["GOTRUE_MAILER_AUTOCONFIRM"] != "false" {
		t.Errorf("autoconfirm with SMTP configured: %q", env["GOTRUE_MAILER_AUTOCONFIRM"])
	}
	// Switching SMTP off resets every smtp_* setting.
	patch(t, m, Auth, map[string]any{"smtp_host": ""})
	got, _ = m.Get(context.Background(), ref, Auth)
	for k := range got.Set {
		if strings.HasPrefix(k, "smtp_") {
			t.Errorf("%s survived switching SMTP off", k)
		}
	}
	if env, _ = m.AuthEnv(context.Background(), ref, ""); env["GOTRUE_MAILER_AUTOCONFIRM"] != "" {
		t.Errorf("autoconfirm override survived: %v", env)
	}
	// null returns a setting to its default.
	patch(t, m, Auth, map[string]any{"site_url": "https://x.example.com"})
	patch(t, m, Auth, map[string]any{"site_url": nil})
	got, _ = m.Get(context.Background(), ref, Auth)
	if _, set := got.Set["site_url"]; set || got.Effective.Str("site_url") != "http://localhost:3000" {
		t.Errorf("null did not restore the default: %v", got.Effective.Str("site_url"))
	}
}

func TestAuthValidation(t *testing.T) {
	m, _ := newManager(t)
	cases := []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"site_url": "not a url"}, "absolute URL"},
		{map[string]any{"site_url": "https://a.example.com,https://b.example.com"}, "invalid format"},
		{map[string]any{"jwt_exp": float64(604801)}, "between"},
		{map[string]any{"jwt_exp": 3.5}, "integer"},
		{map[string]any{"jwt_exp": "3600"}, "number"},
		{map[string]any{"disable_signup": "yes"}, "boolean"},
		{map[string]any{"uri_allow_list": "https://a.example.com/[bad"}, "redirect pattern"},
		{map[string]any{"smtp_port": "99999"}, "port"},
		{map[string]any{"smtp_host": "mail.example.com"}, "smtp_admin_email"},
		{map[string]any{"security_captcha_enabled": true}, "security_captcha_provider"},
		{map[string]any{"security_captcha_enabled": true, "security_captcha_provider": "hcaptcha"}, "security_captcha_secret"},
		{map[string]any{"security_captcha_provider": "recaptcha"}, "one of"},
		{map[string]any{"hook_custom_access_token_enabled": true}, "hook_custom_access_token_uri"},
		{map[string]any{"hook_send_email_uri": "http://example.com/hook"}, "http is only supported"},
		{map[string]any{"hook_send_email_uri": "ftp://example.com/hook"}, "only postgres functions"},
		{map[string]any{"hook_custom_access_token_uri": "pg-functions://postgres/public"}, "pg-functions"},
		{map[string]any{"hook_send_email_enabled": true, "hook_send_email_uri": "https://example.com/hook"}, "hook_send_email_secrets"},
		{map[string]any{"hook_send_email_secrets": "nope"}, "v1,whsec_"},
		{map[string]any{"external_github_enabled": true}, "client_id"},
		{map[string]any{"external_github_enabled": true, "external_github_client_id": "x"}, "secret"},
		{map[string]any{"passkey_enabled": true}, "webauthn_rp_id"},
		{map[string]any{"mailer_allow_unverified_email_sign_ins": true}, "auto-confirm"},
		{map[string]any{"password_min_length": float64(3)}, "between"},
		{map[string]any{"password_required_characters": "abc"}, "one of"},
		{map[string]any{"rate_limit_otp": float64(0)}, "between"},
		{map[string]any{"sms_provider": "carrier-pigeon"}, "one of"},
		{map[string]any{"sms_test_otp": "abc"}, "invalid format"},
		{map[string]any{"mailer_otp_length": float64(11)}, "between"},
		{map[string]any{"db_max_pool_size_unit": "percent", "db_max_pool_size": float64(150)}, "percentage"},
	}
	for _, c := range cases {
		wantInvalid(t, m, Auth, c.body, c.want)
	}
	// Nothing invalid was saved.
	if got, _ := m.Get(context.Background(), ref, Auth); got.Version != 0 {
		t.Fatalf("a rejected patch was saved: version %d", got.Version)
	}
	// A valid whole.
	patch(t, m, Auth, map[string]any{
		"hook_custom_access_token_enabled": true, "hook_custom_access_token_uri": "pg-functions://postgres/public/add_claims",
		"mailer_autoconfirm": false, "mailer_allow_unverified_email_sign_ins": true,
		"security_captcha_enabled": true, "security_captcha_provider": "turnstile", "security_captcha_secret": "0x4AAA",
		"hook_send_email_enabled": true, "hook_send_email_uri": "https://example.com/hook",
		"hook_send_email_secrets": "v1,whsec_" + strings.Repeat("a", 40),
	})
}

func TestConcurrentSavesRetry(t *testing.T) {
	m, st := newManager(t)
	// A writer slips in between this patch's read and write: the patch retries on top.
	racy := &racingStore{Store: st, onFirstPut: func() {
		_, _ = st.Put(context.Background(), &Record{Ref: ref, Service: Auth, Values: map[string]any{"disable_signup": true}, Sealed: map[string][]byte{}}, 0)
	}}
	m.store = racy
	c := patch(t, m, Auth, map[string]any{"jwt_exp": float64(100)})
	if c.State.Version != 2 || !c.State.Set.Bool("disable_signup") || c.State.Set["jwt_exp"] != int64(100) {
		t.Fatalf("lost update: %+v", c.State)
	}
}

type racingStore struct {
	Store
	onFirstPut func()
	done       bool
}

func (r *racingStore) Put(ctx context.Context, rec *Record, expected int64) (*Record, error) {
	if !r.done {
		r.done = true
		r.onFirstPut()
	}
	return r.Store.Put(ctx, rec, expected)
}

func TestPostgRESTSettings(t *testing.T) {
	m, _ := newManager(t)
	patch(t, m, PostgREST, map[string]any{"db_schema": "public, graphql_public ,analytics", "max_rows": float64(50), "db_pool": float64(0), "db_extra_search_path": "public, extensions"})
	patch(t, m, Auth, map[string]any{"jwt_exp": float64(900)})
	env, _ := m.PostgRESTEnv(context.Background(), ref)
	want := map[string]string{
		"PGRST_DB_SCHEMAS": "public,graphql_public,analytics", "PGRST_DB_MAX_ROWS": "50",
		"PGRST_DB_EXTRA_SEARCH_PATH": "public,extensions", "PGRST_APP_SETTINGS_JWT_EXP": "900",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if _, ok := env["PGRST_DB_POOL"]; ok {
		t.Error("db_pool 0 means default and must not render")
	}
	wantInvalid(t, m, PostgREST, map[string]any{"db_schema": `pub"lic`}, "quote")
	wantInvalid(t, m, PostgREST, map[string]any{"db_schema": " , "}, "at least one schema")
	patch(t, m, PostgREST, map[string]any{"db_extra_search_path": ""}) // an empty search path is fine
	wantInvalid(t, m, PostgREST, map[string]any{"max_rows": float64(-1)}, "between")
}

func TestStorageAndRealtimeSettings(t *testing.T) {
	m, _ := newManager(t)
	patch(t, m, Storage, map[string]any{"fileSizeLimit": float64(104857600), "features": map[string]any{"imageTransformation": map[string]any{"enabled": true}}})
	patch(t, m, Storage, map[string]any{"features": map[string]any{"imageTransformation": map[string]any{"maxResolution": float64(1000)}}})
	st, _ := m.StorageSettings(context.Background(), ref)
	it := st.Features["imageTransformation"].(map[string]any)
	if st.FileSizeLimit != 104857600 || it["enabled"] != true || it["maxResolution"] != float64(1000) {
		t.Fatalf("storage: %+v", st)
	}
	wantInvalid(t, m, Storage, map[string]any{"features": map[string]any{"imageTransformation": map[string]any{"enabled": "yes"}}}, "must be a boolean")
	patch(t, m, Storage, map[string]any{"features": map[string]any{"teleport": map[string]any{"enabled": true}, "s3Protocol": map[string]any{"enabled": false, "bogus": 1}}})
	st, _ = m.StorageSettings(context.Background(), ref)
	if _, kept := st.Features["teleport"]; kept || st.Features["s3Protocol"].(map[string]any)["enabled"] != false || len(st.Features["s3Protocol"].(map[string]any)) != 1 {
		t.Fatalf("unknown features and settings must be dropped, known ones kept: %v", st.Features)
	}
	wantInvalid(t, m, Storage, map[string]any{"fileSizeLimit": float64(536870912001)}, "between")

	patch(t, m, Realtime, map[string]any{"max_concurrent_users": float64(500), "connection_pool": float64(3), "postgres_changes_pool": float64(2), "private_only": true})
	rt, _ := m.RealtimeSettings(context.Background(), ref)
	if rt.Tenant["max_concurrent_users"] != int64(500) || rt.Tenant["private_only"] != true || rt.Extension["db_pool"] != int64(3) || rt.Extension["postgres_changes_pool"] != int64(2) {
		t.Fatalf("realtime: %+v", rt)
	}
	if _, bad := rt.Tenant["connection_pool"]; bad {
		t.Fatal("connection_pool belongs to the extension")
	}
	wantInvalid(t, m, Realtime, map[string]any{"max_events_per_second": float64(0)}, "between")
}

func TestPostgresRules(t *testing.T) {
	m, _ := newManager(t)
	patch(t, m, Postgres, map[string]any{"statement_timeout": "30 s", "work_mem": "16MB", "log_connections": true, "max_connections": float64(80), "cron.log_statement": false})
	args, _ := m.PostgresSettings(context.Background(), ref)
	got := strings.Join(args, " ")
	for _, w := range []string{"statement_timeout=30s", "work_mem=16MB", "log_connections=on", "max_connections=80", "cron.log_statement=off"} {
		if !strings.Contains(got, w) {
			t.Errorf("%q missing from %q", w, got)
		}
	}
	for _, c := range []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"work_mem": "16"}, "needs a unit"},
		{map[string]any{"work_mem": "16 parsecs"}, "invalid format"},
		{map[string]any{"work_mem": "1kB"}, "between"},
		{map[string]any{"shared_buffers": "512MB"}, "40%"},
		{map[string]any{"maintenance_work_mem": "900MB"}, "25%"},
		{map[string]any{"checkpoint_timeout": "5s"}, "between"},
		{map[string]any{"max_connections": float64(5)}, "at least 20"},
		{map[string]any{"max_connections": float64(5000)}, "too many"},
		{map[string]any{"max_wal_senders": float64(1)}, "at least 3"},
		{map[string]any{"max_replication_slots": float64(1)}, "at least 2"},
		{map[string]any{"max_wal_size": "16MB"}, "between"},
		{map[string]any{"session_replication_role": "bogus"}, "one of"},
		{map[string]any{"statement_timeout": "-1"}, "between"},
	} {
		wantInvalid(t, m, Postgres, c.body, c.want)
	}
	for _, name := range []string{"shared_buffers", "max_connections", "max_wal_senders", "work_mem"} {
		_ = name
	}
	if !PostgresNeedsRestart("shared_buffers") || PostgresNeedsRestart("work_mem") {
		t.Error("restart classification")
	}
}

func TestRedactIsStable(t *testing.T) {
	if Redact("abc") != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal("Redact is the hex SHA-256")
	}
}

func TestRenderEdgeCases(t *testing.T) {
	m, _ := newManager(t)
	patch(t, m, Auth, map[string]any{
		"db_max_pool_size": float64(10), "db_max_pool_size_unit": "percent",
		"sessions_timebox": float64(0), "sessions_inactivity_timeout": 1.5,
		"api_max_request_duration": float64(30), "password_required_characters": "",
		"external_x_enabled": true, "external_x_client_id": "xid", "external_x_secret": "xs",
		"mfa_phone_max_frequency": float64(45),
	})
	env, _ := m.AuthEnv(context.Background(), ref, "https://abc.api.example.com/auth/v1/")
	want := map[string]string{
		"GOTRUE_DB_CONN_PERCENTAGE": "10", "GOTRUE_DB_MAX_POOL_SIZE": "", // "" removes the unit's own value
		"GOTRUE_SESSIONS_INACTIVITY_TIMEOUT": "1.5h", "GOTRUE_API_MAX_REQUEST_DURATION": "30s",
		"GOTRUE_EXTERNAL_X_ENABLED": "true", "GOTRUE_EXTERNAL_X_CLIENT_ID": "xid", "GOTRUE_MFA_PHONE_MAX_FREQUENCY": "45s",
		"GOTRUE_EXTERNAL_X_REDIRECT_URI": "https://abc.api.example.com/auth/v1/callback",
	}
	for k, v := range want {
		if got, ok := env[k]; !ok || got != v {
			t.Errorf("%s = %q (present %v), want %q", k, got, ok, v)
		}
	}
	if _, ok := env["GOTRUE_SESSIONS_TIMEBOX"]; ok {
		t.Error("a zero timebox must not render: GoTrue refuses it")
	}
}

func TestSecretsAreNeverRenderedForOtherServices(t *testing.T) {
	m, _ := newManager(t)
	patch(t, m, Auth, map[string]any{"external_github_enabled": true, "external_github_client_id": "id", "external_github_secret": "very-secret"})
	rest, _ := m.PostgRESTEnv(context.Background(), ref)
	pg, _ := m.PostgresSettings(context.Background(), ref)
	for _, v := range rest {
		if strings.Contains(v, "very-secret") {
			t.Fatal("a secret leaked into PostgREST's environment")
		}
	}
	for _, s := range pg {
		if strings.Contains(s, "very-secret") {
			t.Fatal("a secret leaked into the server arguments")
		}
	}
}
