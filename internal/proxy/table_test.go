package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

func TestKeysAreCachedPerRef(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 5; i++ {
		if resp, _ := h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	if n := h.keys.callCount(h.ref); n != 1 {
		t.Fatalf("key source called %d times for 5 requests, want 1", n)
	}
}

func TestKeyCacheExpires(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	var mu sync.Mutex
	h.srv.table.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey)
	mu.Lock()
	now = now.Add(keysTTL + time.Second)
	mu.Unlock()
	h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey)
	if n := h.keys.callCount(h.ref); n != 2 {
		t.Fatalf("key source called %d times across the TTL, want 2", n)
	}
}

func TestKeyInvalidationOnSecretChange(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if resp, _ := h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey); resp.StatusCode != 200 {
		t.Fatalf("before rotation: %d", resp.StatusCode)
	}
	// Rotate: new secret, new keys; the registry announces a project_secrets change.
	rotated := testKeys(t, h.ref)
	h.keys.set(h.ref, rotated)
	if err := h.reg.PutSecret(ctx, h.ref, secrets.NameJWTSecret, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "old key rejected after rotation", func() bool {
		resp, _ := h.project("GET", "/rest/v1/x", "apikey", h.k.AnonKey)
		return resp.StatusCode == 401
	})
	if resp, _ := h.project("GET", "/rest/v1/x", "apikey", rotated.PublishableKey); resp.StatusCode != 200 {
		t.Fatalf("new key: %d", resp.StatusCode)
	}
}

func TestProjectAddedAndRemoved(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const ref2 = "zyxwvutsrqponmlkjihg"
	k2 := testKeys(t, ref2)
	h.keys.set(ref2, k2)

	if resp, _ := h.req("GET", h.host(ref2), "/rest/v1/x", "apikey", k2.AnonKey); resp.StatusCode != 404 {
		t.Fatalf("unregistered project: %d", resp.StatusCode)
	}
	if err := h.reg.CreateProject(ctx, &registry.Project{Ref: ref2, Name: "p2", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "new project routable", func() bool {
		resp, _ := h.req("GET", h.host(ref2), "/rest/v1/x", "apikey", k2.AnonKey)
		return resp.StatusCode == 200
	})
	// Projects never see each other's keys.
	if resp, _ := h.req("GET", h.host(ref2), "/rest/v1/x", "apikey", h.k.AnonKey); resp.StatusCode != 401 {
		t.Fatalf("foreign key accepted: %d", resp.StatusCode)
	}
	if err := h.reg.DeleteProject(ctx, ref2); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deleted project unroutable", func() bool {
		resp, _ := h.req("GET", h.host(ref2), "/rest/v1/x", "apikey", k2.AnonKey)
		return resp.StatusCode == 404
	})
}

func TestSystemProjectIsNotRouted(t *testing.T) {
	h := newHarness(t)
	if resp, _ := h.req("GET", "system.api."+testDomain, "/rest/v1/x"); resp.StatusCode != 404 {
		t.Fatalf("system project reachable through the edge: %d", resp.StatusCode)
	}
}

func TestCustomRoutes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const host = "db.customer.example"
	if resp, _ := h.req("GET", host, "/rest/v1/x", "apikey", h.k.AnonKey); resp.StatusCode != 404 {
		t.Fatalf("before route: %d", resp.StatusCode)
	}
	if err := h.reg.PutRoute(ctx, registry.Route{Host: host, Ref: h.ref, Kind: "custom"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "custom host routable", func() bool {
		resp, _ := h.req("GET", host+":443", "/rest/v1/x", "apikey", h.k.AnonKey)
		return resp.StatusCode == 200
	})
	if err := h.reg.DeleteRoute(ctx, host); err != nil {
		t.Fatal(err)
	}
	eventually(t, "custom host removed", func() bool {
		resp, _ := h.req("GET", host, "/rest/v1/x", "apikey", h.k.AnonKey)
		return resp.StatusCode == 404
	})
}

// flakyRegistry hands out change channels the test controls, to exercise the
// resubscribe path that a real LISTEN connection drop takes.
type flakyRegistry struct {
	registry.Registry
	mu       sync.Mutex
	subs     []chan registry.Change
	failNext int32
	count    atomic.Int32
}

