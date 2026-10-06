package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

const testDomain = "example.test"

// captured is one request as an upstream saw it.
type captured struct {
	Method, Path, RawQuery, Host, Body string
	Header                             http.Header
}

// upstream is a fake service that records what it receives.
type upstream struct {
	name    string
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []captured
	handler http.HandlerFunc // optional override after recording
}

func newUpstream(t *testing.T, name string) *upstream {
	t.Helper()
	u := &upstream{name: name}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, captured{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Host: r.Host, Body: string(body), Header: r.Header.Clone()})
		h := u.handler
		u.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		// Services add their own CORS headers; the proxy must replace them.
		w.Header().Set("Access-Control-Allow-Origin", "https://upstream.example")
		w.Header().Set("X-Upstream", name)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"upstream": name})
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) addr() string { return strings.TrimPrefix(u.srv.URL, "http://") }

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

func (u *upstream) last(t *testing.T) captured {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatalf("upstream %s received no request", u.name)
	}
	return u.reqs[len(u.reqs)-1]
}

// fakeKeys is a KeySource that counts calls.
type fakeKeys struct {
	mu    sync.Mutex
	keys  map[string]*secrets.ProjectKeys
	calls map[string]int
}

func newFakeKeys() *fakeKeys {
	return &fakeKeys{keys: map[string]*secrets.ProjectKeys{}, calls: map[string]int{}}
}

func (f *fakeKeys) Keys(_ context.Context, ref string) (*secrets.ProjectKeys, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[ref]++
	k, ok := f.keys[ref]
	if !ok {
		return nil, registry.ErrNotFound
	}
	return k, nil
}

func (f *fakeKeys) set(ref string, k *secrets.ProjectKeys) {
	f.mu.Lock()
	f.keys[ref] = k
	f.mu.Unlock()
}

func (f *fakeKeys) callCount(ref string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[ref]
}

// harness is a Server in front of fake upstreams, served by httptest.
type harness struct {
	t      *testing.T
	cfg    *config.Config
	reg    *registry.Memory
	keys   *fakeKeys
	srv    *Server
	ts     *httptest.Server
	ups    map[service]*upstream
	ref    string
	k      *secrets.ProjectKeys
	waker  *recordingWaker
	apiHit chan string
}

type recordingWaker struct {
	mu   sync.Mutex
	refs []string
	err  error
}

func (w *recordingWaker) Start(_ context.Context, ref string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refs = append(w.refs, ref)
	return w.err
}

func (w *recordingWaker) calls() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.refs...)
}

type harnessOpts struct {
	functions bool
	tls       string
}

func newHarness(t *testing.T, mut ...func(*Options)) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.Domain = testDomain
	cfg.TLS.Mode = "off"
	cfg.StateDir = t.TempDir()

	h := &harness{t: t, cfg: cfg, reg: registry.NewMemory(), keys: newFakeKeys(), ups: map[service]*upstream{}, waker: &recordingWaker{}, apiHit: make(chan string, 8)}
	for _, s := range []service{svcRest, svcAuth, svcRealtime, svcStorage, svcFunctions, svcStudio} {
		h.ups[s] = newUpstream(t, s.String())
	}
	h.ref = testRef
	h.k = testKeys(t, h.ref)
	h.keys.set(h.ref, h.k)
	if err := h.reg.CreateProject(context.Background(), &registry.Project{Ref: h.ref, Name: "p1", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.CreateProject(context.Background(), &registry.Project{Ref: config.SystemRef, Name: "system", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}

	opts := Options{
		Config: cfg, Registry: h.reg, Keys: h.keys, Waker: h.waker,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		APIHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.apiHit <- r.Host + r.URL.Path
			w.WriteHeader(http.StatusTeapot)
		}),
	}
	for _, m := range mut {
		m(&opts)
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv.upstreamFn = func(s service, _ project) string { return h.ups[s].addr() }
	srv.table.retry = 10 * time.Millisecond
	h.srv = srv
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Sync(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	h.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(h.ts.Close)
	return h
}

func (h *harness) host(ref string) string { return ref + ".api." + testDomain }

// req sends a request through the proxy with the given Host and headers
// (alternating name, value) and returns the response and its body.
func (h *harness) req(method, host, target string, headers ...string) (*http.Response, string) {
	h.t.Helper()
	return h.reqBody(method, host, target, "", headers...)
}

func (h *harness) reqBody(method, host, target, body string, headers ...string) (*http.Response, string) {
	h.t.Helper()
	r, err := http.NewRequest(method, h.ts.URL+target, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	r.Host = host
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// project sends a request to the harness project's API host.
func (h *harness) project(method, target string, headers ...string) (*http.Response, string) {
	h.t.Helper()
	return h.req(method, h.host(h.ref), target, headers...)
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
