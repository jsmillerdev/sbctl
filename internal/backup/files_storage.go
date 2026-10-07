package backup

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// StorageDir returns the directory holding ref's Storage objects, or "" when the node does
// not keep them on its own disk (Storage's S3 backend: the objects live in the operator's
// bucket, which this package does not touch).
func (s *Service) storageDir(ref string) string {
	if s.opt.StorageDir != nil {
		return s.opt.StorageDir(ref)
	}
	if s.opt.Config == nil || s.opt.Config.Fleet.StorageBackend == "s3" {
		return ""
	}
	return s.opt.Config.Paths().StorageObjects(ref)
}

// backupStorage takes a snapshot of ref's Storage objects. It reports nil, nil when there
// is nothing to do: no objects directory on this node and no earlier snapshot to supersede.
func (s *Service) backupStorage(ctx context.Context, ref, reason string) (*FilesSnapshot, error) {
	root := s.storageDir(ref)
	if root == "" {
		return nil, nil
	}
	fi, err := os.Lstat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A project that never stored an object has no directory. One that had objects and
		// lost them all still needs a snapshot that says so.
		prev, perr := s.ListFilesSnapshots(ctx, ref, KindStorage)
		if perr != nil || len(prev) == 0 {
			return nil, perr
		}
	case err != nil:
		return nil, err
	case !fi.IsDir():
		return nil, fmt.Errorf("backup: %s is not a directory", root)
	}

	prev, err := s.latestFilesSnapshot(ctx, ref, KindStorage)
	if err != nil {
		return nil, err
	}
	prevEntries := map[string]fileEntry{}
	if prev != nil {
		if err := s.readEntries(ctx, prev.Dir()+"/"+entriesName, func(e fileEntry) error {
			prevEntries[e.Path] = e
			return nil
		}); err != nil {
			s.opt.Log.Warn("previous object snapshot unreadable; copying everything", "ref", ref, "snapshot", prev.ID, "err", err)
			prev, prevEntries = nil, map[string]fileEntry{}
		}
	}

	started := s.opt.Now().UTC()
	id := newFilesID(started)
	unmark, err := s.markRunning(ctx, ref, KindStorage, id)
	if err != nil {
		return nil, err
	}
	defer unmark()

	bw, err := s.newBlobWriter(ref, KindStorage)
	if err != nil {
		return nil, err
	}
	defer bw.close()
	run := &storageRun{s: s, bw: bw, root: root, prev: prevEntries}
	if prev != nil {
		run.racy = prev.StartTime.Add(-racyWindow)
	}
	snap := &FilesSnapshot{Version: filesVersion, Kind: KindStorage, ID: id, Ref: ref, Reason: reason,
		StartTime: started, SupaviseVersion: s.opt.Version}
	_, err = s.writeEntries(ctx, snap.Dir()+"/"+entriesName, func(emit func(fileEntry) error) error {
		run.emit = emit
		return run.walk(ctx)
	})
	if err != nil {
		s.dropSnapshot(ctx, snap)
		return nil, fmt.Errorf("backup: objects of %s: %w", ref, err)
	}
	snap.Files, snap.Bytes = run.files, run.bytes
	snap.NewFiles, snap.NewBytes = int(bw.newFiles.Load()), bw.newBytes.Load()
	snap.StopTime = s.opt.Now().UTC()
	if err := s.writeSnapshot(ctx, snap); err != nil {
		s.dropSnapshot(ctx, snap)
		return nil, err
	}
	return snap, nil
}

// latestFilesSnapshot is the newest snapshot of kind, whatever its reason.
func (s *Service) latestFilesSnapshot(ctx context.Context, ref, kind string) (*FilesSnapshot, error) {
	all, err := s.ListFilesSnapshots(ctx, ref, kind)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return &all[len(all)-1], nil
}

// dropSnapshot removes what a failed run uploaded for snap (not its blobs: another snapshot
// may share them, and prune removes the ones nothing references).
func (s *Service) dropSnapshot(ctx context.Context, m *FilesSnapshot) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	objs, err := s.opt.Store.List(dctx, m.Dir()+"/")
	if err != nil {
		return
	}
	keys := make([]string, len(objs))
	for i, o := range objs {
		keys[i] = o.Key
	}
	_ = s.opt.Store.Delete(dctx, keys...)
}

