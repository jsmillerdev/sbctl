package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
)

func TestRouteForwarding(t *testing.T) {
	h := newHarness(t)
	k := h.k
	pub := k.PublishableKey
	cases := []struct {
		name      string
		method    string
		target    string
		headers   []string
		svc       service
		wantPath  string
		wantQuery string
		wantHost  string            // upstream Host
		wantHdr   map[string]string // upstream request headers
	}{
		{name: "rest", method: "GET", target: "/rest/v1/todos?select=*&order=id.desc", headers: []string{"apikey", pub},
			svc: svcRest, wantPath: "/todos", wantQuery: "select=*&order=id.desc", wantHost: h.host(h.ref),
			wantHdr: map[string]string{"Authorization": "Bearer " + k.AnonKey, "Apikey": k.AnonKey, "X-Forwarded-Prefix": "/rest/v1/", "X-Forwarded-Host": h.host(h.ref), "X-Forwarded-Proto": "http"}},
		{name: "rest rpc with service key", method: "POST", target: "/rest/v1/rpc/do_it", headers: []string{"apikey", k.SecretKey},
			svc: svcRest, wantPath: "/rpc/do_it", wantHdr: map[string]string{"Authorization": "Bearer " + k.ServiceRoleKey}},
		{name: "rest openapi root as service role", method: "GET", target: "/rest/v1/", headers: []string{"apikey", k.SecretKey},
			svc: svcRest, wantPath: "/"},
		{name: "graphql", method: "POST", target: "/graphql/v1", headers: []string{"apikey", pub, "Content-Profile", "evil_schema"},
			svc: svcRest, wantPath: "/rpc/graphql", wantHdr: map[string]string{"Content-Profile": "graphql_public", "X-Forwarded-Prefix": "/graphql/v1"}},
		{name: "auth protected", method: "POST", target: "/auth/v1/token?grant_type=password", headers: []string{"apikey", pub},
			svc: svcAuth, wantPath: "/token", wantQuery: "grant_type=password", wantHdr: map[string]string{"Authorization": "Bearer " + k.AnonKey}},
		{name: "auth user with session JWT", method: "GET", target: "/auth/v1/user", headers: []string{"apikey", pub, "Authorization", "Bearer user.session.jwt"},
			svc: svcAuth, wantPath: "/user", wantHdr: map[string]string{"Authorization": "Bearer user.session.jwt"}},
		{name: "auth verify is open", method: "GET", target: "/auth/v1/verify?token=abc&type=signup&redirect_to=http://x",
			svc: svcAuth, wantPath: "/verify", wantQuery: "token=abc&type=signup&redirect_to=http://x", wantHdr: map[string]string{"X-Forwarded-Prefix": "/auth/v1/verify"}},
		{name: "auth callback is open", method: "GET", target: "/auth/v1/callback?code=1&state=2", svc: svcAuth, wantPath: "/callback", wantQuery: "code=1&state=2"},
		{name: "auth authorize is open", method: "GET", target: "/auth/v1/authorize?provider=github", svc: svcAuth, wantPath: "/authorize", wantQuery: "provider=github"},
		{name: "auth jwks is open", method: "GET", target: "/auth/v1/.well-known/jwks.json", svc: svcAuth, wantPath: "/.well-known/jwks.json"},
		{name: "auth saml acs is open", method: "POST", target: "/auth/v1/sso/saml/acs", svc: svcAuth, wantPath: "/sso/saml/acs"},
		{name: "auth saml metadata is open", method: "GET", target: "/auth/v1/sso/saml/metadata", svc: svcAuth, wantPath: "/sso/saml/metadata"},
		{name: "oauth server metadata is open", method: "GET", target: "/.well-known/oauth-authorization-server", svc: svcAuth, wantPath: "/.well-known/oauth-authorization-server"},
		{name: "storage public object, no key", method: "GET", target: "/storage/v1/object/public/avatars/a.png",
			svc: svcStorage, wantPath: "/object/public/avatars/a.png", wantHost: h.host(h.ref), wantHdr: map[string]string{"X-Forwarded-Host": h.host(h.ref), "X-Forwarded-Prefix": "/storage/v1"}},
		{name: "storage with publishable key", method: "POST", target: "/storage/v1/object/avatars/b.png", headers: []string{"apikey", pub},
			svc: svcStorage, wantPath: "/object/avatars/b.png", wantHdr: map[string]string{"Authorization": "Bearer " + k.AnonKey}},
		{name: "storage s3 keeps SigV4", method: "PUT", target: "/storage/v1/s3/bucket/key", headers: []string{"Authorization", "AWS4-HMAC-SHA256 Credential=abc"},
			svc: svcStorage, wantPath: "/s3/bucket/key", wantHdr: map[string]string{"Authorization": "AWS4-HMAC-SHA256 Credential=abc"}},
		{name: "realtime rest api", method: "POST", target: "/realtime/v1/api/broadcast", headers: []string{"apikey", pub},
			svc: svcRealtime, wantPath: "/api/broadcast", wantHost: config.RealtimeInternalHost(h.ref), wantHdr: map[string]string{"Authorization": "Bearer " + k.AnonKey}},
		{name: "realtime socket (long poll)", method: "GET", target: "/realtime/v1/longpoll?vsn=1.0.0&apikey=" + pub,
			svc: svcRealtime, wantPath: "/socket/longpoll", wantQuery: "vsn=1.0.0&apikey=" + k.AnonKey, wantHost: config.RealtimeInternalHost(h.ref),
			wantHdr: map[string]string{"X-Api-Key": k.AnonKey}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := h.ups[c.svc]
			before := up.count()
			resp, body := h.project(c.method, c.target, c.headers...)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			if up.count() != before+1 {
				t.Fatalf("upstream %s not hit", up.name)
			}
			got := up.last(t)
			if got.Path != c.wantPath || got.RawQuery != c.wantQuery {
				t.Errorf("upstream saw %s?%s, want %s?%s", got.Path, got.RawQuery, c.wantPath, c.wantQuery)
			}
			if c.wantHost != "" && got.Host != c.wantHost {
				t.Errorf("upstream Host %q, want %q", got.Host, c.wantHost)
			}
			for name, want := range c.wantHdr {
				if g := got.Header.Get(name); g != want {
					t.Errorf("upstream header %s = %q, want %q", name, g, want)
				}
			}
			if got.Header.Get("X-Request-Id") == "" || got.Header.Get("X-Request-Id") != resp.Header.Get("X-Request-Id") {
				t.Errorf("request id not propagated: upstream %q response %q", got.Header.Get("X-Request-Id"), resp.Header.Get("X-Request-Id"))
			}
			if strings.Contains(got.RawQuery, "apikey") && c.svc != svcRealtime {
				t.Errorf("apikey leaked into upstream query: %s", got.RawQuery)
			}
		})
	}
}

