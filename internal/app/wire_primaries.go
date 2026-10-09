package app

import (
	"cmp"
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
	// registryWait is how often, and registryTries how many times, Start of the system cluster looks for
	// the registry that the cluster has just started to hold; zero is two seconds and thirty tries.
	registryWait  time.Duration
	registryTries int
}

// primaryPlane is the part of *lifecycle.PostgresPlane that a move uses.
type primaryPlane interface {
	Start(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error
	StartSystemDatabase(ctx context.Context) error
	Stop(ctx context.Context, ref string) error
	Health(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) []lifecycle.ServiceHealth
	FinalCheckpoint(ref string) (lifecycle.ControlInfo, error)
	SetWALKeepSize(ctx context.Context, p *registry.Project, size string) error
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
	l.keepWALForStandbys(ctx, ref)
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

// standbyWALKeep is the WAL a primary keeps on disk for its standbys while it is stopped for a move: four
// segments. A clean shutdown with archiving on switches to a new segment and writes the shutdown checkpoint at the
// start of the next, and the checkpoint recycles the segment it closed. Standbys use no replication slots, so a
// standby that has not yet read the tail of that segment (the switch record) finds it gone, its walreceiver ends,
// and the shutdown checkpoint never reaches it: the move then refuses to promote a replica that is behind the old
// primary's final position. A busy or slow machine loses that race now and then; keeping a few segments closes it.
// The failure was seen once, on arm64 (replication run 37886326567): the standby's log says "could not receive data from
// WAL stream: ERROR: requested WAL segment 000000020000000000000008 has already been removed", and the move refused the replica,
// which had replayed to 0/9000000 while the old primary stopped at 0/9000028. A rare race, so the setting is a mitigation
// whose effect the arm64 runs that followed (none refused) support but cannot prove.
const standbyWALKeep = "64MB"

// keepWALForStandbys sets wal_keep_size on the running primary of ref just before it stops (ALTER SYSTEM and a
// reload, which the checkpointer reads before the shutdown checkpoint). It is best effort: a cluster that is not
// running, or a registry that cannot say what the project is, is stopped all the same. The setting stays in
// postgresql.auto.conf and so goes with the cluster to the standby that replaces it.
func (l *localPrimaries) keepWALForStandbys(ctx context.Context, ref string) {
	reg := l.reg()
	if reg == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	p, err := reg.GetProject(cctx, ref)
	if err != nil {
		return
	}
	if err := l.plane.SetWALKeepSize(cctx, p, standbyWALKeep); err != nil {
		l.log.Debug("could not keep WAL for the standbys of a primary that is about to stop", "ref", ref, "error", err)
	}
}

// Start starts ref's units as a primary and its backup timer. A project the node is fenced for does
// not start (the fence record is what keeps a replaced primary down); units that run are left alone.
//
// The registry is the system cluster, which a planned move stops: a project's row and credentials
// cannot be read while it is down, so the system cluster starts first, from its rendered files, and the
// rest (its GoTrue) once the registry answers again. Without that a resume or an abort could not bring the
// old leader back.
func (l *localPrimaries) Start(ctx context.Context, ref string) error {
	if rec, blocked := fenced.Blocks(l.cfg.Paths(), ref); blocked {
		return fmt.Errorf("%s may not start as a primary on this node: it is fenced (%s)", ref, rec.Reason)
	}
	if ref == config.SystemRef {
		if err := l.plane.StartSystemDatabase(ctx); err != nil {
			return err
		}
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

// project reads ref's row and credentials. The system cluster has just been started when it is asked
// for, and its registry answers a moment later, so the read is repeated for it.
func (l *localPrimaries) project(ctx context.Context, ref string) (*registry.Project, *secrets.ProjectKeys, error) {
	tries, every := 1, time.Duration(0)
	if ref == config.SystemRef {
		tries, every = cmp.Or(l.registryTries, 30), cmp.Or(l.registryWait, 2*time.Second)
	}
	var err error
	for i := 0; i < tries; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(every):
			}
		}
		var p *registry.Project
		if p, err = l.reg().GetProject(ctx, ref); err != nil {
			continue
		}
		var keys *secrets.ProjectKeys
		if keys, err = l.keys(ctx, ref); err != nil {
			continue
		}
		return p, keys, nil
	}
	if ref == config.SystemRef {
		return nil, nil, fmt.Errorf("the system cluster is back, but the registry in it does not answer: %w", err)
	}
	return nil, nil, err
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
