package backup

import (
	"context"
	"errors"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

var _ BaseBackupEnsurer = (*Service)(nil)

// BackupIDOf is the Manifest.ID of the base backup that b indexes (the last element of its
// Location), which is what ReplicaSeedPlan.BackupID takes. Empty when b has no location.
func BackupIDOf(b *registry.Backup) string {
	if b == nil || b.Location == "" {
		return ""
	}
	return path.Base(strings.TrimRight(b.Location, "/"))
}

// EnsureBase implements BaseBackupEnsurer. Calls for one ref run one at a time, so two replicas
// of a project that are set up together share the backup the first call takes. A backup counts
// when it is complete, on the archive's current timeline and its first WAL file is still in the
// archive; its age is the time since it finished.
func (s *Service) EnsureBase(ctx context.Context, ref string, maxAge time.Duration) (*registry.Backup, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	defer s.lockEnsure(ref)()
	if maxAge > 0 {
		m, err := s.newestUsableBase(ctx, ref)
		if err != nil {
			return nil, err
		}
		if m != nil && s.opt.Now().Sub(m.StopTime) < maxAge {
			return s.baseRow(ctx, m), nil
		}
	}
	if s.opt.TakeBase != nil {
		return s.opt.TakeBase(ctx, ref)
	}
	return s.BaseBackup(ctx, ref)
}

// lockEnsure takes ref's EnsureBase lock and returns the function that releases it.
func (s *Service) lockEnsure(ref string) (unlock func()) {
	s.ensureMu.Lock()
	m := s.ensure[ref]
	if m == nil {
		if s.ensure == nil {
			s.ensure = map[string]*sync.Mutex{}
		}
		m = &sync.Mutex{}
		s.ensure[ref] = m
	}
	s.ensureMu.Unlock()
	m.Lock()
	return m.Unlock
}

// newestUsableBase is the newest complete base backup of ref that a standby can start from, or
// nil when there is none.
func (s *Service) newestUsableBase(ctx context.Context, ref string) (*Manifest, error) {
	all, err := s.ListBackups(ctx, ref)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	hist, err := s.latestHistory(ctx, ref)
	if err != nil {
		return nil, err
	}
	var best *Manifest
	for i := range all {
		if hist.onHistory(&all[i]) && (best == nil || all[i].StopTime.After(best.StopTime)) {
			best = &all[i]
		}
	}
	if best == nil {
		return nil, nil
	}
	if _, err := s.opt.Store.Stat(ctx, walKey(ref, best.StartWAL)); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return best, nil
}

// baseRow is the registry row of manifest m: the row the registry holds for it, or one built from
// the manifest when the registry has none (the backup was taken by a node whose rows were lost).
func (s *Service) baseRow(ctx context.Context, m *Manifest) *registry.Backup {
	loc := s.opt.Store.URL(m.Dir())
	if s.opt.Registry != nil {
		if rows, err := s.opt.Registry.ListBackups(ctx, m.Ref); err == nil {
			for i := range rows {
				if rows[i].Location == loc && rows[i].Status == registry.BackupCompleted {
					return &rows[i]
				}
			}
		}
	}
	stop := m.StopTime
	return &registry.Backup{Ref: m.Ref, Kind: "base", Status: registry.BackupCompleted, Location: loc,
		Timeline: m.Timeline, StartLSN: m.StartLSN, StopLSN: m.StopLSN, SizeBytes: m.StoredBytes,
		StartedAt: m.StartTime, FinishedAt: &stop}
}
