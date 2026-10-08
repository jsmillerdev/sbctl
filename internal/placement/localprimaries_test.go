package placement

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

type lpTimers struct{ calls []string }

func (t *lpTimers) StartTimer(_ context.Context, ref string) error {
	t.calls = append(t.calls, "start "+ref)
	return nil
}
func (t *lpTimers) StopTimer(_ context.Context, ref string) error {
	t.calls = append(t.calls, "stop "+ref)
	return nil
}

type lpEnv struct {
	lp     *LocalPrimaries
	plane  *fakeLocal
	timers *lpTimers
	cfg    *config.Config
	ctrl   lifecycle.ControlInfo
	ctrlEr error
	aside  []string
}

func newLPEnv(t *testing.T) *lpEnv {
	t.Helper()
	reg := registry.NewMemory()
	if err := reg.CreateProject(context.Background(), &registry.Project{Ref: testRef, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	e := &lpEnv{plane: newFakeLocal(), timers: &lpTimers{}, cfg: cfg, ctrl: lifecycle.ControlInfo{State: "shut down", Checkpoint: "0/3000060"}}
	e.lp = &LocalPrimaries{
		Plane: e.plane, Registry: reg, Cfg: cfg, Timers: e.timers,
		Control: func(string) (lifecycle.ControlInfo, error) { return e.ctrl, e.ctrlEr },
		Keys:    func(context.Context, string) (*secrets.ProjectKeys, error) { return testKeys(), nil },
		MoveAside: func(_ context.Context, ref string, epoch int64) error {
			e.aside = append(e.aside, ref+" "+strconv.FormatInt(epoch, 10))
			return nil
		},
	}
	return e
}

func TestLocalPrimariesStopReturnsTheShutdownCheckpoint(t *testing.T) {
	ctx := context.Background()
	e := newLPEnv(t)
	lsn, err := e.lp.Stop(ctx, testRef)
	if err != nil || lsn != "0/3000060" {
		t.Fatalf("Stop = %q, %v", lsn, err)
	}
	if got := strings.Join(e.timers.calls, ","); got != "stop "+testRef || !e.plane.has("Stop "+testRef) {
		t.Fatalf("timer calls %s, plane %v", got, e.plane.calls)
	}

	// The position is never guessed: an unreadable control file, an empty checkpoint, or a cluster that
	// did not shut down is an error.
	e.ctrlEr = errors.New("no such file")
	if lsn, err := e.lp.Stop(ctx, testRef); err == nil || lsn != "" {
		t.Fatalf("an unreadable control file = %q, %v", lsn, err)
	}
	e.ctrlEr, e.ctrl = nil, lifecycle.ControlInfo{State: "shut down", Checkpoint: ""}
	if lsn, err := e.lp.Stop(ctx, testRef); err == nil || lsn != "" {
		t.Fatalf("an empty checkpoint = %q, %v", lsn, err)
	}
	e.ctrl = lifecycle.ControlInfo{State: "in production", Checkpoint: "0/3000060"}
	if lsn, err := e.lp.Stop(ctx, testRef); !errors.Is(err, lifecycle.ErrNotCleanShutdown) || lsn != "" {
		t.Fatalf("a cluster in production after the stop = %q, %v", lsn, err)
	}
	e.plane.err["Stop"] = errors.New("unit did not stop")
	if _, err := e.lp.Stop(ctx, testRef); err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("a failed stop: %v", err)
	}
}

func TestLocalPrimariesStartHealthyAndSetAside(t *testing.T) {
	ctx := context.Background()
	e := newLPEnv(t)
	if err := e.lp.Start(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	if !e.plane.has("Start "+testRef) || strings.Join(e.timers.calls, ",") != "start "+testRef {
		t.Fatalf("plane %v, timers %v", e.plane.calls, e.timers.calls)
	}
	e.plane.err["Start"] = lifecycle.ErrFenced
	if err := e.lp.Start(ctx, testRef); !errors.Is(err, lifecycle.ErrFenced) {
		t.Fatalf("Start of a fenced primary: %v", err)
	}
	if err := e.lp.Start(ctx, "zzzzzzzzzzzzzzzzzzzz"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Start of an unknown project: %v", err)
	}

	e.plane.health = []lifecycle.ServiceHealth{{Name: "postgres", Healthy: true}, {Name: "gotrue", Healthy: false, Status: "UNHEALTHY", Error: "refused"}}
	ok, detail, err := e.lp.Healthy(ctx, testRef)
	if err != nil || ok || !strings.Contains(detail, "gotrue") || !strings.Contains(detail, "refused") {
		t.Fatalf("Healthy = %v %q %v", ok, detail, err)
	}
	e.plane.health = []lifecycle.ServiceHealth{{Name: "postgres", Healthy: true}}
	if ok, _, err := e.lp.Healthy(ctx, testRef); !ok || err != nil {
		t.Fatalf("Healthy = %v %v", ok, err)
	}
	if _, _, err := e.lp.Healthy(ctx, "zzzzzzzzzzzzzzzzzzzz"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Healthy of an unknown project: %v", err)
	}

	// Set aside: no postmaster, then a live one (this process), then a stale file.
	if err := e.lp.SetAside(ctx, testRef, 4); err != nil || strings.Join(e.aside, ",") != testRef+" 4" {
		t.Fatalf("SetAside = %v, %v", err, e.aside)
	}
	e.aside = nil
	dir := e.cfg.Paths().PostgresData(testRef)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(dir, "postmaster.pid")
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(os.Getpid())+"\n/data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.lp.SetAside(ctx, testRef, 4); !errors.Is(err, lifecycle.ErrInvalidState) || len(e.aside) != 0 {
		t.Fatalf("SetAside under a running postmaster = %v, %v", err, e.aside)
	}
	if err := os.WriteFile(pidfile, []byte("2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.lp.SetAside(ctx, testRef, 4); err != nil || len(e.aside) != 1 {
		t.Fatalf("SetAside after a crash = %v, %v", err, e.aside)
	}
}
