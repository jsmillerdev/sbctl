package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/failover/fenced"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// localPrimaries is failover.LocalPrimaries: the operations of a move on the primaries of this node,
// over the node's own plane (never the router, which would send a project homed elsewhere away) and its
// backup timers. The orchestrator reaches another node's through the peer endpoints, which call this
// on that node.
type localPrimaries struct {
	cfg    *config.Config
	plane  primaryPlane
	reg    func() registry.Registry
	keys   func(ctx context.Context, ref string) (*secrets.ProjectKeys, error)
	timers lifecycle.Timers // nil: the node has no backup timers (the exec supervisor)
	log    *slog.Logger
	now    func() time.Time
}

// primaryPlane is the part of *lifecycle.PostgresPlane that a move uses.
type primaryPlane interface {
	Start(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error
	Stop(ctx context.Context, ref string) error
	Health(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) []lifecycle.ServiceHealth
	FinalCheckpoint(ref string) (lifecycle.ControlInfo, error)
}

var (
	_ failover.LocalPrimaries = (*localPrimaries)(nil)
	_ primaryPlane            = (*lifecycle.PostgresPlane)(nil)
)

// Stop stops ref's units, PostgREST and GoTrue before Postgres, and its backup timer, and returns the
// shutdown checkpoint of the cluster: the last WAL position it wrote. A cluster that did not shut down
// cleanly has no such position, and that is an error, because the standby would be promoted without
// knowing how far it must replay. Stopping a project that is not running answers with the control
// file as it is.
func (l *localPrimaries) Stop(ctx context.Context, ref string) (string, error) {
	if l.timers != nil {
		if err := l.timers.StopTimer(ctx, ref); err != nil {
			l.log.Warn("backup timer did not stop", "ref", ref, "error", err)
		}
	}
	if err := l.plane.Stop(ctx, ref); err != nil {
		return "", err
	}
	ci, err := l.plane.FinalCheckpoint(ref)
	if err != nil {
		return "", fmt.Errorf("reading where %s stopped: %w", ref, err)
	}
	if !ci.ShutDown() || ci.Checkpoint == "" {
		return "", fmt.Errorf("%s did not shut down cleanly (state %q), so it has no final position", ref, ci.State)
	}
	return ci.Checkpoint, nil
}

// Start starts ref's units as a primary and its backup timer. A project the node is fenced for does
// not start (the fence record is what keeps a replaced primary down); units that run are left alone.
func (l *localPrimaries) Start(ctx context.Context, ref string) error {
	if rec, blocked := fenced.Blocks(l.cfg.Paths(), ref); blocked {
		return fmt.Errorf("%s may not start as a primary on this node: it is fenced (%s)", ref, rec.Reason)
	}
	p, keys, err := l.project(ctx, ref)
	if err != nil {
		return err
	}
	if err := l.plane.Start(ctx, p, keys); err != nil {
		return err
	}
	if l.timers != nil {
		if err := l.timers.StartTimer(ctx, ref); err != nil {
			l.log.Warn("backup timer did not start; nightly base backups will not run until the project is started again", "ref", ref, "error", err)
		}
	}
	return nil
}

// Healthy asks the plane. An error means the project could not be looked at; false with no error is a
// project that was looked at and is not well.
func (l *localPrimaries) Healthy(ctx context.Context, ref string) (bool, string, error) {
	p, err := l.reg().GetProject(ctx, ref)
	if err != nil {
		return false, "", err
	}
	for _, h := range l.plane.Health(ctx, p, nil) {
		if !h.Healthy {
			return false, h.Name + ": " + h.Error, nil
		}
	}
	return true, "", nil
}

// SetAside moves the data of a primary that another node replaced to data.diverged-<epoch>. The
// cluster must not run: a directory renamed under a live postmaster is lost data.
func (l *localPrimaries) SetAside(ctx context.Context, ref string, epoch int64) error {
	data := l.cfg.Paths().PostgresData(ref)
	if alive, pid := postmasterAlive(data); alive {
		return fmt.Errorf("the cluster of %s runs here (postmaster %d); stop it before its data is set aside", ref, pid)
	}
	control := ""
	if ci, err := l.plane.FinalCheckpoint(ref); err == nil {
		control = ci.Checkpoint
	}
	d, err := failover.SetAsideDiverged(l.cfg, ref, epoch, control, "", l.now())
	if err != nil {
		return err
	}
	if d.Path != "" {
		l.log.Info("data of a replaced primary set aside", "ref", ref, "path", d.Path)
	}
	return nil
}

func (l *localPrimaries) project(ctx context.Context, ref string) (*registry.Project, *secrets.ProjectKeys, error) {
	p, err := l.reg().GetProject(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	keys, err := l.keys(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	return p, keys, nil
}

// postmasterAlive reports whether the postmaster.pid in dir names a process that exists.
func postmasterAlive(dir string) (bool, int) {
	b, err := os.ReadFile(dir + "/postmaster.pid")
	if err != nil {
		return false, 0
	}
	first, _, _ := strings.Cut(string(b), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil || pid <= 0 {
		return false, 0
	}
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM), pid
}
