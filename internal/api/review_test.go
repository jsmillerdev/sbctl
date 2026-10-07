package api

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"github.com/jsmillerdev/supavise/internal/members"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// The dashboard session of anyone who signs in to supavise-gotrue@system is not enough:
// the user needs the supavise_admin claim or a place on the allowlist.
func TestAuthAdminGate(t *testing.T) {
	f := newFixture(t)
	f.cfg.API.AdminEmails = " Ops@Example.test , other@example.test"
	srv, err := NewServer(Deps{Registry: f.reg, Secrets: f.mgr.sec, Manager: f.mgr, Config: f.cfg, PGMetaURL: f.meta.URL})
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	plain := func(extra map[string]any) string {
		c := map[string]any{"sub": f.userID, "role": "authenticated", "app_metadata": map[string]any{}}
		for k, v := range extra {
			c[k] = v
		}
		return f.signJWT(c)
	}
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"claim", f.jwt, 200},
		{"signed up by themselves", plain(nil), 403},
		{"claim false", plain(map[string]any{"app_metadata": map[string]any{AdminClaim: false}}), 403},
		{"claim in user_metadata does not count", plain(map[string]any{"user_metadata": map[string]any{AdminClaim: true}}), 403},
		{"claim is not a string", plain(map[string]any{"app_metadata": map[string]any{AdminClaim: "true"}}), 403},
		{"allowlisted email", plain(map[string]any{"email": "ops@example.TEST"}), 200},
		{"other email", plain(map[string]any{"email": "intruder@example.test"}), 403},
	} {
		for _, path := range []string{"/platform/profile", "/v1/projects"} {
			if rec := f.doAs(tc.token, "GET", path, nil); rec.Code != tc.want {
				t.Errorf("%s on %s: %d, want %d: %s", tc.name, path, rec.Code, tc.want, rec.Body)
			}
		}
	}
	// A refused user is not recorded.
	if _, err := f.srv.store.GetUser(context.Background(), "99999999-2222-4333-8444-555555555555"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unexpected user: %v", err)
	}
}

// A signature failure re-reads the system JWT secret (a rotation), but at most once
// per interval, and tokens that are not even JWTs never do.
func TestJWTSecretRefreshIsThrottled(t *testing.T) {
	f := newFixture(t)
	f.do("GET", "/platform/profile", nil) // loads and caches the secret
	f.mgr.mu.Lock()
	base := f.mgr.keyCalls[systemRef()]
	f.mgr.mu.Unlock()
	calls := func() int {
		f.mgr.mu.Lock()
		defer f.mgr.mu.Unlock()
		return f.mgr.keyCalls[systemRef()] - base
	}
	for i := 0; i < 20; i++ {
		if rec := f.doAs("not-a-jwt", "GET", "/platform/profile", nil); rec.Code != 401 {
			t.Fatalf("garbage: %d", rec.Code)
		}
	}
	if n := calls(); n != 0 {
		t.Fatalf("malformed tokens caused %d secret reads", n)
	}
	wrong := f.signJWTWith("some-other-secret", "authenticated")
	for i := 0; i < 20; i++ {
		if rec := f.doAs(wrong, "GET", "/platform/profile", nil); rec.Code != 401 {
			t.Fatalf("wrong secret: %d", rec.Code)
		}
	}
	if n := calls(); n != 1 {
		t.Fatalf("20 wrongly signed tokens caused %d secret reads, want 1", n)
	}
	// A rotated secret is picked up once the throttle allows a refresh.
	f.srv.auth.mu.Lock()
	f.srv.auth.refreshAt = time.Time{}
	f.srv.auth.mu.Unlock()
	f.mgr.mu.Lock()
	f.mgr.keys[systemRef()].JWTSecret = "rotated-secret-rotated-secret-rotated"
	f.mgr.mu.Unlock()
	f.system, _ = f.mgr.Keys(context.Background(), systemRef())
	if rec := f.do("GET", "/platform/profile", nil); rec.Code != 200 {
		t.Fatalf("after rotation: %d %s", rec.Code, rec.Body)
	}
}

func systemRef() string { return "system" }