// storageRun is one walk over a project's objects directory.
type storageRun struct {
	s    *Service
	bw   *blobWriter
	root string
	prev map[string]fileEntry
	racy time.Time // mtimes at or after this are not trusted to mean "unchanged"
	emit func(fileEntry) error

	mu    sync.Mutex
	files int
	bytes int64
}

type walked struct{ abs, rel string }

func (r *storageRun) walk(ctx context.Context) error {
	if fi, err := os.Lstat(r.root); err == nil && !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", r.root)
	} else if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // every earlier object is gone: an empty snapshot
		}
		return err
	}
	g, gctx := errgroup.WithContext(ctx)
	tasks := make(chan walked)
	for i := 0; i < filesWorkers; i++ {
		g.Go(func() error {
			for t := range tasks {
				if err := r.file(gctx, t); err != nil {
					return fmt.Errorf("%s: %w", t.rel, err)
				}
			}
			return nil
		})
	}
	g.Go(func() error {
		defer close(tasks)
		return filepath.WalkDir(r.root, func(p string, d fs.DirEntry, err error) error {
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
				r.s.opt.Log.Warn("object backup skips a file that is not a regular file", "path", p)
				return nil
			}
			rel, err := filepath.Rel(r.root, p)
			if err != nil {
				return err
			}
			select {
			case tasks <- walked{abs: p, rel: filepath.ToSlash(rel)}:
				return nil
			case <-gctx.Done():
				return gctx.Err()
			}
		})
	})
	return g.Wait()
}

// file records one object: it reuses the previous snapshot's hash when size, mtime and
// attributes are unchanged, and otherwise hashes the content and uploads it unless the
// backend already holds it.
func (r *storageRun) file(ctx context.Context, t walked) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		e, err := r.copyFile(ctx, t)
		if errors.Is(err, errChanged) {
			lastErr = err
			continue
		}
		if err != nil {
			return err
		}
		if e == nil {
			return nil // deleted since the walk saw it
		}
		r.mu.Lock()
		r.files++
		r.bytes += e.Size
		r.mu.Unlock()
		return r.emit(*e)
	}
	return lastErr
}

