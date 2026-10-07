package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Storage objects and Edge Function deployments are the files a project owns outside its
// database. They are backed up next to the WAL and base backups, per project:
//
//	<ref>/<kind>/blobs/<hh>/<sha256>.zst         one object per distinct file content, zstd-compressed
//	<ref>/<kind>/snapshots/<id>/entries.ndjson.zst   every file of the project at one time: path, size,
//	                                              mtime, mode, extended attributes and the blob's hash
//	<ref>/<kind>/snapshots/<id>/snapshot.json    summary; written last, so its presence means "complete"
//	<ref>/<kind>/running/<id>                    marker while a backup runs, so prune keeps its blobs
//
// <kind> is "storage" or "functions". A snapshot lists the whole tree, so restoring one
// reproduces the source exactly, deletions included; the blobs are shared between
// snapshots, so a nightly run uploads only the files that are new or changed.

// The two kinds of file trees.
const (
	KindStorage   = "storage"
	KindFunctions = "functions"
)

// ReasonPreRestore marks the snapshot an in-place restore takes of what it is about to
// replace. Point-in-time restores never pick it on their own: it is a state from before a
// restore, not from the timeline the restore continued.
const ReasonPreRestore = "pre-restore"

const (
	filesVersion = 1
	summaryName  = "snapshot.json"
	entriesName  = "entries.ndjson.zst"
	// smallFile is the largest file read into memory at once; bigger ones are hashed in one
	// pass and streamed to the store in a second.
	smallFile = 1 << 20
	// filesWorkers is how many files one run copies at once. With the S3 part size it bounds
	// the memory of a run: a few parts and a few small files, whatever the objects weigh.
	filesWorkers = 4
	// maxEntryLine caps one line of an entries file (a path and a few attributes).
	maxEntryLine = 1 << 20
	// racyWindow is how close to the previous snapshot's start a file's mtime may be before
	// "same size and mtime" stops proving "same content": a write in the same clock tick as
	// the previous run's read could have gone unnoticed.
	racyWindow = 2 * time.Second
)

func filesDir(ref, kind string) string { return ref + "/" + kind + "/" }
func snapshotsDir(ref, kind string) string {
	return filesDir(ref, kind) + "snapshots/"
}
func blobsDir(ref, kind string) string { return filesDir(ref, kind) + "blobs/" }
func runningDir(ref, kind string) string {
	return filesDir(ref, kind) + "running/"
}
func blobKey(ref, kind, hash string) string {
	return blobsDir(ref, kind) + hash[:2] + "/" + hash + ".zst"
}

func validKind(kind string) error {
	if kind != KindStorage && kind != KindFunctions {
		return fmt.Errorf("backup: unknown file kind %q", kind)
	}
	return nil
}

// FilesSnapshot is the summary of one complete snapshot of a project's Storage objects or
// Edge Function deployments. It lives next to the entries and is the source of truth for
// restore and prune.
type FilesSnapshot struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Ref     string `json:"ref"`
	Reason  string `json:"reason"`

	StartTime time.Time `json:"start_time"`
	StopTime  time.Time `json:"stop_time"`

	// Files is the number of files in the snapshot and Bytes their total size. NewFiles
	// and NewBytes are what this run uploaded (stored, compressed): everything else was
	// already in the backend.
	Files    int   `json:"files"`
	Bytes    int64 `json:"bytes"`
	NewFiles int   `json:"new_files"`
	NewBytes int64 `json:"new_bytes"`

	// Functions holds the deployment records of a functions snapshot; the entries hold
	// their files and secrets.
	Functions []FunctionRecord `json:"functions,omitempty"`

	SupaviseVersion string `json:"supavise_version,omitempty"`
}

// Dir is the snapshot's key prefix, without trailing slash.
func (m *FilesSnapshot) Dir() string { return snapshotsDir(m.Ref, m.Kind) + m.ID }

// fileEntry is one line of an entries file.
type fileEntry struct {
	Path  string `json:"p"`
	Size  int64  `json:"s"`
	MTime int64  `json:"t,omitempty"` // unix nanoseconds
	Mode  uint32 `json:"m,omitempty"`
	Hash  string `json:"h"` // SHA-256 of the content, hex; names the blob
	// Attrs are the extended attributes Storage keeps its headers in, base64.
	Attrs map[string]string `json:"x,omitempty"`
}

func newFilesID(t time.Time) string { return newBackupID(t) }

// ListFilesSnapshots returns ref's complete snapshots of kind, oldest first. Directories
// without a summary (a run in flight or abandoned) are not listed.
func (s *Service) ListFilesSnapshots(ctx context.Context, ref, kind string) ([]FilesSnapshot, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	if err := validKind(kind); err != nil {
		return nil, err
	}
	ids, err := s.opt.Store.ListDirs(ctx, snapshotsDir(ref, kind))
	if err != nil {
		return nil, err
	}
	var out []FilesSnapshot
	for _, id := range ids {
		m, err := s.readSnapshot(ctx, ref, kind, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("backup: %s snapshot %s/%s: %w", kind, ref, id, err)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StopTime.Before(out[j].StopTime) })
	return out, nil
}

