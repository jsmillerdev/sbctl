package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

const (
	repEU = testRef + "-rr-eu-abc123" // on n2
	repUS = testRef + "-rr-us-def456" // on n3
)

// fakeCluster is the Cluster the balancer asks, with answers a test can change between requests.
type fakeCluster struct {
	mu   sync.Mutex
	self string
	rtt  map[string]time.Duration
	lag  map[string]time.Duration
	down map[string]bool // nodes the mesh has no session to
}

func (f *fakeCluster) set(fn func(*fakeCluster)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeCluster) cluster() *Cluster {
	return &Cluster{
		Self: func() string { f.mu.Lock(); defer f.mu.Unlock(); return f.self },
		RTT: func(n string) (time.Duration, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			d, ok := f.rtt[n]
			return d, ok
		},
		Lag: func(id string) (time.Duration, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			d, ok := f.lag[id]
			return d, ok
		},
		Connected: func(n string) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return !f.down[n]
		},
	}
}

// logBuffer collects the proxy's log lines as JSON.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// records returns the "request" lines logged so far.
func (l *logBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if m["msg"] == "request" {
			out = append(out, m)
		}
	}
	return out
}

// repHarness is a harness whose project has two replicas, on n2 and n3, each answered by a fake PostgREST
// of its own. The primary (the harness's PostgREST) is on n1.
type repHarness struct {
	*harness
	cl   *fakeCluster
	logs *logBuffer
	reps map[string]*upstream
}

func addNode(t *testing.T, reg registry.Registry, id, name string, state registry.NodeState) {
	t.Helper()
	if err := reg.CreateNode(context.Background(), &registry.Node{ID: id, Name: name, Region: "eu", State: state}); err != nil {
		t.Fatal(err)
	}
}

func addReplica(t *testing.T, reg registry.Registry, id, ref, node, status string) {
	t.Helper()
	ctx := context.Background()
	if err := reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: ref, NodeID: node}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetReplicaStatus(ctx, id, status, registry.ReplicaStepDone, ""); err != nil {
		t.Fatal(err)
	}
}

func newRepHarness(t *testing.T, mut ...func(*Options)) *repHarness {
	t.Helper()
	cl := &fakeCluster{self: "n1"}
	logs := &logBuffer{}
	h := newHarness(t, append([]func(*Options){func(o *Options) {
		o.Cluster = cl.cluster()
		o.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}}, mut...)...)
	rh := &repHarness{harness: h, cl: cl, logs: logs, reps: map[string]*upstream{}}
	addNode(t, h.reg, "n2", "eu-1", registry.NodeActive)
	addNode(t, h.reg, "n3", "us-1", registry.NodeActive)
	addReplica(t, h.reg, repEU, h.ref, "n2", string(registry.StatusActiveHealthy))
	addReplica(t, h.reg, repUS, h.ref, "n3", string(registry.StatusActiveHealthy))
	for _, id := range []string{repEU, repUS} {
		rh.reps[id] = newUpstream(t, id)
	}
	h.srv.replicaUpstreamFn = func(_ project, r replica) string { return rh.reps[r.identifier].addr() }
	eventually(t, "replicas reach the table", func() bool {
		m, ok := h.srv.table.resolve(repUS + ".api." + testDomain)
		return ok && m.kind == kindReplica
	})
	return rh
}

// get sends a GET with the publishable key to host and returns the response.
func (h *repHarness) get(host, target string) (*http.Response, string) {
	h.t.Helper()
	return h.req("GET", host, target, "apikey", h.k.PublishableKey)
}

func (h *repHarness) lbHost() string { return h.ref + "-lb.api." + testDomain }

func TestReplicaHostsResolve(t *testing.T) {
	h := newRepHarness(t)
	base := ".api." + testDomain
	if err := h.reg.PutRoute(context.Background(), registry.Route{Host: "docs.customer.example", Ref: h.ref, Kind: registry.RouteCustom}); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.PutRoute(context.Background(), registry.Route{Host: "shop" + base, Ref: h.ref, Kind: registry.RouteVanity}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "routes reach the table", func() bool { return h.srv.table.routeKind("docs.customer.example") == kindCustom })

	for _, c := range []struct {
		host string
		kind string
	}{
		{h.ref + base, kindDerived},
		{repEU + base, kindReplica},
		{strings.ToUpper(repUS) + base + ":443", kindReplica},
		{h.ref + "-lb" + base, kindBalancer},
		{"docs.customer.example", kindCustom},
		{"shop" + base, kindVanity},
		{testRef[:19] + "x" + base, ""}, // not a project
		{testRef[:19] + "x-lb" + base, ""},
		{testRef + "-rr-eu-zzz999" + base, ""}, // not a replica
		{"system-rr-eu-abc123" + base, ""},
		{"x" + h.ref + "-lb" + base, ""},
		{h.ref + "-lb.example.org", ""},
	} {
		if got := h.srv.table.routeKind(c.host); got != c.kind {
			t.Errorf("%s resolves to %q, want %q", c.host, got, c.kind)
		}
	}
	m, _ := h.srv.table.resolve(repEU + base)
	if m.project.ref != h.ref || m.project.home != "n1" || m.replica.identifier != repEU || m.replica.node != "n2" || len(m.project.replicas) != 2 {
		t.Errorf("replica host resolved to %+v", m)
	}
	if m.project.replicas[0].identifier != repEU || m.project.replicas[1].identifier != repUS {
		t.Errorf("replicas not oldest first: %+v", m.project.replicas)
	}
	// Custom and vanity hosts stay on the primary: their match is the plain project.
	if m, _ := h.srv.table.resolve("docs.customer.example"); m.kind != kindCustom || len(m.project.replicas) != 2 {
		t.Errorf("custom host: %+v", m)
	}
}

