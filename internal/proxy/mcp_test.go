package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeGate stands in for the Management API's MCPGate: it counts the calls, and either refuses with
// refusal or lets the request through with query.
type fakeGate struct {
	calls   atomic.Int32
	query   string
	refusal func(w http.ResponseWriter)
	seen    func(r *http.Request)
}

func (g *fakeGate) gate(w http.ResponseWriter, r *http.Request) (string, bool) {
	g.calls.Add(1)
	if g.seen != nil {
		g.seen(r)
	}
	// The way the real gate sets its CORS headers: on the response before the proxy forwards.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if g.refusal != nil {
		g.refusal(w)
		return "", false
	}
	return g.query, true
}

func newMCPHarness(t *testing.T, g *fakeGate, mut ...func(*Options)) *harness {
	t.Helper()
	return newHarness(t, append([]func(*Options){func(o *Options) { o.MCPGate = g.gate }}, mut...)...)
}

func TestMCPRefusalNeverReachesStudio(t *testing.T) {
	g := &fakeGate{refusal: func(w http.ResponseWriter) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://api.example.test/.well-known/oauth-protected-resource/mcp"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"No access token provided"}`))
	}}
	h := newMCPHarness(t, g)
	for _, method := range []string{"POST", "GET", "DELETE", "OPTIONS"} {
		resp, body := h.reqBody(method, "api."+testDomain, "/mcp?project_ref=abc", `{"jsonrpc":"2.0"}`, "Authorization", "Bearer nope")
		if resp.StatusCode != 401 || body != `{"message":"No access token provided"}` || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer resource_metadata=") {
			t.Errorf("%s: %d %q %v", method, resp.StatusCode, body, resp.Header)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: the gate's CORS header is gone: %v", method, resp.Header)
		}
	}
	if got := g.calls.Load(); got != 4 {
		t.Errorf("the gate was asked %d times, want 4", got)
	}
	if n := h.ups[svcStudio].count(); n != 0 {
		t.Errorf("Studio received %d requests that the gate refused", n)
	}
	if len(h.apiHit) != 0 {
		t.Error("the Management API's mux saw a request for /mcp")
	}
}

// M3: the request that goes to Studio is the client's with the gate's query, no cookies, and the
// proxy's own forwarding headers; the answer carries the gate's CORS headers and none of Studio's.
func TestMCPForward(t *testing.T) {
	g := &fakeGate{query: "project_ref=abcdefghijklmnopqrst&read_only=true"}
	h := newMCPHarness(t, g)
	h.ups[svcStudio].handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://studio.example.test")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Expose-Headers", "X-Studio")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}
	const body = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	resp, got := h.reqBody("POST", "api."+testDomain, "/mcp?project_ref=other&access_token=sbp_oauth_secret&read_only=false", body,
		"Authorization", "Bearer sbp_oauth_token", "Cookie", "sb-access-token=session", "Content-Type", "application/json",
		"X-Forwarded-For", "6.6.6.6", "X-Forwarded-Host", "evil.example", "X-Forwarded-Proto", "https", "X-Forwarded-Port", "9999",
		"X-Forwarded-Prefix", "/evil", "X-Real-Ip", "6.6.6.6", "Forwarded", "for=6.6.6.6")
	if resp.StatusCode != 200 || got != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Fatalf("%d %q", resp.StatusCode, got)
	}
	up := h.ups[svcStudio].last(t)
	if up.Method != "POST" || up.Path != "/api/mcp" || up.RawQuery != g.query || up.Body != body {
		t.Errorf("Studio saw %s %s?%s %q", up.Method, up.Path, up.RawQuery, up.Body)
	}
	if up.Header.Get("Authorization") != "Bearer sbp_oauth_token" || up.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Studio needs the bearer: %v", up.Header)
	}
	if up.Header.Get("Cookie") != "" {
		t.Errorf("a cookie reached Studio: %v", up.Header)
	}
	if up.Header.Get("X-Forwarded-Host") != "api."+testDomain || up.Header.Get("X-Forwarded-Proto") != "http" ||
		up.Header.Get("X-Forwarded-For") == "6.6.6.6" || up.Header.Get("X-Forwarded-For") == "" ||
		up.Header.Get("X-Forwarded-Port") == "9999" || up.Header.Get("X-Forwarded-Prefix") != "" ||
		up.Header.Get("X-Real-Ip") != "" || up.Header.Get("Forwarded") != "" {
		t.Errorf("forwarding headers: %v", up.Header)
	}
	if v := resp.Header.Values("Access-Control-Allow-Origin"); len(v) != 1 || v[0] != "*" {
		t.Errorf("Access-Control-Allow-Origin = %v, want the gate's alone", v)
	}
	if resp.Header.Get("Access-Control-Allow-Credentials") != "" || resp.Header.Get("Access-Control-Expose-Headers") != "" {
		t.Errorf("Studio's CORS headers reached the client: %v", resp.Header)
	}

	// An empty rebuilt query is forwarded as none: the client's never goes through.
	g.query = ""
	h.req("POST", "api."+testDomain, "/mcp?project_ref=other")
	if q := h.ups[svcStudio].last(t).RawQuery; q != "" {
		t.Errorf("the client's query reached Studio: %q", q)
	}
}

