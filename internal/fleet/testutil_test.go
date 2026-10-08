package fleet

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/units"
)

const testRef = "abcdefghijklmnopqrst"

func testSecrets(t *testing.T) secrets.Secrets {
	t.Helper()
	s, err := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// testNode is an in-memory node: config, registry holding the system project with the
// fleet roles' passwords (what lifecycle.InitSystem leaves behind), and a project.
type testNode struct {
	cfg *config.Config
	reg *registry.Memory
	sec secrets.Secrets
}

func newTestNode(t *testing.T) *testNode {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Supervisor = config.SupervisorExec
	cfg.Domain = "supavise.test"
	cfg.TLS.Mode = "off"
	n := &testNode{cfg: cfg, reg: registry.NewMemory(), sec: testSecrets(t)}
	ctx := context.Background()
	if err := n.reg.CreateProject(ctx, &registry.Project{Ref: config.SystemRef, Name: "system", Class: "system", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	for _, ld := range loginDefs {
		n.put(t, config.SystemRef, loginSecretName(ld.Service), "pw-"+ld.Service)
	}
	return n
}

func (n *testNode) put(t *testing.T, ref, name, val string) {
	t.Helper()
	sealed, err := n.sec.Seal([]byte(val))
	if err != nil {
		t.Fatal(err)
	}
	if err := n.reg.PutSecret(context.Background(), ref, name, sealed); err != nil {
		t.Fatal(err)
	}
}

// project registers ref with a full credential set and returns the keys.
func (n *testNode) project(t *testing.T, ref string) *secrets.ProjectKeys {
	t.Helper()
	ctx := context.Background()
	if err := n.reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: ref, Class: "micro", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	k, err := secrets.NewProjectKeys(ref, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for name, val := range k.Map() {
		n.put(t, ref, name, val)
	}
	return k
}

func (n *testNode) deps() Deps {
	return Deps{Cfg: n.cfg, Registry: n.reg, Secrets: n.sec}
}

// listenLoopback returns n listeners on distinct free loopback ports. Serve on them as they
// are: closing one to bind its port again later races every other socket on the host for it.
func listenLoopback(t *testing.T, n int) []net.Listener {
	t.Helper()
	ls := make([]net.Listener, n)
	for i := range ls {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		ls[i] = l
	}
	return ls
}

func portOf(l net.Listener) int { return l.Addr().(*net.TCPAddr).Port }

// fakeSupervisor records what the Manager asks of it. Start makes the unit "active" and
// runs the hook registered for it (tests use it to bring a health server up).
type fakeSupervisor struct {
	mu      sync.Mutex
	calls   []string
	specs   map[string]units.Spec
	state   map[string]units.State
	changed bool // what RenderChanged reports
	hooks   map[string]func()
	failOn  map[string]error // "start supavise-x.service"
}

func newFakeSupervisor() *fakeSupervisor {
	return &fakeSupervisor{specs: map[string]units.Spec{}, state: map[string]units.State{}, hooks: map[string]func(){}, failOn: map[string]error{}, changed: true}
}

func (f *fakeSupervisor) rec(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.failOn[call]
}

func (f *fakeSupervisor) Render(ctx context.Context, s units.Spec) error {
	_, err := f.RenderChanged(ctx, s)
	return err
}

func (f *fakeSupervisor) RenderChanged(_ context.Context, s units.Spec) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "render "+s.Unit())
	f.specs[s.Unit()] = s
	return f.changed, nil
}

func (f *fakeSupervisor) Start(_ context.Context, unit string) error {
	if err := f.rec("start " + unit); err != nil {
		return err
	}
	f.mu.Lock()
	f.state[unit] = units.StateActive
	hook := f.hooks[unit]
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (f *fakeSupervisor) Stop(_ context.Context, unit string) error {
	if err := f.rec("stop " + unit); err != nil {
		return err
	}
	f.mu.Lock()
	f.state[unit] = units.StateInactive
	f.mu.Unlock()
	return nil
}

func (f *fakeSupervisor) Status(_ context.Context, unit string) (units.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.state[unit]
	if st == "" {
		st = units.StateInactive
	}
	return units.Status{Unit: unit, State: st, Since: time.Now().Add(-time.Hour)}, nil
}

func (f *fakeSupervisor) Remove(_ context.Context, unit string) error { return f.rec("remove " + unit) }

func (f *fakeSupervisor) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

type fakeArtifacts map[string]string

func (a fakeArtifacts) Dir(svc string) (string, error) {
	if d, ok := a[svc]; ok {
		return d, nil
	}
	return "", fmt.Errorf("artifact %s not fetched", svc)
}

func allArtifacts() fakeArtifacts {
	a := fakeArtifacts{}
	for _, s := range Services {
		a[s] = "/art/" + s
	}
	return a
}

// healthServer answers the health path of svc on l with 200 once up() was called.
type healthServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	up  bool
}

func serveHealth(t *testing.T, l net.Listener, path string) *healthServer {
	t.Helper()
	h := &healthServer{}
	h.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		up := h.up
		h.mu.Unlock()
		if r.URL.Path != path || !up {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(200)
	}))
	h.srv.Listener.Close()
	h.srv.Listener = l
	h.srv.Start()
	t.Cleanup(h.srv.Close)
	return h
}

func (h *healthServer) setUp(v bool) { h.mu.Lock(); h.up = v; h.mu.Unlock() }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
