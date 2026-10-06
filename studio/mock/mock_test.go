package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testJWTSecret = "test-jwt-secret-0123456789-0123456789-abcdef"
	testCryptoKey = "test-key-123"
	refA          = "mockprojectalphaaaaa"
	refB          = "mockprojectbetabbbbb"
	studioOrigin  = "http://127.0.0.1:3000"
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

func TestProjectDetailConnectionStringDecryptsToTheDBURL(t *testing.T) {
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
	got, err := decryptConnString(out.ConnectionString, testCryptoKey)
	if err != nil || got != s.cfg.Projects[1].DBURL || out.Status != "ACTIVE_HEALTHY" || out.Ref != refB {
		t.Fatalf("connectionString: %q %v (%s)", got, err, body)
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
	code, body, _ := do(t, "GET", ts.URL+"/platform/projects/"+refA+"/databases", tok, "")
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
	do(t, "GET", ts.URL+"/platform/projects/"+refA+"/databases", tok, "")
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
		"| GET | `/platform/projects/{ref}/databases` | 1 | 200 x1 | stub x1 |",
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
		"no secret":   `{"projects":[{"ref":"mockprojectalphaaaaa"}]}`,
		"no projects": `{"jwt_secret":"s","projects":[]}`,
		"bad ref":     `{"jwt_secret":"s","projects":[{"ref":"short"}]}`,
		"duplicate":   `{"jwt_secret":"s","projects":[{"ref":"mockprojectalphaaaaa"},{"ref":"mockprojectalphaaaaa"}]}`,
		"unknown key": `{"jwt_secret":"s","nope":1,"projects":[{"ref":"mockprojectalphaaaaa"}]}`,
	} {
		if _, err := loadConfig(write(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
