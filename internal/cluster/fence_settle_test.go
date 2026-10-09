package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/units"
)

// goingDownSup is a supervisor whose Postgres unit is already going down when the fence looks at it, and whose
// first stop is canceled, as a start job that was queued at boot cancels one.
type goingDownSup struct {
	mu       sync.Mutex
	unit     string
	state    units.State
	cancels  int // stops that answer "canceled" before one is accepted
	stops    int
	downAt   time.Time
	downWait time.Duration
}

func (g *goingDownSup) Render(context.Context, units.Spec) error { return nil }
func (g *goingDownSup) Start(context.Context, string) error      { return nil }
func (g *goingDownSup) Remove(context.Context, string) error     { return nil }
func (g *goingDownSup) Stop(_ context.Context, unit string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if unit != g.unit {
		return nil
	}
	g.stops++
	if g.cancels > 0 {
		g.cancels--
		return errors.New(`units: stop ` + unit + ` job finished with "canceled"`)
	}
	if g.state == units.StateActive {
		g.state, g.downAt = units.StateDeactivating, time.Now().Add(g.downWait)
	}
	return nil
}
func (g *goingDownSup) Status(_ context.Context, unit string) (units.Status, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if unit != g.unit {
		return units.Status{Unit: unit, State: units.StateInactive}, nil
	}
	if g.state == units.StateDeactivating && !time.Now().Before(g.downAt) {
		g.state = units.StateInactive
	}
	return units.Status{Unit: unit, State: g.state}, nil
}

// The system cluster's unit is still going down when a rejoin fences the node (the daemon's own fence at boot began the
// stop): FenceLocal must wait until the unit is down before it reports done, because the data is moved aside and the
// port forwarded right after. A canceled stop is asked for again.
func TestFenceLocalWaitsForAUnitThatIsGoingDown(t *testing.T) {
	old := stopSettleEvery
	stopSettleEvery = 5 * time.Millisecond
	defer func() { stopSettleEvery = old }()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	run := filepath.Join(cfg.Paths().Project("system"), "postgres.run")
	if err := os.MkdirAll(filepath.Dir(run), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(run, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	unit := config.UnitName("postgres", "system")
	for _, start := range []units.State{units.StateActive, units.StateDeactivating} {
		sup := &goingDownSup{unit: unit, state: start, cancels: 1, downWait: 80 * time.Millisecond}
		if start == units.StateDeactivating {
			sup.downAt = time.Now().Add(80 * time.Millisecond)
		}
		began := time.Now()
		stopped, err := FenceLocal(context.Background(), cfg, sup, quiet())
		if err != nil {
			t.Fatalf("%s: %v", start, err)
		}
		if len(stopped) != 1 || stopped[0] != "system" {
			t.Fatalf("%s: stopped %v", start, stopped)
		}
		if st, _ := sup.Status(context.Background(), unit); st.State != units.StateInactive {
			t.Fatalf("%s: the unit is %s when FenceLocal returned", start, st.State)
		}
		if time.Since(began) < 60*time.Millisecond {
			t.Fatalf("%s: FenceLocal returned after %s, before the unit was down", start, time.Since(began))
		}
		if sup.stops < 1 {
			t.Fatalf("%s: no stop was asked for", start)
		}
	}
}
