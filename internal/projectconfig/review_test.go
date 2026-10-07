package projectconfig

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/OWNER/sbctl/internal/secrets"
)

func patchWith(m *Manager, svc Service, body map[string]any, cx CrossContext) error {
	_, err := m.Patch(context.Background(), ref, svc, body, cx)
	return err
}

func wantInvalidWith(t *testing.T, err error, contains string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Msg, contains) {
		t.Fatalf("want a validation error containing %q, got %v", contains, err)
	}
}

// Postgres accepts these values in ALTER SYSTEM and then cannot start with them; the save
// must refuse them whatever the memory limit.
func TestPostgresRefusesSettingsThePostmasterCannotStartWith(t *testing.T) {
	m, _ := newManager(t)
	for _, body := range []map[string]any{
		{"max_locks_per_transaction": float64(2147483640)},
		{"max_locks_per_transaction": float64(1025)},
		{"max_worker_processes": float64(262143)},
		{"max_logical_replication_workers": float64(262143)},
		{"max_parallel_workers": float64(262143)},
		{"max_wal_senders": float64(262143)},
	} {
		for _, cx := range []CrossContext{{}, {MemoryLimit: 1 << 30}} {
			if err := patchWith(m, Postgres, body, cx); err == nil {
				t.Errorf("%v accepted with limit %d", body, cx.MemoryLimit)
			}
		}
	}
	// Within the caps, the lock table must fit the memory limit; without a limit only a
	// modest table is accepted.
	wantInvalidWith(t, patchWith(m, Postgres, map[string]any{"max_locks_per_transaction": float64(1024), "max_connections": float64(100)}, CrossContext{MemoryLimit: 256 << 20}), "lock table")
	wantInvalidWith(t, patchWith(m, Postgres, map[string]any{"max_locks_per_transaction": float64(1024), "max_connections": float64(900)}, CrossContext{}), "needs a memory limit")
	wantInvalidWith(t, patchWith(m, Postgres, map[string]any{"max_connections": float64(900), "max_worker_processes": float64(256)}, CrossContext{}), "needs a memory limit")
	// Sensible values pass with and without a limit.
	for _, cx := range []CrossContext{{}, {MemoryLimit: 1 << 30}, {MemoryLimit: 512 << 20}} {
		if err := patchWith(m, Postgres, map[string]any{"max_locks_per_transaction": float64(256), "max_worker_processes": float64(16), "max_connections": float64(100)}, cx); err != nil {
			t.Errorf("limit %d: %v", cx.MemoryLimit, err)
		}
	}
	// Shared memory is checked as a whole against the limit.
	wantInvalidWith(t, patchWith(m, Postgres, map[string]any{"shared_buffers": "300MB", "max_locks_per_transaction": float64(1024)}, CrossContext{MemoryLimit: 512 << 20}), "memory limit")
}

// pg_cron's launcher and pg_net's worker take two of max_worker_processes and a running job one
// each (cron.max_running_jobs is 8): fewer than 10 leaves jobs without a worker.
func TestPostgresKeepsWorkersForPgCron(t *testing.T) {
	m, _ := newManager(t)
	for _, n := range []float64{0, 4, 9} {
		wantInvalidWith(t, patchWith(m, Postgres, map[string]any{"max_worker_processes": n}, CrossContext{}), "pg_cron")
	}
	if err := patchWith(m, Postgres, map[string]any{"max_worker_processes": float64(10)}, CrossContext{}); err != nil {
		t.Fatal(err)
	}
}

// The pool is bounded by the project's max_connections and the client limit by the node; the
// values a tenant runs with unsaved always pass, and an unknown limit (0) checks nothing.
func TestPoolerLimits(t *testing.T) {
	m, _ := newManager(t)
	cx := CrossContext{MaxConnections: 60, PoolerMaxClients: 5000}
	wantInvalidWith(t, patchWith(m, Pooler, map[string]any{"default_pool_size": float64(51)}, cx), "at most 50")
	wantInvalidWith(t, patchWith(m, Pooler, map[string]any{"max_client_conn": float64(5001)}, cx), "pooler_max_client_conn")
	for _, body := range []map[string]any{{"default_pool_size": float64(50), "max_client_conn": float64(5000)}, {"default_pool_size": nil, "max_client_conn": nil}} {
		if err := patchWith(m, Pooler, body, cx); err != nil {
			t.Errorf("%v: %v", body, err)
		}
	}
	// Small database, small ceiling: the shipped defaults (15 and 1000) are never refused.
	tiny := CrossContext{MaxConnections: 20, PoolerMaxClients: 100}
	if err := patchWith(m, Pooler, map[string]any{"default_pool_size": float64(15), "max_client_conn": float64(1000)}, tiny); err != nil {
		t.Error(err)
	}
	wantInvalidWith(t, patchWith(m, Pooler, map[string]any{"default_pool_size": float64(16)}, tiny), "at most 15")
	if err := patchWith(m, Pooler, map[string]any{"default_pool_size": float64(4950), "max_client_conn": float64(54000)}, CrossContext{}); err != nil {
		t.Errorf("no known limit: %v", err)
	}
}