func TestLoadBalancerExistsWhileTheProjectHasAReplica(t *testing.T) {
	h := newRepHarness(t)
	ctx := context.Background()
	if resp, _ := h.get(h.lbHost(), "/rest/v1/x"); resp.StatusCode != 200 {
		t.Fatalf("balancer with replicas: %d", resp.StatusCode)
	}
	if err := h.reg.DeleteReplica(ctx, repEU); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.DeleteReplica(ctx, repUS); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the balancer goes with the last replica", func() bool { return h.srv.table.routeKind(h.lbHost()) == "" })
	if resp, _ := h.get(h.lbHost(), "/rest/v1/x"); resp.StatusCode != 404 {
		t.Errorf("balancer without replicas: %d, want 404", resp.StatusCode)
	}
	if resp, _ := h.get(repEU+".api."+testDomain, "/rest/v1/x"); resp.StatusCode != 404 {
		t.Errorf("removed replica's endpoint: %d, want 404", resp.StatusCode)
	}
	// The project's own host is unaffected.
	if resp, _ := h.get(h.host(h.ref), "/rest/v1/x"); resp.StatusCode != 200 {
		t.Errorf("project host: %d", resp.StatusCode)
	}
}

func TestReplicaEndpoint(t *testing.T) {
	h := newRepHarness(t)
	host := repEU + ".api." + testDomain
	k := h.k
	primary := h.ups[svcRest]

	t.Run("rest goes to the replica's PostgREST with the project's keys", func(t *testing.T) {
		resp, body := h.get(host, "/rest/v1/todos?select=*")
		if resp.StatusCode != 200 || !strings.Contains(body, repEU) {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
		got := h.reps[repEU].last(t)
		if got.Path != "/todos" || got.RawQuery != "select=*" || got.Header.Get("Authorization") != "Bearer "+k.AnonKey {
			t.Errorf("replica saw %s?%s auth %q", got.Path, got.RawQuery, got.Header.Get("Authorization"))
		}
		if got.Header.Get("X-Forwarded-Host") != host || got.Header.Get("X-Forwarded-Prefix") != "/rest/v1/" {
			t.Errorf("forwarded headers %v", got.Header)
		}
		if primary.count() != 0 || h.reps[repUS].count() != 0 {
			t.Error("another database was asked")
		}
		if resp.Header.Get(routeHeader) != "" {
			t.Errorf("a replica endpoint is not a balancer, but answered %s", resp.Header.Get(routeHeader))
		}
	})
	t.Run("a key is needed and the openapi document is for service_role", func(t *testing.T) {
		if resp, _ := h.req("GET", host, "/rest/v1/todos"); resp.StatusCode != 401 {
			t.Errorf("no key: %d", resp.StatusCode)
		}
		if resp, _ := h.get(host, "/rest/v1/"); resp.StatusCode != 403 {
			t.Errorf("openapi with the publishable key: %d", resp.StatusCode)
		}
		if resp, _ := h.req("GET", host, "/rest/v1/", "apikey", k.SecretKey); resp.StatusCode != 200 {
			t.Errorf("openapi with the secret key: %d", resp.StatusCode)
		}
	})
	t.Run("graphql goes to the replica", func(t *testing.T) {
		before := h.reps[repEU].count()
		if resp, _ := h.req("POST", host, "/graphql/v1", "apikey", k.PublishableKey); resp.StatusCode != 200 {
			t.Fatalf("graphql: %d", resp.StatusCode)
		}
		got := h.reps[repEU].last(t)
		if h.reps[repEU].count() != before+1 || got.Path != "/rpc/graphql" || got.Header.Get("Content-Profile") != "graphql_public" {
			t.Errorf("replica saw %s %v", got.Path, got.Header)
		}
	})
	t.Run("every method passes through to PostgREST", func(t *testing.T) {
		for _, m := range []string{"POST", "PATCH", "DELETE", "PUT"} {
			before := h.reps[repEU].count()
			if resp, _ := h.req(m, host, "/rest/v1/todos", "apikey", k.PublishableKey); resp.StatusCode != 200 {
				t.Errorf("%s: %d", m, resp.StatusCode)
			}
			if h.reps[repEU].count() != before+1 {
				t.Errorf("%s did not reach the replica", m)
			}
		}
	})
	t.Run("other prefixes are not a replica's to answer", func(t *testing.T) {
		counts := func() int {
			n := primary.count() + h.reps[repEU].count() + h.reps[repUS].count()
			for _, s := range []service{svcAuth, svcRealtime, svcStorage, svcFunctions} {
				n += h.ups[s].count()
			}
			return n
		}
		before := counts()
		for _, target := range []string{
			"/auth/v1/user", "/auth/v1/verify?token=x", "/storage/v1/object/public/a/b", "/storage/v1/s3/bucket/key",
			"/realtime/v1/websocket", "/realtime/v1/api/tenants", "/functions/v1/hello", "/.well-known/oauth-authorization-server",
			"/", "/rest", "/rest/v2/x", "/graphql", "/rest/v1/../auth/v1/user",
		} {
			resp, body := h.get(host, target)
			if resp.StatusCode != 404 || !strings.Contains(body, "no Route matched with those values") {
				t.Errorf("%s: %d %s", target, resp.StatusCode, body)
			}
		}
		if counts() != before {
			t.Error("a request for another prefix reached an upstream")
		}
	})
	t.Run("a preflight is answered like the project's", func(t *testing.T) {
		resp, _ := h.req("OPTIONS", host, "/rest/v1/todos", "Origin", "https://app.example", "Access-Control-Request-Method", "GET")
		if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example" {
			t.Errorf("preflight: %d %v", resp.StatusCode, resp.Header)
		}
	})
	t.Run("the replica's and the project's status gate it", func(t *testing.T) {
		ctx := context.Background()
		if err := h.reg.SetReplicaStatus(ctx, repEU, registry.ReplicaInit, "3_initiated_read_replica_setup", ""); err != nil {
			t.Fatal(err)
		}
		eventually(t, "replica status reaches the table", func() bool {
			m, _ := h.srv.table.resolve(host)
			return m.replica.status == registry.ReplicaInit
		})
		before := h.reps[repEU].count()
		resp, body := h.get(host, "/rest/v1/todos")
		if resp.StatusCode != 503 || !strings.Contains(body, "not active") || resp.Header.Get("Retry-After") == "" {
			t.Errorf("replica still setting up: %d %s", resp.StatusCode, body)
		}
		if h.reps[repEU].count() != before {
			t.Error("a replica that is not set up was asked")
		}
		// ACTIVE_UNHEALTHY and RESTARTING replicas are still asked: PostgREST decides.
		if err := h.reg.SetReplicaStatus(ctx, repEU, string(registry.StatusActiveUnhealthy), registry.ReplicaStepDone, ""); err != nil {
			t.Fatal(err)
		}
		eventually(t, "status reaches the table", func() bool {
			resp, _ := h.get(host, "/rest/v1/todos")
			return resp.StatusCode == 200
		})
		if err := h.reg.SetProjectStatus(ctx, h.ref, registry.StatusInactive); err != nil {
			t.Fatal(err)
		}
		eventually(t, "project status reaches the table", func() bool {
			resp, _ := h.get(host, "/rest/v1/todos")
			return resp.StatusCode == 503
		})
	})
}