func TestTrimStatementEnd(t *testing.T) {
	for in, want := range map[string]string{
		"select 1":                         "select 1",
		"select 1;":                        "select 1",
		"select 1 ;  \n":                   "select 1",
		"select 1;;":                       "select 1",
		"select 1; -- done":                "select 1",
		"select 1 -- c;":                   "select 1",
		"select 1 /* a; */ ;":              "select 1",
		"select 1 /* a /* nested */ b */;": "select 1",
		"select ';'":                       "select ';'",
		"select ';';":                      "select ';'",
		"select '--';":                     "select '--'",
		"select 'it''s;';":                 "select 'it''s;'",
		`select E'a\';b';`:                 `select E'a\';b'`,
		`select "a;b";`:                    `select "a;b"`,
		"select $$ a; $$;":                 "select $$ a; $$",
		"select $q$ ; $$ ; $q$ ;":          "select $q$ ; $$ ; $q$",
		"select $1;":                       "select $1",
		"select $1, $2 ;":                  "select $1, $2",
		"select 'unterminated;":            "select 'unterminated;",
		"":                                 "",
		";":                                "",
	} {
		if got := trimStatementEnd(in); got != want {
			t.Errorf("trimStatementEnd(%q) = %q, want %q", in, got, want)
		}
	}
}

var scramRe = regexp.MustCompile(`^SCRAM-SHA-256\$4096:[A-Za-z0-9+/]{22}==\$[A-Za-z0-9+/]{43}=:[A-Za-z0-9+/]{43}=$`)

func TestScramVerifier(t *testing.T) {
	a, err := scramVerifier("pw")
	if err != nil || !scramRe.MatchString(a) {
		t.Fatalf("verifier %q: %v", a, err)
	}
	if b, _ := scramVerifier("pw"); a == b {
		t.Fatal("two verifiers share a salt")
	}
	// Same salt and iterations give the same verifier, a different password does not.
	salt := []byte("0123456789abcdef")
	x, _ := scramVerifierWithSalt("pw", salt, 4096)
	y, _ := scramVerifierWithSalt("pw", salt, 4096)
	z, _ := scramVerifierWithSalt("pw2", salt, 4096)
	if x != y || x == z {
		t.Fatalf("verifier is not a function of password and salt: %s %s %s", x, y, z)
	}
}

// Read-only SQL connects as the restricted role, not as postgres.
func TestReadOnlyUsesRestrictedRole(t *testing.T) {
	f := newFixture(t)
	f.mgr.dsn = "postgres://postgres:s3cret@127.0.0.1:20000/postgres?sslmode=disable"
	for _, path := range []string{"/database/query", "/database/query/read-only"} {
		body := map[string]any{"query": "select 1", "read_only": true}
		if rec := f.do("POST", "/v1/projects/"+testRef+path, body); rec.Code != 201 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		dsn := f.meta.last().DSN
		if !strings.Contains(dsn, "//"+roleReadOnly+":") || strings.Contains(dsn, "s3cret") || !strings.Contains(dsn, "default_transaction_read_only") {
			t.Fatalf("%s: read-only DSN: %s", path, dsn)
		}
	}
	// The role is created by a supabase_admin connection, with a SCRAM verifier
	// instead of a cleartext password.
	f.meta.mu.Lock()
	var ensure *pgmetaRequest
	for i := range f.meta.Requests {
		if strings.Contains(f.meta.Requests[i].Body, "create role") {
			ensure = &f.meta.Requests[i]
			break
		}
	}
	f.meta.mu.Unlock()
	if ensure == nil || !strings.Contains(ensure.Body, "SCRAM-SHA-256$") || !strings.Contains(ensure.Body, "pg_read_all_data") {
		t.Fatalf("role setup request: %+v", ensure)
	}
	// BYPASSRLS: pg_read_all_data alone reads RLS tables as empty.
	if !strings.Contains(ensure.Body, "bypassrls") || strings.Contains(strings.ToLower(ensure.Body), "nobypassrls") {
		t.Fatalf("the read-only role must be BYPASSRLS: %s", ensure.Body)
	}
	k, _ := f.mgr.Keys(context.Background(), testRef)
	pw, _ := f.srv.readOnlyPassword(context.Background(), testRef)
	if strings.Contains(ensure.Body, pw) || strings.Contains(ensure.DSN, k.AdminPassword) || pw == k.AdminPassword {
		t.Fatalf("a password leaked into the setup statement")
	}
	// Writes are not read only: the normal route still connects as postgres.
	if rec := f.do("POST", "/v1/projects/"+testRef+"/database/query", map[string]any{"query": "select 1"}); rec.Code != 201 {
		t.Fatal(rec.Code)
	}
	if dsn := f.meta.last().DSN; dsn != f.mgr.dsn {
		t.Fatalf("read-write DSN: %s", dsn)
	}
}