func (r *storageRun) copyFile(ctx context.Context, t walked) (*fileEntry, error) {
	f, err := openNoFollow(t.abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, nil
	}
	attrs, err := readStorageAttrs(t.abs)
	if err != nil {
		return nil, fmt.Errorf("read extended attributes: %w", err)
	}
	enc := encodeAttrs(attrs)
	e := fileEntry{Path: t.rel, Size: st.Size(), MTime: st.ModTime().UnixNano(), Mode: uint32(st.Mode().Perm()), Attrs: enc}

	if p, ok := r.prev[t.rel]; ok && validHash(p.Hash) && p.Size == e.Size && p.MTime == e.MTime && attrsEqual(p.Attrs, enc) &&
		!r.racy.IsZero() && st.ModTime().Before(r.racy) {
		e.Hash = p.Hash
		return &e, nil
	}

	if st.Size() <= smallFile {
		buf, err := io.ReadAll(io.LimitReader(f, smallFile+1))
		if err != nil {
			return nil, err
		}
		if len(buf) <= smallFile {
			sum := sha256.Sum256(buf)
			e.Hash, e.Size = hex.EncodeToString(sum[:]), int64(len(buf))
			return &e, r.bw.putBytes(ctx, e.Hash, buf)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
	}
	h := sha256.New()
	n, err := io.Copy(h, ctxReader{ctx, f})
	if err != nil {
		return nil, err
	}
	e.Hash, e.Size = hex.EncodeToString(h.Sum(nil)), n
	if ok, err := r.bw.has(ctx, e.Hash); err != nil || ok {
		return &e, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	// The file is read again from the start; a hash that differs this time means someone
	// wrote to it, and the upload fails without storing anything.
	if err := r.bw.putStream(ctx, e.Hash, ctxReader{ctx, f}); err != nil {
		return nil, err
	}
	return &e, nil
}

func encodeAttrs(a map[string][]byte) map[string]string {
	if len(a) == 0 {
		return nil
	}
	out := make(map[string]string, len(a))
	for k, v := range a {
		out[k] = base64.StdEncoding.EncodeToString(v)
	}
	return out
}

func attrsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// ---- restore ----

// cleanRel validates an entry path from a snapshot, which a tampered backend could fill
// with anything: relative, no "..", no empty or "." segments.
func cleanRel(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) || strings.Contains(p, "\\") {
		return "", fmt.Errorf("backup: unsafe path %q in a snapshot", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("backup: unsafe path %q in a snapshot", p)
		}
	}
	return path.Clean(p), nil
}

// restoreStorage rebuilds dest from snap. The files are written to a staging directory next
// to dest first and the swap is two renames, so a failure leaves dest as it was. With
// replace, an existing dest is kept as <dest>.pre-restore-<stamp> (the same rule as an
// in-place restore of the database); without it, dest must not hold anything.
func (s *Service) restoreStorage(ctx context.Context, snap *FilesSnapshot, dest string, replace bool, stamp string) (aside string, err error) {
	if dest == "" {
		return "", errors.New("backup: this node keeps no Storage objects on disk")
	}
	if !replace {
		if ents, err := os.ReadDir(dest); err == nil && len(ents) > 0 {
			return "", fmt.Errorf("backup: %s already holds objects; restore into a project that has none", dest)
		}
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return "", err
	}
	staging := filepath.Join(parent, ".restore-"+filepath.Base(dest)+"-"+snap.ID)
	_ = os.RemoveAll(staging)
	if err := os.Mkdir(staging, 0o750); err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(staging)
		}
	}()

	warnedAttrs := false
	err = s.readEntries(ctx, snap.Dir()+"/"+entriesName, func(e fileEntry) error {
		rel, err := cleanRel(e.Path)
		if err != nil {
			return err
		}
		if !validHash(e.Hash) {
			return fmt.Errorf("backup: snapshot %s has a bad hash for %s", snap.ID, e.Path)
		}
		p := filepath.Join(staging, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := s.restoreOne(ctx, snap, e, p); err != nil {
			return fmt.Errorf("restore %s: %w", e.Path, err)
		}
		attrs, err := decodeAttrs(e.Attrs)
		if err != nil {
			return fmt.Errorf("restore %s: %w", e.Path, err)
		}
		if len(attrs) > 0 {
			ok, err := writeStorageAttrs(p, attrs)
			if err != nil {
				return fmt.Errorf("restore %s: set extended attributes: %w", e.Path, err)
			}
			if !ok && !warnedAttrs {
				warnedAttrs = true
				s.opt.Log.Warn("this file system has no extended attributes: restored objects lose their content type and cache headers", "dest", dest)
			}
		}
		if e.MTime != 0 {
			mt := time.Unix(0, e.MTime)
			if err := os.Chtimes(p, mt, mt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	if !replace {
		_ = os.Remove(dest) // an empty directory left by an earlier attempt
	}
	if _, serr := os.Lstat(dest); serr == nil {
		aside = dest + ".pre-restore-" + stamp
		if err = os.Rename(dest, aside); err != nil {
			return "", err
		}
	} else if !errors.Is(serr, fs.ErrNotExist) {
		return "", serr
	}
	if err = os.Rename(staging, dest); err != nil {
		if aside != "" {
			_ = os.Rename(aside, dest)
		}
		return "", err
	}
	return aside, nil
}

func decodeAttrs(a map[string]string) (map[string][]byte, error) {
	if len(a) == 0 {
		return nil, nil
	}
	out := make(map[string][]byte, len(a))
	for k, v := range a {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("attribute %s: %w", k, err)
		}
		out[k] = b
	}
	return out, nil
}

// restoreOne writes the content of e's blob to p, checking it against the recorded hash.
func (s *Service) restoreOne(ctx context.Context, snap *FilesSnapshot, e fileEntry, p string) error {
	rc, err := s.openBlob(ctx, snap.Ref, snap.Kind, e.Hash)
	if err != nil {
		return err
	}
	defer rc.Close()
	mode := fs.FileMode(e.Mode).Perm() | 0o600
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), ctxReader{ctx, rc})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != e.Hash || n != e.Size {
		return fmt.Errorf("the stored copy does not match its checksum (%d bytes read, %d recorded): the backup is damaged", n, e.Size)
	}
	return os.Chmod(p, mode) // the umask does not apply to the file we were asked to reproduce
}
