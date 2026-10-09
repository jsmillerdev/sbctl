package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover/fenced"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// fakePlane records what a move does to the node's plane.
type fakePlane struct {
	mu      sync.Mutex
	calls   []string
	stopErr error
	info    lifecycle.ControlInfo
	infoErr error
	health  []lifecycle.ServiceHealth
	// systemErr fails StartSystemDatabase; onSystemStart runs when it succeeds (the registry comes back).
	systemErr     error
	onSystemStart func()
}

func (f *fakePlane) rec(c string) { f.mu.Lock(); f.calls = append(f.calls, c); f.mu.Unlock() }
func (f *fakePlane) Start(_ context.Context, p *registry.Project, k *secrets.ProjectKeys) error {
	f.rec("start " + p.Ref + " " + k.AdminPassword)
	return nil
}
func (f *fakePlane) StartSystemDatabase(context.Context) error {
	f.rec("start-system-database")
	if f.systemErr != nil {
		return f.systemErr
	}
	if f.onSystemStart != nil {
		f.onSystemStart()
	}
	return nil
}
func (f *fakePlane) Stop(_ context.Context, ref string) error { f.rec("stop " + ref); return f.stopErr }
func (f *fakePlane) Health(context.Context, *registry.Project, *secrets.ProjectKeys) []lifecycle.ServiceHealth {
	return f.health
}
func (f *fakePlane) FinalCheckpoint(ref string) (lifecycle.ControlInfo, error) {
	f.rec("checkpoint " + ref)
	return f.info, f.infoErr
}

func (f *fakePlane) SetWALKeepSize(_ context.Context, p *registry.Project, size string) error {
	f.rec("keep-wal " + p.Ref + " " + size)
	return nil
}

const primaryRef = "abcdefghijklmnopqrst"