// routeOf sends a request to the balancer host and returns the database that answered, from the
// header and from the access log.
func (h *repHarness) routeOf(method, target string, headers ...string) (string, int) {
	h.t.Helper()
	resp, body := h.req(method, h.lbHost(), target, append([]string{"apikey", h.k.PublishableKey}, headers...)...)
	if method != "HEAD" && resp.StatusCode == 200 {
		var got map[string]string
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			h.t.Fatalf("body %q: %v", body, err)
		}
		route := resp.Header.Get(routeHeader)
		if want := map[string]string{h.ref: "postgrest"}[route]; want != "" && got["upstream"] != want {
			h.t.Errorf("header says the primary, the body comes from %s", got["upstream"])
		} else if want == "" && got["upstream"] != route {
			h.t.Errorf("header says %s, the body comes from %s", route, got["upstream"])
		}
	}
	return resp.Header.Get(routeHeader), resp.StatusCode
}

func TestLoadBalancerPicksTheDatabaseOnThisNode(t *testing.T) {
	h := newRepHarness(t)
	for _, c := range []struct {
		self, want string
	}{
		{"n1", h.ref}, {"n2", repEU}, {"n3", repUS},
	} {
		h.cl.set(func(f *fakeCluster) { f.self = c.self })
		// RTT must not matter once a database runs here.
		h.cl.set(func(f *fakeCluster) {
			f.rtt = map[string]time.Duration{"n1": time.Millisecond, "n2": time.Second, "n3": time.Second}
		})
		for i := 0; i < 3; i++ {
			if got, st := h.routeOf("GET", "/rest/v1/todos"); st != 200 || got != c.want {
				t.Errorf("on %s: routed to %q (%d), want %s", c.self, got, st, c.want)
			}
		}
	}
	// HEAD is balanced too.
	h.cl.set(func(f *fakeCluster) { f.self = "n3" })
	if got, _ := h.routeOf("HEAD", "/rest/v1/todos"); got != repUS {
		t.Errorf("HEAD routed to %q, want %s", got, repUS)
	}
}