func (f *flakyRegistry) Subscribe(ctx context.Context) (<-chan registry.Change, error) {
	f.count.Add(1)
	if atomic.AddInt32(&f.failNext, -1) >= 0 {
		return nil, io.ErrUnexpectedEOF
	}
	ch := make(chan registry.Change, 8)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	return ch, nil
}

func (f *flakyRegistry) sub(i int) chan registry.Change {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < len(f.subs) {
		return f.subs[i]
	}
	return nil
}

func TestResubscribeAndFullReloadWhenChannelCloses(t *testing.T) {
	mem := registry.NewMemory()
	fr := &flakyRegistry{Registry: mem, failNext: 1} // the first subscribe fails, then it recovers
	h := newHarness(t, func(o *Options) { o.Registry = fr })
	ctx := context.Background()

	eventually(t, "first subscription", func() bool { return fr.sub(0) != nil })
	if fr.count.Load() < 2 {
		t.Fatalf("subscribe was not retried after failing: %d calls", fr.count.Load())
	}

	// A project appears while nobody delivers notifications (dropped LISTEN connection).
	const ref2 = "qqqqqqqqqqwwwwwwwwww"
	k2 := testKeys(t, ref2)
	h.keys.set(ref2, k2)
	if err := mem.CreateProject(ctx, &registry.Project{Ref: ref2, Name: "p2", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	if resp, _ := h.req("GET", h.host(ref2), "/rest/v1/x", "apikey", k2.AnonKey); resp.StatusCode != 404 {
		t.Fatalf("project visible without a notification: %d", resp.StatusCode)
	}

	// The stream closes: the proxy resubscribes and reloads everything.
	close(fr.sub(0))
	eventually(t, "second subscription", func() bool { return fr.sub(1) != nil })
	eventually(t, "full reload after resubscribe", func() bool {
		resp, _ := h.req("GET", h.host(ref2), "/rest/v1/x", "apikey", k2.AnonKey)
		return resp.StatusCode == 200
	})

	// Changes delivered on the new stream are applied.
	fr.sub(1) <- registry.Change{Table: "projects", Op: "delete", Key: ref2}
	if err := mem.DeleteProject(ctx, ref2); err != nil {
		t.Fatal(err)
	}
	eventually(t, "change on new stream applied", func() bool {
		resp, _ := h.req("GET", h.host(ref2), "/rest/v1/x", "apikey", k2.AnonKey)
		return resp.StatusCode == 404
	})
}

func TestNewFailsWhenRegistryIsBroken(t *testing.T) {
	h := newHarness(t)
	_, err := New(Options{Config: h.cfg, Registry: brokenRegistry{h.reg}, Keys: h.keys, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil {
		t.Fatal("New succeeded with a registry that cannot list projects")
	}
}

type brokenRegistry struct{ registry.Registry }

func (brokenRegistry) ListProjects(context.Context) ([]registry.Project, error) {
	return nil, io.ErrUnexpectedEOF
}

func TestOptionsValidation(t *testing.T) {
	h := newHarness(t)
	for name, o := range map[string]Options{
		"no config":   {Registry: h.reg, Keys: h.keys},
		"no registry": {Config: h.cfg, Keys: h.keys},
		"no keys":     {Config: h.cfg, Registry: h.reg},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}

func TestRegistryKeys(t *testing.T) {
	reg := registry.NewMemory()
	ctx := context.Background()
	sec, err := secrets.New(secrets.RandomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	rk := RegistryKeys{Registry: reg, Secrets: sec}
	if _, err := rk.Keys(ctx, testRef); err != registry.ErrNotFound {
		t.Fatalf("unknown ref: %v", err)
	}
	if err := reg.CreateProject(ctx, &registry.Project{Ref: testRef, Name: "p"}); err != nil {
		t.Fatal(err)
	}
	want := testKeys(t, testRef)
	for name, val := range want.Map() {
		blob, err := sec.Seal([]byte(val))
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.PutSecret(ctx, testRef, name, blob); err != nil {
			t.Fatal(err)
		}
	}
	got, err := rk.Keys(ctx, testRef)
	if err != nil {
		t.Fatal(err)
	}
	if got.JWTSecret != want.JWTSecret || got.PublishableKey != want.PublishableKey || got.ServiceRoleKey != want.ServiceRoleKey {
		t.Fatalf("keys differ after seal/open: %+v", got)
	}
}

// blockingKeys returns its first call's keys only after release is closed, to hold
// a fetch in flight while the test invalidates the cache.
type blockingKeys struct {
	mu      sync.Mutex
	calls   int
	first   *secrets.ProjectKeys
	later   *secrets.ProjectKeys
	entered chan struct{}
	release chan struct{}
}

func (b *blockingKeys) Keys(_ context.Context, _ string) (*secrets.ProjectKeys, error) {
	b.mu.Lock()
	b.calls++
	n := b.calls
	b.mu.Unlock()
	if n == 1 {
		close(b.entered)
		<-b.release
		return b.first, nil
	}
	return b.later, nil
}

// TestInvalidationBeatsInFlightFetch: a fetch that read the old secrets before a
// rotation committed must not put them back into the cache after the invalidation.
func TestInvalidationBeatsInFlightFetch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		invalidate func(tb *table)
	}{
		{"per-ref", func(tb *table) { tb.invalidateKeys(testRef) }},
		{"reload", func(tb *table) {
			if err := tb.reload(context.Background()); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bk := &blockingKeys{first: testKeys(t, testRef), later: testKeys(t, testRef), entered: make(chan struct{}), release: make(chan struct{})}
			cfg := config.Default()
			cfg.Domain = testDomain
			tb := newTable(cfg, registry.NewMemory(), bk, slog.New(slog.NewTextHandler(io.Discard, nil)))
			done := make(chan *secrets.ProjectKeys, 1)
			go func() {
				k, _ := tb.projectKeys(context.Background(), testRef)
				done <- k
			}()
			<-bk.entered
			tc.invalidate(tb)
			close(bk.release)
			if k := <-done; k != bk.first {
				t.Fatal("the in-flight request should still get its own result")
			}
			k, err := tb.projectKeys(context.Background(), testRef)
			if err != nil {
				t.Fatal(err)
			}
			if k != bk.later {
				t.Fatal("stale keys cached after invalidation: the old keys stay valid for keysTTL")
			}
		})
	}
}

// TestRouteCannotTakeOverOwnedHosts: registry routes naming another project's
// derived host, api.<domain> or studio.<domain> are ignored.
func TestRouteCannotTakeOverOwnedHosts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const other = "bbbbbbbbbbbbbbbbbbbb"
	h.keys.set(other, testKeys(t, other))
	if err := h.reg.CreateProject(ctx, &registry.Project{Ref: other, Name: "p2", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{h.host(h.ref), "api." + testDomain, "studio." + testDomain} {
		if err := h.reg.PutRoute(ctx, registry.Route{Host: host, Ref: other, Kind: "custom"}); err != nil {
			t.Fatal(err)
		}
	}
	// A legitimate route added last proves the table has processed the earlier rows.
	if err := h.reg.PutRoute(ctx, registry.Route{Host: "ok.customer.example", Ref: other, Kind: "custom"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "route table refreshed", func() bool { return h.srv.table.routeKind("ok.customer.example") == "custom" })
	p, ok := h.srv.table.lookup(h.host(h.ref))
	if !ok || p.ref != h.ref {
		t.Fatalf("derived host resolved to %q (ok=%v), want %q", p.ref, ok, h.ref)
	}
	for _, host := range []string{"api." + testDomain, "studio." + testDomain} {
		if k := h.srv.table.routeKind(host); k != "" {
			t.Fatalf("%s is routed as %q", host, k)
		}
	}
}

// TestUpstreamPorts checks the service -> loopback port map that every other test
// replaces with upstreamFn.
func TestUpstreamPorts(t *testing.T) {
	cfg := config.Default()
	cfg.Domain = testDomain
	cfg.TLS.Mode = "off"
	s, err := New(Options{Config: cfg, Registry: registry.NewMemory(), Keys: newFakeKeys(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	p := project{ref: testRef, seq: 4}
	pp := cfg.PortsFor(p.ref, p.seq)
	addr := func(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }
	for svc, want := range map[service]string{
		svcRest:      addr(pp.PostgREST),
		svcAuth:      addr(pp.GoTrue),
		svcRealtime:  addr(cfg.Ports.Realtime),
		svcStorage:   addr(cfg.Ports.Storage),
		svcFunctions: addr(cfg.Ports.EdgeRuntime),
		svcStudio:    addr(cfg.Ports.Studio),
	} {
		if got := s.upstream(svc, p); got != want {
			t.Errorf("%s: upstream = %s, want %s", svc, got, want)
		}
	}
	if pp.PostgREST == pp.GoTrue {
		t.Fatal("test needs distinct PostgREST and GoTrue ports")
	}
}
