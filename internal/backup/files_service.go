package backup

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// FilesOptions tunes BackupFiles.
type FilesOptions struct {
	// Reason is recorded in the snapshot summaries: ReasonManual (default), ReasonScheduled,
	// ReasonFinal or ReasonPreRestore.
	Reason string
	// SkipStorage and SkipFunctions leave one kind out.
	SkipStorage, SkipFunctions bool
}

// FilesResult reports what BackupFiles did. A nil snapshot means that kind had nothing to
// back up or was skipped; Notes say why when the reason is not obvious.
type FilesResult struct {
	Storage   *FilesSnapshot
	Functions *FilesSnapshot
	Notes     []string
}

// BackupFiles snapshots ref's Storage objects and Edge Function deployments into the
// backend, next to its database backups. Only files that are new or changed since the
// previous snapshot are copied (see files.go). It runs on the node, as the user that owns
// the state directory: the clusters and the tenant-facing units never see the backend.
//
// Storage objects are read from the node's disk. A node whose Storage uses its S3
// backend keeps the objects in the operator's own bucket, which this does not copy.
func (s *Service) BackupFiles(ctx context.Context, ref string, fo FilesOptions) (*FilesResult, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	if fo.Reason == "" {
		fo.Reason = ReasonManual
	}
	res := &FilesResult{}
	var errs []error
	if !fo.SkipStorage {
		if s.opt.Config != nil && s.opt.Config.Fleet.StorageBackend == "s3" && s.opt.StorageDir == nil {
			res.Notes = append(res.Notes, "Storage uses its S3 backend, so its objects are in your own bucket and are not copied here; turn on versioning for that bucket")
		} else {
			snap, err := s.backupStorage(ctx, ref, fo.Reason)
			res.Storage = snap
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	if !fo.SkipFunctions {
		snap, err := s.backupFunctions(ctx, ref, fo.Reason)
		res.Functions = snap
		if err != nil {
			errs = append(errs, err)
		}
	}
	s.filesEvent(ctx, ref, res, errs)
	return res, errors.Join(errs...)
}

func (s *Service) filesEvent(ctx context.Context, ref string, res *FilesResult, errs []error) {
	if s.opt.Registry == nil {
		return
	}
	ectx := context.WithoutCancel(ctx)
	if len(errs) > 0 {
		_ = s.opt.Registry.AppendEvent(ectx, ref, "files.backup.failed", map[string]any{"error": errors.Join(errs...).Error()})
		return
	}
	p := map[string]any{}
	if m := res.Storage; m != nil {
		p["storage"] = map[string]any{"id": m.ID, "reason": m.Reason, "files": m.Files, "bytes": m.Bytes, "new_files": m.NewFiles, "new_stored_bytes": m.NewBytes}
	}
	if m := res.Functions; m != nil {
		p["functions"] = map[string]any{"id": m.ID, "reason": m.Reason, "files": m.Files, "bytes": m.Bytes, "new_files": m.NewFiles, "new_stored_bytes": m.NewBytes}
	}
	if len(p) > 0 {
		_ = s.opt.Registry.AppendEvent(ectx, ref, "files.backup.completed", p)
	}
}

// FilesRestoreOptions tunes RestoreFiles.
type FilesRestoreOptions struct {
	// At is the target time: the newest snapshot that finished at or before it is used.
	At time.Time
	// Latest uses the newest snapshot whatever the time.
	Latest bool
	// SnapshotID restores exactly this snapshot (of whichever kind has it) and leaves the
	// other kind alone. It is the way to bring back a pre-restore snapshot.
	SnapshotID string
	// Progress receives one line per notable step (default: dropped).
	Progress func(msg string)
}

// FilesRestoreResult reports what RestoreFiles restored.
type FilesRestoreResult struct {
	Storage   *FilesSnapshot
	Functions *FilesSnapshot
	// Aside is where the replaced objects directory was kept (an in-place restore).
	Aside string
	Notes []string
}

func (o FilesRestoreOptions) note(res *FilesRestoreResult, format string, a ...any) {
	m := fmt.Sprintf(format, a...)
	res.Notes = append(res.Notes, m)
	if o.Progress != nil {
		o.Progress(m)
	}
}

// RestoreFiles brings ref's Storage objects and function deployments back from the newest
// snapshot at or before the target, into project into (ref itself when into is empty or
// equal to ref, which replaces what is there). Objects return to the last snapshot, not to
// the exact second: unlike the database they have no log to replay.
//
// A kind with no snapshot at or before the target is left as it is and said so in Notes;
// the database restore does not fail for a project that was never backed up this way.
func (s *Service) RestoreFiles(ctx context.Context, ref, into string, o FilesRestoreOptions) (*FilesRestoreResult, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	if into == "" {
		into = ref
	}
	if err := validRef(into); err != nil {
		return nil, err
	}
	replace := into == ref
	res := &FilesRestoreResult{}
	stamp := s.opt.Now().UTC().Format("20060102T150405Z")
	var errs []error
	for _, kind := range []string{KindStorage, KindFunctions} {
		all, err := s.ListFilesSnapshots(ctx, ref, kind)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var snap *FilesSnapshot
		switch {
		case o.SnapshotID != "":
			if snap, err = pickFilesSnapshot(all, o.At, o.Latest, o.SnapshotID); err != nil {
				continue // the other kind holds it
			}
		case len(all) == 0:
			o.note(res, "%s: no backup of this project's %s exists, so none was restored", ref, kindName(kind))
			continue
		default:
			if snap, err = pickFilesSnapshot(all, o.At, o.Latest, ""); err != nil {
				o.note(res, "%s: the earliest %s backup finished at %s, after the target, so none was restored",
					ref, kindName(kind), all[0].StopTime.UTC().Format(time.RFC3339))
				continue
			}
		}
		switch kind {
		case KindStorage:
			dest := s.storageDir(into)
			if dest == "" {
				o.note(res, "Storage uses its S3 backend: its objects are in your own bucket and were not restored")
				continue
			}
			aside, err := s.restoreStorage(ctx, snap, dest, replace, stamp)
			if err != nil {
				errs = append(errs, fmt.Errorf("backup: restore objects of %s into %s: %w", ref, into, err))
				continue
			}
			res.Storage, res.Aside = snap, aside
			o.note(res, "objects: %d files restored from the backup of %s", snap.Files, snap.StopTime.UTC().Format(time.RFC3339))
		case KindFunctions:
			if s.opt.Functions == nil {
				o.note(res, "no function store is configured here: functions were not restored")
				continue
			}
			if replace {
				// Rows cannot be set aside like a directory, so what is about to be replaced
				// is snapshotted first.
				if _, err := s.backupFunctions(ctx, ref, ReasonPreRestore); err != nil {
					errs = append(errs, fmt.Errorf("backup: functions of %s were not replaced, because the snapshot of their current state failed: %w", ref, err))
					continue
				}
			}
			if err := s.restoreFunctions(ctx, snap, into, replace); err != nil {
				errs = append(errs, fmt.Errorf("backup: restore functions of %s into %s: %w", ref, into, err))
				continue
			}
			res.Functions = snap
			o.note(res, "functions: %d deployments restored from the backup of %s", len(snap.Functions), snap.StopTime.UTC().Format(time.RFC3339))
		}
	}
	return res, errors.Join(errs...)
}

func kindName(kind string) string {
	if kind == KindFunctions {
		return "Edge Functions"
	}
	return "Storage objects"
}