func (s *Service) readSnapshot(ctx context.Context, ref, kind, id string) (*FilesSnapshot, error) {
	rc, err := s.opt.Store.Get(ctx, snapshotsDir(ref, kind)+id+"/"+summaryName)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 16<<20))
	if err != nil {
		return nil, err
	}
	var m FilesSnapshot
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Version != filesVersion {
		return nil, fmt.Errorf("unsupported snapshot version %d", m.Version)
	}
	if m.Ref != ref || m.ID != id || m.Kind != kind {
		return nil, fmt.Errorf("snapshot says %s/%s/%s but is stored at %s/%s/%s", m.Kind, m.Ref, m.ID, kind, ref, id)
	}
	return &m, nil
}

func (s *Service) writeSnapshot(ctx context.Context, m *FilesSnapshot) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return s.opt.Store.Put(ctx, m.Dir()+"/"+summaryName, bytes.NewReader(append(b, '\n')))
}

// pickFilesSnapshot returns the snapshot to restore: the one named by id, or the newest
// that finished at or before at (everything when latest). Pre-restore snapshots are only
// chosen by id.
func pickFilesSnapshot(all []FilesSnapshot, at time.Time, latest bool, id string) (*FilesSnapshot, error) {
	if id != "" {
		for i := range all {
			if all[i].ID == id {
				return &all[i], nil
			}
		}
		return nil, fmt.Errorf("backup: no snapshot %q", id)
	}
	var best *FilesSnapshot
	for i := range all {
		m := &all[i]
		if m.Reason == ReasonPreRestore || (!latest && m.StopTime.After(at)) {
			continue
		}
		if best == nil || m.StopTime.After(best.StopTime) {
			best = m
		}
	}
	if best == nil {
		return nil, errNoSnapshot
	}
	return best, nil
}

var errNoSnapshot = errors.New("backup: no snapshot at or before the target")

// ---- entries files ----

// writeEntries streams the entries produce emits into one zstd-compressed NDJSON object
// under key and returns the stored size. emit is safe for concurrent use. Nothing is
// stored if produce fails.
func (s *Service) writeEntries(ctx context.Context, key string, produce func(emit func(fileEntry) error) error) (int64, error) {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := s.opt.Store.Put(ctx, key, pr)
		pr.CloseWithError(err)
		done <- err
	}()
	cw := &countingWriter{w: pw}
	err := func() error {
		enc, err := newEncoder(cw, 1)
		if err != nil {
			return err
		}
		var mu sync.Mutex
		je := json.NewEncoder(enc)
		emit := func(e fileEntry) error {
			mu.Lock()
			defer mu.Unlock()
			return je.Encode(e)
		}
		if err := produce(emit); err != nil {
			enc.Close()
			return err
		}
		return enc.Close()
	}()
	pw.CloseWithError(err)
	if perr := <-done; err == nil {
		err = perr
	}
	return cw.n, err
}

// readEntries calls fn for every entry of the entries object at key, in stored order.
func (s *Service) readEntries(ctx context.Context, key string, fn func(fileEntry) error) error {
	rc, err := s.opt.Store.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	dec, err := newDecoder(rc)
	if err != nil {
		return err
	}
	defer dec.Close()
	sc := bufio.NewScanner(dec)
	sc.Buffer(make([]byte, 64<<10), maxEntryLine)
	for sc.Scan() {
		var e fileEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return fmt.Errorf("backup: entries %s: %w", key, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return sc.Err()
}

// ---- blobs ----

// blobWriter stores file contents as content-addressed blobs of one run.
type blobWriter struct {
	s         *Service
	ref, kind string
	enc       *zstd.Encoder
	seen      sync.Map // hash -> *blobState: one decision per distinct content per run
	newFiles  atomic.Int64
	newBytes  atomic.Int64
}

// blobState makes concurrent files with the same content share one stat and one upload.
type blobState struct {
	once sync.Once
	err  error
}

func (s *Service) newBlobWriter(ref, kind string) (*blobWriter, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1), zstd.WithLowerEncoderMem(true))
	if err != nil {
		return nil, err
	}
	return &blobWriter{s: s, ref: ref, kind: kind, enc: enc}, nil
}

func (w *blobWriter) close() { w.enc.Close() }

