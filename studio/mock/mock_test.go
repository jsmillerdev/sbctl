package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testJWTSecret = "test-jwt-secret-0123456789-0123456789-abcdef"
	testCryptoKey = "test-key-123"
	refA          = "mockprojectalphaaaaa"
	refB          = "mockprojectbetabbbbb"
	studioOrigin  = "http://127.0.0.1:3000"
	authApprove   = "11111111-1111-4111-8111-111111111111"
	authDecline   = "22222222-2222-4222-8222-222222222222"
)

func testConfig(t *testing.T, pgmeta string) *Config {
	t.Helper()
	c := &Config{
		JWTSecret: testJWTSecret, StudioOrigins: []string{studioOrigin}, PgmetaURL: pgmeta, PgmetaCryptoKey: testCryptoKey,
		Scheme: "http", ProjectHost: "localhost", RequestLog: filepath.Join(t.TempDir(), "log.jsonl"),
		BuiltinAuth: &BuiltinAuth{Email: "admin@example.test", Password: "pw-for-tests"},
		Projects: []Project{
			{Ref: refA, Name: "Alpha", DBURL: "postgresql://postgres:p%40ss@127.0.0.1:5433/proj_a?sslmode=disable"},
			{Ref: refB, Name: "Beta", DBURL: "postgresql://postgres:p%40ss@127.0.0.1:5433/proj_b?sslmode=disable"},
		},
		Authorizations: []Authorization{
			{ID: authApprove, Name: "Test MCP Client", RedirectURI: "http://127.0.0.1:41234/callback?keep=1"},
			{ID: authDecline, Name: "Other Client", RedirectURI: "https://client.example.test/cb", Scopes: []string{"projects:read"}},
		},
	}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	return c
}

func newTestServer(t *testing.T, pgmeta string) (*server, *httptest.Server, string) {
	t.Helper()
	cfg := testConfig(t, pgmeta)
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(func() { ts.Close(); s.close() })
	return s, ts, signIn(t, ts.URL)
}

