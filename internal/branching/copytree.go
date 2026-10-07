package branching

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/OWNER/sbctl/internal/backup"
)

// errNoClone is returned by cloneFile when the filesystem (or the pair of paths) cannot
// share blocks between files.
var errNoClone = errors.New("branching: copy-on-write cloning is not supported here")

// Copy-on-write methods, as recorded on the branch (registry.BranchInfo.CloneMethod).
const (
	MethodSchema    = "schema"       // empty cluster plus the parent's migrations
	MethodClonefile = "clonefile"    // APFS clonefile(2)
	MethodReflink   = "reflink"      // FICLONE on XFS, btrfs, OpenZFS 2.2+
	MethodZFS       = "zfs-snapshot" // zfs snapshot of the dataset, copied from the snapshot
	MethodBackup    = "base-backup"  // restore of the parent's latest base backup plus WAL
)

// CloneStats reports one copy-on-write clone of a data directory.
type CloneStats struct {
	Method string `json:"method"`
	// FS is the filesystem of the parent's data directory.
	FS            string `json:"fs"`
	Files         int    `json:"files"`
	Cloned        int    `json:"cloned_files"`
	Copied        int    `json:"copied_files"` // files that fell back to a byte copy
	Bytes         int64  `json:"bytes"`        // apparent size of the files in the clone
	WALSegments   int    `json:"wal_segments"`
	StartLSN      string `json:"start_lsn"`
	StopLSN       string `json:"stop_lsn"`
	Attempts      int    `json:"attempts"`
	CopyMillis    int64  `json:"copy_ms"`  // the file copy alone
	TotalMillis   int64  `json:"total_ms"` // pg_backup_start to the last WAL segment
	ExtraDiskByte int64  `json:"extra_disk_bytes"`
}

// copier copies a data directory tree file by file, cloning where it can.
type copier struct {
	ctx context.Context
	// clone makes dst a copy-on-write copy of src.
	clone func(src, dst string) error
	// byteCopy allows a plain copy for a file that cannot be cloned. Without it that file
	// is an error.
	byteCopy bool
	stats    *CloneStats
}

// lastFile is copied after everything else: PostgreSQL's own base backup sends it last.
const lastFile = "global/pg_control"

// copyDataDir copies the data directory srcRoot to dstRoot (which exists), leaving out
// what a base backup leaves out. pg_wal is created empty with archive_status.
func (c *copier) copyDataDir(srcRoot, dstRoot string) error {
	if _, err := os.Lstat(filepath.Join(srcRoot, backup.DataDirInitPending)); err == nil {
		return fmt.Errorf("branching: %s is mid-initialization (%s exists)", srcRoot, backup.DataDirInitPending)
	}
	if err := c.dir(srcRoot, dstRoot, ""); err != nil {
		return err
	}
	return c.file(filepath.Join(srcRoot, lastFile), filepath.Join(dstRoot, lastFile), lastFile)
}

func (c *copier) dir(srcRoot, dstRoot, rel string) error {
	ents, err := os.ReadDir(filepath.Join(srcRoot, filepath.FromSlash(rel)))
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := c.ctx.Err(); err != nil {
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
		src, dst := filepath.Join(srcRoot, filepath.FromSlash(erel)), filepath.Join(dstRoot, filepath.FromSlash(erel))
		if erel == "pg_wal" { // sometimes a symlink to another disk; the clone gets a plain directory
			if err := os.MkdirAll(filepath.Join(dst, "archive_status"), 0o700); err != nil {
				return err
			}
			continue
		}
		mode := info.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			if rel == "pg_tblspc" {
				return backup.ErrTablespaces
			}
			return fmt.Errorf("branching: unsupported symlink %s in the data directory", erel)
		case mode.IsDir():
			if backup.DataDirSkip(erel, true) {
				continue
			}
			if err := os.Mkdir(dst, mode.Perm()); err != nil {
				return err
			}
			if rel == "" && backup.DataDirKeepsEmpty(e.Name()) {
				continue
			}
			if err := c.dir(srcRoot, dstRoot, erel); err != nil {
				return err
			}
		case mode.IsRegular():
			if backup.DataDirSkip(erel, false) || erel == lastFile {
				continue
			}
			if err := c.file(src, dst, erel); err != nil {
				return err
			}
		}
	}
	return nil
}

// file clones, or copies, one regular file. A file that disappears is skipped: the
// cluster is running, and WAL replay repairs what changed underneath the copy.
func (c *copier) file(src, dst, rel string) error {
	err := c.clone(src, dst)
	switch {
	case err == nil:
		c.stats.Cloned++
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case errors.Is(err, errNoClone) && c.byteCopy:
		if err := copyBytes(c.ctx, src, dst); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("copy %s: %w", rel, err)
		}
		c.stats.Copied++
	default:
		return fmt.Errorf("clone %s: %w", rel, err)
	}
	if fi, err := os.Stat(dst); err == nil {
		c.stats.Bytes += fi.Size()
	}
	c.stats.Files++
	return nil
}

// copyBytes is the fallback for a file that cannot be cloned.
func copyBytes(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, ctxReader{ctx, in}); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// syncDirs fsyncs every directory under root, deepest first, so the new tree's entries are durable.
func syncDirs(root string) error {
	var dirs []string
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		f, err := os.Open(d)
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// freeBytes is the space available to unprivileged writers on the filesystem of path.
func freeBytes(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// sameDevice reports whether a and b are on one filesystem (clones cannot cross).
func sameDevice(a, b string) bool {
	var sa, sb syscall.Stat_t
	if syscall.Stat(a, &sa) != nil || syscall.Stat(b, &sb) != nil {
		return false
	}
	return sa.Dev == sb.Dev
}
