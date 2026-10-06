// Package backup is sbctl's WAL archiver and base-backup engine: archive_command is
// `sbctl wal push`, restore_command is `sbctl wal fetch`, and a nightly timer takes a
// pg_basebackup per project. Backends: file:// and s3:// (any S3-compatible endpoint).
package backup

import (
	"context"
	"errors"
	"time"

	"github.com/OWNER/sbctl/internal/registry"
)

// ErrNoWAL is returned by FetchWAL when the segment is not in the archive; the CLI
// then exits non-zero, which Postgres's restore_command treats as end of archive.
var ErrNoWAL = errors.New("backup: WAL file not in archive")

type Backup interface {
	// PushWAL archives the file at path (absolute, or relative to the cluster's data dir
	// as %p is) under ref. It must not return success unless the file is durable.
	PushWAL(ctx context.Context, ref, path string) error
	// FetchWAL copies archived WAL file name (as %f) of ref to dest (as %p).
	FetchWAL(ctx context.Context, ref, name, dest string) error
	BaseBackup(ctx context.Context, ref string) (*registry.Backup, error)
	// Restore builds project newRef (ref itself when newRef is "") from the latest base
	// backup at or before target plus archived WAL up to target.
	Restore(ctx context.Context, ref string, target time.Time, newRef string) (*registry.Project, error)
}