func signIn(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Post(base+"/auth/v1/token?grant_type=password", "application/json",
		strings.NewReader(`{"email":"admin@example.test","password":"pw-for-tests"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&out) != nil || out.AccessToken == "" {
		t.Fatalf("sign-in failed: %d", resp.StatusCode)
	}
	return out.AccessToken
}

func do(t *testing.T, method, url, token string, body string, hdr ...string) (int, []byte, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func TestBuiltinAuthSignInRefreshAndUser(t *testing.T) {
	_, ts, tok := newTestServer(t, "")
	if code, _, _ := do(t, "POST", ts.URL+"/auth/v1/token?grant_type=password", "", `{"email":"admin@example.test","password":"nope"}`); code != 400 {
		t.Fatalf("bad password: %d", code)
	}
	code, body, _ := do(t, "GET", ts.URL+"/auth/v1/user", tok, "")
	if code != 200 || !strings.Contains(string(body), "admin@example.test") {
		t.Fatalf("user: %d %s", code, body)
	}
	// refresh token round trip
	_, b, _ := do(t, "POST", ts.URL+"/auth/v1/token?grant_type=password", "", `{"email":"admin@example.test","password":"pw-for-tests"}`)
	var sess struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.Unmarshal(b, &sess)
	if code, _, _ := do(t, "POST", ts.URL+"/auth/v1/token?grant_type=refresh_token", "", `{"refresh_token":"`+sess.RefreshToken+`"}`); code != 200 {
		t.Fatalf("refresh: %d", code)
	}
	if code, _, _ := do(t, "POST", ts.URL+"/auth/v1/token?grant_type=refresh_token", "", `{"refresh_token":"`+sess.RefreshToken+`"}`); code != 400 {
		t.Fatalf("reused refresh token must fail: %d", code)
	}
}

func TestAPIRequiresAValidDashboardToken(t *testing.T) {
	_, ts, tok := newTestServer(t, "")
	if code, _, _ := do(t, "GET", ts.URL+"/platform/profile", "", ""); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _, _ := do(t, "GET", ts.URL+"/platform/profile", "garbage", ""); code != 401 {
		t.Fatalf("garbage token: %d", code)
	}
	other := &authState{secret: "another-secret-another-secret-another-secret"}
	forged, _ := other.accessToken(&user{ID: "x", Email: "x@example.test"}, time.Now())
	if code, _, _ := do(t, "GET", ts.URL+"/platform/profile", forged, ""); code != 401 {
		t.Fatalf("token signed with another secret: %d", code)
	}
	code, body, _ := do(t, "GET", ts.URL+"/platform/profile", tok, "")
	if code != 200 || !strings.Contains(string(body), `"primary_email":"admin@example.test"`) {
		t.Fatalf("profile: %d %s", code, body)
	}
	// A GoTrue token from `admin createuser` has an empty role claim and still is a dashboard session.
	emptyRole, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u1", "aud": "authenticated", "role": "", "email": "u@example.test", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(testJWTSecret))
	if code, _, _ := do(t, "GET", ts.URL+"/platform/profile", emptyRole, ""); code != 200 {
		t.Fatalf("empty role claim: %d", code)
	}
	// A project API key is not a dashboard session.
	apiKey, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "supabase", "ref": refA, "role": "service_role", "sub": "x", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(testJWTSecret))
	if code, _, _ := do(t, "GET", ts.URL+"/platform/profile", apiKey, ""); code != 401 {
		t.Fatalf("service key accepted as dashboard session: %d", code)
	}
}

// Every hand-written handler must return the required fields the OpenAPI spec documents.
func TestRealHandlersCarryTheDocumentedRequiredFields(t *testing.T) {
	s, ts, tok := newTestServer(t, "")
	checked := 0
	for _, rt := range s.routes {
		if !rt.real || rt.shape == nil || rt.method != "GET" {
			continue
		}
		path := rt.template
		path = strings.ReplaceAll(path, "{ref}", refA)
		path = strings.ReplaceAll(path, "{slug}", orgSlug)
		path = strings.ReplaceAll(path, "{id}", authApprove)
		code, body, _ := do(t, "GET", ts.URL+path, tok, "")
		if code != rt.shape.Status {
			t.Errorf("%s: status %d, spec says %d", rt.template, code, rt.shape.Status)
			continue
		}
		var obj map[string]any
		switch rt.shape.Kind {
		case "array":
			var arr []map[string]any
			if err := json.Unmarshal(body, &arr); err != nil {
				t.Errorf("%s: not an array: %s", rt.template, body)
				continue
			}
			if len(arr) == 0 {
				continue
			}
			obj = arr[0]
		case "object":
			if err := json.Unmarshal(body, &obj); err != nil {
				t.Errorf("%s: not an object: %s", rt.template, body)
				continue
			}
		default:
			continue
		}
		for _, f := range rt.shape.Required {
			if _, ok := obj[f.Name]; !ok {
				t.Errorf("%s: missing required field %q", rt.template, f.Name)
			}
		}
		checked++
	}
	if checked < 10 {
		t.Fatalf("only %d handlers were checked", checked)
	}
}

// Studio's ServiceStatus only treats ACTIVE_HEALTHY as healthy (the V1ServiceHealthResponse enum is
// COMING_UP | ACTIVE_HEALTHY | UNHEALTHY); anything else makes it re-poll every 5 s.
func TestProjectHealthReportsActiveHealthy(t *testing.T) {
	_, ts, tok := newTestServer(t, "")
	code, body, _ := do(t, "GET", ts.URL+"/v1/projects/"+refA+"/health", tok, "")
	var out []struct {
		Name    string `json:"name"`
		Healthy bool   `json:"healthy"`
		Status  string `json:"status"`
	}
	if code != 200 || json.Unmarshal(body, &out) != nil || len(out) == 0 {
		t.Fatalf("health: %d %s", code, body)
	}
	for _, e := range out {
		if !e.Healthy || e.Status != "ACTIVE_HEALTHY" {
			t.Errorf("service %s: healthy=%v status=%q", e.Name, e.Healthy, e.Status)
		}
	}
}

func TestProjectsListsAndPagination(t *testing.T) {
	_, ts, tok := newTestServer(t, "")
	_, body, _ := do(t, "GET", ts.URL+"/platform/organizations/mock-org/projects?limit=1&offset=1&sort=name_asc", tok, "")
	var out struct {
		Projects   []map[string]any `json:"projects"`
		Pagination map[string]int   `json:"pagination"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Projects) != 1 || out.Projects[0]["ref"] != refB || out.Pagination["count"] != 2 {
		t.Fatalf("page 2: %s", body)
	}
	_, body, _ = do(t, "GET", ts.URL+"/platform/projects?search=alp", tok, "")
	if !strings.Contains(string(body), refA) || strings.Contains(string(body), refB) {
		t.Fatalf("search: %s", body)
	}
	if code, _, _ := do(t, "GET", ts.URL+"/platform/projects/nosuchprojectxxxxxxx", tok, ""); code != 404 {
		t.Fatalf("unknown project: %d", code)
	}
	if code, _, _ := do(t, "GET", ts.URL+"/platform/organizations/other-org", tok, ""); code != 404 {
		t.Fatalf("unknown org: %d", code)
	}
}

func TestProjectDetailConnectionStringCarriesNoSecret(t *testing.T) {
	s, ts, tok := newTestServer(t, "")
	_, body, _ := do(t, "GET", ts.URL+"/platform/projects/"+refB, tok, "")
	var out struct {
		ConnectionString string `json:"connectionString"`
		Status           string `json:"status"`
		Ref              string `json:"ref"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.ConnectionString == "" || out.Status != "ACTIVE_HEALTHY" || out.Ref != refB {
		t.Fatalf("project detail: %s", body)
	}
	// Neither the password nor an encryption of the DB URL may reach a dashboard user.
	if _, err := decryptConnString(out.ConnectionString, testCryptoKey); err == nil {
		t.Fatalf("connectionString decrypts to a database URL: %q", out.ConnectionString)
	}
	if strings.Contains(string(body), "p%40ss") || strings.Contains(string(body), "postgresql://") {
		t.Fatalf("database URL in the response: %s", body)
	}
	// The encrypted connection that postgres-meta needs is built per request, from the project.
	if got, err := decryptConnString(s.encryptedConnection(&s.cfg.Projects[1]), testCryptoKey); err != nil || got != s.cfg.Projects[1].DBURL {
		t.Fatalf("encryptedConnection: %q %v", got, err)
	}
}

func TestCryptoJSCompatibility(t *testing.T) {
	// Produced by crypto-js 4 in the postgres-meta artifact:
	// CryptoJS.AES.encrypt(url, "test-key-123").toString()
	const url = "postgresql://postgres:p%40ss@127.0.0.1:5433/proj_a?sslmode=disable"
	const fromCryptoJS = "U2FsdGVkX197pOZcPrfNOMaqnkko3x0+/vdRHmxNg+KE2jepyXHj1r4RXvqlwiA06ImujApBobyIdpT82a5/8JRBQ6jaxXiXuwz8ap1YRcU0Y97koGybvnQXPHD2WcOa"
	got, err := decryptConnString(fromCryptoJS, "test-key-123")
	if err != nil || got != url {
		t.Fatalf("decrypt crypto-js output: %q %v", got, err)
	}
	enc, err := encryptConnString(url, "test-key-123")
	if err != nil {
		t.Fatal(err)
	}
	if back, err := decryptConnString(enc, "test-key-123"); err != nil || back != url {
		t.Fatalf("round trip: %q %v", back, err)
	}
	if _, err := decryptConnString(enc, "wrong-key"); err == nil {
		// A wrong key can by chance give valid padding; the result must at least differ.
		if back, _ := decryptConnString(enc, "wrong-key"); back == url {
			t.Fatal("wrong key decrypted")
		}
	}
}

func TestPgMetaProxyBuildsTheConnectionHeaderAndPassesStatusAndBody(t *testing.T) {
	var gotQuery, gotConn, gotApp, gotPath string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath = r.URL.Path
		gotConn, _ = decryptConnString(r.Header.Get("X-Connection-Encrypted"), testCryptoKey)
		gotApp = r.Header.Get("X-Pg-Application-Name")
		var in struct{ Query string }
		_ = json.Unmarshal(b, &in)
		gotQuery = in.Query
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(in.Query, "boom") {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"syntax error","formattedError":"LINE 1: boom"}`))
			return
		}
		_, _ = w.Write([]byte(`[{"x":1}]`))
	}))
	defer fake.Close()
	s, ts, tok := newTestServer(t, fake.URL)

	code, body, _ := do(t, "POST", ts.URL+"/platform/pg-meta/"+refB+"/query?key=entity-types", tok,
		`{"query":"select 1 as x","disable_statement_timeout":false}`,
		"X-Connection-Encrypted", "opaque-from-studio", "X-Pg-Application-Name", "supabase/dashboard", "Content-Type", "application/json")
	if code != 200 || string(body) != `[{"x":1}]` {
		t.Fatalf("query: %d %s", code, body)
	}
	if gotPath != "/query" || gotQuery != "select 1 as x" || gotApp != "supabase/dashboard" || gotConn != s.cfg.Projects[1].DBURL {
		t.Fatalf("pg-meta saw path=%q query=%q app=%q conn=%q", gotPath, gotQuery, gotApp, gotConn)
	}
	code, body, _ = do(t, "POST", ts.URL+"/platform/pg-meta/"+refA+"/query", tok, `{"query":"boom"}`, "X-Connection-Encrypted", "x")
	if code != 400 || !strings.Contains(string(body), "formattedError") {
		t.Fatalf("error passthrough: %d %s", code, body)
	}
	if code, _, _ = do(t, "POST", ts.URL+"/platform/pg-meta/"+refA+"/query", tok, `{"query":"select 1"}`); code != 400 {
		t.Fatalf("without x-connection-encrypted: %d", code)
	}
	if code, _, _ = do(t, "POST", ts.URL+"/platform/pg-meta/nosuchprojectxxxxxxx/query", tok, `{"query":"select 1"}`, "X-Connection-Encrypted", "x"); code != 404 {
		t.Fatalf("unknown project: %d", code)
	}
}

