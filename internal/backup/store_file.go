package backup

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
	"strings"
	"time"
)

// tmpMarker is part of the name of in-flight files in the file store. List skips them.
const tmpMarker = ".tmp-"

// FileStore keeps objects as files under a root directory. Put writes a temporary
// file in the destination directory, fsyncs it, renames it into place and fsyncs the
// directory, so a crash leaves either the old object or the whole new one.
type FileStore struct{ root string }

// NewFileStore returns a FileStore rooted at dir, creating it if needed.
func NewFileStore(dir string) (*FileStore, error) {
	dir = filepath.Clean(dir)
	if err := mkdirAllSync(dir, 0o700); err != nil {
		return nil, fmt.Errorf("backup: file store %s: %w", dir, err)
	}
	return &FileStore{root: dir}, nil
}

func (s *FileStore) path(key string) string { return filepath.Join(s.root, filepath.FromSlash(key)) }

// URL implements Store.
func (s *FileStore) URL(key string) string { return "file://" + filepath.ToSlash(s.path(key)) }

// Put implements Store.
func (s *FileStore) Put(ctx context.Context, key string, r io.Reader) (err error) {
	if err := validKey(key); err != nil {
		return err
	}
	dst := s.path(key)
	dir := filepath.Dir(dst)
	if err := mkdirAllSync(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(dst)+tmpMarker+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err = io.Copy(f, ctxReader{ctx, r}); err != nil {
		return err
	}
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, dst); err != nil {
		return err
	}
	return syncDir(dir)
}

// Get implements Store.
func (s *FileStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	f, err := os.Open(s.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// Stat implements Store.
func (s *FileStore) Stat(_ context.Context, key string) (ObjectInfo, error) {
	if err := validKey(key); err != nil {
		return ObjectInfo{}, err
	}
	fi, err := os.Stat(s.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return ObjectInfo{}, ErrNotFound
	}
	if err != nil {
		return ObjectInfo{}, err
	}
	if fi.IsDir() {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// List implements Store.
func (s *FileStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	dir := prefix
	if !strings.HasSuffix(prefix, "/") {
		if dir = path.Dir(prefix); dir == "." {
			dir = ""
		}
	}
	var out []ObjectInfo
	err := filepath.WalkDir(filepath.Join(s.root, filepath.FromSlash(dir)), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() || strings.Contains(d.Name(), tmpMarker) {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		out = append(out, ObjectInfo{Key: key, Size: fi.Size(), ModTime: fi.ModTime()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, err
}

// ListDirs implements Store.
func (s *FileStore) ListDirs(_ context.Context, prefix string) ([]string, error) {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		return nil, fmt.Errorf("backup: ListDirs prefix %q must end in /", prefix)
	}
	ents, err := os.ReadDir(filepath.Join(s.root, filepath.FromSlash(prefix)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Delete implements Store. Directories left empty are removed up to the root.
func (s *FileStore) Delete(_ context.Context, keys ...string) error {
	for _, k := range keys {
		if err := validKey(k); err != nil {
			return err
		}
		p := s.path(k)
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		for d := filepath.Dir(p); d != s.root && strings.HasPrefix(d, s.root); d = filepath.Dir(d) {
			if os.Remove(d) != nil { // not empty (or gone): stop
				break
			}
		}
	}
	return nil
}

var _ TempCleaner = (*FileStore)(nil)

// DeleteStaleTemps implements TempCleaner: it removes *.tmp-* files that a crashed Put
// left behind and that have not been written to since cutoff.
func (s *FileStore) DeleteStaleTemps(ctx context.Context, prefix string, cutoff time.Time) (int, error) {
	n := 0
	err := filepath.WalkDir(filepath.Join(s.root, filepath.FromSlash(prefix)), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() || !strings.Contains(d.Name(), tmpMarker) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !fi.ModTime().Before(cutoff) {
			return nil // a Put in flight
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		n++
		return nil
	})
	return n, err
}

// mkdirAllSync is os.MkdirAll that also fsyncs the parent of every directory it creates.
func mkdirAllSync(dir string, perm os.FileMode) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	for _, d := range missing {
		if err := syncDir(filepath.Dir(d)); err != nil {
			return err
		}
	}
	return nil
}

// syncDir fsyncs a directory so renames and creations inside it survive a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", dir, err)
	}
	return nil
}
