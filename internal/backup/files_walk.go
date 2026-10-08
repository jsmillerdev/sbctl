package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// StorageObject is one object of a project's Storage objects directory, as WalkStorage reports it.
type StorageObject struct {
	// Path is the slash-separated path below the objects directory, which is Storage's key:
	// <bucket>/<name>/<version>. It is not checked to be ASCII.
	Path string
	// Abs is the path on disk. Open reads it without following a link.
	Abs     string
	Size    int64
	ModTime time.Time
	// Attrs are the extended attributes Storage keeps its metadata in, by their full names
	// ("user.supabase.content-type" on Linux); nil when the file has none or the file system
	// has no extended attributes.
	Attrs map[string][]byte
}

// Attr returns the metadata Storage kept for the object under name ("content-type",
// "cache-control", "content-encoding", "etag"), and false when it kept none.
func (o StorageObject) Attr(name string) (string, bool) {
	for _, p := range storageAttrPrefixes {
		if v, ok := o.Attrs[p+name]; ok {
			return string(v), true
		}
	}
	return "", false
}

// Open opens the object's content for reading. A symbolic link is refused.
func (o StorageObject) Open() (*os.File, error) { return openNoFollow(o.Abs) }

// WalkStorageOptions tunes WalkStorage.
type WalkStorageOptions struct {
	// Since leaves out the objects last modified at or before it (zero: every object). A copy
	// that takes passes uses the start of the previous pass.
	Since time.Time
	// Skipped is called with the path of each entry that is not a regular file (a link or a device
	// would take a reader out of the tree); such entries are left out. Optional.
	Skipped func(abs string)
}

// WalkStorage calls fn for each object below root (a project's Storage objects directory,
// config.Paths.StorageObjects) in path order, one at a time, with the size, modification time and
// Storage's extended attributes read from the file system. It is the walk the nightly copy of the
// objects uses. A missing root holds no objects; files that vanish during the walk are left out.
// It stops at the first error of fn or of the context.
func WalkStorage(ctx context.Context, root string, o WalkStorageOptions, fn func(StorageObject) error) error {
	if fi, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("backup: %s is not a directory", root)
	}
	skipped := o.Skipped
	if skipped == nil {
		skipped = func(string) {}
	}
	return walkObjects(ctx, root, skipped, func(abs, rel string) error {
		fi, err := os.Lstat(abs)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() || (!o.Since.IsZero() && !fi.ModTime().After(o.Since)) {
			return nil
		}
		attrs, err := readStorageAttrs(abs)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: read extended attributes: %w", rel, err)
		}
		return fn(StorageObject{Path: rel, Abs: abs, Size: fi.Size(), ModTime: fi.ModTime(), Attrs: attrs})
	})
}

// walkObjects calls fn for every regular file below root in path order with its absolute path and
// its slash-separated path below root. Entries that are not regular files go to skipped and are
// left out; entries removed during the walk are ignored.
func walkObjects(ctx context.Context, root string, skipped func(abs string), fn func(abs, rel string) error) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while walking
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			// Storage writes regular files only. A link or device here would make the
			// backup read something outside the tree, so it is left out.
			skipped(p)
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		return fn(p, filepath.ToSlash(rel))
	})
}