// The MCP tools run SQL through the Management API, not through the dashboard's pg-meta route.
func TestManagementQueryRunsThroughPgMeta(t *testing.T) {
	var gotQuery, gotConn string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotConn, _ = decryptConnString(r.Header.Get("X-Connection-Encrypted"), testCryptoKey)
		var in struct{ Query string }
		_ = json.Unmarshal(b, &in)
		gotQuery = in.Query
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"db":"proj_b"}]`))
	}))
	defer fake.Close()
	s, ts, tok := newTestServer(t, fake.URL)

	code, body, _ := do(t, "POST", ts.URL+"/v1/projects/"+refB+"/database/query", tok,
		`{"query":"select current_database() as db","parameters":[],"read_only":true}`, "Content-Type", "application/json")
	if code != 200 || string(body) != `[{"db":"proj_b"}]` {
		t.Fatalf("query: %d %s", code, body)
	}
	if gotQuery != "select current_database() as db" || gotConn != s.cfg.Projects[1].DBURL {
		t.Fatalf("pg-meta saw query=%q conn=%q", gotQuery, gotConn)
	}
	if code, _, _ = do(t, "POST", ts.URL+"/v1/projects/nosuchprojectxxxxxxx/database/query", tok, `{"query":"select 1"}`); code != 404 {
		t.Fatalf("unknown project: %d", code)
	}
	if code, _, _ = do(t, "POST", ts.URL+"/v1/projects/"+refB+"/database/query", "", `{"query":"select 1"}`); code != 401 {
		t.Fatalf("without a token: %d", code)
	}
}

func TestCORS(t *testing.T) {
	_, ts, _ := newTestServer(t, "")
	code, _, h := do(t, "OPTIONS", ts.URL+"/platform/profile", "", "", "Origin", studioOrigin,
		"Access-Control-Request-Method", "GET", "Access-Control-Request-Headers", "authorization,x-request-id,version")
	if code != 204 || h.Get("Access-Control-Allow-Origin") != studioOrigin || h.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("preflight: %d %v", code, h)
	}
	for _, want := range []string{"authorization", "x-request-id", "version", "x-connection-encrypted", "x-pg-application-name"} {
		if !strings.Contains(h.Get("Access-Control-Allow-Headers"), want) {
			t.Errorf("Allow-Headers lacks %s", want)
		}
	}
	_, _, h = do(t, "OPTIONS", ts.URL+"/platform/profile", "", "", "Origin", "http://evil.example")
	if h.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("foreign origin allowed")
	}
}

func TestStubsFollowTheDocumentedShape(t *testing.T) {
	_, ts, tok := newTestServer(t, "")
	// documented array -> []
	code, body, _ := do(t, "GET", ts.URL+"/platform/projects/"+refA+"/load-balancers", tok, "")
	if code != 200 || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("array stub: %d %s", code, body)
	}
	// documented object with required fields -> neutral values for them
	code, body, _ = do(t, "GET", ts.URL+"/platform/organizations/mock-org/billing/subscription", tok, "")
	var sub map[string]any
	if code != 200 || json.Unmarshal(body, &sub) != nil || sub["billing_via_partner"] != false || sub["current_period_end"] != float64(0) {
		t.Fatalf("object stub: %d %s", code, body)
	}
	// documented mutation without a body -> documented status
	code, _, _ = do(t, "POST", ts.URL+"/platform/telemetry/event", tok, `{}`)
	if code != 201 {
		t.Fatalf("telemetry event: %d", code)
	}
	// analytics endpoints wrap rows in result
	code, body, _ = do(t, "GET", ts.URL+"/platform/projects/"+refA+"/analytics/endpoints/usage.api-counts?interval=1hr", tok, "")
	if code != 200 || strings.TrimSpace(string(body)) != `{"result":[]}` {
		t.Fatalf("analytics stub: %d %s", code, body)
	}
	// not documented at all: baseline from the supastack mock
	if code, body, _ = do(t, "GET", ts.URL+"/platform/never/heard/of/it", tok, ""); code != 200 || strings.TrimSpace(string(body)) != "{}" {
		t.Fatalf("unknown GET: %d %s", code, body)
	}
	if code, _, _ = do(t, "POST", ts.URL+"/platform/never/heard/of/it", tok, ""); code != 204 {
		t.Fatalf("unknown POST: %d", code)
	}
}

func TestLiteralSegmentsBeatParameters(t *testing.T) {
	s, _, _ := newTestServer(t, "")
	rt, params := s.find("GET", "/platform/organizations/mock-org/members/invitations")
	if rt == nil || rt.template != "/platform/organizations/{slug}/members/invitations" || params["slug"] != "mock-org" {
		t.Fatalf("got %+v", rt)
	}
}

func TestEntitlementsGrantEveryKey(t *testing.T) {
	s, _, _ := newTestServer(t, "")
	if len(s.entitlements()) != len(entitlementKeys) {
		t.Fatal("entitlements are not one per key")
	}
	for _, e := range s.entitlements() {
		if e["hasAccess"] != true {
			t.Fatalf("not granted: %v", e)
		}
	}
}

func TestRequestLogAndSummarize(t *testing.T) {
	s, ts, tok := newTestServer(t, "")
	do(t, "GET", ts.URL+"/platform/profile", tok, "")
	do(t, "GET", ts.URL+"/platform/profile", tok, "")
	do(t, "GET", ts.URL+"/platform/projects/"+refA, tok, "")
	do(t, "GET", ts.URL+"/platform/projects/"+refA+"/load-balancers", tok, "")
	do(t, "GET", ts.URL+"/platform/zzz", tok, "")
	do(t, "OPTIONS", ts.URL+"/platform/profile", "", "", "Origin", studioOrigin)
	s.log.close()

	raw, err := os.ReadFile(s.cfg.RequestLog)
	if err != nil {
		t.Fatal(err)
	}
	var lines []logEntry
	for _, l := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var e logEntry
		if err := json.Unmarshal(l, &e); err != nil {
			t.Fatalf("bad log line %q: %v", l, err)
		}
		lines = append(lines, e)
	}
	if len(lines) < 6 {
		t.Fatalf("%d lines", len(lines))
	}
	var sb strings.Builder
	if err := summarize(s.cfg.RequestLog, &sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"| GET | `/platform/profile` | 2 | 200 x2 | real x2 |",
		"| GET | `/platform/projects/{ref}` | 1 |",
		"| GET | `/platform/projects/{ref}/load-balancers` | 1 | 200 x1 | stub x1 |",
		"| GET | `/platform/zzz` | 1 | 200 x1 | unknown x1 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "preflight") {
		t.Error("preflights should not be summarized")
	}
}

func TestConfigValidation(t *testing.T) {
	good := `{"jwt_secret":"s","projects":[{"ref":"mockprojectalphaaaaa","db_url":"postgresql://x"}]}`
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "c.json")
		_ = os.WriteFile(p, []byte(body), 0o600)
		return p
	}
	if _, err := loadConfig(write(good)); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"no secret":                `{"projects":[{"ref":"mockprojectalphaaaaa"}]}`,
		"no projects":              `{"jwt_secret":"s","projects":[]}`,
		"bad ref":                  `{"jwt_secret":"s","projects":[{"ref":"short"}]}`,
		"duplicate":                `{"jwt_secret":"s","projects":[{"ref":"mockprojectalphaaaaa"},{"ref":"mockprojectalphaaaaa"}]}`,
		"unknown key":              `{"jwt_secret":"s","nope":1,"projects":[{"ref":"mockprojectalphaaaaa"}]}`,
		"short authorization id":   `{"jwt_secret":"s","projects":[{"ref":"mockprojectalphaaaaa"}],"authorizations":[{"id":"abc","name":"n","redirect_uri":"http://127.0.0.1/cb"}]}`,
		"authorization no name":    `{"jwt_secret":"s","projects":[{"ref":"mockprojectalphaaaaa"}],"authorizations":[{"id":"abcdefgh","redirect_uri":"http://127.0.0.1/cb"}]}`,
		"authorization bad scheme": `{"jwt_secret":"s","projects":[{"ref":"mockprojectalphaaaaa"}],"authorizations":[{"id":"abcdefgh","name":"n","redirect_uri":"javascript:alert(1)"}]}`,
		"authorization fragment":   `{"jwt_secret":"s","projects":[{"ref":"mockprojectalphaaaaa"}],"authorizations":[{"id":"abcdefgh","name":"n","redirect_uri":"http://127.0.0.1/cb#x"}]}`,
		"authorization duplicate":  `{"jwt_secret":"s","projects":[{"ref":"mockprojectalphaaaaa"}],"authorizations":[{"id":"abcdefgh","name":"n","redirect_uri":"http://127.0.0.1/cb"},{"id":"abcdefgh","name":"m","redirect_uri":"http://127.0.0.1/cb"}]}`,
	} {
		if _, err := loadConfig(write(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestClientCancelIsNotAServerError(t *testing.T) {
	block := make(chan struct{})
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer fake.Close()
	defer close(block)
	s, ts, tok := newTestServer(t, fake.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/platform/pg-meta/"+refA+"/query", strings.NewReader(`{"query":"select pg_sleep(60)"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Connection-Encrypted", "x")
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected the client to give up")
	}
	// the mock logs the cancelled request as 499, not 502
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(s.cfg.RequestLog)
		if strings.Contains(string(raw), `"status":499`) {
			return
		}
		if strings.Contains(string(raw), `"status":502`) {
			t.Fatalf("cancelled request logged as 502:\n%s", raw)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no 499 in the request log")
}