func primariesFixture(t *testing.T) (*localPrimaries, *fakePlane, *recTimers, *registry.Memory) {
	t.Helper()
	reg := registry.NewMemory()
	if err := reg.CreateProject(context.Background(), &registry.Project{Ref: primaryRef, Name: "p", Class: "micro", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	pl := &fakePlane{info: lifecycle.ControlInfo{State: "shut down", Checkpoint: "0/3000060"}}
	tm := &recTimers{}
	return &localPrimaries{
		cfg: cfg, plane: pl, reg: func() registry.Registry { return reg }, timers: tm,
		keys: func(context.Context, string) (*secrets.ProjectKeys, error) {
			return &secrets.ProjectKeys{AdminPassword: "pw"}, nil
		},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return time.Unix(1700000000, 0) },
	}, pl, tm, reg
}

// A planned stop returns the position the cluster stopped at, and only a cluster that shut down cleanly has one.
func TestLocalPrimariesStopReturnsTheShutdownCheckpoint(t *testing.T) {
	l, pl, tm, _ := primariesFixture(t)
	lsn, err := l.Stop(context.Background(), primaryRef)
	if err != nil || lsn != "0/3000060" {
		t.Fatalf("Stop = %q, %v", lsn, err)
	}
	// The primary keeps WAL for its standbys before it stops (the shutdown checkpoint would recycle the segment a
	// lagging walsender still has to read).
	if strings.Join(pl.calls, ",") != "keep-wal "+primaryRef+" 64MB,stop "+primaryRef+",checkpoint "+primaryRef || strings.Join(tm.calls, ",") != "stop "+primaryRef {
		t.Fatalf("plane %v, timers %v", pl.calls, tm.calls)
	}
	pl.info = lifecycle.ControlInfo{State: "in production", Checkpoint: "0/1000000"}
	if lsn, err := l.Stop(context.Background(), primaryRef); err == nil || lsn != "" || !strings.Contains(err.Error(), "did not shut down cleanly") {
		t.Fatalf("a cluster that is still in production gave %q, %v", lsn, err)
	}
	pl.info, pl.infoErr = lifecycle.ControlInfo{}, errors.New("no pg_control")
	if _, err := l.Stop(context.Background(), primaryRef); err == nil {
		t.Fatal("an unreadable control file gave a position")
	}
	pl.infoErr, pl.stopErr = nil, errors.New("unit did not stop")
	if _, err := l.Stop(context.Background(), primaryRef); !errors.Is(err, pl.stopErr) {
		t.Fatalf("a failed stop: %v", err)
	}
}

func TestLocalPrimariesStartStartsTheUnitsAndTheTimerUnlessFenced(t *testing.T) {
	l, pl, tm, _ := primariesFixture(t)
	if err := l.Start(context.Background(), primaryRef); err != nil {
		t.Fatal(err)
	}
	if strings.Join(pl.calls, ",") != "start "+primaryRef+" pw" || strings.Join(tm.calls, ",") != "start "+primaryRef {
		t.Fatalf("plane %v, timers %v", pl.calls, tm.calls)
	}
	pl.calls, tm.calls = nil, nil
	// A project the node is fenced for does not start, whoever asks.
	if err := fenced.WriteProject(l.cfg.Paths(), fenced.Record{Ref: primaryRef, Epoch: 4, Reason: "replaced by n2"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Start(context.Background(), primaryRef); err == nil || !strings.Contains(err.Error(), "fenced") || len(pl.calls) != 0 {
		t.Fatalf("a fenced project started: %v, %v", err, pl.calls)
	}
	if err := l.Start(context.Background(), "bcdefghijklmnopqrstu"); err == nil {
		t.Fatal("a project the registry does not know started")
	}
}

func TestLocalPrimariesHealthy(t *testing.T) {
	l, pl, _, _ := primariesFixture(t)
	pl.health = []lifecycle.ServiceHealth{{Name: "postgres", Healthy: true}, {Name: "postgrest", Healthy: true}}
	if ok, detail, err := l.Healthy(context.Background(), primaryRef); !ok || detail != "" || err != nil {
		t.Fatalf("%v %q %v", ok, detail, err)
	}
	pl.health = []lifecycle.ServiceHealth{{Name: "postgres", Healthy: true}, {Name: "postgrest", Error: "connection refused"}}
	if ok, detail, err := l.Healthy(context.Background(), primaryRef); ok || detail != "postgrest: connection refused" || err != nil {
		t.Fatalf("%v %q %v", ok, detail, err)
	}
	if _, _, err := l.Healthy(context.Background(), "bcdefghijklmnopqrstu"); err == nil {
		t.Fatal("an unknown project is not an answer")
	}
}

// The data of a replaced primary is set aside only when no postmaster runs on it.
func TestLocalPrimariesSetAsideRefusesARunningCluster(t *testing.T) {
	l, pl, _, _ := primariesFixture(t)
	data := l.cfg.Paths().PostgresData(primaryRef)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	// This very process stands for the postmaster.
	if err := os.WriteFile(filepath.Join(data, "postmaster.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"+data+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAside(context.Background(), primaryRef, 5); err == nil || !strings.Contains(err.Error(), "runs here") {
		t.Fatalf("a running cluster's data was set aside: %v", err)
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatalf("the data directory moved: %v", err)
	}
	// A pid file whose process is gone is a crash's leftover.
	if err := os.WriteFile(filepath.Join(data, "postmaster.pid"), []byte("2147483646\n"+data+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAside(context.Background(), primaryRef, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("the data directory is still there")
	}
	if _, err := os.Stat(filepath.Join(data+".diverged-5", "DIVERGED.json")); err != nil {
		t.Fatalf("no record of why: %v", err)
	}
	if len(pl.calls) == 0 {
		t.Fatal("the control file was not read for the record")
	}
	// A project with no data directory is not an error.
	if err := l.SetAside(context.Background(), "bcdefghijklmnopqrstu", 5); err != nil {
		t.Fatal(err)
	}
}

// downRegistry is the registry of a node whose system cluster is stopped: it refuses every read until up
// is set, and counts the reads it refused.
type downRegistry struct {
	registry.Registry
	mu    sync.Mutex
	up    bool
	tried int
}

func (d *downRegistry) GetProject(ctx context.Context, ref string) (*registry.Project, error) {
	d.mu.Lock()
	up := d.up
	if !up {
		d.tried++
	}
	d.mu.Unlock()
	if !up {
		return nil, errors.New("registry: connection refused")
	}
	return d.Registry.GetProject(ctx, ref)
}

func (d *downRegistry) come() { d.mu.Lock(); d.up = true; d.mu.Unlock() }

// The registry is the system cluster, which a planned move stops. Starting the old leader again (a resume,
// `supavise failover --abort`) cannot read the system project's row or credentials first: the database
// starts from its rendered files, and the rest follows when the registry answers.
func TestLocalPrimariesStartsTheSystemClusterBeforeItReadsTheRegistry(t *testing.T) {
	l, pl, tm, reg := primariesFixture(t)
	if err := reg.CreateProject(context.Background(), &registry.Project{Ref: config.SystemRef, Name: "system", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	down := &downRegistry{Registry: reg}
	l.reg = func() registry.Registry { return down }
	l.keys = func(ctx context.Context, ref string) (*secrets.ProjectKeys, error) {
		if _, err := down.GetProject(ctx, ref); err != nil {
			return nil, err
		}
		return &secrets.ProjectKeys{AdminPassword: "pw"}, nil
	}
	l.registryWait, l.registryTries = time.Millisecond, 5
	pl.onSystemStart = down.come
	if err := l.Start(context.Background(), config.SystemRef); err != nil {
		t.Fatal(err)
	}
	if down.tried != 0 {
		t.Fatalf("the registry was read %d times while the system cluster was down", down.tried)
	}
	if got, want := strings.Join(pl.calls, ","), "start-system-database,start system pw"; got != want {
		t.Fatalf("plane calls = %s, want %s", got, want)
	}
	if strings.Join(tm.calls, ",") != "start system" {
		t.Fatalf("timers %v", tm.calls)
	}

	// The registry that takes a few tries to answer is waited for; one that never does is named.
	pl.calls = nil
	down = &downRegistry{Registry: reg}
	l.reg = func() registry.Registry { return down }
	pl.onSystemStart = nil
	err := l.Start(context.Background(), config.SystemRef)
	if err == nil || !strings.Contains(err.Error(), "registry in it does not answer") || down.tried != 5 {
		t.Fatalf("a registry that stays down: %v after %d tries", err, down.tried)
	}

	// A cluster that does not start is the error, and the registry is not asked.
	down = &downRegistry{Registry: reg}
	l.reg = func() registry.Registry { return down }
	pl.systemErr = errors.New("unit did not start")
	if err := l.Start(context.Background(), config.SystemRef); !errors.Is(err, pl.systemErr) || down.tried != 0 {
		t.Fatalf("a system cluster that does not start: %v, registry tried %d times", err, down.tried)
	}
}

// A project other than the system project is read at once: no retry hides a project the registry does not know.
func TestLocalPrimariesStartDoesNotRetryAProject(t *testing.T) {
	l, pl, _, reg := primariesFixture(t)
	down := &downRegistry{Registry: reg}
	l.reg = func() registry.Registry { return down }
	l.registryWait, l.registryTries = time.Millisecond, 5
	if err := l.Start(context.Background(), primaryRef); err == nil || down.tried != 1 {
		t.Fatalf("err = %v after %d tries", err, down.tried)
	}
	if strings.Contains(strings.Join(pl.calls, ","), "start-system-database") {
		t.Fatalf("a project started the system cluster: %v", pl.calls)
	}
}
