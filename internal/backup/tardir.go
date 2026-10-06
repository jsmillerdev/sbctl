package backup

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The exclusions below follow PostgreSQL's own base backup (src/backend/backup/
// basebackup.c, excludeFiles and excludeDirContents), so the tar holds what
// pg_basebackup would send. postmaster.opts is excluded too; the restore seeder
// recreates it because the Supabase launcher refuses a data dir without one.

// excludeDirContents are top-level directories whose contents are omitted but whose
// (empty) directory must exist in the restored cluster.
var excludeDirContents = map[string]bool{
	"pg_stat_tmp": true, "pg_replslot": true, "pg_dynshmem": true, "pg_notify": true,
	"pg_serial": true, "pg_snapshots": true, "pg_subtrans": true,
	"pg_wal": true, // handled separately: archive_status is created, nothing else is copied
}

var excludeFiles = map[string]bool{
	"postgresql.auto.conf.tmp": true, "current_logfiles.tmp": true,
	"backup_label": true, "tablespace_map": true, "backup_manifest": true,
	"postmaster.pid": true, "postmaster.opts": true,
}

// tempRelRE matches temporary relation files ("t<backend>_<relfilenode>[_fork]").
var tempRelRE = regexp.MustCompile(`^t[0-9]+_[0-9]+`)

const initPendingFile = ".supabase-postgres-init-pending"

// ErrTablespaces is returned for clusters that use tablespaces, which the tar format
// here does not carry. Supabase projects do not use them.
var ErrTablespaces = errors.New("backup: data directory uses tablespaces (pg_tblspc is not empty), which are not supported")

type tarStats struct {
	Files int
	Bytes int64
}

func skipEntry(rel string, isDir bool) bool {
	base := path.Base(rel)
	if strings.HasPrefix(base, "pgsql_tmp") {
		return true
	}
	if isDir {
		return false
	}
	if excludeFiles[base] || strings.HasPrefix(base, "pg_internal.init") {
		return true
	}
	return strings.HasPrefix(rel, "base/") && tempRelRE.MatchString(base)
}

// writeDataDirTar writes the data directory under root to tw. It does not write
// backup_label or tablespace_map: those come from pg_backup_stop.
//
// The cluster is running, so files change underneath the copy. That is what
// pg_backup_start/pg_backup_stop exist for: WAL replay repairs torn and stale pages.
// A file that vanishes is skipped; one that shrinks is zero-padded to the size the
// header promised; one that grows is cut at the size seen at open.
func writeDataDirTar(ctx context.Context, root string, tw *tar.Writer) (tarStats, error) {
	var st tarStats
	if _, err := os.Lstat(filepath.Join(root, initPendingFile)); err == nil {
		return st, fmt.Errorf("backup: %s is mid-initialization (%s exists)", root, initPendingFile)
	}
	err := addDir(ctx, root, "", tw, &st)
	return st, err
}

func addDir(ctx context.Context, root, rel string, tw *tar.Writer, st *tarStats) error {
	ents, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := ctx.Err(); err != nil {
			return err
		}
		erel := path.Join(rel, e.Name())
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		// pg_wal is sometimes a symlink to another disk; archive it as a plain directory.
		if erel == "pg_wal" {
			if err := writeDir(tw, erel, 0o700, info.ModTime()); err != nil {
				return err
			}
			if err := writeDir(tw, "pg_wal/archive_status", 0o700, info.ModTime()); err != nil {
				return err
			}
			continue
		}
		mode := info.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			if rel == "pg_tblspc" {
				return ErrTablespaces
			}
			return fmt.Errorf("backup: unsupported symlink %s in data directory", erel)
		case mode.IsDir():
			if skipEntry(erel, true) {
				continue
			}
			if err := writeDir(tw, erel, mode.Perm(), info.ModTime()); err != nil {
				return err
			}
			if rel == "" && excludeDirContents[e.Name()] {
				continue
			}
			if err := addDir(ctx, root, erel, tw, st); err != nil {
				return err
			}
		case mode.IsRegular():
			if skipEntry(erel, false) {
				continue
			}
			if err := addFile(ctx, filepath.Join(root, filepath.FromSlash(erel)), erel, tw, st); err != nil {
				return err
			}
		}
		// Sockets, FIFOs and devices do not belong in a backup.
	}
	return nil
}

func writeDir(tw *tar.Writer, rel string, perm fs.FileMode, mod time.Time) error {
	return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: int64(perm), ModTime: mod})
}

func addFile(ctx context.Context, p, rel string, tw *tar.Writer, st *tarStats) error {
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: int64(fi.Mode().Perm()), Size: size, ModTime: fi.ModTime()}); err != nil {
		return err
	}
	n, err := io.CopyN(tw, ctxReader{ctx, f}, size)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("backup: reading %s: %w", rel, err)
	}
	if n < size { // shrank while we read it
		if _, err := io.CopyN(tw, zeroReader{}, size-n); err != nil {
			return err
		}
	}
	st.Files++
	st.Bytes += size
	return nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