func TestRedactSQLHidesPasswordLiterals(t *testing.T) {
	for in, want := range map[string]string{
		`alter role app with password 'hunter2'`:     `alter role app with password '***'`,
		`ALTER ROLE app PASSWORD = 'it''s secret'`:   `ALTER ROLE app PASSWORD = '***'`,
		`create role r login password E'x\'y'`:       `create role r login password '***'`,
		`select 1 as password_hint, 'password' as x`: `select 1 as password_hint, 'password' as x`,
	} {
		if got := redactSQL(in); got != want {
			t.Errorf("redactSQL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequestLogFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	if _, err := newRequestLog(path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("request log mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestAuthorizationDescribeApproveAndDecline(t *testing.T) {
	_, ts, tok := newTestServer(t, "")
	describe := func(id string) (int, map[string]any) {
		t.Helper()
		code, body, _ := do(t, "GET", ts.URL+"/platform/oauth/authorizations/"+id, tok, "")
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		return code, out
	}

	code, d := describe(authApprove)
	if code != 200 || d["name"] != "Test MCP Client" || d["domain"] != "127.0.0.1" || d["registration_type"] != "dynamic" ||
		d["approved_at"] != nil || d["website"] != "" || d["icon"] != nil {
		t.Fatalf("describe: %d %v", code, d)
	}
	if scopes, _ := d["scopes"].([]any); len(scopes) == 0 {
		t.Fatalf("a request without scopes gets the default set: %v", d)
	}
	if exp, err := time.Parse(time.RFC3339, d["expires_at"].(string)); err != nil || !exp.After(time.Now()) {
		t.Fatalf("expires_at = %v (%v)", d["expires_at"], err)
	}
	if code, _ = describe("00000000-0000-4000-8000-000000000000"); code != 404 {
		t.Fatalf("unknown id: %d", code)
	}

	// Approve answers with the redirect URL the client would get: its own query kept, plus code,
	// state and the issuer.
	approve := ts.URL + "/platform/organizations/" + orgSlug + "/oauth/authorizations/" + authApprove + "?skip_browser_redirect=true"
	code, body, _ := do(t, "POST", approve, tok, "")
	var res struct {
		URL string `json:"url"`
	}
	if code != 201 || json.Unmarshal(body, &res) != nil {
		t.Fatalf("approve: %d %s", code, body)
	}
	u, err := url.Parse(res.URL)
	if err != nil || u.Host != "127.0.0.1:41234" || u.Path != "/callback" {
		t.Fatalf("approve url = %q", res.URL)
	}
	q := u.Query()
	if q.Get("keep") != "1" || q.Get("code") == "" || q.Get("state") == "" || q.Get("iss") != "http://"+strings.TrimPrefix(ts.URL, "http://") {
		t.Fatalf("approve url query = %v", q)
	}
	// The request now reads as approved, in the organization that approved it, and cannot be decided again.
	code, d = describe(authApprove)
	if code != 200 || d["approved_at"] == nil || d["approved_organization_slug"] != orgSlug {
		t.Fatalf("describe after approve: %d %v", code, d)
	}
	if code, _, _ = do(t, "POST", approve, tok, ""); code != 409 {
		t.Fatalf("second approve: %d", code)
	}
	decline := ts.URL + "/platform/organizations/" + orgSlug + "/oauth/authorizations/" + authApprove
	if code, _, _ = do(t, "DELETE", decline, tok, ""); code != 409 {
		t.Fatalf("decline after approve: %d", code)
	}

	// Decline removes the request.
	decline = ts.URL + "/platform/organizations/" + orgSlug + "/oauth/authorizations/" + authDecline
	code, body, _ = do(t, "DELETE", decline, tok, "")
	if code != 200 || !strings.Contains(string(body), authDecline) {
		t.Fatalf("decline: %d %s", code, body)
	}
	if code, _ = describe(authDecline); code != 404 {
		t.Fatalf("describe after decline: %d", code)
	}
	if code, _, _ = do(t, "DELETE", decline, tok, ""); code != 404 {
		t.Fatalf("second decline: %d", code)
	}
	// Another organization is not this mock's.
	other := ts.URL + "/platform/organizations/other-org/oauth/authorizations/" + authApprove
	if code, _, _ = do(t, "POST", other, tok, ""); code != 404 {
		t.Fatalf("approve in another organization: %d", code)
	}
}

func TestAuthorizationRoutesNeedAToken(t *testing.T) {
	_, ts, _ := newTestServer(t, "")
	if code, _, _ := do(t, "GET", ts.URL+"/platform/oauth/authorizations/"+authApprove, "", ""); code != 401 {
		t.Fatalf("describe without a token: %d", code)
	}
	if code, _, _ := do(t, "POST", ts.URL+"/platform/organizations/"+orgSlug+"/oauth/authorizations/"+authApprove, "", ""); code != 401 {
		t.Fatalf("approve without a token: %d", code)
	}
}

// The page the browser lands on after approving answers without a token, and its query (the
// authorization code) never reaches the request log.
func TestCallbackPageAnswersAndLogsNoQuery(t *testing.T) {
	s, ts, _ := newTestServer(t, "")
	code, body, hdr := do(t, "GET", ts.URL+callbackPath+"?code=secret-code&state=s", "", "")
	if code != 200 || !strings.Contains(string(body), "Authorization received") || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") {
		t.Fatalf("callback: %d %s", code, body)
	}
	s.close()
	b, err := os.ReadFile(s.cfg.RequestLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), callbackPath) || strings.Contains(string(b), "secret-code") {
		t.Fatalf("request log: %s", b)
	}
}
