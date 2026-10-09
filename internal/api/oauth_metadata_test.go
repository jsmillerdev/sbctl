package api

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/oauth"
)

const (
	hostedAPI = "https://api.supabase.com"
	hostedMCP = "https://mcp.supabase.com"
)

// loadHosted reads a document that was captured from hosted (testdata/hosted-*.json).
func loadHosted(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// swapHost replaces the hosted origin in every string of v with ours.
func swapHost(v any, from, to string) any {
	switch x := v.(type) {
	case string:
		return strings.Replace(x, from, to, 1)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = swapHost(x[i], from, to)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = swapHost(e, from, to)
		}
		return out
	}
	return v
}

// sortedScopes puts the scopes_supported list of a document in order: hosted lists the 13 scopes in
// an order of its own, and the order carries no meaning.
func sortedScopes(m map[string]any) {
	l, _ := m["scopes_supported"].([]any)
	s := make([]string, len(l))
	for i, v := range l {
		s[i], _ = v.(string)
	}
	slices.Sort(s)
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	m["scopes_supported"] = out
}

func fetchDoc(t *testing.T, f *fixture, path string, headers ...string) (map[string]any, http.Header) {
	t.Helper()
	rec := f.epDo("", http.MethodGet, path, "", headers...)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("GET %s: Content-Type %q", path, ct)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("GET %s: %v: %s", path, err, rec.Body)
	}
	return m, rec.Header()
}

// The authorization server document is the one hosted serves (testdata/hosted-as-metadata.json,
// captured from api.supabase.com) with exactly the differences of design 2.3: our origin, S256
// only, no jwt-bearer grant and no grant profile, and a revocation endpoint.
func TestMetadataGoldenAuthorizationServer(t *testing.T) {
	f := newFixture(t)
	issuer := f.cfg.APIURL()
	if issuer != "https://api.example.test" {
		t.Fatalf("issuer %q", issuer)
	}
	want := swapHost(loadHosted(t, "hosted-as-metadata.json"), hostedAPI, issuer).(map[string]any)
	// plain is dropped from the PKCE methods.
	want["code_challenge_methods_supported"] = []any{"S256"}
	// jwt-bearer and the grant profile that goes with it are dropped.
	want["grant_types_supported"] = []any{"authorization_code", "refresh_token"}
	delete(want, "authorization_grant_profiles_supported")
	// A revocation endpoint is advertised, with the methods of the token endpoint.
	want["revocation_endpoint"] = issuer + "/v1/oauth/revoke"
	want["revocation_endpoint_auth_methods_supported"] = []any{"client_secret_basic", "client_secret_post"}
	sortedScopes(want)

	got, h := fetchDoc(t, f, "/.well-known/oauth-authorization-server")
	sortedScopes(got)
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Errorf("authorization server metadata differs from hosted's plus the listed differences:\n got: %s\nwant: %s", g, w)
	}
	if cc := h.Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Errorf("Cache-Control %q", cc)
	}
}

// The protected resource document is hosted's (testdata/hosted-prm-metadata.json, from
// mcp.supabase.com) with our resource and authorization server, our name, and no documentation URL.
func TestMetadataGoldenProtectedResource(t *testing.T) {
	f := newFixture(t)
	issuer := f.cfg.APIURL()
	want := loadHosted(t, "hosted-prm-metadata.json")
	want["resource"] = issuer + "/mcp"
	want["authorization_servers"] = []any{issuer}
	want["resource_name"] = "Supavise MCP"
	delete(want, "resource_documentation")
	sortedScopes(want)

	got, h := fetchDoc(t, f, "/.well-known/oauth-protected-resource/mcp")
	sortedScopes(got)
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Errorf("protected resource metadata differs from hosted's plus the listed differences:\n got: %s\nwant: %s", g, w)
	}
	if cc := h.Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Errorf("Cache-Control %q", cc)
	}
}

// Both documents advertise the 13 scopes of oauth.AdvertisedScopes, and every endpoint the
// authorization server document names is a route of this server.
func TestMetadataMatchesTheRoutes(t *testing.T) {
	f := newFixture(t)
	as, _ := fetchDoc(t, f, "/.well-known/oauth-authorization-server")
	prm, _ := fetchDoc(t, f, "/.well-known/oauth-protected-resource/mcp")
	for name, doc := range map[string]map[string]any{"authorization server": as, "protected resource": prm} {
		var got []string
		for _, v := range doc["scopes_supported"].([]any) {
			got = append(got, v.(string))
		}
		if !reflect.DeepEqual(oauth.NormalizeScopes(got), oauth.NormalizeScopes(oauth.AdvertisedScopes)) || len(got) != 13 {
			t.Errorf("%s scopes_supported = %v", name, got)
		}
	}
	impl := f.srv.implemented()
	for field, key := range map[string]string{
		"authorization_endpoint": "GET /v1/oauth/authorize",
		"token_endpoint":         "POST /v1/oauth/token",
		"registration_endpoint":  "POST /platform/oauth/apps/register",
		"revocation_endpoint":    "POST /v1/oauth/revoke",
	} {
		_, path, _ := strings.Cut(key, " ")
		if as[field] != f.cfg.APIURL()+path {
			t.Errorf("%s = %v, want %s", field, as[field], f.cfg.APIURL()+path)
		}
		if _, ok := impl[key]; !ok {
			t.Errorf("%s names %s, which is not an implemented route", field, key)
		}
	}
}

// The documents are built from configuration. A request with a forged Host (or forwarding headers)
// cannot make a client trust another authorization server.
func TestMetadataIgnoresHost(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/mcp"} {
		plain, _ := fetchDoc(t, f, path)
		forged, _ := fetchDoc(t, f, path, "Host", "evil.example", "X-Forwarded-Host", "evil.example", "X-Forwarded-Proto", "http", "Forwarded", "host=evil.example")
		if !reflect.DeepEqual(plain, forged) {
			t.Errorf("%s depends on the request headers:\n%v\n%v", path, plain, forged)
		}
		if b, _ := json.Marshal(forged); strings.Contains(string(b), "evil.example") {
			t.Errorf("%s repeats the forged host: %s", path, b)
		}
	}
	// A configured public URL is the issuer.
	f.cfg.API.PublicURL = "https://auth.corp.test/"
	as, _ := fetchDoc(t, f, "/.well-known/oauth-authorization-server")
	if as["issuer"] != "https://auth.corp.test" || as["token_endpoint"] != "https://auth.corp.test/v1/oauth/token" {
		t.Errorf("[api] public_url is not the issuer: %v", as)
	}
	prm, _ := fetchDoc(t, f, "/.well-known/oauth-protected-resource/mcp")
	if prm["resource"] != "https://auth.corp.test/mcp" {
		t.Errorf("resource %v", prm["resource"])
	}
}

// With [api] disable_oauth the documents are gone: a client that finds none does not try to sign in.
func TestMetadataOffWhenOAuthDisabled(t *testing.T) {
	f := newFixture(t)
	f.cfg.API.DisableOAuth = true
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/mcp"} {
		rec := f.epDo("", http.MethodGet, path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, rec.Code)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: a disabled endpoint carries the open CORS policy", path)
		}
	}
}