func TestLoadBalancerPicksTheNearestWhenNoneRunsHere(t *testing.T) {
	h := newRepHarness(t)
	h.cl.set(func(f *fakeCluster) { f.self = "n9" })
	for _, c := range []struct {
		name string
		rtt  map[string]time.Duration
		want string
	}{
		{"primary nearest", map[string]time.Duration{"n1": 3 * time.Millisecond, "n2": 30 * time.Millisecond, "n3": 80 * time.Millisecond}, h.ref},
		{"eu replica nearest", map[string]time.Duration{"n1": 90 * time.Millisecond, "n2": 20 * time.Millisecond, "n3": 70 * time.Millisecond}, repEU},
		{"us replica nearest", map[string]time.Duration{"n1": 90 * time.Millisecond, "n2": 70 * time.Millisecond, "n3": 2 * time.Millisecond}, repUS},
		{"a node with no reading yet is passed over", map[string]time.Duration{"n3": 200 * time.Millisecond}, repUS},
	} {
		h.cl.set(func(f *fakeCluster) { f.rtt = c.rtt })
		for i := 0; i < 3; i++ {
			if got, st := h.routeOf("GET", "/rest/v1/todos"); st != 200 || got != c.want {
				t.Errorf("%s: routed to %q (%d), want %s", c.name, got, st, c.want)
			}
		}
	}
}

func TestLoadBalancerRoundRobinWithoutAnyReading(t *testing.T) {
	h := newRepHarness(t)
	h.cl.set(func(f *fakeCluster) { f.self = "" })
	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		got, st := h.routeOf("GET", "/rest/v1/todos")
		if st != 200 {
			t.Fatalf("status %d", st)
		}
		seen[got]++
	}
	for _, id := range []string{h.ref, repEU, repUS} {
		if seen[id] != 3 {
			t.Errorf("round robin: %v", seen)
			break
		}
	}
}

func TestLoadBalancerSendsEverythingElseToThePrimary(t *testing.T) {
	h := newRepHarness(t, func(o *Options) { o.FunctionsEnabled = true; o.FunctionsProxyToken = "node-secret" })
	h.cl.set(func(f *fakeCluster) { f.self = "n2" }) // a replica runs here: only the rules below keep traffic off it
	k := h.k
	for _, c := range []struct {
		name, method, target string
		svc                  service
	}{
		{"insert", "POST", "/rest/v1/todos", svcRest},
		{"update", "PATCH", "/rest/v1/todos?id=eq.1", svcRest},
		{"delete", "DELETE", "/rest/v1/todos?id=eq.1", svcRest},
		{"rpc by POST", "POST", "/rest/v1/rpc/f", svcRest},
		{"graphql", "POST", "/graphql/v1", svcRest},
		{"graphql by GET", "GET", "/graphql/v1", svcRest},
		{"auth", "GET", "/auth/v1/user", svcAuth},
		{"auth by POST", "POST", "/auth/v1/token?grant_type=password", svcAuth},
		{"storage", "GET", "/storage/v1/object/public/a/b", svcStorage},
		{"functions", "GET", "/functions/v1/hello", svcFunctions},
		{"realtime api", "GET", "/realtime/v1/api/broadcast", svcRealtime},
	} {
		t.Run(c.name, func(t *testing.T) {
			up := h.ups[c.svc]
			before, after := up.count(), 0
			resp, body := h.req(c.method, h.lbHost(), c.target, "apikey", k.PublishableKey)
			if resp.StatusCode != 200 {
				t.Fatalf("%d %s", resp.StatusCode, body)
			}
			if after = up.count(); after != before+1 {
				t.Fatalf("the primary's %s was not asked", up.name)
			}
			if resp.Header.Get(routeHeader) != h.ref {
				t.Errorf("%s = %q, want the primary %q", routeHeader, resp.Header.Get(routeHeader), h.ref)
			}
			if h.reps[repEU].count()+h.reps[repUS].count() != 0 {
				t.Error("a replica was asked")
			}
		})
	}
	// The same rules as the project's own host: a key is needed, the host the client used reaches Storage as the project's.
	if resp, _ := h.req("GET", h.lbHost(), "/rest/v1/todos"); resp.StatusCode != 401 {
		t.Errorf("no key: %d", resp.StatusCode)
	}
	if got := h.ups[svcStorage].last(t).Header.Get("X-Forwarded-Host"); got != h.host(h.ref) {
		t.Errorf("Storage was told the host %q", got)
	}
	if got := h.ups[svcRealtime].last(t).Host; got != config.RealtimeInternalHost(h.ref) {
		t.Errorf("Realtime was sent host %q", got)
	}
}

