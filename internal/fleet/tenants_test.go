package fleet

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
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// call is one request a fake service received.
type call struct {
	Method, Path string
	Header       http.Header
	Body         map[string]any
}

// fakeAPI is a service admin API stand-in: it records calls, answers from a script, and
// holds tenants in a map so GET reflects earlier PUT, POST and DELETE.
type fakeAPI struct {
	mu      sync.Mutex
	calls   []call
	tenants map[string]bool
	// fail returns a status to answer instead of handling the request (0 means handle it).
	fail func(c call, n int) int
	// authOK checks the credentials of a request.
	authOK func(h http.Header) bool
	// tenantPath extracts the tenant id from a request path ("" if it is not a tenant path).
	tenantPath func(method, path string) string
	// createStatus is the status of a successful create or upsert.
	createStatus int
	srv          *httptest.Server
}

func newFakeAPI(t *testing.T, authOK func(http.Header) bool, tenantPath func(method, path string) string, createStatus int) *fakeAPI {
	t.Helper()
	f := &fakeAPI{tenants: map[string]bool{}, authOK: authOK, tenantPath: tenantPath, createStatus: createStatus}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	c := call{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()}
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		if err := json.Unmarshal(b, &c.Body); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "content type", 415)
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if !f.authOK(r.Header) {
		w.WriteHeader(403)
		return
	}
	if f.fail != nil {
		if s := f.fail(c, len(f.calls)); s != 0 {
			http.Error(w, "scripted failure", s)
			return
		}
	}
	id := f.tenantPath(r.Method, r.URL.Path)
	switch {
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/terminate"):
		w.WriteHeader(200)
	case r.Method == "GET":
		if !f.tenants[id] {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"data":{}}`))
	case r.Method == "PUT" || r.Method == "POST":
		if id == "" {
			id, _ = c.Body["tenant"].(map[string]any)["external_id"].(string)
		}
		f.tenants[id] = true
		w.WriteHeader(f.createStatus)
	case r.Method == "DELETE":
		if !f.tenants[id] {
			w.WriteHeader(404)
			return
		}
		delete(f.tenants, id)
		w.WriteHeader(204)
	default:
		w.WriteHeader(405)
	}
}

func (f *fakeAPI) methods() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return strings.Join(out, " ")
}

func (f *fakeAPI) last() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func testClient(name string) (*apiClient, *[]time.Duration) {
	var waits []time.Duration
	return &apiClient{
		name: name, http: &http.Client{Timeout: 5 * time.Second}, retry: Retry{Attempts: 4, Base: 10 * time.Millisecond, Max: 40 * time.Millisecond},
		log:   discardLog(),
		sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
	}, &waits
}

func bearerOK(secret string) func(http.Header) bool {
	return func(h http.Header) bool {
		tok := strings.TrimPrefix(h.Get("Authorization"), "Bearer ")
		claims, err := secrets.ParseHS256(tok, secret)
		return err == nil && claims["exp"] != nil
	}
}

func pathTenant(prefix string) func(method, path string) string {
	return func(_, p string) string {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			return ""
		}
		id, _, _ := strings.Cut(rest, "/")
		return id
	}
}

func testSpec(k *secrets.ProjectKeys, port int) TenantSpec {
	return TenantSpec{
		Ref: testRef, DBHost: "127.0.0.1", DBPort: port, DBName: "postgres", DBUser: "supabase_admin", DBPassword: k.AdminPassword,
		PostgresPassword: k.DBPassword, JWTSecret: k.JWTSecret, AnonKey: k.AnonKey, ServiceRoleKey: k.ServiceRoleKey,
		Host: testRef + ".api.supavise.test",
	}
}

func sub(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		cur = cur.(map[string]any)[k]
	}
	return cur
}

func TestSupavisorTenantLifecycle(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	const apiSecret = "supavisor-api-secret-0123456789abcdef"
	api := newFakeAPI(t, bearerOK(apiSecret), pathTenant("/api/tenants/"), 201)
	cl, _ := testClient(config.SvcSupavisor)
	var managerCalls []string
	tn := &supavisorTenant{
		cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: api.srv.URL, secret: apiSecret, now: time.Now,
		setManager: func(_ context.Context, spec TenantSpec, pw string) error {
			managerCalls = append(managerCalls, spec.Ref+":"+pw)
			return nil
		},
	}
	ctx := context.Background()
	spec := testSpec(k, 20003)

	if err := tn.EnsureTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET PUT" {
		t.Fatalf("calls = %s, want GET then PUT", got)
	}
	put := api.last()
	if put.Path != "/api/tenants/"+testRef {
		t.Errorf("path = %s", put.Path)
	}
	tenant := put.Body["tenant"].(map[string]any)
	wantTenant := map[string]any{
		"db_host": "127.0.0.1", "db_port": float64(20003), "db_database": "postgres", "require_user": false,
		"auth_query": "SELECT * FROM pgbouncer.get_auth($1)", "upstream_ssl": false, "enforce_ssl": false, "default_pool_size": float64(15),
		// sent even when nothing is saved: an update that omits it keeps a limit that was reset
		"default_max_clients": float64(1000),
	}
	for key, want := range wantTenant {
		if tenant[key] != want {
			t.Errorf("tenant.%s = %v, want %v", key, tenant[key], want)
		}
	}
	users := tenant["users"].([]any)
	if len(users) != 1 {
		t.Fatalf("users = %v", users)
	}
	u := users[0].(map[string]any)
	pw := managerPassword(k.AdminPassword)
	if u["db_user"] != "pgbouncer" || u["db_password"] != pw || u["is_manager"] != true || u["mode_type"] != "transaction" || u["pool_size"] != float64(15) {
		t.Errorf("manager user = %v", u)
	}
	if strings.Contains(string(mustJSON(t, put.Body)), k.AdminPassword) {
		t.Error("the supabase_admin password must not be sent to Supavisor")
	}
	if len(managerCalls) != 1 || managerCalls[0] != testRef+":"+pw {
		t.Errorf("manager role setup = %v", managerCalls)
	}

	// Same spec again: nothing is sent, so Supavisor does not drop the tenant's pools.
	if err := tn.EnsureTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET PUT GET" || len(managerCalls) != 1 {
		t.Fatalf("calls = %s, manager calls %d; an unchanged tenant must not be updated", got, len(managerCalls))
	}

	// A changed spec is sent.
	spec.PoolSize, spec.MaxClients = 7, 300
	if err := tn.EnsureTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET PUT GET GET PUT" {
		t.Fatalf("calls = %s", got)
	}
	tenant = api.last().Body["tenant"].(map[string]any)
	if tenant["default_pool_size"] != float64(7) || tenant["default_max_clients"] != float64(300) {
		t.Errorf("tenant = %v", tenant)
	}

	// The tenant vanished from Supavisor (its database was reset): it is created again even
	// though the fingerprint matches.
	api.mu.Lock()
	delete(api.tenants, testRef)
	api.mu.Unlock()
	if err := tn.EnsureTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET PUT GET GET PUT GET PUT" {
		t.Fatalf("calls = %s", got)
	}

	// Remove: pools are terminated, the record deleted, the fingerprint cleared.
	if err := tn.RemoveTenant(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); !strings.HasSuffix(got, "GET DELETE") {
		t.Fatalf("calls = %s", got)
	}
	if fp := tn.store.get(ctx, testRef, config.SvcSupavisor); fp != "" {
		t.Errorf("fingerprint survived removal: %q", fp)
	}
	if err := tn.RemoveTenant(ctx, testRef); err != nil {
		t.Fatalf("removing a missing tenant: %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSupavisorRejectsBadInput(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	api := newFakeAPI(t, bearerOK("s"), pathTenant("/api/tenants/"), 201)
	cl, _ := testClient("supavisor")
	tn := &supavisorTenant{cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: api.srv.URL, secret: "s", now: time.Now,
		setManager: func(context.Context, TenantSpec, string) error { return nil }}
	ctx := context.Background()
	for _, ref := range []string{"system", "short", "ABCDEFGHIJKLMNOPQRST", "abcdefghijklmnopqrs1"} {
		spec := testSpec(k, 20003)
		spec.Ref = ref
		if err := tn.EnsureTenant(ctx, spec); err == nil {
			t.Errorf("ref %q accepted", ref)
		}
		if err := tn.RemoveTenant(ctx, ref); err == nil {
			t.Errorf("remove of ref %q accepted", ref)
		}
	}
	spec := testSpec(k, 0)
	if err := tn.EnsureTenant(ctx, spec); err == nil {
		t.Error("missing port accepted")
	}
	if len(api.calls) != 0 {
		t.Errorf("%d calls for invalid input", len(api.calls))
	}
}

func TestSupavisorManagerRoleFailureStopsBeforePut(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	api := newFakeAPI(t, bearerOK("s"), pathTenant("/api/tenants/"), 201)
	cl, _ := testClient("supavisor")
	tn := &supavisorTenant{cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: api.srv.URL, secret: "s", now: time.Now,
		setManager: func(context.Context, TenantSpec, string) error { return errors.New("connection refused") }}
	err := tn.EnsureTenant(context.Background(), testSpec(k, 20003))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(api.methods(), "PUT") {
		t.Fatal("the tenant was created although its manager role could not be set up")
	}
	if fp := tn.store.get(context.Background(), testRef, config.SvcSupavisor); fp != "" {
		t.Fatal("a fingerprint was recorded for a tenant that was never created")
	}
}

func TestRealtimeTenantLifecycle(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	const apiSecret = "realtime-api-secret-0123456789abcdef"
	api := newFakeAPI(t, bearerOK(apiSecret), pathTenant("/api/tenants/"), 201)
	cl, _ := testClient(config.SvcRealtime)
	tn := &realtimeTenant{cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: api.srv.URL, secret: apiSecret, now: time.Now}
	ctx := context.Background()
	spec := testSpec(k, 20003)

	if err := tn.EnsureTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET POST" {
		t.Fatalf("calls = %s", got)
	}
	post := api.last()
	if post.Path != "/api/tenants" {
		t.Errorf("path = %s", post.Path)
	}
	tenant := post.Body["tenant"].(map[string]any)
	if tenant["external_id"] != testRef || tenant["name"] != testRef || tenant["jwt_secret"] != k.JWTSecret || tenant["postgres_cdc_default"] != "postgres_cdc_rls" {
		t.Errorf("tenant = %v", tenant)
	}
	ext := tenant["extensions"].([]any)[0].(map[string]any)
	if ext["type"] != "postgres_cdc_rls" || ext["tenant_external_id"] != testRef {
		t.Errorf("extension = %v", ext)
	}
	st := ext["settings"].(map[string]any)
	// db_port must be a string: Realtime validates it with is_binary/1.
	wantSettings := map[string]any{
		"region": "us-east-1", "db_host": "127.0.0.1", "db_port": "20003", "db_name": "postgres", "db_user": "supabase_admin",
		"db_password": k.AdminPassword, "slot_name": "supabase_realtime_replication_slot", "publication": "supabase_realtime",
		"poll_interval_ms": float64(100), "poll_max_changes": float64(100), "poll_max_record_bytes": float64(1048576), "ssl_enforced": false,
	}
	for key, want := range wantSettings {
		if st[key] != want {
			t.Errorf("settings.%s = %#v, want %#v", key, st[key], want)
		}
	}

	if err := tn.EnsureTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET POST GET" {
		t.Fatalf("calls = %s; an unchanged tenant must not be re-sent (it would disconnect the tenant's sockets)", got)
	}
	// Key rotation changes the JWT secret: the tenant is updated.
	spec.JWTSecret = "rotated-secret-0123456789abcdef0123456789"
	if err := tn.EnsureTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET POST GET GET POST" {
		t.Fatalf("calls = %s", got)
	}
	if sub(api.last().Body, "tenant", "jwt_secret") != spec.JWTSecret {
		t.Error("the rotated secret was not sent")
	}

	if err := tn.RemoveTenant(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	if err := tn.RemoveTenant(ctx, testRef); err != nil {
		t.Fatalf("removing a missing tenant: %v", err)
	}
	if got := api.methods(); !strings.HasSuffix(got, "DELETE DELETE") {
		t.Fatalf("calls = %s", got)
	}
}

func TestRealtimeTenantQuiesce(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	const apiSecret = "realtime-api-secret-0123456789abcdef"
	api := newFakeAPI(t, bearerOK(apiSecret), pathTenant("/api/tenants/"), 204)
	cl, _ := testClient(config.SvcRealtime)
	tn := &realtimeTenant{cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: api.srv.URL, secret: apiSecret, now: time.Now}
	_ = k
	if err := tn.QuiesceTenant(context.Background(), testRef); err != nil {
		t.Fatal(err)
	}
	if c := api.last(); c.Method != "POST" || c.Path != "/api/tenants/"+testRef+"/reload" {
		t.Fatalf("call %s %s", c.Method, c.Path)
	}
	// A tenant the service does not know is quiet already; a server error is reported.
	api.fail = func(c call, _ int) int { return 404 }
	if err := tn.QuiesceTenant(context.Background(), testRef); err != nil {
		t.Fatalf("404: %v", err)
	}
	api.fail = func(c call, _ int) int { return 400 }
	if err := tn.QuiesceTenant(context.Background(), testRef); err == nil {
		t.Fatal("a refused reload must be reported")
	}
	if err := tn.QuiesceTenant(context.Background(), "../x"); err == nil {
		t.Fatal("a bad ref was accepted")
	}
	if err := (Fleet{tn}).QuiesceTenant(context.Background(), testRef); err == nil {
		t.Fatal("the fleet must report the failure of a tenant")
	}
}

func TestStorageTenantLifecycle(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	const adminKey = "storage-admin-key-0123456789"
	api := newFakeAPI(t, func(h http.Header) bool { return h.Get("apikey") == adminKey }, pathTenant("/tenants/"), 204)
	cl, _ := testClient(config.SvcStorage)
	tn := &storageTenant{cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: api.srv.URL, adminKey: adminKey, fileSize: 1234,
		adminPassword: registryAdminPassword(n.reg, n.sec)}
	ctx := context.Background()
	spec := testSpec(k, 20003) // no StorageAdminPassword: it comes from the registry

	if err := tn.EnsureTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET PUT" {
		t.Fatalf("calls = %s (PUT upserts; POST fails on a second call)", got)
	}
	put := api.last()
	if put.Path != "/tenants/"+testRef {
		t.Errorf("path = %s", put.Path)
	}
	want := map[string]any{
		"anonKey": k.AnonKey, "serviceKey": k.ServiceRoleKey, "jwtSecret": k.JWTSecret,
		"databaseUrl":    "postgres://supabase_storage_admin:" + k.StorageAdminPassword + "@127.0.0.1:20003/postgres",
		"maxConnections": float64(5), "fileSizeLimit": float64(1234),
	}
	for key, w := range want {
		if put.Body[key] != w {
			t.Errorf("%s = %v, want %v", key, put.Body[key], w)
		}
	}
	if sub(put.Body, "features", "s3Protocol", "enabled") != true || sub(put.Body, "features", "imageTransformation", "enabled") != false {
		t.Errorf("features = %v", put.Body["features"])
	}

	if err := tn.EnsureTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET PUT GET" {
		t.Fatalf("calls = %s", got)
	}
	// A password from the spec wins over the registry.
	spec.StorageAdminPassword = "from-spec"
	if err := tn.EnsureTenant(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(api.last().Body["databaseUrl"].(string), ":from-spec@") {
		t.Errorf("databaseUrl = %v", api.last().Body["databaseUrl"])
	}

	if err := tn.RemoveTenant(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	if err := tn.RemoveTenant(ctx, testRef); err != nil {
		t.Fatalf("removing a missing tenant: %v", err)
	}
}

func TestStorageTenantWithoutAdminPassword(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	api := newFakeAPI(t, func(http.Header) bool { return true }, pathTenant("/tenants/"), 204)
	cl, _ := testClient("storage")
	tn := &storageTenant{cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: api.srv.URL, adminKey: "x",
		adminPassword: func(context.Context, string) (string, error) { return "", errors.New("no such secret") }}
	err := tn.EnsureTenant(context.Background(), testSpec(k, 20003))
	if err == nil || !strings.Contains(err.Error(), "supabase_storage_admin") {
		t.Fatalf("err = %v", err)
	}
	if len(api.calls) != 0 {
		t.Fatal("storage was called without a password")
	}
}

func TestRetryTransientFailuresThenSucceed(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	api := newFakeAPI(t, bearerOK("s"), pathTenant("/api/tenants/"), 201)
	api.fail = func(c call, n int) int {
		if n <= 3 { // the service is still starting
			return 503
		}
		return 0
	}
	cl, waits := testClient("realtime")
	tn := &realtimeTenant{cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: api.srv.URL, secret: "s", now: time.Now}
	if err := tn.EnsureTenant(context.Background(), testSpec(k, 20003)); err != nil {
		t.Fatal(err)
	}
	if got := api.methods(); got != "GET GET GET GET POST" {
		t.Fatalf("calls = %s", got)
	}
	// Exponential backoff, capped: 10ms base, 40ms max, up to 25% jitter.
	if len(*waits) != 3 {
		t.Fatalf("waits = %v", *waits)
	}
	for i, base := range []time.Duration{10, 20, 40} {
		base *= time.Millisecond
		if w := (*waits)[i]; w < base || w > base+base/4+1 {
			t.Errorf("wait %d = %v, want %v..%v", i, w, base, base+base/4)
		}
	}
}

func TestRetryGivesUpAfterTheBound(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	api := newFakeAPI(t, bearerOK("s"), pathTenant("/api/tenants/"), 201)
	api.fail = func(call, int) int { return 502 }
	cl, waits := testClient("realtime")
	tn := &realtimeTenant{cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: api.srv.URL, secret: "s", now: time.Now}
	err := tn.EnsureTenant(context.Background(), testSpec(k, 20003))
	if err == nil || !strings.Contains(err.Error(), "after 4 tries") || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}
	if len(api.calls) != 4 || len(*waits) != 3 {
		t.Fatalf("%d calls and %d waits, want 4 and 3", len(api.calls), len(*waits))
	}
}

func TestConnectionRefusedIsRetried(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // nothing listens any more
	cl, waits := testClient("storage")
	tn := &storageTenant{cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: base, adminKey: "x", adminPassword: registryAdminPassword(n.reg, n.sec)}
	err := tn.EnsureTenant(context.Background(), testSpec(k, 20003))
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v", err)
	}
	if len(*waits) != 3 {
		t.Fatalf("waits = %v", *waits)
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	api := newFakeAPI(t, bearerOK("right"), pathTenant("/api/tenants/"), 201)
	cl, waits := testClient("realtime")
	tn := &realtimeTenant{cl: cl, store: tenantStore{reg: n.reg, sec: n.sec}, base: api.srv.URL, secret: "wrong", now: time.Now}
	err := tn.EnsureTenant(context.Background(), testSpec(k, 20003))
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
	if len(api.calls) != 1 || len(*waits) != 0 {
		t.Fatalf("%d calls, %d waits: a 403 is final", len(api.calls), len(*waits))
	}
}

func TestRetryStopsWhenTheContextEnds(t *testing.T) {
	api := newFakeAPI(t, func(http.Header) bool { return true }, pathTenant("/tenants/"), 204)
	api.fail = func(call, int) int { return 503 }
	ctx, cancel := context.WithCancel(context.Background())
	cl := &apiClient{name: "storage", http: http.DefaultClient, retry: Retry{Attempts: 50, Base: time.Hour, Max: time.Hour}, log: discardLog(),
		sleep: func(ctx context.Context, d time.Duration) error { cancel(); <-ctx.Done(); return ctx.Err() }}
	_, err := cl.do(ctx, "GET", api.srv.URL+"/x", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(api.calls) != 1 {
		t.Fatalf("%d calls after cancel", len(api.calls))
	}
}

func TestSetupBuildsTheFleetInOrder(t *testing.T) {
	n := newTestNode(t)
	fl, err := Setup(context.Background(), n.deps())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tn := range fl {
		got = append(got, tn.Service())
	}
	if strings.Join(got, ",") != "supavisor,realtime,storage" {
		t.Fatalf("fleet = %v", got)
	}
	if _, err := Setup(context.Background(), Deps{Cfg: n.cfg}); err == nil {
		t.Fatal("Setup without a registry succeeded")
	}
	// A second Setup (the daemon after the CLI) reuses the stored secrets.
	a, _ := loadCreds(context.Background(), n.deps(), false)
	if _, err := Setup(context.Background(), n.deps()); err != nil {
		t.Fatal(err)
	}
	b, _ := loadCreds(context.Background(), n.deps(), false)
	if a.storageAdminKey != b.storageAdminKey {
		t.Fatal("secrets regenerated")
	}
}

func TestLoadTenantSpec(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	n.cfg.Ports.ProjectBase = 30000
	spec, err := LoadTenantSpec(context.Background(), n.deps(), testRef)
	if err != nil {
		t.Fatal(err)
	}
	if spec.DBPort != 30003 || spec.DBPassword != k.AdminPassword || spec.StorageAdminPassword != k.StorageAdminPassword || spec.Host != testRef+".api.supavise.test" || spec.JWTSecret != k.JWTSecret {
		t.Fatalf("spec = %+v", spec)
	}
	if _, err := LoadTenantSpec(context.Background(), n.deps(), "system"); err == nil {
		t.Fatal("system has no tenants")
	}
	if _, err := LoadTenantSpec(context.Background(), n.deps(), "zzzzzzzzzzzzzzzzzzzz"); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestFleetFansOutAndStopsOnFirstEnsureError(t *testing.T) {
	// The Fleet type is lifecycle's interface to the tenants; keep its contract.
	var order []string
	mk := func(name string, err error) Tenant { return &stubTenant{name: name, err: err, order: &order} }
	f := Fleet{mk("a", nil), mk("b", errors.New("boom")), mk("c", nil)}
	if err := f.EnsureTenant(context.Background(), TenantSpec{Ref: testRef}); err == nil {
		t.Fatal("error swallowed")
	}
	if strings.Join(order, ",") != "ensure a,ensure b" {
		t.Fatalf("order = %v", order)
	}
	order = nil
	if err := f.RemoveTenant(context.Background(), testRef); err == nil || order[len(order)-1] != "remove c" {
		t.Fatalf("remove must try every service: err %v order %v", err, order)
	}
}

type stubTenant struct {
	name  string
	err   error
	order *[]string
}

func (s *stubTenant) Service() string { return s.name }
func (s *stubTenant) EnsureTenant(context.Context, TenantSpec) error {
	*s.order = append(*s.order, "ensure "+s.name)
	return s.err
}
func (s *stubTenant) RemoveTenant(context.Context, string) error {
	*s.order = append(*s.order, "remove "+s.name)
	return s.err
}

// A Supavise release that moves the pin of Storage or Realtime must send every tenant again once:
// Storage migrates a tenant's database when the tenant is updated, Realtime when it is created.
// The same release sends nothing.
func TestANewServiceReleaseSendsTheTenantAgain(t *testing.T) {
	n := newTestNode(t)
	k := n.project(t, testRef)
	ctx := context.Background()
	spec := testSpec(k, 20003)

	const adminKey = "storage-admin-key-0123456789"
	sapi := newFakeAPI(t, func(h http.Header) bool { return h.Get("apikey") == adminKey }, pathTenant("/tenants/"), 204)
	scl, _ := testClient(config.SvcStorage)
	st := &storageTenant{cl: scl, store: tenantStore{reg: n.reg, sec: n.sec}, base: sapi.srv.URL, adminKey: adminKey, fileSize: 1234,
		release: "storage-v1.79.36-r0", adminPassword: registryAdminPassword(n.reg, n.sec)}
	steps := []struct {
		release string
		want    string
	}{
		{"storage-v1.79.36-r0", "GET PUT"},
		{"storage-v1.79.36-r0", "GET PUT GET"},
		{"storage-v1.80.0-r0", "GET PUT GET GET PUT"},
		{"storage-v1.80.0-r0", "GET PUT GET GET PUT GET"},
	}
	for i, s := range steps {
		st.release = s.release
		if err := st.EnsureTenant(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if got := sapi.methods(); got != s.want {
			t.Fatalf("storage step %d (%s): calls = %s, want %s", i, s.release, got, s.want)
		}
	}

	const apiSecret = "realtime-api-secret-0123456789abcdef"
	rapi := newFakeAPI(t, bearerOK(apiSecret), pathTenant("/api/tenants/"), 201)
	rcl, _ := testClient(config.SvcRealtime)
	rt := &realtimeTenant{cl: rcl, store: tenantStore{reg: n.reg, sec: n.sec}, base: rapi.srv.URL, secret: apiSecret, now: time.Now, release: "realtime-v2.140.10-r0"}
	for i, s := range []struct{ release, want string }{
		{"realtime-v2.140.10-r0", "GET POST"},
		{"realtime-v2.140.10-r0", "GET POST GET"},
		{"realtime-v2.141.0-r0", "GET POST GET GET POST"},
	} {
		rt.release = s.release
		if err := rt.EnsureTenant(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if got := rapi.methods(); got != s.want {
			t.Fatalf("realtime step %d (%s): calls = %s, want %s", i, s.release, got, s.want)
		}
	}
}

func TestWithReleaseKeepsAnUnknownReleaseAsItWas(t *testing.T) {
	fp := fingerprint("body")
	if withRelease(fp, "") != fp {
		t.Fatal("an empty tag changed the fingerprint")
	}
	if withRelease(fp, "storage-v1-r0") == fp || withRelease(fp, "storage-v1-r0") == withRelease(fp, "storage-v2-r0") {
		t.Fatal("the tag is not part of the fingerprint")
	}
}