// has reports whether the backend holds the blob for hash, without uploading anything.
func (w *blobWriter) has(ctx context.Context, hash string) (bool, error) {
	if v, ok := w.seen.Load(hash); ok {
		if st := v.(*blobState); st.err == nil {
			return true, nil // being stored by another file of this run, or already there
		}
	}
	_, err := w.s.opt.Store.Stat(ctx, blobKey(w.ref, w.kind, hash))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// ensure makes sure the backend holds the blob for hash, calling upload (which returns the
// stored size) only if it does not. Files of one run with the same content share the outcome.
func (w *blobWriter) ensure(ctx context.Context, hash string, upload func() (int64, error)) error {
	v, _ := w.seen.LoadOrStore(hash, &blobState{})
	st := v.(*blobState)
	st.once.Do(func() {
		_, err := w.s.opt.Store.Stat(ctx, blobKey(w.ref, w.kind, hash))
		switch {
		case err == nil:
		case errors.Is(err, ErrNotFound):
			var n int64
			if n, err = upload(); err == nil {
				w.newFiles.Add(1)
				w.newBytes.Add(n)
			}
			st.err = err
		default:
			st.err = err
		}
	})
	if st.err != nil {
		w.seen.CompareAndDelete(hash, v) // a retry decides again
	}
	return st.err
}

// putBytes stores content (whose SHA-256 is hash) unless the backend has it.
func (w *blobWriter) putBytes(ctx context.Context, hash string, content []byte) error {
	return w.ensure(ctx, hash, func() (int64, error) {
		z := w.enc.EncodeAll(content, make([]byte, 0, len(content)/2+64))
		return int64(len(z)), w.s.opt.Store.Put(ctx, blobKey(w.ref, w.kind, hash), bytes.NewReader(z))
	})
}

// errChanged is returned when a file's content differs from what was hashed a moment
// before: it was written while it was being backed up.
var errChanged = errors.New("file changed while it was being backed up")

// putStream stores the content of r, which must hash to hash (computed by an earlier pass
// over the same file), without holding it in memory. A reader that ends with another hash
// fails the upload, so the backend never holds a blob under the wrong name.
func (w *blobWriter) putStream(ctx context.Context, hash string, r io.Reader) error {
	return w.ensure(ctx, hash, func() (int64, error) { return w.streamBlob(ctx, hash, r) })
}

func (w *blobWriter) streamBlob(ctx context.Context, hash string, r io.Reader) (int64, error) {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := w.s.opt.Store.Put(ctx, blobKey(w.ref, w.kind, hash), pr)
		pr.CloseWithError(err)
		done <- err
	}()
	cw := &countingWriter{w: pw}
	err := func() error {
		enc, err := newEncoder(cw, 1)
		if err != nil {
			return err
		}
		if _, err := io.Copy(enc, &hashCheckReader{r: r, h: sha256.New(), want: hash}); err != nil {
			enc.Close()
			return err
		}
		return enc.Close()
	}()
	pw.CloseWithError(err)
	if perr := <-done; err == nil {
		err = perr
	}
	return cw.n, err
}

// hashCheckReader reports errChanged instead of io.EOF when the data read does not hash to want.
type hashCheckReader struct {
	r    io.Reader
	h    hash.Hash
	want string
}

func (c *hashCheckReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.h.Write(p[:n])
	if errors.Is(err, io.EOF) && hex.EncodeToString(c.h.Sum(nil)) != c.want {
		return n, errChanged
	}
	return n, err
}

// openBlob opens the decompressed content of the blob for hash.
func (s *Service) openBlob(ctx context.Context, ref, kind, hash string) (io.ReadCloser, error) {
	rc, err := s.opt.Store.Get(ctx, blobKey(ref, kind, hash))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("backup: the %s blob %s is missing from the backend: %w", kind, hash, err)
		}
		return nil, err
	}
	dec, err := newDecoder(rc)
	if err != nil {
		rc.Close()
		return nil, err
	}
	return &blobReader{Reader: dec, dec: dec, rc: rc}, nil
}

type blobReader struct {
	io.Reader
	dec *zstd.Decoder
	rc  io.Closer
}

func (b *blobReader) Close() error {
	b.dec.Close()
	return b.rc.Close()
}

func validHash(h string) bool {
	if len(h) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil && h == strings.ToLower(h)
}

// markRunning writes the marker that tells prune a run is in flight, and returns the
// function that removes it. Prune never deletes blobs while a recent marker exists: a run
// that finds a blob already in the backend skips uploading it, and the sweep must not
// remove it before the run's snapshot references it.
func (s *Service) markRunning(ctx context.Context, ref, kind, id string) (func(), error) {
	key := runningDir(ref, kind) + id
	if err := s.opt.Store.Put(ctx, key, strings.NewReader(s.opt.Now().UTC().Format(time.RFC3339)+"\n")); err != nil {
		return nil, err
	}
	return func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_ = s.opt.Store.Delete(dctx, key)
	}, nil
}