func TestRejections(t *testing.T) {
	h := newHarness(t)
	pub := h.k.PublishableKey
	for _, c := range []struct {
		name    string
		method  string
		target  string
		headers []string
		status  int
		body    string
	}{
		{"rest without key", "GET", "/rest/v1/todos", nil, 401, "Unauthorized"},
		{"rest with a key of another project", "GET", "/rest/v1/todos", []string{"apikey", testKeys(t, "zyxwvutsrqponmlkjihg").PublishableKey}, 401, "Unauthorized"},
		{"rest with garbage", "GET", "/rest/v1/todos?apikey=junk", nil, 401, "Unauthorized"},
		{"auth without key", "POST", "/auth/v1/token", nil, 401, "Unauthorized"},
		{"graphql without key", "POST", "/graphql/v1", nil, 401, "Unauthorized"},
		{"realtime socket without key", "GET", "/realtime/v1/websocket", nil, 401, "Unauthorized"},
		{"openapi root with anon key", "GET", "/rest/v1/", []string{"apikey", pub}, 403, "RBAC: access denied"},
		{"openapi root without trailing slash with anon key", "GET", "/rest/v1", []string{"apikey", pub}, 403, "RBAC: access denied"},
		{"openapi root via dot segments", "GET", "/rest/v1/x/..", []string{"apikey", pub}, 403, "RBAC: access denied"},
		{"realtime tenants admin API", "GET", "/realtime/v1/api/tenants", []string{"apikey", h.k.SecretKey}, 403, "RBAC: access denied"},
		{"realtime tenants admin API sub path", "DELETE", "/realtime/v1/api/tenants/abc", []string{"apikey", h.k.SecretKey}, 403, "RBAC: access denied"},
		{"realtime tenants via dot segments", "GET", "/realtime/v1/api/x/../tenants", []string{"apikey", h.k.SecretKey}, 403, "RBAC: access denied"},
		{"realtime tenants via double slash", "GET", "/realtime/v1//api//tenants", []string{"apikey", h.k.SecretKey}, 403, "RBAC: access denied"},
		{"realtime openapi", "GET", "/realtime/v1/api/openapi", []string{"apikey", h.k.SecretKey}, 403, "RBAC: access denied"},
		{"escaped slash", "GET", "/realtime/v1/api%2Ftenants", []string{"apikey", h.k.SecretKey}, 400, `{"message":"Bad Request"}`},
		{"unknown path", "GET", "/pg/tables", []string{"apikey", h.k.SecretKey}, 404, `{"message":"no Route matched with those values"}`},
		{"root", "GET", "/", nil, 404, `{"message":"no Route matched with those values"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := h.ups[svcRealtime].count() + h.ups[svcRest].count() + h.ups[svcAuth].count()
			resp, body := h.project(c.method, c.target, c.headers...)
			if resp.StatusCode != c.status || body != c.body {
				t.Fatalf("got %d %q, want %d %q", resp.StatusCode, body, c.status, c.body)
			}
			if after := h.ups[svcRealtime].count() + h.ups[svcRest].count() + h.ups[svcAuth].count(); after != before {
				t.Fatal("rejected request reached an upstream")
			}
			if resp.Header.Get("Access-Control-Allow-Origin") == "" {
				t.Error("error responses must carry CORS headers so browsers can read them")
			}
		})
	}
	if got := h.waker.calls(); len(got) != 0 {
		t.Fatalf("waker called for rejected requests: %v", got)
	}
}

func TestUnknownHosts(t *testing.T) {
	h := newHarness(t)
	for _, host := range []string{"nope.example.org", "zzzzzzzzzzzzzzzzzzzz.api." + testDomain, "system.api." + testDomain, "api." + testDomain + ".evil.test", testDomain, "studio2." + testDomain, "", "pooler." + testDomain} {
		resp, body := h.req("GET", host, "/rest/v1/todos", "apikey", h.k.PublishableKey)
		if resp.StatusCode != 404 || body != `{"message":"Not Found"}` {
			t.Errorf("host %q: %d %q", host, resp.StatusCode, body)
		}
	}
	for _, s := range []service{svcRest, svcAuth, svcRealtime, svcStorage, svcStudio} {
		if h.ups[s].count() != 0 {
			t.Errorf("unknown host reached %s", s)
		}
	}
}

func TestHostNormalization(t *testing.T) {
	h := newHarness(t)
	for _, host := range []string{strings.ToUpper(h.host(h.ref)), h.host(h.ref) + ":443", h.host(h.ref) + "."} {
		resp, body := h.req("GET", host, "/rest/v1/todos", "apikey", h.k.PublishableKey)
		if resp.StatusCode != 200 {
			t.Errorf("host %q: %d %s", host, resp.StatusCode, body)
		}
	}
}

func TestAPIAndStudioHosts(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.req("GET", "api."+testDomain, "/v1/projects")
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("api host: %d", resp.StatusCode)
	}
	if got := <-h.apiHit; got != "api."+testDomain+"/v1/projects" {
		t.Fatalf("api handler saw %q", got)
	}
	resp, body := h.req("GET", "studio."+testDomain+":443", "/project/default?x=1", "Authorization", "Bearer dash")
	if resp.StatusCode != 200 || !strings.Contains(body, "studio") {
		t.Fatalf("studio host: %d %s", resp.StatusCode, body)
	}
	got := h.ups[svcStudio].last(t)
	if got.Path != "/project/default" || got.RawQuery != "x=1" || got.Header.Get("Authorization") != "Bearer dash" {
		t.Fatalf("studio upstream saw %+v", got)
	}
	if got.Header.Get("X-Forwarded-Host") != "studio."+testDomain+":443" {
		t.Fatalf("studio X-Forwarded-Host %q", got.Header.Get("X-Forwarded-Host"))
	}
	// Studio keeps its own CORS headers (the proxy only rewrites the project API's).
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://upstream.example" {
		t.Fatalf("studio response headers were rewritten: %v", resp.Header)
	}

	// No API handler: 503, not a panic.
	h2 := newHarness(t, func(o *Options) { o.APIHandler = nil })
	if resp, _ := h2.req("GET", "api."+testDomain, "/"); resp.StatusCode != 503 {
		t.Fatalf("nil APIHandler: %d", resp.StatusCode)
	}
}

// The loopback-only /internal/ routes of the Management API (mail templates for GoTrue) are
// not reachable through the edge, even for a client on the node itself.
func TestInternalRoutesAreNotServedThroughTheEdge(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/internal/templates/x/y", "/internal", "/internal/", "/v1/../internal/templates/x/y"} {
		if resp, _ := h.req("GET", "api."+testDomain, p); resp.StatusCode != 404 {
			t.Errorf("GET %s: %d, want 404", p, resp.StatusCode)
		}
		select {
		case hit := <-h.apiHit:
			t.Errorf("GET %s reached the Management API (%s)", p, hit)
		default:
		}
	}
	if resp, _ := h.req("GET", "api."+testDomain, "/v1/projects"); resp.StatusCode != http.StatusTeapot {
		t.Errorf("an ordinary API path must still reach the handler: %d", resp.StatusCode)
	}
}

func TestCORS(t *testing.T) {
	h := newHarness(t)
	// Preflight is answered by the proxy, without a key and without reaching the upstream.
	resp, _ := h.project("OPTIONS", "/rest/v1/todos", "Origin", "https://app.example", "Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "authorization, apikey, x-client-info, content-type")
	if resp.StatusCode != 204 {
		t.Fatalf("preflight: %d", resp.StatusCode)
	}
	wantH := map[string]string{
		"Access-Control-Allow-Origin":  "https://app.example",
		"Access-Control-Allow-Headers": "authorization, apikey, x-client-info, content-type",
		"Access-Control-Max-Age":       "3600",
	}
	for k, v := range wantH {
		if resp.Header.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, resp.Header.Get(k), v)
		}
	}
	if !strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), "DELETE") {
		t.Errorf("methods: %q", resp.Header.Get("Access-Control-Allow-Methods"))
	}
	if h.ups[svcRest].count() != 0 {
		t.Fatal("preflight reached the upstream")
	}

	// A proxied response carries exactly one CORS policy: ours, not the upstream's.
	resp, _ = h.project("GET", "/rest/v1/todos", "apikey", h.k.PublishableKey, "Origin", "https://app.example")
	if v := resp.Header.Values("Access-Control-Allow-Origin"); len(v) != 1 || v[0] != "https://app.example" {
		t.Fatalf("Access-Control-Allow-Origin = %v", v)
	}
	if resp.Header.Get("X-Upstream") != "postgrest" {
		t.Fatal("other upstream headers must pass through")
	}
	resp, _ = h.project("GET", "/rest/v1/todos", "apikey", h.k.PublishableKey)
	if v := resp.Header.Values("Access-Control-Allow-Origin"); len(v) != 1 || v[0] != "*" {
		t.Fatalf("no Origin: Access-Control-Allow-Origin = %v", v)
	}
	// A plain OPTIONS without preflight headers is a normal request (needs a key).
	if resp, _ := h.project("OPTIONS", "/rest/v1/todos"); resp.StatusCode != 401 {
		t.Fatalf("plain OPTIONS: %d", resp.StatusCode)
	}
}

func TestSpoofedHeadersAreReplaced(t *testing.T) {
	h := newHarness(t)
	h.project("POST", "/storage/v1/object/b/f", "X-Forwarded-Host", "victim.api."+testDomain, "X-Forwarded-For", "6.6.6.6",
		"X-Forwarded-Proto", "https", "X-Forwarded-Port", "9", "X-Forwarded-Prefix", "/evil", "X-Real-Ip", "6.6.6.6",
		TenantHeader, "victim", "X-Request-Id", "bad id with spaces")
	got := h.ups[svcStorage].last(t)
	if got.Header.Get("X-Forwarded-Host") != h.host(h.ref) {
		t.Errorf("storage tenant header spoofable: %q", got.Header.Get("X-Forwarded-Host"))
	}
	if got.Header.Get("X-Forwarded-For") != "127.0.0.1" || got.Header.Get("X-Forwarded-Proto") != "http" {
		t.Errorf("forwarded for/proto: %q %q", got.Header.Get("X-Forwarded-For"), got.Header.Get("X-Forwarded-Proto"))
	}
	if got.Header.Get("X-Forwarded-Port") == "9" || got.Header.Get("X-Forwarded-Prefix") != "/storage/v1" {
		t.Errorf("forwarded port/prefix: %q %q", got.Header.Get("X-Forwarded-Port"), got.Header.Get("X-Forwarded-Prefix"))
	}
	if got.Header.Get("X-Real-Ip") != "" || got.Header.Get(TenantHeader) != "" {
		t.Errorf("X-Real-Ip %q, tenant %q must not pass", got.Header.Get("X-Real-Ip"), got.Header.Get(TenantHeader))
	}
	if id := got.Header.Get("X-Request-Id"); id == "" || id == "bad id with spaces" {
		t.Errorf("request id %q", id)
	}

	// A well-formed client request id is kept.
	resp, _ := h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey, "X-Request-Id", "client-req.42")
	if resp.Header.Get("X-Request-Id") != "client-req.42" || h.ups[svcRest].last(t).Header.Get("X-Request-Id") != "client-req.42" {
		t.Error("client request id dropped")
	}
	// Realtime ignores Host-based spoofing: the tenant host is always ours.
	h.project("GET", "/realtime/v1/api/x", "apikey", h.k.AnonKey, "Host", "victim.realtime.internal")
	if h.ups[svcRealtime].last(t).Host != config.RealtimeInternalHost(h.ref) {
		t.Errorf("realtime Host %q", h.ups[svcRealtime].last(t).Host)
	}
}

func TestBodyAndStreaming(t *testing.T) {
	h := newHarness(t)
	payload := strings.Repeat("x", 3<<20)
	resp, _ := h.reqBody("POST", h.host(h.ref), "/rest/v1/big", payload, "apikey", h.k.AnonKey, "Content-Type", "text/plain")
	if resp.StatusCode != 200 || len(h.ups[svcRest].last(t).Body) != len(payload) {
		t.Fatalf("body not forwarded intact: %d, %d bytes", resp.StatusCode, len(h.ups[svcRest].last(t).Body))
	}

	// The first chunk reaches the client while the upstream is still producing.
	release := make(chan struct{})
	h.ups[svcStorage].mu.Lock()
	h.ups[svcStorage].handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "second\n")
	}
	h.ups[svcStorage].mu.Unlock()
	req, _ := http.NewRequest("GET", h.ts.URL+"/storage/v1/object/public/b/stream", nil)
	req.Host = h.host(h.ref)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := make([]byte, 6)
	if _, err := io.ReadFull(res.Body, buf); err != nil || string(buf) != "first\n" {
		t.Fatalf("first chunk: %q %v", buf, err)
	}
	close(release)
	rest, _ := io.ReadAll(res.Body)
	if string(rest) != "second\n" {
		t.Fatalf("rest: %q", rest)
	}
}

func TestUpstreamFailures(t *testing.T) {
	h := newHarness(t)
	h.ups[svcRest].srv.Close()
	resp, body := h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey)
	if resp.StatusCode != 502 || body != `{"message":"Upstream unavailable"}` {
		t.Fatalf("closed upstream: %d %s", resp.StatusCode, body)
	}
	// Response-header timeout becomes 504.
	h.srv.mu.Lock()
	h.srv.transports[defaultTimeout] = nil
	delete(h.srv.transports, defaultTimeout)
	h.srv.mu.Unlock()
	slow := h.ups[svcAuth]
	slow.mu.Lock()
	slow.handler = func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }
	slow.mu.Unlock()
	old := projectRoutes
	t.Cleanup(func() { projectRoutes = old })
	routes := append([]route(nil), projectRoutes...)
	for i := range routes {
		if routes[i].svc == svcAuth {
			routes[i].timeout = 50 * time.Millisecond
		}
	}
	projectRoutes = routes
	resp, body = h.project("GET", "/auth/v1/user", "apikey", h.k.AnonKey)
	if resp.StatusCode != 504 || body != `{"message":"Upstream timed out"}` {
		t.Fatalf("slow upstream: %d %s", resp.StatusCode, body)
	}
}

func TestWaker(t *testing.T) {
	h := newHarness(t)
	h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey)
	h.project("GET", "/storage/v1/object/public/b/f")
	h.project("GET", "/rest/v1/x") // 401: must not wake
	if got := h.waker.calls(); len(got) != 2 || got[0] != h.ref || got[1] != h.ref {
		t.Fatalf("waker calls: %v", got)
	}

	// A failing wake answers 503 with Retry-After and does not forward.
	h.waker.mu.Lock()
	h.waker.err = errors.New("boom")
	h.waker.mu.Unlock()
	before := h.ups[svcRest].count()
	resp, body := h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" || !strings.Contains(body, "starting") {
		t.Fatalf("failed wake: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	if h.ups[svcRest].count() != before {
		t.Fatal("request forwarded despite failed wake")
	}
}

func TestProjectStatusGate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, st := range []registry.Status{registry.StatusInactive, registry.StatusPausing, registry.StatusInitFailed} {
		if err := h.reg.SetProjectStatus(ctx, h.ref, st); err != nil {
			t.Fatal(err)
		}
		eventually(t, "status "+string(st), func() bool {
			resp, _ := h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey)
			return resp.StatusCode == 503
		})
	}
	// Unauthenticated callers learn nothing about the status.
	if resp, _ := h.project("GET", "/rest/v1/x"); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated on paused project: %d", resp.StatusCode)
	}
	if err := h.reg.SetProjectStatus(ctx, h.ref, registry.StatusActiveHealthy); err != nil {
		t.Fatal(err)
	}
	eventually(t, "active again", func() bool {
		resp, _ := h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey)
		return resp.StatusCode == 200
	})
}

func TestFunctions(t *testing.T) {
	h := newHarness(t)
	resp, body := h.project("POST", "/functions/v1/hello", "apikey", h.k.PublishableKey)
	if resp.StatusCode != 503 || body != `{"message":"Edge Functions are not enabled on this node"}` {
		t.Fatalf("disabled: %d %s", resp.StatusCode, body)
	}
	if h.ups[svcFunctions].count() != 0 {
		t.Fatal("disabled functions reached the runtime")
	}

	h = newHarness(t, func(o *Options) { o.FunctionsEnabled = true; o.FunctionsProxyToken = "node-secret" })
	resp, body = h.project("POST", "/functions/v1/hello/world?x=1&apikey=left", "apikey", h.k.PublishableKey, "Authorization", "Bearer user.jwt", "Sb-Api-Key", "forged", TenantHeader, "victim", config.FunctionsProxyTokenHeader, "forged-secret")
	if resp.StatusCode != 200 {
		t.Fatalf("enabled: %d %s", resp.StatusCode, body)
	}
	got := h.ups[svcFunctions].last(t)
	if got.Path != "/hello/world" || got.RawQuery != "x=1&apikey=left" {
		t.Errorf("functions saw %s?%s", got.Path, got.RawQuery)
	}
	if got.Header.Get("Sb-Api-Key") != h.k.AnonKey || got.Header.Get("Authorization") != "Bearer user.jwt" || got.Header.Get("Apikey") != h.k.PublishableKey {
		t.Errorf("functions credentials: sb-api-key=%q authorization=%q apikey=%q", got.Header.Get("Sb-Api-Key"), got.Header.Get("Authorization"), got.Header.Get("Apikey"))
	}
	if got.Header.Get(TenantHeader) != h.ref {
		t.Errorf("tenant header %q", got.Header.Get(TenantHeader))
	}
	if v := got.Header.Values(config.FunctionsProxyTokenHeader); len(v) != 1 || v[0] != "node-secret" {
		t.Errorf("the runtime got the secret %q, want the proxy's own once", v)
	}
	// The secret goes to the edge runtime and nowhere else.
	h.project("GET", "/rest/v1/t", "apikey", h.k.PublishableKey, config.FunctionsProxyTokenHeader, "node-secret", TenantHeader, "victim")
	if rest := h.ups[svcRest].last(t); rest.Header.Get(config.FunctionsProxyTokenHeader) != "" || rest.Header.Get(TenantHeader) != "" {
		t.Errorf("PostgREST got the runtime's headers: %v", rest.Header)
	}
	// Anonymous calls pass (verify_jwt is the runtime's business); a bad sb_ key does not.
	if resp, _ := h.project("GET", "/functions/v1/open"); resp.StatusCode != 200 {
		t.Errorf("anonymous function call: %d", resp.StatusCode)
	}
	if resp, body := h.project("GET", "/functions/v1/x", "apikey", "sb_secret_wrong"); resp.StatusCode != 401 || body != "Invalid API key" {
		t.Errorf("bad sb_ key: %d %s", resp.StatusCode, body)
	}
}

func TestWebSocketPassthrough(t *testing.T) {
	h := newHarness(t)
	rt := h.ups[svcRealtime]
	seen := make(chan captured, 1)
	rt.mu.Lock()
	rt.handler = func(w http.ResponseWriter, r *http.Request) {
		// Origin policy is the real Realtime's business; the fake accepts any origin.
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(1 << 20)
		seen <- captured{Path: r.URL.Path, RawQuery: r.URL.RawQuery, Host: r.Host, Header: r.Header.Clone()}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		for {
			typ, msg, err := c.Read(ctx)
			if err != nil {
				return
			}
			if err := c.Write(ctx, typ, append([]byte("echo:"), msg...)); err != nil {
				return
			}
		}
	}
	rt.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(h.ts.URL, "http") + "/realtime/v1/websocket?apikey=" + h.k.PublishableKey + "&vsn=1.0.0"
	c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{Host: h.host(h.ref), HTTPHeader: http.Header{"Origin": {"https://app.example"}}})
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, resp)
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	for _, m := range []string{"hello", strings.Repeat("big", 100000)} {
		if err := c.Write(ctx, websocket.MessageText, []byte(m)); err != nil {
			t.Fatal(err)
		}
		_, got, err := c.Read(ctx)
		if err != nil || string(got) != "echo:"+m {
			t.Fatalf("echo: %v len=%d", err, len(got))
		}
	}
	up := <-seen
	if up.Path != "/socket/websocket" || up.Host != config.RealtimeInternalHost(h.ref) {
		t.Errorf("realtime saw %s on host %s", up.Path, up.Host)
	}
	if up.Header.Get("X-Api-Key") != h.k.AnonKey || up.RawQuery != "vsn=1.0.0&apikey="+h.k.AnonKey {
		t.Errorf("realtime credentials: x-api-key=%q query=%q", up.Header.Get("X-Api-Key"), up.RawQuery)
	}
	if up.Header.Get("X-Forwarded-Host") != h.host(h.ref) {
		t.Errorf("X-Forwarded-Host %q", up.Header.Get("X-Forwarded-Host"))
	}
	if got := h.waker.calls(); len(got) != 1 || got[0] != h.ref {
		t.Errorf("waker on upgrade: %v", got)
	}

	// Without a key the handshake is refused before reaching Realtime.
	_, resp, err = websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.ts.URL, "http")+"/realtime/v1/websocket", &websocket.DialOptions{Host: h.host(h.ref)})
	if err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("keyless handshake: err=%v resp=%v", err, resp)
	}
}

// TestEscapedPathReachesUpstreamUnchanged: S3 SigV4 signs the client's canonical
// URI, so reserved characters must not be re-escaped on the way to Storage.
func TestEscapedPathReachesUpstreamUnchanged(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		name, target, svc string
		wantURI           string
	}{
		{"s3 plus and parens", "/storage/v1/s3/b/a%28b%29%2Bc%21.txt?x-id=PutObject", "storage", "/s3/b/a%28b%29%2Bc%21.txt?x-id=PutObject"},
		{"s3 raw parens and plus", "/storage/v1/s3/b/a(b)+c.txt", "storage", "/s3/b/a(b)+c.txt"},
		{"object name with encoded space", "/storage/v1/object/b/my%20file%2B1.txt", "storage", "/object/b/my%20file%2B1.txt"},
		{"rest filter path", "/rest/v1/rpc/f%C3%A9", "rest", "/rpc/f%C3%A9"},
		// An encoded dot segment is cleaned on the decoded form; the escaped form would disagree, so it is re-escaped.
		{"encoded dot segments", "/rest/v1/a/%2e%2e/b", "rest", "/b"},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := svcStorage
			if c.svc == "rest" {
				svc = svcRest
			}
			resp, body := h.project("PUT", c.target, "apikey", h.k.PublishableKey, "Authorization", "AWS4-HMAC-SHA256 Credential=abc")
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			if got := h.ups[svc].last(t).RequestURI; got != c.wantURI {
				t.Fatalf("upstream saw %q, want %q", got, c.wantURI)
			}
		})
	}
}

// Open routes need no key, so a key lookup that fails (registry hiccup, decrypt error)
// must not take them down; protected routes still fail closed.
func TestOpenRoutesSurviveKeyLookupFailure(t *testing.T) {
	h := newHarness(t)
	h.keys.setFail(errors.New("registry unavailable"))
	h.srv.table.mu.Lock()
	h.srv.table.dropAllKeysLocked() // no cached keys to hide the failure
	h.srv.table.mu.Unlock()
	for _, target := range []string{"/auth/v1/verify?token=abc&type=signup", "/auth/v1/callback?code=1", "/storage/v1/object/public/avatars/a.png"} {
		if resp, body := h.project("GET", target); resp.StatusCode != 200 {
			t.Errorf("GET %s with failing keys = %d %s, want it forwarded", target, resp.StatusCode, body)
		}
	}
	if resp, _ := h.project("GET", "/rest/v1/todos", "apikey", h.k.PublishableKey); resp.StatusCode != 503 {
		t.Errorf("protected route with failing keys = %d, want 503 (fail closed)", resp.StatusCode)
	}
	// A project that is not active is still held back, keys or not.
	if err := h.reg.SetProjectStatus(context.Background(), h.ref, registry.StatusInactive); err != nil {
		t.Fatal(err)
	}
	eventually(t, "status reaches the table", func() bool {
		resp, _ := h.project("GET", "/auth/v1/verify?token=abc")
		return resp.StatusCode == 503
	})
}

func TestDerivedProjectRouteRowIsQuiet(t *testing.T) {
	h := newHarness(t)
	var buf strings.Builder
	tb := newTable(h.cfg, h.reg, h.keys, slog.New(slog.NewTextHandler(&buf, nil))) // own table: the harness's is being synced
	tb.customRoutes([]registry.Route{
		{Host: h.host(h.ref), Ref: h.ref, Kind: "api"},     // what lifecycle writes for every project
		{Host: h.host("tsrqponmlkjihgfedcba"), Ref: h.ref}, // a takeover attempt
	})
	if n := strings.Count(buf.String(), "ignoring route"); n != 1 {
		t.Fatalf("%d warnings, want exactly one (for the takeover only):\n%s", n, buf.String())
	}
}
