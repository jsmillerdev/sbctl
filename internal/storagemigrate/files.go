package storagemigrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Object is a regular file below a project's objects directory.
type Object struct {
	// Path is the slash-separated path below the project's directory, <bucket>/<name>/<version>.
	// Storage's S3 backend uses <ref>/<Path> as the key (design spike S5).
	Path    string
	Abs     string
	Size    int64
	ModTime time.Time
}

// Files reads and writes Storage's objects directory.
type Files interface {
	// Walk calls fn for every regular file below root. Anything else goes to skip, because a link or
	// a device would take the copy out of the tree. A root that does not exist holds no files; a file
	// that vanishes during the walk is left out.
	Walk(ctx context.Context, root string, skip func(abs string), fn func(Object) error) error
	// Open opens the file at rel (slash-separated) below root for reading. Nothing may take the open
	// out of root: a parent directory that is replaced by a link, between the walk and the open,
	// to a place the daemon's user can read must not make the copy send that file as an object. A
	// link that stays inside root is followed, and the leaf must be a regular file.
	Open(root, rel string) (*os.File, error)
	// Meta reads the content type and cache control Storage keeps in the file's extended attributes.
	Meta(abs string) (FileMeta, error)
	// MetaOf reads them from an open file, the one whose bytes are sent.
	MetaOf(f *os.File) (FileMeta, error)
	// SetMeta writes them. It reports false, not an error, when the file system has no extended
	// attributes.
	SetMeta(abs string, m FileMeta) (bool, error)
}

// OSFiles is Files over the operating system. It reads a file's attributes only when asked, so a
// pass that copies nothing costs one lstat per file.
type OSFiles struct{}

// Walk implements Files.
func (OSFiles) Walk(ctx context.Context, root string, skip func(abs string), fn func(Object) error) error {
	if fi, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
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
			skip(p)
			return nil
		}
		fi, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		return fn(Object{Path: filepath.ToSlash(rel), Abs: p, Size: fi.Size(), ModTime: fi.ModTime()})
	})
}

// Meta implements Files.
func (OSFiles) Meta(abs string) (FileMeta, error) {
	ct, _, err := readAttr(abs, "content-type")
	if err != nil {
		return FileMeta{}, err
	}
	cc, _, err := readAttr(abs, "cache-control")
	if err != nil {
		return FileMeta{}, err
	}
	return FileMeta{ContentType: ct, CacheControl: cc}, nil
}

// MetaOf implements Files.
func (OSFiles) MetaOf(f *os.File) (FileMeta, error) {
	fd := int(f.Fd())
	ct, _, err := readAttrFd(fd, "content-type")
	if err != nil {
		return FileMeta{}, err
	}
	cc, _, err := readAttrFd(fd, "cache-control")
	if err != nil {
		return FileMeta{}, err
	}
	return FileMeta{ContentType: ct, CacheControl: cc}, nil
}

// Open implements Files, through os.Root.
func (OSFiles) Open(root, rel string) (*os.File, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close() // a file opened through the Root stays open
	name := filepath.FromSlash(rel)
	fi, err := r.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file: %w", quote(filepath.Join(root, name)), fs.ErrNotExist)
	}
	return r.Open(name)
}

// SetMeta implements Files.
func (OSFiles) SetMeta(abs string, m FileMeta) (bool, error) {
	for name, v := range map[string]string{"content-type": m.ContentType, "cache-control": m.CacheControl} {
		if v == "" {
			continue
		}
		if ok, err := writeAttr(abs, name, v); err != nil || !ok {
			return ok, err
		}
	}
	return true, nil
}

func lstat(path string) (os.FileInfo, error) { return os.Lstat(path) }