func TestMigrationVersionIsPickedInSQL(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 2; i++ {
		if rec := f.do("POST", "/v1/projects/"+testRef+"/database/migrations", map[string]any{"query": "create table t (id int)", "name": "n"}); rec.Code != 200 {
			t.Fatalf("migration %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	body := f.meta.last().Body
	for _, want := range []string{"pg_advisory_xact_lock", "greatest(", "max(", "+ 1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("migration batch lacks %q: %s", want, body)
		}
	}
}

func TestCreateTimeoutAnswersComingUp(t *testing.T) {
	f := newFixture(t)
	f.srv.createWait = 50 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	f.mgr.createFn = func(req lifecycle.CreateRequest) (*registry.Project, error) { <-release; return nil, nil }
	for _, path := range []string{"/v1/projects", "/platform/projects"} {
		rec := f.do("POST", path, map[string]any{"name": "slow"})
		if rec.Code != 201 || jsonField(t, rec, "status") != "COMING_UP" || len(jsonField(t, rec, "ref").(string)) != 20 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
}

func TestContentAuthorization(t *testing.T) {
	f := newFixture(t)
	other := f.signJWT(map[string]any{"sub": "22222222-2222-4333-8444-555555555555", "email": "other@example.test", "role": "authenticated"})
	f.addMember("22222222-2222-4333-8444-555555555555", members.RoleDeveloper)
	base := "/platform/projects/" + testRef + "/content"
	id := "5b3b3d2a-8a51-4f6e-8c7e-0b2f4f6a1a11"
	if rec := f.do("PUT", base, map[string]any{"id": id, "name": "mine", "type": "sql", "visibility": "user", "content": map[string]any{"sql": "select 1"}}); rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	// Another user cannot delete a private item (nor read or replace it).
	if rec := f.doAs(other, "DELETE", base+"?ids="+id, nil); rec.Code != 200 || strings.Contains(rec.Body.String(), id) {
		t.Fatalf("delete by another user: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", base+"/item/"+id, nil); rec.Code != 200 {
		t.Fatalf("the private item is gone: %d", rec.Code)
	}
	if rec := f.doAs(other, "PUT", base, map[string]any{"id": id, "name": "taken"}); rec.Code != 404 {
		t.Fatalf("replace by another user: %d", rec.Code)
	}
	// An id that lives in another project is a 404, not a 500.
	f.mgr.addProject(t, "zzzzzzzzzzzzzzzzzzzz", "Second", f.org.ID, registry.StatusActiveHealthy)
	if rec := f.do("PUT", "/platform/projects/zzzzzzzzzzzzzzzzzzzz/content", map[string]any{"id": id, "name": "x", "type": "sql"}); rec.Code != 404 {
		t.Fatalf("id of another project: %d %s", rec.Code, rec.Body)
	}
	// A folder of another project is refused.
	if rec := f.do("PUT", base, map[string]any{"name": "x", "folder_id": "6c3b3d2a-8a51-4f6e-8c7e-0b2f4f6a1a11"}); rec.Code != 400 {
		t.Fatalf("unknown folder: %d %s", rec.Code, rec.Body)
	}
	// The owner can delete.
	if rec := f.do("DELETE", base+"?ids="+id, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("delete by owner: %d %s", rec.Code, rec.Body)
	}
}

func TestEnsurePGMetaCryptoKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	k1, err := EnsurePGMetaCryptoKey(ctx, f.reg, f.mgr.sec)
	if err != nil || len(k1) < 16 {
		t.Fatalf("key %q: %v", k1, err)
	}
	if k2, err := EnsurePGMetaCryptoKey(ctx, f.reg, f.mgr.sec); err != nil || k2 != k1 {
		t.Fatalf("second call: %q %v", k2, err)
	}
	// A registry that cannot store the key is an error, never an ephemeral key.
	f.cfg.API.PGMetaCryptoKey = ""
	srv, _ := NewServer(Deps{Registry: failingPut{registry.NewMemory()}, Secrets: f.mgr.sec, Manager: f.mgr, Config: f.cfg, PGMetaURL: f.meta.URL})
	if k, err := srv.pgmetaKey(ctx); err == nil || k != "" {
		t.Fatalf("expected an error, got key %q", k)
	}
}

type failingPut struct{ registry.Registry }

func (failingPut) PutSecret(context.Context, string, string, []byte) error {
	return errors.New("registry is read only")
}

func TestProxyPathValuesAreEscaped(t *testing.T) {
	f := newFixture(t)
	var paths []string
	up := newRecordingUpstream(t, &paths)
	f.srv.upstreamOverride = func(*registry.Project, string) string { return up }
	for _, id := range []string{"..", "%2e%2e", "a%2Fb", "a%2F..%2F..%2Fx"} {
		f.do("DELETE", "/platform/auth/"+testRef+"/users/"+id, nil)
	}
	for _, p := range paths {
		if strings.Contains(p, "..") || strings.Count(p, "/") != 3 { // /admin/users/<id>
			t.Fatalf("an id escaped its path segment: %q", p)
		}
	}
	// A bucket id with a space is escaped, and a sign path keeps its slashes.
	paths = nil
	f.do("POST", "/platform/storage/"+testRef+"/buckets/my%20bucket/objects/sign", map[string]any{"path": "dir one/../x.txt", "expiresIn": 60})
	f.do("POST", "/platform/storage/"+testRef+"/buckets/b1/objects/sign", map[string]any{"path": "dir one/x y.txt", "expiresIn": 60})
	if len(paths) != 1 || paths[0] != "/object/sign/b1/dir%20one/x%20y.txt" {
		t.Fatalf("storage paths: %q", paths)
	}
}

func TestAPIKeysTemplateAndReveal(t *testing.T) {
	f := newFixture(t)
	for _, v := range []string{"true", "1", "yes", "on", "y", "enabled", "TRUE"} {
		rec := f.do("GET", "/v1/projects/"+testRef+"/api-keys?reveal="+v, nil)
		if !strings.Contains(rec.Body.String(), "sb_secret_") || strings.Contains(rec.Body.String(), "••") {
			t.Fatalf("reveal=%s did not reveal: %s", v, rec.Body)
		}
	}
	for _, v := range []string{"", "false", "0", "no", "maybe"} {
		rec := f.do("GET", "/v1/projects/"+testRef+"/api-keys?reveal="+v, nil)
		if strings.Contains(rec.Body.String(), "sb_secret_") && !strings.Contains(rec.Body.String(), "••") {
			t.Fatalf("reveal=%q revealed", v)
		}
	}
	rec := f.do("GET", "/v1/projects/"+testRef+"/api-keys", nil)
	if jsonField(t, rec, "3.secret_jwt_template.role") != "service_role" {
		t.Fatalf("secret key template: %s", rec.Body)
	}
}

// A login session that expires, is replaced or is burned takes its token with it.
func TestDeviceLoginDoesNotOrphanTokens(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	count := func() int {
		ts, err := f.reg.ListAccessTokens(ctx, f.userID)
		if err != nil {
			t.Fatal(err)
		}
		return len(ts)
	}
	authorize := func(session string) {
		t.Helper()
		cli, _ := newP256()
		rec := f.do("POST", "/platform/cli/login", map[string]any{"session_id": session, "public_key": cli})
		if rec.Code != 201 {
			t.Fatalf("authorize: %d %s", rec.Code, rec.Body)
		}
	}
	s1, s2 := "9d3b3d2a-8a51-4f6e-8c7e-0b2f4f6a1a11", "7e1b3d2a-8a51-4f6e-8c7e-0b2f4f6a1a22"
	authorize(s1)
	authorize(s1) // replaces the session: the first token must go
	if n := count(); n != 1 {
		t.Fatalf("replaced session left %d tokens", n)
	}
	// Expire it; the next authorization reaps it and its token.
	sess, _ := f.srv.store.GetLoginSession(ctx, s1)
	sess.ExpiresAt = time.Now().Add(-time.Minute)
	if err := f.srv.store.PutLoginSession(ctx, *sess); err != nil {
		t.Fatal(err)
	}
	authorize(s2)
	if n := count(); n != 1 {
		t.Fatalf("expired session left %d tokens, want only the live session's", n)
	}
	// Claiming an expired session fails and removes its token.
	sess, _ = f.srv.store.GetLoginSession(ctx, s2)
	sess.ExpiresAt = time.Now().Add(-time.Minute)
	_ = f.srv.store.PutLoginSession(ctx, *sess)
	rec := f.doAs("", "GET", "/platform/cli/login/"+s2+"?device_code="+sess.Nonce[:8], nil)
	if rec.Code != http.StatusNotFound || count() != 0 {
		t.Fatalf("expired claim: %d, %d tokens left", rec.Code, count())
	}
}

// newP256 returns the hex public key of a fresh P-256 key, like the CLI sends.
func newP256() (string, error) {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(k.PublicKey().Bytes()), nil
}

// newRecordingUpstream is an HTTP server that records the escaped path of every
// request into *paths and answers {"ok":true}.
func newRecordingUpstream(t *testing.T, paths *[]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestMigrationRetryRacedByAnotherRequest(t *testing.T) {
	f := newFixture(t)
	// The pre-check misses; the batch then fails (the other request won the advisory
	// lock and ran first); the key is recorded by now, so this retry is a success.
	f.meta.Rules = []pgmetaRule{
		{Contains: "idempotency_key = ", Skip: 1, Status: 200, Body: `[{"hit":true}]`},
		{Contains: "pg_advisory_xact_lock", Status: 400, Body: `{"error":"duplicate key value violates unique constraint"}`},
	}
	rec := f.do("POST", "/v1/projects/"+testRef+"/database/migrations", map[string]any{"query": "create table t (id int)"}, "Idempotency-Key", "k1")
	if rec.Code != 200 {
		t.Fatalf("raced retry: %d %s", rec.Code, rec.Body)
	}
	// Without a recorded key the failure stays a failure.
	f.meta.Rules = []pgmetaRule{{Contains: "pg_advisory_xact_lock", Status: 400, Body: `{"error":"boom"}`}}
	if rec := f.do("POST", "/v1/projects/"+testRef+"/database/migrations", map[string]any{"query": "select 1"}, "Idempotency-Key", "k2"); rec.Code != 400 {
		t.Fatalf("real failure: %d %s", rec.Code, rec.Body)
	}
}

func TestContentFolderParentChecked(t *testing.T) {
	f := newFixture(t)
	base := "/platform/projects/" + testRef + "/content/folders"
	missing := "00000000-0000-4000-8000-000000000000"
	if rec := f.do("POST", base, map[string]any{"name": "x", "parent_id": missing}); rec.Code != 400 {
		t.Fatalf("unknown parent: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", base, map[string]any{"name": "x", "parent_id": "nope"}); rec.Code != 400 {
		t.Fatalf("malformed parent: %d %s", rec.Code, rec.Body)
	}
	rec := f.do("POST", base, map[string]any{"name": "root"})
	if rec.Code != 201 {
		t.Fatalf("root folder: %d %s", rec.Code, rec.Body)
	}
	parent := jsonField(t, rec, "id").(string)
	if rec := f.do("POST", base, map[string]any{"name": "child", "parent_id": parent}); rec.Code != 201 {
		t.Fatalf("child folder: %d %s", rec.Code, rec.Body)
	}
	// A folder of another project is not a parent.
	f.mgr.addProject(t, "zzzzzzzzzzzzzzzzzzzz", "Second", f.org.ID, registry.StatusActiveHealthy)
	if rec := f.do("POST", "/platform/projects/zzzzzzzzzzzzzzzzzzzz/content/folders", map[string]any{"name": "x", "parent_id": parent}); rec.Code != 400 {
		t.Fatalf("parent in another project: %d %s", rec.Code, rec.Body)
	}
}

func TestCreateReadsDBRegion(t *testing.T) {
	f := newFixture(t)
	if rec := f.do("POST", "/platform/projects", map[string]any{"name": "r", "db_region": "us-east-1", "cloud_provider": "AWS", "organization_slug": f.org.Slug, "db_pass": "x"}); rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	if got := f.mgr.created[len(f.mgr.created)-1].Region; got != "us-east-1" {
		t.Fatalf("region = %q", got)
	}
}

func TestCreateFailureAfterTimeoutIsRecorded(t *testing.T) {
	f := newFixture(t)
	f.srv.createWait = 50 * time.Millisecond
	release := make(chan struct{})
	f.mgr.createFn = func(req lifecycle.CreateRequest) (*registry.Project, error) {
		<-release
		return nil, errors.New("boom")
	}
	rec := f.do("POST", "/v1/projects", map[string]any{"name": "doomed"})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	ref := jsonField(t, rec, "id").(string)
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if p, err := f.reg.GetProject(context.Background(), ref); err == nil {
			if p.Status != registry.StatusInitFailed {
				t.Fatalf("status = %s", p.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no INIT_FAILED row for the ref the client holds")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec := f.do("GET", "/v1/projects/"+ref, nil); rec.Code != 200 || jsonField(t, rec, "status") != "INIT_FAILED" {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
}

func TestReadOnlyLoginRoleBypassesRLS(t *testing.T) {
	f := newFixture(t)
	for _, ro := range []bool{true, false} {
		if rec := f.do("POST", "/v1/projects/"+testRef+"/cli/login-role", map[string]any{"read_only": ro}); rec.Code != 201 {
			t.Fatalf("login role: %d %s", rec.Code, rec.Body)
		}
		body := f.meta.last().Body
		if got := strings.Contains(body, "bypassrls in role pg_read_all_data"); got != ro {
			t.Fatalf("read_only=%v: bypassrls on the read-only role = %v: %s", ro, got, body)
		}
	}
}
