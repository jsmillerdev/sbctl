package cluster

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/units"
)

// FenceLocal stops everything on this node that could write as a primary or serve as the leader:
// the Postgres, GoTrue and PostgREST units of every project directory (the system project's
// included) and the shared services. It removes the projects' run scripts, which the units
// require, so that systemd does not start them again at boot. It returns the refs whose Postgres
// unit it stopped. A unit that is not there is not an error; the first real failure is returned
// after the rest were tried.
//
// It is the one local action of a fence: the cooperative fence of the failover procedure calls it,
// and so does the node that finds itself fenced at boot or at run time.
func FenceLocal(ctx context.Context, cfg *config.Config, sup units.Supervisor, log *slog.Logger) ([]string, error) {
	paths := cfg.Paths()
	ents, err := os.ReadDir(filepath.Join(paths.Root, "projects"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var stopped []string
	var first error
	note := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		ref := e.Name()
		for _, svc := range []string{config.SvcPostgREST, config.SvcGoTrue, config.SvcPostgres} { // the client-facing units first
			unit := config.UnitName(svc, ref)
			// The run script goes first: once it is gone the unit cannot restart, whatever the stop does.
			run := filepath.Join(paths.Project(ref), svc+".run")
			if err := os.Remove(run); err != nil && !errors.Is(err, fs.ErrNotExist) {
				note(err)
			}
			st, err := sup.Status(ctx, unit)
			if err != nil || !running(st) {
				continue
			}
			if err := stopSettled(ctx, sup, unit); err != nil {
				log.Error("fence: a unit did not stop", "unit", unit, "error", err)
				note(err)
				continue
			}
			if svc == config.SvcPostgres {
				stopped = append(stopped, ref)
			}
		}
	}
	for _, svc := range []string{config.SvcStudio, config.SvcSupavisor, config.SvcRealtime, config.SvcStorage, config.SvcImgproxy, config.SvcEdgeRuntime, config.SvcPGMeta} {
		unit := config.UnitName(svc, "")
		if st, err := sup.Status(ctx, unit); err != nil || !running(st) {
			continue
		}
		if err := stopSettled(ctx, sup, unit); err != nil {
			log.Error("fence: a unit did not stop", "unit", unit, "error", err)
			note(err)
		}
	}
	sort.Strings(stopped)
	return stopped, first
}

// running reports whether a unit is up, coming up or going down, and so needs a stop: a unit that is
// still going down holds its ports and its data directory until it is inactive, and whoever fenced
// the node must not go on to move that data aside or to listen on those ports before then.
func running(st units.Status) bool {
	return st.State == units.StateActive || st.State == units.StateActivating || st.State == units.StateDeactivating
}

// Timings of stopSettled; variables so that a test can shorten them.
var (
	stopSettleTimeout = 3 * time.Minute
	stopSettleEvery   = 250 * time.Millisecond
	stopRetries       = 5
)

// stopSettled stops unit and returns once it is down. Stop waits for its own job, but a job that
// was queued before it (a start at boot that the fence overtook) can cancel it, and a stop that
// found the unit already going down returns while the unit still holds what it holds: it asks
// again for a canceled stop, and then polls until the unit is neither active, activating nor
// deactivating.
func stopSettled(ctx context.Context, sup units.Supervisor, unit string) error {
	var err error
	for attempt := 0; attempt < stopRetries; attempt++ {
		if err = sup.Stop(ctx, unit); err != nil {
			if !strings.Contains(err.Error(), `"canceled"`) || ctx.Err() != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(stopSettleEvery):
			}
			continue
		}
		deadline := time.Now().Add(stopSettleTimeout)
		for {
			st, serr := sup.Status(ctx, unit)
			if serr != nil || !running(st) {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("cluster: %s is still %s/%s after %s", unit, st.State, st.SubState, stopSettleTimeout)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(stopSettleEvery):
			}
		}
	}
	return err
}
