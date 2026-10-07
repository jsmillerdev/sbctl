package backup

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/registry"
)

// orphanAge is how old an upload without a manifest must be before prune treats it
// as abandoned (a backup in flight stays younger than this).
const orphanAge = 24 * time.Hour

// PruneResult reports what Prune removed.
type PruneResult struct {
	Ref            string
	KeptBackups    []string // backup ids
	DeletedBackups []string
	DeletedWAL     int
	DeletedOrphans int
}

// retainedBackups applies the retention rule to backups (oldest first). A backup is
// kept when it finished within the window, plus the newest one that finished before the
// window starts and can still be restored from (the anchor: without it the oldest
// instant in the window could not be restored), so at least one backup always survives.
//
// usable reports whether a restore can start from a backup (on the archive's current
// timeline history, see timelineHistory.onHistory); nil accepts every backup. An
// off-history backup, such as a timeline-1 backup taken after an in-place restore forked
// timeline 2, must not become the anchor: it would crowd out the older backup that
// restores can actually use, and the WAL that backup needs would be deleted.
func retainedBackups(all []Manifest, now time.Time, days int, usable func(*Manifest) bool) (keep, drop []Manifest) {
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	anchor, newest := -1, -1
	for i := range all {
		m := &all[i]
		if newest < 0 || m.StopTime.After(all[newest].StopTime) {
			newest = i
		}
		if m.StopTime.Before(cutoff) && (usable == nil || usable(m)) && (anchor < 0 || m.StopTime.After(all[anchor].StopTime)) {
			anchor = i
		}
	}
	for i, m := range all {
		if !m.StopTime.Before(cutoff) || i == anchor {
			keep = append(keep, m)
		} else {
			drop = append(drop, m)
		}
	}
	if len(keep) == 0 && newest >= 0 { // every backup is old and off-history: still keep one
		keep, drop = []Manifest{all[newest]}, nil
		for i, m := range all {
			if i != newest {
				drop = append(drop, m)
			}
		}
	}
	return keep, drop
}

// walNeeded reports whether WAL file name must be kept for at least one of the
// retained backups: anything on a later timeline than the backup's, or on its
// timeline at or after the segment the backup starts from. Timeline history files
// are tiny and always kept. Names that are not WAL files are kept.
func walNeeded(name string, retained []Manifest) bool {
	w, ok := parseWALName(name)
	if !ok || w.Kind == "history" {
		return true
	}
	for _, m := range retained {
		b, ok := parseWALName(m.StartWAL)
		if !ok {
			return true // cannot reason about it: keep everything
		}
		if w.Timeline > b.Timeline || (w.Timeline == b.Timeline && w.Seg >= b.Seg) {
			return true
		}
	}
	return false
}

// Prune applies config.Backup.RetentionDays to ref: it deletes base backups outside
// the window (always keeping at least one, see retainedBackups), the WAL no kept
// backup needs, and abandoned uploads, and drops the matching registry rows. With
// no base backup at all it deletes nothing, because WAL alone restores nothing.
func (s *Service) Prune(ctx context.Context, ref string) (*PruneResult, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	if err := s.need("config", s.opt.Config != nil); err != nil {
		return nil, err
	}
	res := &PruneResult{Ref: ref}
	days := s.opt.Config.Backup.RetentionDays
	if days <= 0 {
		return res, nil // retention disabled
	}
	st := s.opt.Store
	all, err := s.ListBackups(ctx, ref)
	if err != nil {
		return nil, err
	}
	if res.DeletedOrphans, err = s.pruneOrphans(ctx, ref, all); err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return res, nil
	}
	hist, err := s.latestHistory(ctx, ref)
	if err != nil {
		return nil, err
	}
	keep, drop := retainedBackups(all, s.opt.Now(), days, hist.onHistory)
	for _, m := range keep {
		res.KeptBackups = append(res.KeptBackups, m.ID)
	}

	for _, m := range drop {
		objs, err := st.List(ctx, m.Dir()+"/")
		if err != nil {
			return res, err
		}
		keys := make([]string, 0, len(objs))
		for _, o := range objs {
			if o.Key != m.Dir()+"/"+manifestName {
				keys = append(keys, o.Key)
			}
		}
		// Manifest first: a half-deleted backup must not look restorable.
		if err := st.Delete(ctx, m.Dir()+"/"+manifestName); err != nil {
			return res, err
		}
		if err := st.Delete(ctx, keys...); err != nil {
			return res, err
		}
		res.DeletedBackups = append(res.DeletedBackups, m.ID)
		s.dropRegistryRow(ctx, ref, m.ID)
	}

	wal, err := st.List(ctx, walDir(ref))
	if err != nil {
		return res, err
	}
	var stale []string
	for _, o := range wal {
		name := strings.TrimSuffix(strings.TrimPrefix(o.Key, walDir(ref)), ".zst")
		if !walNeeded(name, keep) {
			stale = append(stale, o.Key)
		}
	}
	if err := st.Delete(ctx, stale...); err != nil {
		return res, err
	}
	res.DeletedWAL = len(stale)
	return res, nil
}