// A line break cannot be carried by a unit's environment: the save is refused, so a resume
// never fails on it.
func TestEnvSettingsRefuseLineBreaks(t *testing.T) {
	m, _ := newManager(t)
	for _, body := range []map[string]any{
		{"sms_template": "Your code\nis {{ .Code }}"},
		{"mailer_subjects_invite": "Hi\r\nthere"},
		{"smtp_pass": "pa\nss"},
		{"external_github_secret": "a\nb"},
		{"mfa_phone_template": "x\ny"},
		{"site_url": "https://x.example\n"},
	} {
		wantInvalidWith(t, patchWith(m, Auth, body, CrossContext{}), "line break")
	}
	wantInvalidWith(t, patchWith(m, PostgREST, map[string]any{"db_schema": "public\nextra"}, CrossContext{}), "line break")
	// Template bodies are served by URL and may be multi-line.
	if err := patchWith(m, Auth, map[string]any{"mailer_templates_invite_content": "<h2>Hi</h2>\n<p>{{ .Email }}</p>\n"}, CrossContext{}); err != nil {
		t.Fatal(err)
	}
	if env, _ := m.AuthEnv(context.Background(), ref, ""); strings.ContainsAny(env["GOTRUE_MAILER_TEMPLATES_INVITE"], "\n\r") {
		t.Fatal("the template URL must be a single line")
	}
}

// A stored value that no longer passes (saved before the check existed) is dropped on read,
// so it cannot break the render.
func TestStoredLineBreakIsIgnored(t *testing.T) {
	m, st := newManager(t)
	if _, err := st.Put(context.Background(), &Record{Ref: ref, Service: Auth, Values: map[string]any{"sms_template": "a\nb", "jwt_exp": float64(900)}, Sealed: map[string][]byte{}}, 0); err != nil {
		t.Fatal(err)
	}
	env, err := m.AuthEnv(context.Background(), ref, "")
	if err != nil || env["GOTRUE_SMS_TEMPLATE"] != "" || env["GOTRUE_JWT_EXP"] != "900" {
		t.Fatalf("%v %v", env, err)
	}
}

// Realtime applies only what it is sent, so returning a setting to its default must send
// the default.
func TestRealtimeResetSendsTheDefault(t *testing.T) {
	m, _ := newManager(t)
	patch(t, m, Realtime, map[string]any{"private_only": true, "max_concurrent_users": float64(500), "connection_pool": float64(3)})
	patch(t, m, Realtime, map[string]any{"private_only": nil, "max_concurrent_users": nil, "connection_pool": nil})
	rt, _ := m.RealtimeSettings(context.Background(), ref)
	if rt.Tenant["private_only"] != false || rt.Tenant["max_concurrent_users"] != int64(200) || rt.Extension["db_pool"] != int64(1) {
		t.Fatalf("a reset must send the defaults: %+v", rt)
	}
	st, _ := m.Get(context.Background(), ref, Realtime)
	if st.Effective["private_only"] != false || st.Effective["max_concurrent_users"] != int64(200) {
		t.Fatalf("effective: %v", st.Effective)
	}
	// Other services still drop a reset setting.
	patch(t, m, PostgREST, map[string]any{"max_rows": float64(50)})
	patch(t, m, PostgREST, map[string]any{"max_rows": nil})
	if got, _ := m.PostgRESTEnv(context.Background(), ref); len(got) != 0 {
		t.Fatalf("postgrest after a reset: %v", got)
	}
}

func TestTemplateTokens(t *testing.T) {
	m, _ := newManager(t)
	tok := m.TemplateToken(ref, "invite")
	if len(tok) != 64 || tok == m.TemplateToken(ref, "recovery") || tok == m.TemplateToken("zyxwvutsrqponmlkjihg", "invite") {
		t.Fatalf("tokens must differ per project and template: %q", tok)
	}
	if !m.TemplateTokenOK(ref, "invite", tok) || m.TemplateTokenOK(ref, "invite", "") || m.TemplateTokenOK(ref, "recovery", tok) || m.TemplateTokenOK("zyxwvutsrqponmlkjihg", "invite", tok) {
		t.Fatal("a token opens only its own template")
	}
	// Another node (another master key) signs differently.
	other, _ := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	m2 := NewManager(NewMemory(), other, Options{})
	if m2.TemplateToken(ref, "invite") == tok {
		t.Fatal("the token must depend on the node's master key")
	}
	// Secrets that cannot derive keys leave the route open (tests and mocks only).
	m3 := NewManager(NewMemory(), noDerive{other}, Options{})
	if m3.TemplateToken(ref, "invite") != "" || !m3.TemplateTokenOK(ref, "invite", "") {
		t.Fatal("without a derivable key there is no token and no check")
	}
}

type noDerive struct{ s secrets.Secrets }

func (n noDerive) Seal(b []byte) ([]byte, error) { return n.s.Seal(b) }
func (n noDerive) Open(b []byte) ([]byte, error) { return n.s.Open(b) }

// The render check is the net under the per-setting check: a secret stored with a line break
// (sealed values are not re-validated on read) still cannot get past a save.
func TestRenderCheckCatchesAStoredSecretWithALineBreak(t *testing.T) {
	m, st := newManager(t)
	sealed, err := m.sec.Seal([]byte("pa\nss"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), &Record{Ref: ref, Service: Auth, Values: map[string]any{}, Sealed: map[string][]byte{"smtp_pass": sealed}}, 0); err != nil {
		t.Fatal(err)
	}
	wantInvalidWith(t, patchWith(m, Auth, map[string]any{"jwt_exp": float64(900)}, CrossContext{}), "GOTRUE_SMTP_PASS")
	// Replacing the secret fixes it.
	if err := patchWith(m, Auth, map[string]any{"smtp_pass": "pass"}, CrossContext{}); err != nil {
		t.Fatal(err)
	}
}
