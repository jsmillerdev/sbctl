package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestIncidentBannerIsAnsweredByTheProxy(t *testing.T) {
	h := newHarness(t)
	before := h.ups[svcStudio].count()
	resp, body := h.req("GET", "studio."+testDomain, "/api/incident-banner")
	if resp.StatusCode != 200 || strings.TrimSpace(body) != `{"incidents":[]}` {
		t.Fatalf("incident banner: %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("content type %q", got)
	}
	if h.ups[svcStudio].count() != before {
		t.Error("the request reached Studio; the proxy must answer it itself")
	}
	if resp, _ := h.req("HEAD", "studio."+testDomain, "/api/incident-banner"); resp.StatusCode != 200 {
		t.Errorf("HEAD: %d", resp.StatusCode)
	}
	// Other methods and other paths still go to Studio.
	if _, _ = h.req("POST", "studio."+testDomain, "/api/incident-banner"); h.ups[svcStudio].count() != before+1 {
		t.Error("POST must be forwarded")
	}
	// Only the Studio host: a project host has no such route.
	if resp, _ := h.project("GET", "/api/incident-banner"); resp.StatusCode != 404 {
		t.Errorf("project host: %d, want 404", resp.StatusCode)
	}
}

func TestStudioCSPDropsUsercentrics(t *testing.T) {
	const csp = "default-src 'self' https://*.supabase.co https://*.usercentrics.eu; " +
		"connect-src 'self' https://api.usercentrics.eu/ wss://*.supabase.co https://app.usercentrics.eu:443; " +
		"img-src 'self' data: https://ss.supabase.com; script-src 'self' 'unsafe-inline' https://*.usercentrics.eu https://js.stripe.com; frame-ancestors 'none'"
	got := scrubCSP(csp, blockedStudioHosts)
	if strings.Contains(got, "usercentrics") {
		t.Fatalf("usercentrics left in the policy: %s", got)
	}
	for _, keep := range []string{"default-src 'self' https://*.supabase.co", "connect-src 'self' wss://*.supabase.co", "img-src 'self' data: https://ss.supabase.com", "script-src 'self' 'unsafe-inline' https://js.stripe.com", "frame-ancestors 'none'"} {
		if !strings.Contains(got, keep) {
			t.Errorf("policy lost %q:\n%s", keep, got)
		}
	}
	if scrubCSP("default-src 'self'", blockedStudioHosts) != "default-src 'self'" {
		t.Error("a policy without the host must come back unchanged")
	}
	if blockedSource("https://notusercentrics.eu", blockedStudioHosts) {
		t.Error("a host that merely ends in the same letters is not usercentrics")
	}

	h := newHarness(t)
	h.ups[svcStudio].handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Access-Control-Allow-Origin", "https://upstream.example")
		_, _ = w.Write([]byte("ok"))
	}
	resp, _ := h.req("GET", "studio."+testDomain, "/project/default")
	if v := resp.Header.Get("Content-Security-Policy"); strings.Contains(v, "usercentrics") || !strings.Contains(v, "js.stripe.com") {
		t.Fatalf("CSP through the proxy: %s", v)
	}
}
