// Package backup is supavise's WAL archiver and base-backup engine: archive_command is
// `supavise wal push`, restore_command is `supavise wal fetch`, and a nightly timer takes a
// base backup per project. Backends: file:// and s3:// (any S3-compatible endpoint).
//
// Layout under the backend root, one tree per project ref:
//
//	<ref>/wal/<segment>.zst                  every archived WAL file, zstd-compressed
//	<ref>/base/<id>/data.tar.zst             the data directory as one tar stream
//	<ref>/base/<id>/secrets.json             the project's sealed secrets at backup time
//	<ref>/base/<id>/backup.json              the manifest; written last, so its presence
//	                                         means the base backup is complete
//
// <id> is the backup start time, UTC, as 20060102T150405Z, plus "-" and six random hex
// digits (20060102T150405Z-3f9a1c), so two backups that start in the same second never
// share a directory.
package backup

import (
	"context"
	"errors"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// ErrNoWAL is returned by FetchWAL when the segment is not in the archive; the CLI
// then exits non-zero, which Postgres's restore_command treats as end of archive.
var ErrNoWAL = errors.New("backup: WAL file not in archive")

// Backup is the contract the rest of supavise programs against. Service implements it.
type Backup interface {
	// PushWAL archives the file at path (absolute, or relative to the cluster's data dir
	// as %p is) under ref. It must not return success unless the file is durable.
	PushWAL(ctx context.Context, ref, path string) error
	// FetchWAL copies archived WAL file name (as %f) of ref to dest (as %p).
	FetchWAL(ctx context.Context, ref, name, dest string) error
	BaseBackup(ctx context.Context, ref string) (*registry.Backup, error)
	// Restore builds project newRef (ref itself when newRef is "") from the latest base
	// backup at or before target plus archived WAL up to target. Restoring in place
	// needs Service.RestoreWith and an explicit Force.
	Restore(ctx context.Context, ref string, target time.Time, newRef string) (*registry.Project, error)
}