// dropRegistryRow removes the registry row of backup id, if the registry has one.
func (s *Service) dropRegistryRow(ctx context.Context, ref, id string) {
	if s.opt.Registry == nil {
		return
	}
	rows, err := s.opt.Registry.ListBackups(ctx, ref)
	if err != nil {
		return
	}
	loc := s.opt.Store.URL(baseDir(ref) + id)
	for _, r := range rows {
		if r.Location == loc {
			_ = s.opt.Registry.DeleteBackup(ctx, r.ID)
		}
	}
}

// pruneOrphans deletes upload directories without a manifest that have been idle for
// orphanAge, and marks registry rows stuck in "running" for that long as failed.
func (s *Service) pruneOrphans(ctx context.Context, ref string, complete []Manifest) (int, error) {
	st := s.opt.Store
	n := 0
	if tc, ok := st.(TempCleaner); ok {
		// Files from Puts that crashed half way: List never shows them.
		c, err := tc.DeleteStaleTemps(ctx, ref+"/", s.opt.Now().Add(-orphanAge))
		if err != nil {
			return 0, err
		}
		n += c
	}
	ids, err := st.ListDirs(ctx, baseDir(ref))
	if err != nil {
		return n, err
	}
	have := map[string]bool{}
	for _, m := range complete {
		have[m.ID] = true
	}
	for _, id := range ids {
		if have[id] {
			continue
		}
		objs, err := st.List(ctx, baseDir(ref)+id+"/")
		if err != nil {
			return n, err
		}
		fresh := false
		keys := make([]string, len(objs))
		for i, o := range objs {
			keys[i] = o.Key
			if s.opt.Now().Sub(o.ModTime) < orphanAge {
				fresh = true
			}
		}
		if fresh || len(keys) == 0 {
			continue
		}
		if err := st.Delete(ctx, keys...); err != nil {
			return n, err
		}
		n++
	}
	if s.opt.Registry != nil {
		rows, err := s.opt.Registry.ListBackups(ctx, ref)
		if err == nil {
			for _, r := range rows {
				if r.Status == registry.BackupRunning && s.opt.Now().Sub(r.StartedAt) > orphanAge {
					now := s.opt.Now()
					r.Status, r.Error, r.FinishedAt = registry.BackupFailed, "abandoned: no manifest after 24h", &now
					_ = s.opt.Registry.UpdateBackup(ctx, &r)
				}
			}
		}
	}
	return n, nil
}

// PruneAll prunes every project known to the registry and every ref that has data in
// the store (deleted projects keep their final backup).
func (s *Service) PruneAll(ctx context.Context) ([]*PruneResult, error) {
	if err := s.need("registry", s.opt.Registry != nil); err != nil {
		return nil, err
	}
	refs := map[string]bool{}
	ps, err := s.opt.Registry.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		refs[p.Ref] = true
	}
	dirs, err := s.opt.Store.ListDirs(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if validRef(d) == nil {
			refs[d] = true
		}
	}
	names := make([]string, 0, len(refs))
	for r := range refs {
		names = append(names, r)
	}
	sort.Strings(names)
	var out []*PruneResult
	var firstErr error
	for _, r := range names {
		res, err := s.Prune(ctx, r)
		if err != nil {
			s.opt.Log.Error("prune failed", "ref", r, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("prune %s: %w", r, err)
			}
			continue
		}
		out = append(out, res)
	}
	return out, firstErr
}
