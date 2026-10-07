package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/units"
)

// managerRig is a Manager over a fake supervisor whose units bring a health endpoint up
// when they start.
type managerRig struct {
	n      *testNode
	sup    *fakeSupervisor
	m      *Manager
	health map[string]*healthServer
}

func newManagerRig(t *testing.T, mutate func(*Deps)) *managerRig {
	t.Helper()
	n := newTestNode(t)
	ports := freePorts(t, 5)
	n.cfg.Ports.PGMeta, n.cfg.Ports.Realtime, n.cfg.Ports.Storage, n.cfg.Ports.Studio, n.cfg.Fleet.SupavisorAPIPort = ports[0], ports[1], ports[2], ports[3], ports[4]
	r := &managerRig{n: n, sup: newFakeSupervisor(), health: map[string]*healthServer{}}
	for _, svc := range Services {
		h := serveHealth(t, Port(n.cfg, svc), healthPath(svc))
		r.health[svc] = h
		r.sup.hooks[unitOf(svc)] = func() { h.setUp(true) }
	}
	d := n.deps()
	d.Supervisor, d.Artifacts, d.ReadyTimeout = r.sup, allArtifacts(), 5*time.Second
	if mutate != nil {
		mutate(&d)
	}
	var err error
	if r.m, err = NewManager(d); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestManagerStartRendersStartsAndWaitsInOrder(t *testing.T) {
	r := newManagerRig(t, nil)
	if err := r.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"render sb-pgmeta.service", "start sb-pgmeta.service",
		"render sb-supavisor.service", "start sb-supavisor.service",
		"render sb-realtime.service", "start sb-realtime.service",
		"render sb-storage.service", "start sb-storage.service",
		"render sb-studio.service", "start sb-studio.service",
	}
	if got := strings.Split(r.sup.log(), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls:\n%s", r.sup.log())
	}
	if env := r.sup.specs["sb-storage.service"].Env; env["MULTI_TENANT"] != "true" {
		t.Errorf("storage env = %v", env)
	}
	for _, h := range r.m.Status(context.Background()) {
		if !h.Healthy || h.Status != "ACTIVE_HEALTHY" {
			t.Errorf("%+v", h)
		}
	}
	// The work directories the units' templates bind exist.
	for _, svc := range Services {
		if !dirExists(r.n.cfg.Paths().System(svc)) {
			t.Errorf("no work directory for %s", svc)
		}
	}
}

func TestManagerStartRestartsOnlyWhenTheFilesChanged(t *testing.T) {
	r := newManagerRig(t, nil)
	ctx := context.Background()
	if err := r.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.sup.mu.Lock()
	r.sup.calls, r.sup.changed = nil, false
	r.sup.mu.Unlock()
	if err := r.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.sup.log(), "stop ") {
		t.Fatalf("unchanged units were restarted:\n%s", r.sup.log())
	}
	r.sup.mu.Lock()
	r.sup.calls, r.sup.changed = nil, true
	r.sup.mu.Unlock()
	if err := r.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, svc := range Services {
		if !strings.Contains(r.sup.log(), "stop "+unitOf(svc)+"\nstart "+unitOf(svc)) {
			t.Errorf("%s: no stop before start after its files changed:\n%s", svc, r.sup.log())
		}
	}
}

func TestManagerOneFailureDoesNotBlockTheRest(t *testing.T) {
	r := newManagerRig(t, nil)
	r.sup.failOn["start sb-realtime.service"] = errors.New("exec format error")
	err := r.m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "realtime") || !strings.Contains(err.Error(), "exec format error") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(r.sup.log(), "start sb-storage.service") || !strings.Contains(r.sup.log(), "start sb-studio.service") {
		t.Fatalf("later services were skipped:\n%s", r.sup.log())
	}
}

func TestManagerMissingArtifactIsReportedPerService(t *testing.T) {
	arts := allArtifacts()
	delete(arts, config.SvcStudio)
	r := newManagerRig(t, func(d *Deps) { d.Artifacts = arts })
	err := r.m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "studio") || strings.Contains(err.Error(), "realtime") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(r.sup.log(), "start sb-storage.service") {
		t.Fatal("the other services must still start")
	}
}

func TestManagerSkip(t *testing.T) {
	r := newManagerRig(t, func(d *Deps) { d.Skip = []string{config.SvcStudio} })
	if err := r.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.sup.log(), "studio") {
		t.Fatalf("a skipped service was touched:\n%s", r.sup.log())
	}
	if hs := r.m.Status(context.Background()); len(hs) != 4 {
		t.Fatalf("status lists %d services", len(hs))
	}
}

func TestManagerStartFailsFastWhenTheUnitDies(t *testing.T) {
	r := newManagerRig(t, nil)
	// The unit starts, then is failed: its health endpoint never comes up.
	r.sup.hooks[unitOf(config.SvcStorage)] = func() {
		r.sup.mu.Lock()
		r.sup.state[unitOf(config.SvcStorage)] = units.StateFailed
		r.sup.mu.Unlock()
	}
	began := time.Now()
	err := r.m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sb-storage.service is failed") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(began) > 3*time.Second {
		t.Fatalf("took %v: a dead unit must not wait for the ready timeout", time.Since(began))
	}
}

func TestManagerStartTimesOut(t *testing.T) {
	r := newManagerRig(t, func(d *Deps) { d.ReadyTimeout = 400 * time.Millisecond })
	r.sup.hooks[unitOf(config.SvcRealtime)] = nil // started, never answers
	err := r.m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not ready after") {
		t.Fatalf("err = %v", err)
	}
}

func TestManagerStopReverseOrderAndStatus(t *testing.T) {
	r := newManagerRig(t, nil)
	ctx := context.Background()
	if err := r.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.sup.mu.Lock()
	r.sup.calls = nil
	r.sup.mu.Unlock()
	if err := r.m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	want := "stop sb-studio.service|stop sb-storage.service|stop sb-realtime.service|stop sb-supavisor.service|stop sb-pgmeta.service"
	if got := strings.ReplaceAll(r.sup.log(), "\n", "|"); got != want {
		t.Fatalf("calls = %s", got)
	}
	for _, h := range r.m.Status(ctx) {
		if h.Healthy || h.Status != "STOPPED" {
			t.Errorf("%+v", h)
		}
	}
	// Active unit, endpoint down: unhealthy.
	r.sup.mu.Lock()
	r.sup.state[unitOf(config.SvcPGMeta)] = units.StateActive
	r.sup.mu.Unlock()
	r.health[config.SvcPGMeta].setUp(false)
	for _, h := range r.m.Status(ctx) {
		if h.Service == config.SvcPGMeta && (h.Healthy || h.Status != "UNHEALTHY" || h.Error == "") {
			t.Errorf("%+v", h)
		}
	}
}

func TestNewManagerValidates(t *testing.T) {
	if _, err := NewManager(Deps{}); err == nil {
		t.Fatal("empty deps accepted")
	}
	n := newTestNode(t)
	m, _ := NewManager(Deps{Cfg: n.cfg, Supervisor: newFakeSupervisor()})
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("Start without registry accepted")
	}
}
