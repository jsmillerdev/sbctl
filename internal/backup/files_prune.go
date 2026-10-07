package backup

import (
	"context"
	"fmt"
	"strings"
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

	// A marker younger than orphanAge means a run may be about to reference blobs that no
	// snapshot lists yet; older ones are runs that died.
	markers, err := st.List(ctx, runningDir(ref, kind))
	if err != nil {
		return err
	}
	running := false
	var stale []string
	for _, m := range markers {
		if now.Sub(m.ModTime) < orphanAge {
			running = true
		} else {
			stale = append(stale, m.Key)
		}
	}
	if err := st.Delete(ctx, stale...); err != nil {
		return err
	}

	all, err := s.ListFilesSnapshots(ctx, ref, kind)
	if err != nil {
		return err
	}
	if res.DeletedOrphans, err = s.addOrphanSnapshots(ctx, ref, kind, all, res.DeletedOrphans); err != nil {
		return err
	}

	// The same retention rule as base backups, over the snapshots' finish times.
	stubs := make([]Manifest, len(all))
	for i, m := range all {
		stubs[i] = Manifest{ID: m.ID, StopTime: m.StopTime}
	}
	keepStubs, dropStubs := retainedBackups(stubs, now, days, nil)
	keepIDs := map[string]bool{}
	for _, m := range keepStubs {
		keepIDs[m.ID] = true
	}
	var keep []FilesSnapshot
	for _, m := range all {
		if keepIDs[m.ID] {
			keep = append(keep, m)
		}
	}
	for _, d := range dropStubs {
		var m *FilesSnapshot
		for i := range all {
			if all[i].ID == d.ID {
				m = &all[i]
			}
		}
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
	if err := st.Delete(ctx, doomed...); err != nil {
		return err
	}
	res.DeletedBlobs += len(doomed)
	return nil
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
