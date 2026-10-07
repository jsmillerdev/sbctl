package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/api/cryptojs"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

func TestProjectLifecycle(t *testing.T) {
	f := newFixture(t)
	rec := f.do("POST", "/v1/projects", map[string]any{"name": "Second", "organization_slug": "default", "db_pass": "pw", "region": "local"})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "POST /v1/projects", rec.Body.Bytes())
	ref := jsonField(t, rec, "ref").(string)
	if !secrets.ValidRef(ref) {
		t.Fatalf("ref %q", ref)
	}
	if len(f.mgr.created) != 1 || f.mgr.created[0].DBPassword != "pw" || f.mgr.created[0].Ref != ref || f.mgr.created[0].OrgSlug != "default" {
		t.Fatalf("manager request: %+v", f.mgr.created)
	}
	if rec := f.do("POST", "/v1/projects", map[string]any{"name": ""}); rec.Code != 400 {
		t.Fatalf("empty name: %d", rec.Code)
	}
	if rec := f.do("POST", "/v1/projects", map[string]any{"name": "x", "organization_slug": "nope"}); rec.Code != 404 {
		t.Fatalf("unknown org: %d", rec.Code)
	}
	// Platform create answers with the keys.
	rec = f.do("POST", "/platform/projects", map[string]any{"name": "Third"})
	if rec.Code != 201 {
		t.Fatalf("platform create: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "POST /platform/projects", rec.Body.Bytes())
	if jsonField(t, rec, "anon_key").(string) == "" {
		t.Fatal("no anon key")
	}

	if rec := f.do("POST", "/v1/projects/"+ref+"/pause", nil); rec.Code != 200 {
		t.Fatalf("pause: %d", rec.Code)
	}
	if rec := f.do("GET", "/v1/projects/"+ref, nil); jsonField(t, rec, "status") != "INACTIVE" {
		t.Fatalf("status after pause: %s", rec.Body)
	}
	// SQL against a paused project explains itself.
	if rec := f.do("POST", "/v1/projects/"+ref+"/database/query", map[string]any{"query": "select 1"}); rec.Code != 409 {
		t.Fatalf("query on paused project: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/v1/projects/"+ref+"/restore", nil); rec.Code != 200 {
		t.Fatalf("restore: %d", rec.Code)
	}
	if rec := f.do("PATCH", "/v1/projects/"+ref, map[string]any{"name": "Renamed"}); rec.Code != 200 || jsonField(t, rec, "name") != "Renamed" {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("DELETE", "/v1/projects/"+ref, nil); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/v1/projects/"+ref, nil); rec.Code != 404 {
		t.Fatalf("get deleted: %d", rec.Code)
	}
}

func TestCreateReportsEarlyFailure(t *testing.T) {
	f := newFixture(t)
	f.mgr.createFn = func(lifecycle.CreateRequest) (*registry.Project, error) { return nil, lifecycle.ErrInvalidState }
	if rec := f.do("POST", "/v1/projects", map[string]any{"name": "x"}); rec.Code != 409 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestCreateReturnsWhileProvisioning(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	f.mgr.createFn = func(req lifecycle.CreateRequest) (*registry.Project, error) {
		p := &registry.Project{Ref: req.Ref, Name: req.Name, Status: registry.StatusComingUp}
		if err := f.reg.CreateProject(nil, p); err != nil { //nolint:staticcheck // memory registry ignores ctx
			return nil, err
		}
		<-release
		return p, nil
	}
	defer close(release)
	start := time.Now()
	rec := f.do("POST", "/v1/projects", map[string]any{"name": "slow"})
	if rec.Code != 201 || jsonField(t, rec, "status") != "COMING_UP" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("create blocked on provisioning for %v", time.Since(start))
	}
}

// TestDeviceLogin plays the Supabase CLI against the login endpoints with the
// crypto of supabase/cli login-crypto.layer.ts: ECDH P-256, shared secret as the
// AES-256-GCM key, ciphertext with the tag appended.
func TestDeviceLogin(t *testing.T) {
	f := newFixture(t)
	cli, _ := ecdh.P256().GenerateKey(rand.Reader)
	session := "9d3b3d2a-8a51-4f6e-8c7e-0b2f4f6a1a11"
	body := map[string]any{"session_id": session, "public_key": hex.EncodeToString(cli.PublicKey().Bytes()), "token_name": "cli_test@host"}

	if rec := f.doAs("", "POST", "/platform/cli/login", body); rec.Code != 401 {
		t.Fatalf("authorizing needs a dashboard session: %d", rec.Code)
	}
	rec := f.do("POST", "/platform/cli/login", body)
	if rec.Code != 201 {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body)
	}
	nonce := jsonField(t, rec, "nonce").(string)
	code := nonce[:8]

	poll := func(c string) *httptest.ResponseRecorder {
		return f.doAs("", "GET", "/platform/cli/login/"+session+"?device_code="+c, nil)
	}
	if rec := poll("00000000"); rec.Code != 400 {
		t.Fatalf("wrong code: %d %s", rec.Code, rec.Body)
	}
	rec = poll(code) // the session survives a wrong guess
	if rec.Code != 200 {
		t.Fatalf("poll: %d %s", rec.Code, rec.Body)
	}
	var got struct{ AccessToken, PublicKey, Nonce string }
	var raw map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	got.AccessToken, got.PublicKey, got.Nonce = raw["access_token"], raw["public_key"], raw["nonce"]

	serverPub, err := hex.DecodeString(got.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ecdh.P256().NewPublicKey(serverPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := cli.ECDH(pub)
	block, _ := aes.NewCipher(shared)
	gcm, _ := cipher.NewGCM(block)
	n, _ := hex.DecodeString(got.Nonce)
	ct, _ := hex.DecodeString(got.AccessToken)
	plain, err := gcm.Open(nil, n, ct, nil)
	if err != nil {
		t.Fatalf("cannot decrypt access token: %v", err)
	}
	if !patRe.MatchString(string(plain)) {
		t.Fatalf("decrypted token %q is not a PAT", plain)
	}
	if rec := f.doAs(string(plain), "GET", "/v1/projects", nil); rec.Code != 200 {
		t.Fatalf("the login token does not work: %d", rec.Code)
	}
	if rec := f.doAs(string(plain), "GET", "/v1/profile", nil); jsonField(t, rec, "gotrue_id") != f.userID {
		t.Fatalf("profile via PAT: %s", rec.Body)
	}
	if rec := poll(code); rec.Code != 404 {
		t.Fatalf("a session works once: %d", rec.Code)
	}
	// Five wrong codes destroy the session and its token.
	session2 := "7e1b3d2a-8a51-4f6e-8c7e-0b2f4f6a1a22"
	body["session_id"] = session2
	rec = f.do("POST", "/platform/cli/login", body)
	for i := 0; i < maxLoginFailures; i++ {
		f.doAs("", "GET", "/platform/cli/login/"+session2+"?device_code=deadbeef", nil)
	}
	if rec := f.doAs("", "GET", "/platform/cli/login/"+session2+"?device_code="+jsonField(t, rec, "nonce").(string)[:8], nil); rec.Code != 404 {
		t.Fatalf("burned session still claimable: %d", rec.Code)
	}
	if rec := f.do("POST", "/platform/cli/login", map[string]any{"session_id": "x", "public_key": "zz"}); rec.Code != 400 {
		t.Fatalf("bad request: %d", rec.Code)
	}
}

func TestPGMetaProxyAndSQL(t *testing.T) {
	f := newFixture(t)
	f.mgr.dsn = "postgres://supabase_admin:s3cret@127.0.0.1:20000/postgres?sslmode=disable"

	rec := f.do("POST", "/platform/pg-meta/"+testRef+"/query?statementTimeoutSecs=5", map[string]any{"query": "select 1"},
		"X-Connection-Encrypted", "bogus-from-studio", "X-Pg-Application-Name", "supabase/dashboard")
	if rec.Code != 200 {
		t.Fatalf("proxy: %d %s", rec.Code, rec.Body)
	}
	got := f.meta.last()
	if got.Path != "/query" || got.RawQuery != "statementTimeoutSecs=5" || got.DSN != f.mgr.dsn || !strings.Contains(got.Body, "select 1") {
		t.Fatalf("upstream request: %+v", got)
	}
	// GET pg-meta routes of the platform spec are proxied the same way.
	if rec := f.do("GET", "/platform/pg-meta/"+testRef+"/tables?included_schemas=public", nil); rec.Code != 200 || f.meta.last().Path != "/tables" {
		t.Fatalf("GET tables: %d %+v", rec.Code, f.meta.last())
	}
	// Read-only SQL asks Postgres for read-only transactions through the DSN.
	if rec := f.do("POST", "/v1/projects/"+testRef+"/database/query", map[string]any{"query": "select 1", "read_only": true}); rec.Code != 201 {
		t.Fatalf("read only: %d %s", rec.Code, rec.Body)
	}
	if dsn := f.meta.last().DSN; !strings.Contains(dsn, "default_transaction_read_only") || !strings.Contains(dsn, "sslmode=disable") {
		t.Fatalf("read-only DSN: %s", dsn)
	}
	// pg-meta down.
	f.meta.Close()
	if rec := f.do("POST", "/v1/projects/"+testRef+"/database/query", map[string]any{"query": "select 1"}); rec.Code != 503 {
		t.Fatalf("pg-meta down: %d %s", rec.Code, rec.Body)
	}
}

func TestPGMetaKeyPersisted(t *testing.T) {
	f := newFixture(t)
	f.cfg.API.PGMetaCryptoKey = ""
	srv, err := NewServer(Deps{Registry: f.reg, Secrets: f.mgr.sec, Manager: f.mgr, Config: f.cfg, PGMetaURL: f.meta.URL})
	if err != nil {
		t.Fatal(err)
	}
	k1, err := srv.pgmetaKey(nil) //nolint:staticcheck // memory registry ignores ctx
	if err != nil || len(k1) < 16 {
		t.Fatalf("key %q: %v", k1, err)
	}
	srv2, _ := NewServer(Deps{Registry: f.reg, Secrets: f.mgr.sec, Manager: f.mgr, Config: f.cfg, PGMetaURL: f.meta.URL})
	if k2, _ := srv2.pgmetaKey(nil); k2 != k1 { //nolint:staticcheck
		t.Fatalf("key not persisted: %q vs %q", k2, k1)
	}
	enc, _ := srv.pgmetaConn(nil, testRef, "postgres", false) //nolint:staticcheck
	if dsn, err := cryptojs.Decrypt(enc, k1); err != nil || dsn != f.mgr.dsn {
		t.Fatalf("header: %q %v", dsn, err)
	}
}

func TestAuthAndStorageProxy(t *testing.T) {
	f := newFixture(t)
	var seen []*http.Request
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r2 := r.Clone(r.Context())
		r2.Body = io.NopCloser(strings.NewReader(string(b)))
		seen = append(seen, r2)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	f.srv.upstreamOverride = func(*registry.Project, string) string { return up.URL }
	keys, _ := f.mgr.Keys(nil, testRef) //nolint:staticcheck

	if rec := f.do("PATCH", "/platform/auth/"+testRef+"/users/u1", map[string]any{"ban_duration": "none"}); rec.Code != 200 {
		t.Fatalf("auth proxy: %d %s", rec.Code, rec.Body)
	}
	r := seen[len(seen)-1]
	if r.Method != "PUT" || r.URL.Path != "/admin/users/u1" || r.Header.Get("Authorization") != "Bearer "+keys.ServiceRoleKey {
		t.Fatalf("upstream: %s %s auth=%s", r.Method, r.URL.Path, r.Header.Get("Authorization"))
	}
	if rec := f.do("DELETE", "/platform/storage/"+testRef+"/buckets/b1/objects", map[string]any{"paths": []string{"a/b.txt"}}); rec.Code != 200 {
		t.Fatalf("storage proxy: %d %s", rec.Code, rec.Body)
	}
	r = seen[len(seen)-1]
	b, _ := io.ReadAll(r.Body)
	if r.Method != "DELETE" || r.URL.Path != "/object/b1" || !strings.Contains(string(b), `"prefixes":["a/b.txt"]`) ||
		r.Header.Get("X-Forwarded-Host") != testRef+".api.example.test" {
		t.Fatalf("storage upstream: %s %s %s host=%s", r.Method, r.URL.Path, b, r.Header.Get("X-Forwarded-Host"))
	}
	// Paused projects are not proxied to.
	f.mgr.Pause(nil, testRef) //nolint:errcheck,staticcheck
	if rec := f.do("GET", "/platform/storage/"+testRef+"/buckets", nil); rec.Code != 409 {
		t.Fatalf("paused: %d", rec.Code)
	}
}

func TestFunctionsRoundTrip(t *testing.T) {
	f := newFixture(t)
	body, ctype := functionUpload(t, "index.ts", "console.log(1)")
	for i := 1; i <= 2; i++ {
		rec := f.do("POST", "/v1/projects/"+testRef+"/functions/deploy?slug=hello", body, "Content-Type", ctype)
		if rec.Code != 201 || jsonField(t, rec, "version") != float64(i) {
			t.Fatalf("deploy %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := f.do("GET", "/v1/projects/"+testRef+"/functions/hello/body", nil)
	_, params, err := mime.ParseMediaType(rec.Header().Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	part, err := multipart.NewReader(rec.Body, params["boundary"]).NextPart()
	if err != nil || part.FileName() != "index.ts" {
		t.Fatalf("body part: %v %v", part, err)
	}
	if b, _ := io.ReadAll(part); string(b) != "console.log(1)" {
		t.Fatalf("stored source %q", b)
	}
	// Nested paths survive; path traversal is refused.
	nested, ctn := functionUpload(t, "lib/util.ts", "export {}")
	if rec := f.do("POST", "/v1/projects/"+testRef+"/functions/deploy?slug=nest", nested, "Content-Type", ctn); rec.Code != 201 || jsonField(t, rec, "entrypoint_path") != "lib/util.ts" {
		t.Fatalf("nested: %d %s", rec.Code, rec.Body)
	}
	bad, ct2 := functionUpload(t, "../../etc/passwd", "x")
	if rec := f.do("POST", "/v1/projects/"+testRef+"/functions/deploy?slug=evil", bad, "Content-Type", ct2); rec.Code != 400 {
		t.Fatalf("traversal: %d", rec.Code)
	}
	if rec := f.do("POST", "/v1/projects/"+testRef+"/functions/deploy?slug=1bad", body, "Content-Type", ctype); rec.Code != 400 {
		t.Fatalf("bad slug: %d", rec.Code)
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/functions/missing", nil); rec.Code != 404 {
		t.Fatalf("missing: %d", rec.Code)
	}
}

func TestSecretsAreSealed(t *testing.T) {
	f := newFixture(t)
	if rec := f.do("POST", "/v1/projects/"+testRef+"/secrets", []map[string]string{{"name": "SUPABASE_URL", "value": "x"}}); rec.Code != 400 {
		t.Fatalf("reserved name: %d", rec.Code)
	}
	f.do("POST", "/v1/projects/"+testRef+"/secrets", []map[string]string{{"name": "API_KEY", "value": "hunter2"}})
	rec := f.do("GET", "/v1/projects/"+testRef+"/secrets", nil)
	if strings.Contains(rec.Body.String(), "hunter2") || jsonField(t, rec, "0.value") != digest([]byte("hunter2")) {
		t.Fatalf("secrets list must show digests only: %s", rec.Body)
	}
}

func TestAPIKeysMasking(t *testing.T) {
	f := newFixture(t)
	keys, _ := f.mgr.Keys(nil, testRef) //nolint:staticcheck
	rec := f.do("GET", "/v1/projects/"+testRef+"/api-keys", nil)
	if strings.Contains(rec.Body.String(), keys.ServiceRoleKey) || strings.Contains(rec.Body.String(), keys.SecretKey) {
		t.Fatal("secret keys leaked without reveal=true")
	}
	if !strings.Contains(rec.Body.String(), keys.AnonKey) || !strings.Contains(rec.Body.String(), keys.PublishableKey) {
		t.Fatal("public keys missing")
	}
	rec = f.do("GET", "/v1/projects/"+testRef+"/api-keys?reveal=true", nil)
	if !strings.Contains(rec.Body.String(), keys.ServiceRoleKey) || !strings.Contains(rec.Body.String(), keys.SecretKey) {
		t.Fatal("reveal=true must include secret keys")
	}
	id := jsonField(t, rec, "2.id").(string)
	if rec := f.do("GET", "/v1/projects/"+testRef+"/api-keys/"+id, nil); rec.Code != 200 || jsonField(t, rec, "type") != "publishable" {
		t.Fatalf("by id: %d %s", rec.Code, rec.Body)
	}
}

func TestTemporaryKeyIsValid(t *testing.T) {
	f := newFixture(t)
	rec := f.do("POST", "/platform/projects/"+testRef+"/api-keys/temporary", nil)
	keys, _ := f.mgr.Keys(nil, testRef) //nolint:staticcheck
	claims, err := secrets.ParseHS256(jsonField(t, rec, "api_key").(string), keys.JWTSecret)
	if err != nil || claims["role"] != "service_role" || claims["ref"] != testRef {
		t.Fatalf("claims %v err %v", claims, err)
	}
}

func TestProfileFromJWTClaims(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/platform/profile", nil)
	if jsonField(t, rec, "first_name") != "Dev" || jsonField(t, rec, "last_name") != "Eloper" || jsonField(t, rec, "primary_email") != "dev@example.test" {
		t.Fatalf("profile: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "billing:all") {
		t.Fatal("billing must be disabled for Studio")
	}
}

func TestHealth(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/v1/projects/"+testRef+"/health", nil)
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	byName := map[string]map[string]any{}
	for _, e := range list {
		byName[e["name"].(string)] = e
	}
	if byName["db"]["healthy"] != true || byName["rest"]["healthy"] != false || byName["rest"]["error"] != "connection refused" || byName["realtime"] == nil {
		t.Fatalf("health: %s", rec.Body)
	}
}

func TestPlatformProjectOperations(t *testing.T) {
	f := newFixture(t)
	ref := testRef
	if rec := f.do("POST", "/platform/projects/"+ref+"/pause", nil); rec.Code != 201 {
		t.Fatalf("pause: %d", rec.Code)
	}
	if rec := f.do("GET", "/platform/projects/"+ref+"/status", nil); jsonField(t, rec, "status") != "INACTIVE" {
		t.Fatalf("status: %s", rec.Body)
	}
	if rec := f.do("POST", "/platform/projects/"+ref+"/restore", nil); rec.Code != 201 {
		t.Fatalf("restore: %d", rec.Code)
	}
	if rec := f.do("POST", "/platform/projects/"+ref+"/restart", nil); rec.Code != 201 || len(f.mgr.paused) != 2 || len(f.mgr.resumed) != 2 {
		t.Fatalf("restart: %d paused=%v resumed=%v", rec.Code, f.mgr.paused, f.mgr.resumed)
	}
	if rec := f.do("PATCH", "/platform/projects/"+ref, map[string]any{"name": "Platform name"}); rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	} else {
		validateAgainstSpec(t, "PATCH /platform/projects/{ref}", rec.Body.Bytes())
	}
	if rec := f.do("DELETE", "/platform/projects/"+ref, nil); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	} else {
		validateAgainstSpec(t, "DELETE /platform/projects/{ref}", rec.Body.Bytes())
	}
	if rec := f.do("GET", "/platform/projects", nil); jsonField(t, rec, "pagination.count") != float64(0) {
		t.Fatalf("list after delete: %s", rec.Body)
	}
}

func TestMiscRoutes(t *testing.T) {
	f := newFixture(t)
	f.run(t, []step{
		{key: "POST /platform/organizations", body: map[string]any{"name": "Second Team"}, status: 201, check: want("slug", "second-team")},
		{key: "PUT /v1/projects/{ref}/api-keys/legacy", path: "/v1/projects/" + testRef + "/api-keys/legacy?enabled=true"},
		{key: "POST /v1/projects/{ref}/functions", body: map[string]any{"slug": "legacy", "name": "Legacy", "verify_jwt": false}, status: 201, check: want("verify_jwt", false)},
	})
	rec := f.do("GET", "/platform/projects/"+testRef+"/settings", nil)
	if jsonField(t, rec, "jwt_secret") == nil || !strings.Contains(rec.Body.String(), "service_role") {
		t.Fatalf("settings carry the keys the dashboard shows: %s", rec.Body)
	}
	tok := decodeBody(t, f.do("POST", "/platform/profile/access-tokens", map[string]any{"name": "t"})).(map[string]any)
	id := itoa(int64(tok["id"].(float64)))
	if rec := f.do("GET", "/platform/profile/access-tokens/"+id, nil); rec.Code != 200 {
		t.Fatalf("get token: %d", rec.Code)
	} else {
		validateAgainstSpec(t, "GET /platform/profile/access-tokens/{id}", rec.Body.Bytes())
	}
}

// TestProxyRoutes calls every proxy route once against a fake upstream.
func TestProxyRoutes(t *testing.T) {
	f := newFixture(t)
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	f.srv.upstreamOverride = func(*registry.Project, string) string { return up.URL }
	n := 0
	call := func(key string) {
		method, tmpl, _ := strings.Cut(key, " ")
		path := strings.NewReplacer("{ref}", testRef, "{id}", "id1").Replace(tmpl)
		var body any
		if method != "GET" {
			body = map[string]any{"paths": []string{"x"}}
		}
		if rec := f.do(method, path, body); rec.Code != 200 {
			t.Errorf("%s: %d %s", key, rec.Code, rec.Body)
		}
		n++
	}
	for key := range authMap {
		call(key)
	}
	for key := range storageMap {
		call(key)
	}
	ops, _ := Operations()
	for _, op := range ops {
		if strings.HasPrefix(op.Path, "/platform/pg-meta/{ref}/") {
			path := strings.ReplaceAll(op.Path, "{ref}", testRef)
			if rec := f.do(op.Method, path, map[string]any{"query": "select 1"}); rec.Code != 200 {
				t.Errorf("%s: %d %s", op.Key(), rec.Code, rec.Body)
			}
			if got := f.meta.last(); got.Method != op.Method || got.Path != strings.TrimPrefix(path, "/platform/pg-meta/"+testRef) {
				t.Errorf("%s forwarded as %s %s", op.Key(), got.Method, got.Path)
			}
		}
	}
	if len(paths) != n {
		t.Errorf("%d upstream requests for %d proxy calls: %v", len(paths), n, paths)
	}
}

func TestStorageRequestMapping(t *testing.T) {
	f := newFixture(t)
	type seen struct{ method, path, body string }
	var got seen
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = seen{r.Method, r.URL.RequestURI(), string(b)}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"signedURL":"/object/sign/b1/a/b.png?token=tok"}`))
	}))
	defer up.Close()
	f.srv.upstreamOverride = func(*registry.Project, string) string { return up.URL }
	base := "/platform/storage/" + testRef + "/buckets"
	for _, tc := range []struct {
		method, path string
		body         any
		wantPath     string
		wantBody     string
	}{
		{"POST", base, map[string]any{"id": "avatars", "public": true}, "/bucket", `"name":"avatars"`},
		{"PATCH", base + "/avatars", map[string]any{"public": false}, "/bucket/avatars", `"public":false`},
		{"POST", base + "/avatars/objects/list", map[string]any{"path": "folder", "options": map[string]any{"limit": 10, "search": "x"}}, "/object/list/avatars", `"prefix":"folder"`},
		{"POST", base + "/avatars/objects/move", map[string]any{"from": "a", "to": "b"}, "/object/move", `"bucketId":"avatars","destinationKey":"b","sourceKey":"a"`},
		{"DELETE", base + "/avatars/objects", map[string]any{"paths": []any{"a", map[string]any{"path": "b", "versionId": "v"}}}, "/object/avatars", `"prefixes":["a","b"]`},
		{"DELETE", "/platform/auth/" + testRef + "/users/u1?soft_delete=true", nil, "/admin/users/u1", `"should_soft_delete":true`},
	} {
		if rec := f.do(tc.method, tc.path, tc.body); rec.Code != 200 {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body)
		}
		if !strings.HasPrefix(got.path, tc.wantPath) || !strings.Contains(got.body, tc.wantBody) {
			t.Errorf("%s %s -> upstream %s %s %s; want path %s body containing %s", tc.method, tc.path, got.method, got.path, got.body, tc.wantPath, tc.wantBody)
		}
	}
	rec := f.do("POST", base+"/b1/objects/sign", map[string]any{"path": "a/b.png", "expiresIn": 60})
	if got.path != "/object/sign/b1/a/b.png" || jsonField(t, rec, "signedUrl") != "http://"+testRef+".api.example.test/storage/v1/object/sign/b1/a/b.png?token=tok" && jsonField(t, rec, "signedUrl") != "https://"+testRef+".api.example.test/storage/v1/object/sign/b1/a/b.png?token=tok" {
		t.Fatalf("sign: upstream %s response %s", got.path, rec.Body)
	}
	validateAgainstSpec(t, "POST /platform/storage/{ref}/buckets/{id}/objects/sign", rec.Body.Bytes())
}