// The path is cleaned the way /internal is: only /mcp is the endpoint, whatever its spelling.
func TestMCPPathMatching(t *testing.T) {
	g := &fakeGate{}
	h := newMCPHarness(t, g)
	api := "api." + testDomain
	for _, p := range []string{"/mcp", "/mcp/", "//mcp", "/./mcp", "/x/../mcp", "/mcp/.", "/%6dcp"} {
		before := g.calls.Load()
		resp, _ := h.req("POST", api, p)
		if g.calls.Load() != before+1 || resp.StatusCode != 200 {
			t.Errorf("%s: gate asked %d times, status %d", p, g.calls.Load()-before, resp.StatusCode)
		}
	}
	for _, p := range []string{"/mcpx", "/mcp/x", "/MCP", "/v1/mcp", "/api/mcp", "/.well-known/oauth-protected-resource/mcp", "/mcp.json"} {
		before := g.calls.Load()
		resp, _ := h.req("POST", api, p)
		if g.calls.Load() != before || resp.StatusCode != http.StatusTeapot {
			t.Errorf("%s: gate asked %d times, status %d; the Management API serves it", p, g.calls.Load()-before, resp.StatusCode)
		}
		<-h.apiHit
	}
	// Only api.<domain>: a project host has no /mcp.
	before := g.calls.Load()
	if resp, _ := h.project("POST", "/mcp"); g.calls.Load() != before || resp.StatusCode != 404 {
		t.Errorf("project host: gate asked %d times, status %d", g.calls.Load()-before, resp.StatusCode)
	}
}

// Without a gate, /mcp is any other path of the Management API: nothing is forwarded to Studio.
func TestMCPWithoutGateIsNotSpecial(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.req("POST", "api."+testDomain, "/mcp", "Authorization", "Bearer sbp_oauth_x")
	if resp.StatusCode != http.StatusTeapot || h.ups[svcStudio].count() != 0 {
		t.Errorf("%d, Studio saw %d", resp.StatusCode, h.ups[svcStudio].count())
	}
	if got := <-h.apiHit; !strings.HasSuffix(got, "/mcp") {
		t.Errorf("the API saw %q", got)
	}
}

// On a node that does not lead, api.<domain> goes to the leader, but /mcp does not: the gate runs on
// the node that got the request, and Studio's loopback port there is the mesh's forwarder to the
// leader's Studio (mesh.KindStudio), so the request is forwarded like it is on the leader.
func TestFollowerGatesAndForwardsMCP(t *testing.T) {
	admin := newUpstream(t, "admin")
	g := &fakeGate{query: "project_ref=abcdefghijklmnopqrst"}
	h := newMCPHarness(t, g, func(o *Options) {
		o.Config.Listen.Admin = admin.addr()
		o.Cluster = &Cluster{Leader: func() bool { return false }}
	})
	resp, _ := h.reqBody("POST", "api."+testDomain, "/mcp?x=1", `{}`, "Authorization", "Bearer t")
	if resp.StatusCode != 200 || g.calls.Load() != 1 {
		t.Fatalf("%d, gate asked %d times", resp.StatusCode, g.calls.Load())
	}
	if up := h.ups[svcStudio].last(t); up.Path != "/api/mcp" || up.RawQuery != g.query || up.Header.Get("Authorization") != "Bearer t" {
		t.Errorf("Studio saw %+v", up)
	}
	if admin.count() != 0 {
		t.Error("the follower sent /mcp to the leader's Management API listener")
	}
	// A refusal on a follower is the follower's own.
	g.refusal = func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) }
	before := h.ups[svcStudio].count()
	if resp, _ := h.req("POST", "api."+testDomain, "/mcp"); resp.StatusCode != 401 || h.ups[svcStudio].count() != before {
		t.Errorf("refusal: %d, Studio saw %d more", resp.StatusCode, h.ups[svcStudio].count()-before)
	}
	// Other paths still go to the leader.
	if resp, _ := h.req("GET", "api."+testDomain, "/v1/projects"); admin.count() != 1 || resp.StatusCode != 200 {
		t.Errorf("/v1/projects: %d, leader saw %d", resp.StatusCode, admin.count())
	}
	// The dashboard's GoTrue is unaffected, and so is /internal.
	if resp, _ := h.req("GET", "api."+testDomain, "/internal/x"); resp.StatusCode != 404 || g.calls.Load() != 2 {
		t.Errorf("/internal: %d", resp.StatusCode)
	}
}

