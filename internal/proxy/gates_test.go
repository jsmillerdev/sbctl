package proxy

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/failover/fenced"
	"github.com/supavise/supavise/internal/storagemigrate/hold"
)

// testClock is a clock the gates read, moved by the test.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Now()} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// withClock gives the harness's server the clock and returns it.
func withClock(h *harness) *testClock {
	c := newTestClock()
	h.srv.now = c.Now
	return c
}

func TestFencedNodeAnswers503(t *testing.T) {
	h := newHarness(t)
	clock := withClock(h)
	paths := h.cfg.Paths()
	get := func(host string) (*http.Response, string) {
		return h.req("GET", host, "/rest/v1/x", "apikey", h.k.AnonKey)
	}
	if resp, _ := get(h.host(h.ref)); resp.StatusCode != 200 {
		t.Fatalf("before the fence: %d", resp.StatusCode)
	}

	if err := fenced.WriteNode(paths, fenced.Record{Epoch: 3, Leader: "n2", Reason: "node n2 leads at epoch 3", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// The answer of the gate is reused for a second.
	if resp, _ := get(h.host(h.ref)); resp.StatusCode != 200 {
		t.Errorf("inside the gate's second: %d", resp.StatusCode)
	}
	clock.advance(gateTTL)
	before := h.ups[svcRest].count()
	for _, host := range []string{h.host(h.ref), "api." + testDomain, "studio." + testDomain, "docs.customer.example", "elsewhere.example"} {
		resp, body := get(host)
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "60" ||
			!strings.Contains(body, "This Supavise node is fenced: node n2 leads at epoch 3") {
			t.Errorf("%s on a fenced node: %d %q (Retry-After %q)", host, resp.StatusCode, body, resp.Header.Get("Retry-After"))
		}
	}
	if h.ups[svcRest].count() != before || len(h.apiHit) != 0 {
		t.Error("a request of a fenced node reached a service")
	}
	// A browser can read the answer, and its preflight is answered.
	resp, _ := h.req("GET", h.host(h.ref), "/rest/v1/x", "Origin", "https://app.example")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Errorf("CORS on the answer: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = h.req("OPTIONS", h.host(h.ref), "/rest/v1/x", "Origin", "https://app.example", "Access-Control-Request-Method", "GET")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight on a fenced node: %d", resp.StatusCode)
	}

	// `supavise node rejoin` removes the record; within a second the node serves again.
	if err := fenced.ClearNode(paths); err != nil {
		t.Fatal(err)
	}
	clock.advance(gateTTL)
	if resp, _ := get(h.host(h.ref)); resp.StatusCode != 200 {
		t.Errorf("after the record was removed: %d", resp.StatusCode)
	}

	// A record that cannot be read fences too, as it does for the plane that starts the primaries.
	if err := os.WriteFile(fenced.NodePath(paths), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock.advance(gateTTL)
	if resp, _ := get(h.host(h.ref)); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("an unreadable record: %d", resp.StatusCode)
	}
}

// A record for one project (a project failover) is not the node's: the rest of the node carries on.
func TestFencedProjectDoesNotFenceTheNode(t *testing.T) {
	h := newHarness(t)
	clock := withClock(h)
	if err := fenced.WriteProject(h.cfg.Paths(), fenced.Record{Epoch: 2, Ref: h.ref, Reason: "project failover", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	clock.advance(gateTTL)
	if resp, _ := h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey); resp.StatusCode != 200 {
		t.Errorf("a project record fenced the node: %d", resp.StatusCode)
	}
}

// The cluster role can fence the node without a record (Cluster.Fenced).
func TestFencedRoleAnswers503(t *testing.T) {
	var role struct {
		sync.Mutex
		fenced bool
	}
	h := newHarness(t, func(o *Options) {
		o.Cluster = &Cluster{Fenced: func() bool { role.Lock(); defer role.Unlock(); return role.fenced }}
	})
	if resp, _ := h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey); resp.StatusCode != 200 {
		t.Fatalf("a node that is not fenced: %d", resp.StatusCode)
	}
	role.Lock()
	role.fenced = true
	role.Unlock()
	resp, body := h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "fenced") {
		t.Errorf("a fenced role: %d %q", resp.StatusCode, body)
	}
}

func TestStorageWritesWaitWhileTheyAreHeld(t *testing.T) {
	h := newHarness(t)
	clock := withClock(h)
	paths := h.cfg.Paths()
	storage := h.ups[svcStorage]
	send := func(method, target string, headers ...string) (*http.Response, string) {
		return h.project(method, target, headers...)
	}
	hit := func(method, target string) int {
		n := storage.count()
		resp, _ := send(method, target, "apikey", h.k.AnonKey)
		if resp.StatusCode == http.StatusServiceUnavailable {
			if storage.count() != n {
				t.Errorf("%s %s: refused and forwarded", method, target)
			}
			return resp.StatusCode
		}
		if storage.count() != n+1 {
			t.Errorf("%s %s: answered %d without reaching Storage", method, target, resp.StatusCode)
		}
		return resp.StatusCode
	}

	// No marker: writes go through.
	if code := hit("POST", "/storage/v1/object/bucket/a.txt"); code == http.StatusServiceUnavailable {
		t.Fatal("a write was refused with no marker")
	}

	now := clock.Now()
	if err := hold.Write(paths, hold.Marker{PID: 1, Since: now, Until: now.Add(time.Minute), Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	clock.advance(gateTTL)
	for _, c := range []struct{ method, target string }{
		{"POST", "/storage/v1/object/bucket/a.txt"},
		{"PUT", "/storage/v1/object/bucket/a.txt"},
		{"PATCH", "/storage/v1/upload/resumable/x"},
		{"DELETE", "/storage/v1/object/bucket/a.txt"},
		{"POST", "/storage/v1/bucket"},
	} {
		resp, body := send(c.method, c.target, "apikey", h.k.AnonKey)
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "5" || !strings.Contains(body, "writes are paused") {
			t.Errorf("%s %s while held: %d %q (Retry-After %q)", c.method, c.target, resp.StatusCode, body, resp.Header.Get("Retry-After"))
		}
	}
	// The S3 endpoint answers in S3's own document.
	resp, body := send("PUT", "/storage/v1/s3/bucket/key", "Authorization", "AWS4-HMAC-SHA256 Credential=x")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "5" ||
		resp.Header.Get("Content-Type") != "application/xml" || !strings.Contains(body, "<Code>ServiceUnavailable</Code>") {
		t.Errorf("S3 write while held: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	// Reads, and a preflight, go on; so do the writes to everything but Storage.
	for _, c := range []struct{ method, target string }{
		{"GET", "/storage/v1/object/public/bucket/a.txt"},
		{"HEAD", "/storage/v1/object/bucket/a.txt"},
		{"GET", "/storage/v1/s3/bucket/key"},
	} {
		if code := hit(c.method, c.target); code == http.StatusServiceUnavailable {
			t.Errorf("%s %s was held", c.method, c.target)
		}
	}
	n := storage.count()
	if resp, _ := send("OPTIONS", "/storage/v1/object/bucket/a.txt", "Origin", "https://app.example", "Access-Control-Request-Method", "POST"); resp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight while held: %d", resp.StatusCode)
	}
	if storage.count() != n {
		t.Error("a preflight reached Storage")
	}
	rest := h.ups[svcRest].count()
	if resp, _ := send("POST", "/rest/v1/table", "apikey", h.k.AnonKey); resp.StatusCode == http.StatusServiceUnavailable || h.ups[svcRest].count() != rest+1 {
		t.Errorf("a write to the Data API while Storage is held: %d", resp.StatusCode)
	}

	// The run ends and removes the marker: writes go through within a second.
	if err := hold.Remove(paths); err != nil {
		t.Fatal(err)
	}
	clock.advance(gateTTL)
	if code := hit("POST", "/storage/v1/object/bucket/a.txt"); code == http.StatusServiceUnavailable {
		t.Error("a write was refused after the marker was removed")
	}

	// A marker that expired holds nothing, whether or not it is still there.
	if err := hold.Write(paths, hold.Marker{PID: 1, Since: now, Until: clock.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	clock.advance(gateTTL)
	if code := hit("POST", "/storage/v1/object/bucket/a.txt"); code != http.StatusServiceUnavailable {
		t.Fatalf("held again: %d", code)
	}
	clock.advance(2 * time.Minute)
	if code := hit("POST", "/storage/v1/object/bucket/a.txt"); code == http.StatusServiceUnavailable {
		t.Error("an expired marker held a write")
	}
}
