package placement

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// Defaults of ScheduledBackups.
const (
	// scheduledEvery is the age of a project's newest completed base backup at which the next is due.
	scheduledEvery = 24 * time.Hour
	// scheduledRetry is how long a project whose scheduled backup failed waits for the next try.
	scheduledRetry = time.Hour
	// scheduledTimeout bounds the backup of one project, so that a home that hangs does not hold up
	// the projects after it.
	scheduledTimeout = 2 * time.Hour
)

// ScheduledBackups takes the nightly backup of the projects that are homed on other nodes. It runs on
// the leader only. The nightly timer of a project (supavise-basebackup@<ref>.timer) runs `supavise
// backups create`, which writes the registry, and a follower's copy of the registry takes no write:
// a follower runs no timer (lifecycle's follower timers), and the leader does what the timer does for
// a project homed here. The Storage objects and Edge Functions of every project are on the leader, where
// the shared services run, so it snapshots them; the base backup is taken on the home and recorded
// here (RoutedBackups). Retention is the node's prune timer's, which prunes every project in the
// registry.
//
// A project is due when its newest completed base backup is older than Every (the timer's calendar
// is not read: a project that has none is due at once). A project that is paused, being upgraded or
// restored, or homed here is left out. One project is backed up at a time, each under a deadline of
// Timeout, and the project is read again just before its backup: a round can run for hours, and a
// project that moved or paused meanwhile is skipped, not asked on a node that is no longer its home.
// A project whose backup failed is tried again after Retry, with a backup.failed event in its history.
type ScheduledBackups struct {
	Registry registry.Registry
	// Self is the id of this node.
	Self func() string
	// Routed takes the base backups; Routed.Local snapshots the files.
	Routed *RoutedBackups
	// Leader says whether this node leads; nil: it does. A round on a node that does not lead does nothing.
	Leader func() bool
	// Every and Retry default to a day and an hour.
	Every, Retry time.Duration
	// Timeout bounds the backup of one project; it defaults to two hours.
	Timeout time.Duration
	Now     func() time.Time
	Log     *slog.Logger

	failed map[string]time.Time // ref -> when its last scheduled backup failed; Once runs one at a time
}

func (s *ScheduledBackups) every() time.Duration {
	if s.Every > 0 {
		return s.Every
	}
	return scheduledEvery
}

func (s *ScheduledBackups) retry() time.Duration {
	if s.Retry > 0 {
		return s.Retry
	}
	return scheduledRetry
}

func (s *ScheduledBackups) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return scheduledTimeout
}

func (s *ScheduledBackups) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *ScheduledBackups) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// Run takes the due backups every interval until ctx ends, the first round at once.
func (s *ScheduledBackups) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once takes the backup of every due project and returns the error of each that failed, by ref.
func (s *ScheduledBackups) Once(ctx context.Context) map[string]error {
	errs := map[string]error{}
	if s.Leader != nil && !s.Leader() {
		return errs
	}
	if s.Registry == nil || s.Self == nil || s.Routed == nil || s.Routed.Local == nil || s.Routed.Self == nil || s.Routed.Ops == nil {
		err := errors.New("placement: ScheduledBackups needs the registry, the node's id and the routed backups with their node id, ops and local service")
		s.log().Warn("scheduled backups: not set up", "error", err)
		errs[""] = err
		return errs
	}
	ps, err := s.Registry.ListProjects(ctx)
	if err != nil {
		s.log().Warn("scheduled backups: listing the projects failed", "error", err)
		errs[""] = err
		return errs
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Ref < ps[j].Ref })
	if s.failed == nil {
		s.failed = map[string]time.Time{}
	}
	for i := range ps {
		p := &ps[i]
		if ctx.Err() != nil {
			break
		}
		if !s.homedElsewhere(p) {
			continue
		}
		if at, ok := s.failed[p.Ref]; ok && s.now().Sub(at) < s.retry() {
			continue
		}
		due, err := s.due(ctx, p.Ref)
		if err != nil {
			s.log().Warn("scheduled backups: reading the backups failed", "ref", p.Ref, "error", err)
			errs[p.Ref] = err
			continue
		}
		if !due {
			delete(s.failed, p.Ref)
			continue
		}
		// The listing is older than this project's turn by the backups before it.
		cur, err := s.Registry.GetProject(ctx, p.Ref)
		if errors.Is(err, registry.ErrNotFound) {
			continue // deleted meanwhile
		}
		if err != nil {
			s.log().Warn("scheduled backups: reading the project failed", "ref", p.Ref, "error", err)
			errs[p.Ref] = err
			continue
		}
		if !s.homedElsewhere(cur) {
			continue
		}
		if err := s.backupWithin(ctx, cur); err != nil {
			s.failed[p.Ref] = s.now()
			errs[p.Ref] = err
			s.log().Warn("scheduled backup failed", "ref", p.Ref, "node", cur.NodeID, "error", err)
			_ = s.Registry.AppendEvent(context.WithoutCancel(ctx), p.Ref, "backup.failed",
				map[string]any{"reason": backup.ReasonScheduled, "node": cur.NodeID, "error": err.Error()})
			continue
		}
		delete(s.failed, p.Ref)
	}
	return errs
}

// homedElsewhere reports whether p is a running project of another node: the ones the schedule backs up.
func (s *ScheduledBackups) homedElsewhere(p *registry.Project) bool {
	return p.Ref != config.SystemRef && p.NodeID != "" && p.NodeID != s.Self() &&
		(p.Status == registry.StatusActiveHealthy || p.Status == registry.StatusActiveUnhealthy)
}

// due reports whether ref has no completed base backup newer than Every.
func (s *ScheduledBackups) due(ctx context.Context, ref string) (bool, error) {
	rows, err := s.Registry.ListBackups(ctx, ref)
	if err != nil {
		return false, err
	}
	cutoff := s.now().Add(-s.every())
	for _, b := range rows {
		if b.Kind == "base" && b.Status == registry.BackupCompleted && b.StartedAt.After(cutoff) {
			return false, nil
		}
	}
	return true, nil
}

// backupWithin is backup under the deadline of one project.
func (s *ScheduledBackups) backupWithin(ctx context.Context, p *registry.Project) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	return s.backup(ctx, p)
}

// backup snapshots the files of p here and takes its base backup on its home, as `backups create`
// does: the files go first, so that the snapshot a restore pairs with the base backup finished before
// the backup did, and a snapshot that failed does not keep the base backup from being taken.
func (s *ScheduledBackups) backup(ctx context.Context, p *registry.Project) error {
	var errs []error
	if _, err := s.Routed.Local.BackupFiles(ctx, p.Ref, backup.FilesOptions{Reason: backup.ReasonScheduled}); err != nil {
		errs = append(errs, err)
	}
	if _, err := s.Routed.at(ctx, p.NodeID, p.Ref, backup.BackupOptions{Reason: backup.ReasonScheduled}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