func TestLoadBalancerSkipsReplicasThatCannotAnswer(t *testing.T) {
	h := newRepHarness(t)
	ctx := context.Background()
	h.cl.set(func(f *fakeCluster) { f.self = "n2" })
	if got, _ := h.routeOf("GET", "/rest/v1/x"); got != repEU {
		t.Fatalf("routed to %q before anything was wrong", got)
	}

	// A replica that is not ACTIVE_HEALTHY gets no read, although it runs here.
	for _, st := range []string{string(registry.StatusActiveUnhealthy), registry.ReplicaInit, "RESTARTING", "GOING_DOWN"} {
		if err := h.reg.SetReplicaStatus(ctx, repEU, st, registry.ReplicaStepDone, ""); err != nil {
			t.Fatal(err)
		}
		eventually(t, st+" reaches the table", func() bool {
			m, _ := h.srv.table.resolve(repEU + ".api." + testDomain)
			return m.replica.status == st
		})
		h.cl.set(func(f *fakeCluster) {
			f.rtt = map[string]time.Duration{"n1": 50 * time.Millisecond, "n3": 5 * time.Millisecond}
		})
		if got, _ := h.routeOf("GET", "/rest/v1/x"); got != repUS {
			t.Errorf("replica %s: routed to %q, want the nearest healthy one %s", st, got, repUS)
		}
	}
	if err := h.reg.SetReplicaStatus(ctx, repEU, string(registry.StatusActiveHealthy), registry.ReplicaStepDone, ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, "healthy again", func() bool { got, _ := h.routeOf("GET", "/rest/v1/x"); return got == repEU })

	// A replica on a node that is not active gets none either.
	if err := h.reg.SetNodeState(ctx, "n2", registry.NodeFenced); err != nil {
		t.Fatal(err)
	}
	eventually(t, "node state reaches the table", func() bool { return !h.srv.table.nodeActive("n2") })
	if got, _ := h.routeOf("GET", "/rest/v1/x"); got == repEU {
		t.Errorf("routed to %q on a fenced node", got)
	}
	// ... but the replica's own endpoint is still the replica's.
	if resp, _ := h.get(repEU+".api."+testDomain, "/rest/v1/x"); resp.StatusCode != 200 {
		t.Errorf("replica endpoint on a fenced node: %d", resp.StatusCode)
	}

	// When nothing else is healthy the primary answers.
	h.cl.set(func(f *fakeCluster) { f.self = "n3" })
	if err := h.reg.SetReplicaStatus(ctx, repUS, registry.ReplicaInit, "0_requested", ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both replicas out", func() bool { got, _ := h.routeOf("GET", "/rest/v1/x"); return got == h.ref })
}

func TestLoadBalancerLagLimit(t *testing.T) {
	h := newRepHarness(t, func(o *Options) { o.Config.Replicas.LBMaxLagSeconds = 5 })
	h.cl.set(func(f *fakeCluster) {
		f.self = "n2"
		f.lag = map[string]time.Duration{repEU: time.Second, repUS: time.Second}
	})
	if got, _ := h.routeOf("GET", "/rest/v1/x"); got != repEU {
		t.Fatalf("routed to %q with a lag under the limit", got)
	}
	h.cl.set(func(f *fakeCluster) { f.lag[repEU] = 6 * time.Second })
	if got, _ := h.routeOf("GET", "/rest/v1/x"); got == repEU {
		t.Errorf("routed to %q with a lag over the limit", got)
	}
	h.cl.set(func(f *fakeCluster) { f.lag[repEU] = 5 * time.Second }) // the limit itself is allowed
	if got, _ := h.routeOf("GET", "/rest/v1/x"); got != repEU {
		t.Errorf("routed to %q at the limit", got)
	}
	h.cl.set(func(f *fakeCluster) { delete(f.lag, repEU) })
	if got, _ := h.routeOf("GET", "/rest/v1/x"); got == repEU {
		t.Errorf("routed to %q with an unknown lag", got)
	}
	// The endpoint of the replica itself ignores the limit.
	h.cl.set(func(f *fakeCluster) { f.lag[repEU] = time.Hour })
	if resp, _ := h.get(repEU+".api."+testDomain, "/rest/v1/x"); resp.StatusCode != 200 {
		t.Errorf("replica endpoint: %d", resp.StatusCode)
	}
}

func TestLoadBalancerIgnoresLagWhenNoLimitIsSet(t *testing.T) {
	h := newRepHarness(t)
	h.cl.set(func(f *fakeCluster) {
		f.self = "n2"
		f.lag = map[string]time.Duration{repEU: 24 * time.Hour}
	})
	if got, _ := h.routeOf("GET", "/rest/v1/x"); got != repEU {
		t.Errorf("routed to %q", got)
	}
}

func TestCustomAndVanityHostsBypassTheBalancer(t *testing.T) {
	h := newRepHarness(t)
	ctx := context.Background()
	h.cl.set(func(f *fakeCluster) { f.self = "n2" }) // the balancer would pick the replica here
	for _, r := range []registry.Route{
		{Host: "api.customer.example", Ref: h.ref, Kind: registry.RouteCustom},
		{Host: "shop.api." + testDomain, Ref: h.ref, Kind: registry.RouteVanity},
	} {
		if err := h.reg.PutRoute(ctx, r); err != nil {
			t.Fatal(err)
		}
		eventually(t, r.Host+" reaches the table", func() bool { return h.srv.table.routeKind(r.Host) != "" })
		before := h.ups[svcRest].count()
		resp, _ := h.get(r.Host, "/rest/v1/todos")
		if resp.StatusCode != 200 || h.ups[svcRest].count() != before+1 || h.reps[repEU].count() != 0 {
			t.Errorf("%s: %d, primary asked %d times, replica %d", r.Host, resp.StatusCode, h.ups[svcRest].count()-before, h.reps[repEU].count())
		}
		if resp.Header.Get(routeHeader) != "" {
			t.Errorf("%s answered with %s", r.Host, routeHeader)
		}
	}
	// The project's own host does not balance either.
	if resp, _ := h.get(h.host(h.ref), "/rest/v1/todos"); resp.StatusCode != 200 || h.reps[repEU].count() != 0 || resp.Header.Get(routeHeader) != "" {
		t.Errorf("project host: %d, replica asked %d times", resp.StatusCode, h.reps[repEU].count())
	}
}

func TestLoadBalancerAccessLogNamesTheDatabase(t *testing.T) {
	h := newRepHarness(t)
	h.cl.set(func(f *fakeCluster) { f.self = "n3" })
	h.get(h.lbHost(), "/rest/v1/todos")
	h.req("POST", h.lbHost(), "/rest/v1/todos", "apikey", h.k.PublishableKey)
	h.get(repEU+".api."+testDomain, "/rest/v1/todos")
	h.get(h.host(h.ref), "/rest/v1/todos")

	recs := h.logs.records(t)
	if len(recs) != 4 {
		t.Fatalf("%d request records, want 4: %v", len(recs), recs)
	}
	want := []struct{ kind, route string }{{kindBalancer, repUS}, {kindBalancer, h.ref}, {kindReplica, ""}, {"project", ""}}
	for i, w := range want {
		got, _ := recs[i]["load_balancer_redirect_identifier"].(string)
		if recs[i]["kind"] != w.kind || got != w.route || recs[i]["ref"] != h.ref {
			t.Errorf("record %d: kind %v route %q ref %v, want %s %q", i, recs[i]["kind"], got, recs[i]["ref"], w.kind, w.route)
		}
	}
	if _, has := recs[3]["load_balancer_redirect_identifier"]; has {
		t.Error("a request that did not go through the balancer carries the field")
	}
}

// TestReplicaBeyondThePortRange: a project whose sequence leaves no room for replica ports is never
// sent to a replica port, which would be another project's.
func TestReplicaBeyondThePortRange(t *testing.T) {
	h := newRepHarness(t)
	h.srv.replicaUpstreamFn = nil
	h.srv.cfg.Ports.ProjectBase = h.srv.cfg.ReplicaBase() + 3*h.srv.table.project(h.ref).seq // MaxReplicaSeq is now below the seq
	h.cl.set(func(f *fakeCluster) { f.self = "n2" })
	if got, _ := h.routeOf("GET", "/rest/v1/x"); got != h.ref {
		t.Errorf("routed to %q", got)
	}
	if resp, _ := h.get(repEU+".api."+testDomain, "/rest/v1/x"); resp.StatusCode != 503 {
		t.Errorf("replica endpoint: %d, want 503", resp.StatusCode)
	}
}

func TestReplicaPorts(t *testing.T) {
	h := newRepHarness(t)
	h.srv.replicaUpstreamFn = nil
	p := h.srv.table.project(h.ref)
	addr, ok := h.srv.replicaAddr(p, p.replicas[0])
	want := h.srv.cfg.ReplicaPorts(h.ref, p.seq).PostgREST
	if !ok || addr != "127.0.0.1:"+itoa(want) {
		t.Errorf("replica address %q (%v), want port %d", addr, ok, want)
	}
	if want == h.srv.cfg.PortsFor(h.ref, p.seq).PostgREST {
		t.Error("the replica's PostgREST port is the primary's")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// project is the table's entry for ref.
func (t *table) project(ref string) project {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.projects[ref]
}

// has reports whether any log line mentions s.
func (l *logBuffer) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.b.String(), s)
}

// newRegistryWithProject is a Memory registry holding the harness project.
func newRegistryWithProject(t *testing.T) *registry.Memory {
	t.Helper()
	reg := registry.NewMemory()
	if err := reg.CreateProject(context.Background(), &registry.Project{Ref: testRef, Name: "p", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestAllowHostReplicaAndBalancer: the endpoints of a replica and the balancer of its project are one
// label under api.<domain>, so they are certified like a project host: on demand where there is no
// wildcard, and never where the wildcard covers them.
func TestAllowHostReplicaAndBalancer(t *testing.T) {
	base := "example.com"
	for mode, want := range map[string]bool{"http01": true, "auto": false, "dns01": false} {
		s := tlsServer(t, func(c *config.Config) {
			c.Domain, c.TLS.Mode = base, mode
			if mode != "http01" {
				c.TLS.DNSProvider = "route53"
			}
		})
		addNode(t, s.reg(), "n2", "eu-1", registry.NodeActive)
		addReplica(t, s.reg(), repEU, testRef, "n2", string(registry.StatusActiveHealthy))
		if err := s.table.reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{repEU + ".api." + base, testRef + "-lb.api." + base} {
			if err := s.allowHost(context.Background(), host); (err == nil) != want {
				t.Errorf("mode %s: allowHost(%s) = %v, want allowed=%v", mode, host, err, want)
			}
		}
		// A host that only has the shape of one is not served.
		for _, host := range []string{testRef + "-rr-eu-zzz999.api." + base, "zzzzzzzzzzzzzzzzzzzz-lb.api." + base} {
			if err := s.allowHost(context.Background(), host); err == nil {
				t.Errorf("mode %s: allowHost(%s) allowed", mode, host)
			}
		}
		// The redirect on :80 knows them too.
		if !s.serves(repEU+".api."+base) || !s.serves(testRef+"-lb.api."+base) {
			t.Errorf("mode %s: the :80 redirect does not serve the replica or balancer host", mode)
		}
	}
}

// A replica on a node the mesh has no session to is not asked, although its heartbeat time is the last one
// it answered and its row says it is healthy.
func TestLoadBalancerSkipsReplicasOnNodesWithoutASession(t *testing.T) {
	h := newRepHarness(t)
	h.cl.set(func(f *fakeCluster) {
		f.self = "n9"
		f.rtt = map[string]time.Duration{"n1": 90 * time.Millisecond, "n2": 5 * time.Millisecond, "n3": 20 * time.Millisecond}
	})
	if got, _ := h.routeOf("GET", "/rest/v1/x"); got != repEU {
		t.Fatalf("routed to %q with every node connected", got)
	}
	h.cl.set(func(f *fakeCluster) { f.down = map[string]bool{"n2": true} })
	if got, _ := h.routeOf("GET", "/rest/v1/x"); got != repUS {
		t.Errorf("routed to %q with no session to n2, want %s", got, repUS)
	}
	h.cl.set(func(f *fakeCluster) { f.down = map[string]bool{"n2": true, "n3": true} })
	if got, _ := h.routeOf("GET", "/rest/v1/x"); got != h.ref {
		t.Errorf("routed to %q with no session to either replica", got)
	}
	// This node's own replica needs no session.
	h.cl.set(func(f *fakeCluster) { f.self = "n3"; f.down = map[string]bool{"n3": true} })
	if got, _ := h.routeOf("GET", "/rest/v1/x"); got != repUS {
		t.Errorf("routed to %q: the replica on this node needs no session", got)
	}
}

// Every answer of a balancer host names the database, the refusals before a read has picked one included.
func TestLoadBalancerNamesTheDatabaseOnEveryAnswer(t *testing.T) {
	h := newRepHarness(t)
	ctx := context.Background()
	lb := h.lbHost()
	check := func(what string, resp *http.Response, status int) {
		t.Helper()
		if resp.StatusCode != status || resp.Header.Get(routeHeader) != h.ref {
			t.Errorf("%s: %d with %s %q, want %d and %s", what, resp.StatusCode, routeHeader, resp.Header.Get(routeHeader), status, h.ref)
		}
	}
	resp, _ := h.req("GET", lb, "/rest/v1/x")
	check("no key", resp, 401)
	resp, _ = h.get(lb, "/rest/v1/")
	check("the openapi document with a publishable key", resp, 403)
	resp, _ = h.get(lb, "/nothing/here")
	check("a path nobody serves", resp, 404)
	resp, _ = h.req("OPTIONS", lb, "/rest/v1/x", "Origin", "https://app.example", "Access-Control-Request-Method", "GET")
	check("a preflight", resp, 204)
	if err := h.reg.SetProjectStatus(ctx, h.ref, registry.StatusInactive); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the project is inactive", func() bool {
		resp, _ := h.get(lb, "/rest/v1/x")
		return resp.StatusCode == 503
	})
	resp, _ = h.get(lb, "/rest/v1/x")
	check("an inactive project", resp, 503)

	for _, rec := range h.logs.records(t) {
		if id, _ := rec["load_balancer_redirect_identifier"].(string); rec["kind"] == kindBalancer && id != h.ref {
			t.Errorf("the access log of a refused request on the balancer names %q, want %s: %v", id, h.ref, rec)
		}
	}
}

// A read that picked a replica which cannot be reached is repeated once on the primary; a replica that
// answered, whatever it said, is not asked twice, and a request that cannot be repeated is not.
func TestLoadBalancerRetriesTheReadOnThePrimary(t *testing.T) {
	h := newRepHarness(t)
	h.cl.set(func(f *fakeCluster) { f.self = "n2" })
	primary := h.ups[svcRest]
	h.reps[repEU].srv.Close() // n2's PostgREST is gone: connection refused

	for _, m := range []string{"GET", "HEAD"} {
		before := primary.count()
		resp, _ := h.req(m, h.lbHost(), "/rest/v1/todos?select=*", "apikey", h.k.PublishableKey)
		if resp.StatusCode != 200 || resp.Header.Get(routeHeader) != h.ref || primary.count() != before+1 {
			t.Errorf("%s: %d from %q, primary asked %d times", m, resp.StatusCode, resp.Header.Get(routeHeader), primary.count()-before)
		}
		if got := primary.last(t); got.Path != "/todos" || got.RawQuery != "select=*" || got.Header.Get("Authorization") != "Bearer "+h.k.AnonKey {
			t.Errorf("%s: the primary saw %s?%s auth %q", m, got.Path, got.RawQuery, got.Header.Get("Authorization"))
		}
	}
	recs := h.logs.records(t)
	if last := recs[len(recs)-1]; last["load_balancer_redirect_identifier"] != h.ref || last["status"] != float64(200) {
		t.Errorf("the access log of the retried read: %v", last)
	}

	// A body cannot be sent twice.
	before := primary.count()
	if resp, _ := h.reqBody("GET", h.lbHost(), "/rest/v1/todos", `{"a":1}`, "apikey", h.k.PublishableKey); resp.StatusCode != http.StatusBadGateway || primary.count() != before {
		t.Errorf("a GET with a body: %d, primary asked %d times", resp.StatusCode, primary.count()-before)
	}

	// A replica that answers with an error has answered.
	h.cl.set(func(f *fakeCluster) { f.self = "n3" })
	h.reps[repUS].mu.Lock()
	h.reps[repUS].handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
	h.reps[repUS].mu.Unlock()
	before = primary.count()
	if resp, _ := h.get(h.lbHost(), "/rest/v1/todos"); resp.StatusCode != http.StatusInternalServerError || resp.Header.Get(routeHeader) != repUS || primary.count() != before {
		t.Errorf("a replica that answered 500: %d from %q, primary asked %d times", resp.StatusCode, resp.Header.Get(routeHeader), primary.count()-before)
	}
}

// A vanity row made before -rr- and -lb were reserved keeps serving its host; one that names a project's
// balancer cannot take the balancer over, because the balancer is resolved first.
func TestVanityRowsOfReplicaHostShapedNames(t *testing.T) {
	h := newRepHarness(t)
	ctx := context.Background()
	base := ".api." + testDomain
	for _, host := range []string{"my-lb" + base, "shop-rr-eu" + base, h.ref + "-lb" + base} {
		if err := h.reg.PutRoute(ctx, registry.Route{Host: host, Ref: h.ref, Kind: registry.RouteVanity}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "routes reach the table", func() bool { return h.srv.table.routeKind("my-lb"+base) != "" })
	eventually(t, "routes reach the table", func() bool { return h.srv.table.routeKind("shop-rr-eu"+base) != "" })
	if got := h.srv.table.routeKind("my-lb" + base); got != kindVanity {
		t.Errorf("a vanity row for my-lb resolves to %q", got)
	}
	if got := h.srv.table.routeKind("shop-rr-eu" + base); got != kindVanity {
		t.Errorf("a vanity row for shop-rr-eu resolves to %q", got)
	}
	if got := h.srv.table.routeKind(h.ref + "-lb" + base); got != kindBalancer {
		t.Errorf("the project's balancer host resolves to %q, a vanity row must not take it", got)
	}
}

func TestRetryable(t *testing.T) {
	timeout := &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}
	for name, c := range map[string]struct {
		err  error
		want bool
	}{
		"connection refused":     {&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		"dial timeout":           {&net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}, true},
		"reset before an answer": {&net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		"closed without answer":  {io.EOF, true},
		"slow to answer":         {timeout, false},
		"the client left":        {context.Canceled, false},
	} {
		if got := retryable(c.err); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}
