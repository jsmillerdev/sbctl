package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestRouteTableBuilds(t *testing.T) {
	f := newFixture(t) // NewServer panics-or-errors on mux pattern conflicts
	ops, _ := Operations()
	impl := f.srv.implemented()
	inSpec := 0
	for key := range impl {
		switch {
		case operationByKey(key) != nil:
			inSpec++
		case strings.Contains(key, "/platform/storage/"), key == "GET /platform/auth/{ref}/users":
			// Studio calls these; the platform spec omits them.
		case key == "GET /healthz/detail":
			// supavise's own: the node's health for operators (health.go).
		case strings.Contains(key, "/platform/organizations/{slug}/sso/"):
			// supavise's own routes for several identity providers and the users waiting for approval.
		default:
			t.Errorf("implemented route %q is neither an operation of the pinned specs nor a known extra", key)
		}
	}
	t.Logf("%d operations in the specs: %d implemented by hand, %d stubbed; %d extra routes", len(ops), inSpec, len(ops)-inSpec, len(impl)-inSpec)
}

func TestAuth(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name, token, path string
		want              int
	}{
		{"no credentials", "", "/v1/projects", 401},
		{"garbage", "nope", "/v1/projects", 401},
		{"dashboard jwt on v1", f.jwt, "/v1/projects", 200},
		{"dashboard jwt on platform", f.jwt, "/platform/profile", 200},
		{"wrong secret", f.signJWTWith("other-secret", "authenticated"), "/platform/profile", 401},
		{"anon role", f.signJWT(map[string]any{"sub": f.userID, "role": "anon"}), "/platform/profile", 401},
		// GoTrue's admin createuser leaves auth.users.role empty, so these users' tokens
		// carry role "": the session is identified by audience and signature.
		{"admin-created user, empty role", f.signJWT(map[string]any{"sub": f.userID, "role": ""}), "/platform/profile", 200},
		{"admin-created user, no role claim", f.signJWT(map[string]any{"sub": f.userID, "role": nil}), "/platform/profile", 200},
		{"audience list", f.signJWT(map[string]any{"sub": f.userID, "aud": []string{"authenticated"}}), "/platform/profile", 200},
		{"other audience", f.signJWT(map[string]any{"sub": f.userID, "aud": "service"}), "/platform/profile", 401},
		{"no audience", f.signJWT(map[string]any{"sub": f.userID, "aud": nil}), "/platform/profile", 401},
		{"no exp", f.signJWT(map[string]any{"sub": f.userID, "exp": nil}), "/platform/profile", 401},
		{"expired", f.signJWT(map[string]any{"sub": f.userID, "role": "authenticated", "exp": 1}), "/platform/profile", 401},
		{"project key is not a dashboard session", f.projectJWT(), "/v1/projects", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := f.doAs(tc.token, "GET", tc.path, nil); rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func (f *fixture) signJWTWith(secret, role string) string {
	f.t.Helper()
	k := *f.system
	k.JWTSecret = secret
	saved := f.system
	f.system = &k
	defer func() { f.system = saved }()
	return f.signJWT(map[string]any{"sub": f.userID, "role": role})
}

func (f *fixture) projectJWT() string {
	k, _ := f.mgr.Keys(nil, testRef) //nolint:staticcheck // fake ignores ctx
	saved := f.system
	f.system = k
	defer func() { f.system = saved }()
	return f.signJWT(map[string]any{"sub": f.userID, "role": "authenticated"})
}

func TestPATAuth(t *testing.T) {
	f := newFixture(t)
	rec := f.do("POST", "/platform/profile/access-tokens", map[string]any{"name": "ci"})
	if rec.Code != 201 {
		t.Fatalf("create token: %d %s", rec.Code, rec.Body)
	}
	tok := decodeBody(t, rec).(map[string]any)["token"].(string)
	if !patRe.MatchString(tok) {
		t.Fatalf("token %q does not match the CLI's pattern", tok)
	}
	if rec := f.doAs(tok, "GET", "/v1/projects", nil); rec.Code != 200 {
		t.Fatalf("PAT on /v1: %d %s", rec.Code, rec.Body)
	}
	if rec := f.doAs(tok, "GET", "/platform/profile", nil); rec.Code != 401 {
		t.Fatalf("PAT on /platform must be rejected, got %d", rec.Code)
	}
	if rec := f.doAs("sbp_"+strings.Repeat("0", 40), "GET", "/v1/projects", nil); rec.Code != 401 {
		t.Fatalf("unknown PAT: %d", rec.Code)
	}
	// Listing hides the token itself and deleting revokes it.
	list := decodeBody(t, f.do("GET", "/platform/profile/access-tokens", nil)).([]any)
	if len(list) != 1 || strings.Contains(list[0].(map[string]any)["token_alias"].(string), tok[8:]) {
		t.Fatalf("list: %v", list)
	}
	id := list[0].(map[string]any)["id"].(float64)
	if rec := f.do("DELETE", "/platform/profile/access-tokens/"+itoa(int64(id)), nil); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := f.doAs(tok, "GET", "/v1/projects", nil); rec.Code != 401 {
		t.Fatalf("revoked PAT: %d", rec.Code)
	}
}

func TestUnknownRoutes(t *testing.T) {
	f := newFixture(t)
	if rec := f.do("GET", "/v1/nope", nil); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"message"`) {
		t.Fatalf("v1 unknown: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/platform/never/heard/of/it", nil); rec.Code != 200 || rec.Body.String() != "{}" {
		t.Fatalf("platform unknown GET: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/platform/never/heard/of/it", nil); rec.Code != 204 {
		t.Fatalf("platform unknown POST: %d", rec.Code)
	}
	if rec := f.doAs("", "GET", "/platform/never", nil); rec.Code != 401 {
		t.Fatalf("unknown routes still need credentials: %d", rec.Code)
	}
}

func TestCORS(t *testing.T) {
	f := newFixture(t)
	for origin, want := range map[string]bool{f.cfg.DashboardURL(): true, "https://evil.example": false} {
		rec := f.doAs("", "OPTIONS", "/platform/profile", nil, "Origin", origin, "Access-Control-Request-Headers", "authorization,x-request-id")
		got := rec.Header().Get("Access-Control-Allow-Origin") == origin
		if got != want {
			t.Errorf("origin %s: allowed=%v, want %v (status %d)", origin, got, want, rec.Code)
		}
	}
}

func TestErrorEnvelope(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/v1/projects/zzzzzzzzzzzzzzzzzzzz", nil)
	if rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	m := decodeBody(t, rec).(map[string]any)
	if m["message"] != "Project not found" || len(m) != 1 {
		t.Fatalf("envelope: %v", m)
	}
	if rec := f.do("GET", "/v1/projects/system", nil); rec.Code != 404 {
		t.Fatalf("system project must be hidden: %d", rec.Code)
	}
}
