package proxy

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/notice"
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

// Studio's sign-in goes to api.<domain>/auth/v1 (its NEXT_PUBLIC_GOTRUE_URL and the system
// GoTrue's API_EXTERNAL_URL): the edge forwards it to the system project's GoTrue.
func TestDashboardAuthIsForwardedToTheSystemGoTrue(t *testing.T) {
	h := newHarness(t)
	var addrFor string
	h.srv.upstreamFn = func(s service, p project) string {
		if s == svcAuth {
			addrFor = p.ref
		}
		return h.ups[s].addr()
	}
	api := "api." + testDomain
	studioOrigin := "http://studio." + testDomain

	resp, body := h.reqBody("POST", api, "/auth/v1/token?grant_type=password", `{"email":"a@b.c","password":"x"}`,
		"Origin", studioOrigin, "Content-Type", "application/json")
	if resp.StatusCode != 200 {
		t.Fatalf("token: %d %s", resp.StatusCode, body)
	}
	got := h.ups[svcAuth].last(t)
	if got.Path != "/token" || got.RawQuery != "grant_type=password" || addrFor != "system" {
		t.Fatalf("upstream saw %s?%s for project %q", got.Path, got.RawQuery, addrFor)
	}
	if got.Header.Get("Apikey") != "" || got.Header.Get("Authorization") != "" {
		t.Errorf("no credentials must be invented: %v", got.Header)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != studioOrigin || resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("CORS for the dashboard origin: %v", resp.Header)
	}
	if n := len(resp.Header.Values("Access-Control-Allow-Origin")); n != 1 {
		t.Errorf("%d Access-Control-Allow-Origin headers, want 1 (upstream's own must be replaced)", n)
	}

	// Any other origin gets no CORS headers (the browser blocks the call).
	resp, _ = h.req("GET", api, "/auth/v1/settings", "Origin", "https://evil.example")
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("foreign origin allowed: %v", resp.Header)
	}
	// Preflight from the dashboard answers without reaching GoTrue; from elsewhere it is refused.
	before := h.ups[svcAuth].count()
	resp, _ = h.req("OPTIONS", api, "/auth/v1/token", "Origin", studioOrigin, "Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "authorization,content-type,x-client-info")
	if resp.StatusCode != 204 || !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "x-client-info") {
		t.Errorf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ = h.req("OPTIONS", api, "/auth/v1/token", "Origin", "https://evil.example", "Access-Control-Request-Method", "POST"); resp.StatusCode != 403 {
		t.Errorf("foreign preflight: %d", resp.StatusCode)
	}
	if h.ups[svcAuth].count() != before {
		t.Error("preflight reached GoTrue")
	}
	// The Management API is untouched.
	if resp, _ := h.req("GET", api, "/v1/projects"); resp.StatusCode != http.StatusTeapot {
		t.Errorf("management API: %d", resp.StatusCode)
	}
	// Traversal cannot step out of /auth/v1.
	if resp, _ := h.req("GET", api, "/auth/v1/../../v1/projects"); resp.StatusCode != 404 {
		t.Errorf("path traversal out of /auth/v1: %d, want 404", resp.StatusCode)
	}
}

// The answer stays empty while the operator has a window announced or an upgrade runs, and
// whatever is available: Studio would draw any incident as an unexplained outage.
func TestIncidentBannerStaysEmptyDuringMaintenanceAndUpgrade(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	incidents := func() []any {
		t.Helper()
		resp, body := h.req("GET", "studio."+testDomain, "/api/incident-banner")
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%v: %q", err, body)
		}
		return out["incidents"].([]any)
	}
	sysdir := filepath.Join(h.cfg.StateDir, "system")
	if err := os.MkdirAll(sysdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysdir, "update.json"), []byte(`{"latest":"v9.9.9","available":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := notice.WriteMaintenance(h.cfg.Paths(), notice.Maintenance{Message: "Database maintenance", StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	up, _ := json.Marshal(notice.Upgrade{Phase: "rollout", From: "v1.0.0", To: "v1.1.0", StartedAt: now.Add(-time.Minute)})
	if err := os.WriteFile(filepath.Join(sysdir, "upgrade.json"), up, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := incidents(); len(got) != 0 {
		t.Errorf("Studio would show %v as an outage", got)
	}
	// HEAD still answers 200 with no body, and the answer is never cached.
	resp, body := h.req("HEAD", "studio."+testDomain, "/api/incident-banner")
	if resp.StatusCode != 200 || body != "" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("HEAD: %d %q %q", resp.StatusCode, body, resp.Header.Get("Cache-Control"))
	}
}