// M2: Studio's MCP route is not reachable on the host Studio is served on, in any spelling.
func TestStudioHostBlocksMCP(t *testing.T) {
	g := &fakeGate{}
	h := newMCPHarness(t, g)
	studio := "studio." + testDomain
	for _, p := range []string{"/api/mcp", "/api/mcp/", "/api/mcp?project_ref=abc", "//api/mcp", "/api/./mcp", "/api/x/../mcp", "/api%2Fmcp",
		"/API/MCP", "/api/Mcp", "/api/mcp/index", "/dashboard/api/mcp", "/api/%6dcp"} {
		for _, method := range []string{"POST", "GET", "OPTIONS", "DELETE"} {
			resp, body := h.req(method, studio, p, "Authorization", "Bearer sbp_oauth_x")
			if resp.StatusCode != 404 {
				t.Errorf("%s %s: %d %q, want 404", method, p, resp.StatusCode, body)
			}
		}
	}
	if n := h.ups[svcStudio].count(); n != 0 {
		t.Errorf("Studio received %d requests for its MCP route", n)
	}
	if g.calls.Load() != 0 {
		t.Error("the gate answers for api.<domain> only")
	}
	// Other API routes of Studio and similar names still go through.
	for _, p := range []string{"/api/mcpx", "/api/platform/profile", "/api/incident", "/mcp", "/project/default"} {
		before := h.ups[svcStudio].count()
		if resp, _ := h.req("GET", studio, p); resp.StatusCode != 200 || h.ups[svcStudio].count() != before+1 {
			t.Errorf("%s: %d, forwarded %d", p, resp.StatusCode, h.ups[svcStudio].count()-before)
		}
	}
}

// K6: the pages that approve access cannot be framed, whatever Studio sends.
func TestStudioFrameHeaders(t *testing.T) {
	h := newHarness(t)
	set := func(csp ...string) {
		h.ups[svcStudio].handler = func(w http.ResponseWriter, _ *http.Request) {
			for _, c := range csp {
				w.Header().Add("Content-Security-Policy", c)
			}
			w.Header().Set("X-Frame-Options", "SAMEORIGIN")
			w.Header().Set("Content-Security-Policy-Report-Only", "frame-ancestors 'self'")
			_, _ = w.Write([]byte("<html>"))
		}
	}
	studio := "studio." + testDomain
	for _, p := range []string{"/authorize", "/authorize?auth_id=abc", "/authorize/", "/cli/login", "/cli/login?session_id=s", "/Authorize", "//authorize"} {
		set("default-src 'self'; script-src 'self' 'unsafe-inline'")
		resp, _ := h.req("GET", studio, p)
		csp := resp.Header.Get("Content-Security-Policy")
		if resp.Header.Get("X-Frame-Options") != "DENY" || csp != "default-src 'self'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'" {
			t.Errorf("%s: X-Frame-Options %q, CSP %q", p, resp.Header.Get("X-Frame-Options"), csp)
		}

		// A policy with a frame-ancestors directive has its sources replaced: a second one is ignored.
		set("frame-ancestors 'self' https://*.example.test; default-src 'self'")
		resp, _ = h.req("GET", studio, p)
		if csp := resp.Header.Get("Content-Security-Policy"); csp != "frame-ancestors 'none'; default-src 'self'" {
			t.Errorf("%s: CSP %q", p, csp)
		}

		// Studio sent several policies: each one forbids framing.
		set("default-src 'self'", "img-src 'self'; frame-ancestors *")
		resp, _ = h.req("GET", studio, p)
		if got := resp.Header.Values("Content-Security-Policy"); len(got) != 2 || got[0] != "default-src 'self'; frame-ancestors 'none'" || got[1] != "img-src 'self'; frame-ancestors 'none'" {
			t.Errorf("%s: CSP %q", p, got)
		}

		// No policy at all: one is added.
		set()
		resp, _ = h.req("GET", studio, p)
		if got := resp.Header.Values("Content-Security-Policy"); len(got) != 1 || got[0] != "frame-ancestors 'none'" {
			t.Errorf("%s: no CSP from Studio, got %q", p, got)
		}
	}
	// Other pages are as Studio sent them.
	set("default-src 'self'")
	for _, p := range []string{"/", "/project/default", "/authorize-app", "/authorize/x", "/cli", "/cli/login/x", "/api/platform/profile"} {
		resp, _ := h.req("GET", studio, p)
		if resp.Header.Get("X-Frame-Options") != "SAMEORIGIN" || resp.Header.Get("Content-Security-Policy") != "default-src 'self'" {
			t.Errorf("%s: framing headers were touched: %q %q", p, resp.Header.Get("X-Frame-Options"), resp.Header.Get("Content-Security-Policy"))
		}
	}
	// And the host of a project does not get Studio's treatment.
	if resp, _ := h.project("GET", "/authorize"); resp.Header.Get("X-Frame-Options") == "DENY" {
		t.Error("a project host got the consent page headers")
	}
}

