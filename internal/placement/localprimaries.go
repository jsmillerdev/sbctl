package placement

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// LocalPrimaries is the failover orchestrator's hand on the primaries homed on this node (it
// implements failover.LocalPrimaries; this package does not import failover, which imports the
// layers below it): it stops a project's primary and learns the position its WAL ended at, starts
// one again, asks whether one answers and sets aside the data of one that another node replaced. It
// works on the node's own plane, so it acts on the units and the data directory of this node whoever
// asks, and the lifecycle plane refuses to start a primary that a fence record holds back
// (lifecycle.ErrFenced).
type LocalPrimaries struct {
	// Plane is the node's own plane (never the router: these calls are about this node's disk).
	Plane lifecycle.Plane
	// Control reads the control file of ref's cluster on this node (lifecycle.PostgresPlane.FinalCheckpoint).
	Control func(ref string) (lifecycle.ControlInfo, error)
	// Registry and Keys load the project row and its credentials for Start and Healthy.
	Registry registry.Registry
	Keys     func(ctx context.Context, ref string) (*secrets.ProjectKeys, error)
	// Timers starts and stops the project's nightly base backup timer; nil does nothing.
	Timers lifecycle.Timers
	// Cfg gives the data directories SetAside inspects.
	Cfg *config.Config
	// MoveAside moves ref's data directory to data.diverged-<epoch> and clears the project's fence
	// record (failover.SetAsideDiverged, in a closure over the node's config). The cluster is stopped
	// when it is called.
	MoveAside func(ctx context.Context, ref string, epoch int64) error
	Log       *slog.Logger
}

func (l *LocalPrimaries) log() *slog.Logger {
	if l.Log == nil {
		return slog.Default()
	}
	return l.Log
}

// Stop stops ref's PostgREST, GoTrue and cluster, in that order, with the fast shutdown of the unit's
// stop, and the nightly backup timer, and returns the cluster's latest checkpoint location from its
// control file: after a clean shutdown that is the shutdown checkpoint, the last record of its WAL, and
// the position a standby must have replayed past. The daemon stays up (the cluster archives its last
// segments through the relay). A project that is not running returns the checkpoint of its control
// file. An unreadable control file, or a cluster that did not shut down cleanly, is an error: the
// caller must not promote a standby on a position it does not have.
func (l *LocalPrimaries) Stop(ctx context.Context, ref string) (string, error) {
	if l.Timers != nil {
		if err := l.Timers.StopTimer(ctx, ref); err != nil {
			l.log().Warn("the backup timer did not stop", "ref", ref, "error", err)
		}
	}
	if err := l.Plane.Stop(ctx, ref); err != nil {
		return "", fmt.Errorf("stopping %s: %w", ref, err)
	}
	ci, err := l.Control(ref)
	if err != nil {
		return "", fmt.Errorf("reading the control file of %s: %w", ref, err)
	}
	if !ci.ShutDown() {
		return "", fmt.Errorf("%w: the control file of %s says %q", lifecycle.ErrNotCleanShutdown, ref, ci.State)
	}
	if ci.Checkpoint == "" {
		return "", fmt.Errorf("the control file of %s holds no checkpoint location", ref)
	}
	return ci.Checkpoint, nil
}

// Start starts ref's units as a primary and its backup timer. Units that run are left alone. A
// primary that a fence record holds back does not start (lifecycle.ErrFenced).
func (l *LocalPrimaries) Start(ctx context.Context, ref string) error {
	p, err := l.Registry.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	keys, err := l.Keys(ctx, ref)
	if err != nil {
		return err
	}
	if err := l.Plane.Start(ctx, p, keys); err != nil {
		return err
	}
	if l.Timers != nil {
		if err := l.Timers.StartTimer(ctx, ref); err != nil {
			l.log().Warn("the backup timer did not start", "ref", ref, "error", err)
		}
	}
	return nil
}

// Healthy asks whether ref's primary answers: every service of the project healthy. The error means
// the project could not be looked up.
func (l *LocalPrimaries) Healthy(ctx context.Context, ref string) (bool, string, error) {
	p, err := l.Registry.GetProject(ctx, ref)
	if err != nil {
		return false, "", err
	}
	var bad []string
	for _, h := range l.Plane.Health(ctx, p, nil) {
		if !h.Healthy {
			bad = append(bad, strings.TrimSpace(h.Name+" "+h.Status+" "+h.Error))
		}
	}
	return len(bad) == 0, strings.Join(bad, "; "), nil
}

// SetAside moves ref's data directory away (MoveAside) for a replica to be built in its place. It
// refuses while a postmaster of that directory is alive: the rename would pull the directory from
// under a running cluster.
func (l *LocalPrimaries) SetAside(ctx context.Context, ref string, epoch int64) error {
	if l.Cfg != nil {
		if pid, alive := postmasterAlive(l.Cfg.Paths().PostgresData(ref)); alive {
			return fmt.Errorf("%w: the cluster of %s runs here (postmaster %d); stop it first", lifecycle.ErrInvalidState, ref, pid)
		}
	}
	if l.MoveAside == nil {
		return errors.New("placement: no way to set the data of a primary aside")
	}
	return l.MoveAside(ctx, ref, epoch)
}

// postmasterAlive reads postmaster.pid in dataDir and reports the pid and whether a process of that
// pid exists. A directory with no such file, or one that does not parse, holds no running cluster.
func postmasterAlive(dataDir string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(dataDir, "postmaster.pid"))
	if err != nil {
		return 0, false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid < 1 {
		return 0, false
	}
	// Signal 0 tests for the process without sending anything: EPERM means it exists.
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return pid, false
	}
	return pid, true
}
