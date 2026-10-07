package backup

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// pruneFiles applies config.Backup.RetentionDays to ref's Storage and function snapshots
// with the rule Prune applies to base backups (retainedBackups): every snapshot that
// finished inside the window, plus the newest one before it, and at least one. Then it
// deletes the blobs that no kept snapshot lists.
func (s *Service) pruneFiles(ctx context.Context, ref string, res *PruneResult) error {
	days := s.opt.Config.Backup.RetentionDays
	if days <= 0 {
		return nil
	}
	for _, kind := range []string{KindStorage, KindFunctions} {
		if err := s.pruneKind(ctx, ref, kind, days, res); err != nil {
			return fmt.Errorf("backup: prune %s of %s: %w", kind, ref, err)
		}
	}
	return nil
}

func (s *Service) pruneKind(ctx context.Context, ref, kind string, days int, res *PruneResult) error {
	st := s.opt.Store
	now := s.opt.Now()

	running, err := s.sweepMarkers(ctx, ref, kind)
	if err != nil {
		return err
	}

	all, err := s.ListFilesSnapshots(ctx, ref, kind)
	if err != nil {
		return err
	}
	if res.DeletedOrphans, err = s.addOrphanSnapshots(ctx, ref, kind, all, res.DeletedOrphans); err != nil {
		return err
	}

	// The same retention rule as base backups, over the snapshots' finish times. The rule's
	// anchor (the newest snapshot before the window) is chosen among the snapshots a
	// restore to a time can pick: a pre-restore snapshot is never one of them, so it must
	// not push the older, usable snapshot out. Pre-restore snapshots are kept for the
	// window and then dropped.
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	var stubs []Manifest
	for _, m := range all {
		if m.Reason != ReasonPreRestore {
			stubs = append(stubs, Manifest{ID: m.ID, StopTime: m.StopTime})
		}
	}
	keepStubs, _ := retainedBackups(stubs, now, days, nil)
	keepIDs := map[string]bool{}
	for _, m := range keepStubs {
		keepIDs[m.ID] = true
	}
	var keep, drop []FilesSnapshot
	for _, m := range all {
		switch {
		case m.Reason == ReasonPreRestore && !m.StopTime.Before(cutoff):
			keep = append(keep, m)
		case m.Reason == ReasonPreRestore:
			drop = append(drop, m)
		case keepIDs[m.ID]:
			keep = append(keep, m)
		default:
			drop = append(drop, m)
		}
	}
	for i := range drop {
		m := &drop[i]
		objs, err := st.List(ctx, m.Dir()+"/")
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(objs))
		for _, o := range objs {
			if o.Key != m.Dir()+"/"+summaryName {
				keys = append(keys, o.Key)
			}
		}
		// Summary first: a half-deleted snapshot must not look restorable.
		if err := st.Delete(ctx, m.Dir()+"/"+summaryName); err != nil {
			return err
		}
		if err := st.Delete(ctx, keys...); err != nil {
			return err
		}
		res.DeletedFileSnapshots++
	}
	if running {
		return nil
	}

	referenced := map[string]bool{}
	for _, m := range keep {
		if err := s.readEntries(ctx, m.Dir()+"/"+entriesName, func(e fileEntry) error {
			referenced[e.Hash] = true
			return nil
		}); err != nil {
			// Deleting blobs on the strength of a list that could not be read would destroy
			// the snapshots that need them.
			return fmt.Errorf("snapshot %s is unreadable, so no blob is deleted: %w", m.ID, err)
		}
	}
	blobs, err := st.List(ctx, blobsDir(ref, kind))
	if err != nil {
		return err
	}
	var doomed []string
	for _, b := range blobs {
		name := b.Key[strings.LastIndex(b.Key, "/")+1:]
		hash := strings.TrimSuffix(name, ".zst")
		if !referenced[hash] && now.Sub(b.ModTime) >= orphanAge {
			doomed = append(doomed, b.Key)
		}
	}
	if len(doomed) == 0 {
		return nil
	}

	// The sweep above took a while. A run that began after the first look at the markers
	// may have found one of these blobs in the backend, skipped uploading it and be about
	// to reference it, or have finished already with a snapshot this sweep never read. Look
	// again right before deleting, and leave the blobs for the next prune if anything is
	// new.
	if running, err := s.sweepMarkers(ctx, ref, kind); err != nil || running {
		return err
	}
	known := map[string]bool{}
	for _, m := range all {
		known[m.ID] = true
	}
	ids, err := st.ListDirs(ctx, snapshotsDir(ref, kind))
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !known[id] {
			return nil
		}
	}
	if err := st.Delete(ctx, doomed...); err != nil {
		return err
	}
	res.DeletedBlobs += len(doomed)
	return nil
}

// sweepMarkers reports whether a backup of ref's kind is running: a marker younger than
// orphanAge means a run may be about to reference blobs that no snapshot lists yet. Older
// markers are runs that died, and it deletes them.
func (s *Service) sweepMarkers(ctx context.Context, ref, kind string) (running bool, err error) {
	st := s.opt.Store
	markers, err := st.List(ctx, runningDir(ref, kind))
	if err != nil {
		return false, err
	}
	var stale []string
	for _, m := range markers {
		if s.opt.Now().Sub(m.ModTime) < orphanAge {
			running = true
		} else {
			stale = append(stale, m.Key)
		}
	}
	return running, st.Delete(ctx, stale...)
}

// addOrphanSnapshots deletes snapshot directories without a summary that have been idle for
// orphanAge (a run that died after uploading its entries) and adds them to n.
func (s *Service) addOrphanSnapshots(ctx context.Context, ref, kind string, complete []FilesSnapshot, n int) (int, error) {
	st := s.opt.Store
	ids, err := st.ListDirs(ctx, snapshotsDir(ref, kind))
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
		objs, err := st.List(ctx, snapshotsDir(ref, kind)+id+"/")
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
	return n, nil
}