func TestWithFrameAncestorsNone(t *testing.T) {
	for in, want := range map[string]string{
		"":                    "frame-ancestors 'none'",
		"default-src 'self'":  "default-src 'self'; frame-ancestors 'none'",
		"default-src 'self';": "default-src 'self'; frame-ancestors 'none'",
		"  default-src   'self' ;; img-src data: ":             "default-src   'self'; img-src data:; frame-ancestors 'none'",
		"Frame-Ancestors 'self'":                               "frame-ancestors 'none'",
		"frame-ancestors a; img-src b; frame-ancestors c":      "frame-ancestors 'none'; img-src b",
		"default-src 'self'; frame-ancestors-x https://x.test": "default-src 'self'; frame-ancestors-x https://x.test; frame-ancestors 'none'",
	} {
		if got := withFrameAncestorsNone(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// T4: nothing of a request's query reaches the access log, and nothing of the gate's answer does either.
func TestAccessLogHasNoQuery(t *testing.T) {
	var logs bytes.Buffer
	g := &fakeGate{query: "project_ref=abcdefghijklmnopqrst"}
	h := newMCPHarness(t, g, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	const secret = "sbp_oauth_0123456789abcdef0123456789abcdef01234567"
	for _, host := range []string{"api." + testDomain, "studio." + testDomain} {
		h.req("POST", host, "/mcp?access_token="+secret+"&code=sbc_codevalue&state=stateval&project_ref=abc", "Authorization", "Bearer "+secret)
		h.req("GET", host, "/authorize?auth_id=authidvalue&organization_slug=orgslug")
	}
	g.refusal = func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) }
	h.req("POST", "api."+testDomain, "/mcp?code=sbc_codevalue2", "Authorization", "Bearer "+secret)
	out := logs.String()
	if !strings.Contains(out, "path=/mcp") || !strings.Contains(out, "path=/authorize") {
		t.Fatalf("the requests were not logged:\n%s", out)
	}
	for _, leak := range []string{secret, "sbc_codevalue", "stateval", "authidvalue", "orgslug", "access_token", "project_ref", "?"} {
		if strings.Contains(out, leak) {
			t.Errorf("the access log has %q:\n%s", leak, out)
		}
	}
}

// A body the gate capped is answered, not forwarded as a broken upload.
func TestMCPBodyOverTheCap(t *testing.T) {
	g := &fakeGate{seen: func(r *http.Request) { r.Body = http.MaxBytesReader(nil, r.Body, 16) }}
	h := newMCPHarness(t, g)
	h.ups[svcStudio].handler = func(_ http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) }
	resp, body := h.reqBody("POST", "api."+testDomain, "/mcp", strings.Repeat("x", 4096), "Content-Type", "application/json")
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(body, "too large") {
		t.Errorf("%d %q", resp.StatusCode, body)
	}
	// Within the cap it goes through.
	if resp, _ := h.reqBody("POST", "api."+testDomain, "/mcp", `{}`); resp.StatusCode != 200 {
		t.Errorf("within the cap: %d", resp.StatusCode)
	}
}
