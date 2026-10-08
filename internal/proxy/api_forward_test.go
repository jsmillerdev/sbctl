package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// On a node that does not lead, api.<domain> is forwarded to the Management API's loopback listener (the
// mesh binds it to the leader's); the follower's own registry is read-only. The API's answer, its CORS
// headers included, goes back as it is.
func TestFollowerForwardsTheManagementAPI(t *testing.T) {
	admin := newUpstream(t, "admin")
	admin.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://studio.example.test")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.WriteHeader(http.StatusAccepted)
	}
	leads := false
	h := newHarness(t, func(o *Options) {
		o.Config.Listen.Admin = admin.addr()
		o.Cluster = &Cluster{Leader: func() bool { return leads }}
	})

	resp, _ := h.reqBody("POST", "api."+testDomain, "/platform/projects/abc?x=1%2F2", `{"name":"n"}`,
		"Origin", "https://studio.example.test", "Authorization", "Bearer t", "Content-Type", "application/json")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: the leader's answer is the follower's", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://studio.example.test" || resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("the API's CORS headers were not kept: %v", resp.Header)
	}
	got := admin.last(t)
	if got.Method != "POST" || got.Path != "/platform/projects/abc" || got.RawQuery != "x=1%2F2" || got.Body != `{"name":"n"}` ||
		got.Host != "api."+testDomain || got.Header.Get("Authorization") != "Bearer t" {
		t.Errorf("the leader saw %+v", got)
	}
	if got.Header.Get("X-Forwarded-Host") != "api."+testDomain || got.Header.Get("X-Forwarded-For") == "" {
		t.Errorf("forwarded headers: %v", got.Header)
	}
	if len(h.apiHit) != 0 {
		t.Error("the follower served the API against its own registry")
	}

	// /internal/ is not for the edge, forwarded or not; the dashboard's GoTrue stays with the proxy.
	n := admin.count()
	if resp, _ := h.req("GET", "api."+testDomain, "/internal/mail-templates/x"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/internal on a follower: %d", resp.StatusCode)
	}
	if admin.count() != n {
		t.Error("/internal reached the leader's listener")
	}

	// A leader serves it itself.
	leads = true
	if resp, _ := h.req("GET", "api."+testDomain, "/platform/projects"); resp.StatusCode != http.StatusTeapot || admin.count() != n {
		t.Errorf("on the leader: %d, forwarded %d", resp.StatusCode, admin.count()-n)
	}
	if got := <-h.apiHit; !strings.HasSuffix(got, "/platform/projects") {
		t.Errorf("the API saw %q", got)
	}
}

// With the leader's listener gone the follower says so.
func TestFollowerReportsAnUnreachableLeader(t *testing.T) {
	admin := newUpstream(t, "admin")
	addr := admin.addr()
	admin.srv.Close()
	h := newHarness(t, func(o *Options) {
		o.Config.Listen.Admin = addr
		o.Cluster = &Cluster{Leader: func() bool { return false }}
	})
	if resp, body := h.req("GET", "api."+testDomain, "/platform/projects"); resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "Upstream unavailable") {
		t.Errorf("%d %q", resp.StatusCode, body)
	}
}
