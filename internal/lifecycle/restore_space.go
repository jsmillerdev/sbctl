package lifecycle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ErrInsufficientDisk is returned by BeginRestore when the disk holding the project's data
// directory cannot take the restored copy next to the current one. The restore keeps the
// current directory aside until it is over, and a full disk stops WAL writes and archiving
// for every project on the node, so the request is refused (409) before anything moves.
var ErrInsufficientDisk = errors.New("lifecycle: not enough free disk to restore")

// restoreReserve is the room a restore leaves beyond two copies of the data: a fifth of the
// data (WAL replayed during recovery, the base backup taken afterwards on a local backend)
// and no less than a gibibyte.
const restoreReserve = 1 << 30

// The directories an in-place restore leaves next to PGDATA: <data>.pre-restore-<time> (the
// data from before a restore) and <data>.failed-restore-<time> (a restore that did not work).
const (
	preRestoreInfix    = ".pre-restore-"
	failedRestoreInfix = ".failed-restore-"
)

// dirSize is the apparent size of a directory tree. A directory that does not exist, or a
// file that vanishes during the walk, counts as zero.
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// diskFree is the space available to unprivileged writers on the filesystem of path (-1:
// unknown).
func diskFree(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// restoreNeed is the free space an in-place restore of a project holding size bytes wants:
// the current directory stays, the restored copy is about as large, and the reserve covers
// what the recovery writes.
func restoreNeed(size int64) int64 {
	return 2*size + max(size/5, restoreReserve)
}

// checkRestoreSpace refuses a restore the disk cannot hold. When the free space cannot be
// read (the directory is not there, or the filesystem does not report it) the check is
// skipped with a warning, so it never blocks a restore on a node it cannot measure.
func (e *Engine) checkRestoreSpace(ref string) error {
	dataDir := e.cfg.Paths().PostgresData(ref)
	free := e.freeBytes(dataDir)
	for d := dataDir; free < 0 && filepath.Dir(d) != d; {
		d = filepath.Dir(d)
		free = e.freeBytes(d)
	}
	if free < 0 {
		e.log.Warn("restore disk check skipped: the free space could not be read", "ref", ref, "dir", dataDir)
		return nil
	}
	size := dirSize(dataDir)
	need := restoreNeed(size)
	if free >= need {
		return nil
	}
	return fmt.Errorf("%w: the disk has %s free and restoring project %s needs about %s (its data is %s; the restore keeps it "+
		"aside, writes a second copy and needs room to replay WAL). Free some disk space on the server and try again",
		ErrInsufficientDisk, humanBytes(free), ref, humanBytes(need), humanBytes(size))
}

// pruneRestoreLeftovers bounds what restores leave on disk for ref. After a restore that
// worked, the newest <data>.pre-restore-* (the data from just before it) stays and the older
// ones and every <data>.failed-restore-* go. After one that failed, the newest
// <data>.failed-restore-* stays for diagnosis and older failed ones go; the pre-restore
// directories are untouched, because a failed rollback leaves the original data in one.
// Removal problems are logged: they never fail a restore that is over.
func (e *Engine) pruneRestoreLeftovers(ref string, restored bool) {
	dataDir := e.cfg.Paths().PostgresData(ref)
	pre := leftovers(dataDir + preRestoreInfix)
	failed := leftovers(dataDir + failedRestoreInfix)
	var drop []string
	if restored {
		if len(pre) > 0 {
			pre = pre[:len(pre)-1]
		}
		drop = append(pre, failed...)
	} else if len(failed) > 0 {
		drop = failed[:len(failed)-1]
	}
	for _, d := range drop {
		if err := os.RemoveAll(d); err != nil {
			e.log.Warn("restore: could not remove an old data directory", "ref", ref, "dir", d, "error", err)
			continue
		}
		e.log.Info("restore: removed an old data directory", "ref", ref, "dir", d)
	}
}

// leftovers lists the directories named prefix+<time>, oldest first (the time is in a sortable
// layout).
func leftovers(prefix string) []string {
	m, _ := filepath.Glob(prefix + "*")
	out := m[:0]
	for _, d := range m {
		if fi, err := os.Lstat(d); err == nil && fi.IsDir() && !strings.ContainsRune(strings.TrimPrefix(d, prefix), filepath.Separator) {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
