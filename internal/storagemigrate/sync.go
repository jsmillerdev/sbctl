package storagemigrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/storagemigrate/hold"
)

// passResult is what one pass over one project did.
type passResult struct {
	Files    int // files the project has
	Uploaded Counts
	Deleted  int
	Skipped  []Skipped
}

func (r *passResult) add(o passResult) {
	r.Files += o.Files
	r.Uploaded.Files += o.Uploaded.Files
	r.Uploaded.Bytes += o.Uploaded.Bytes
	r.Deleted += o.Deleted
	r.Skipped = append(r.Skipped, o.Skipped...)
}

// walkTenant lists the files of one project. A file whose name cannot be an S3 key is left out and
// reported.
func (e *Engine) walkTenant(ctx context.Context, ref string) (inventory, []Skipped, error) {
	root := e.paths.StorageObjects(ref)
	var inv inventory
	var skipped []Skipped
	err := e.files.Walk(ctx, root, func(abs string) {
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			rel = abs
		}
		skipped = append(skipped, Skipped{Path: quote(ref + "/" + filepath.ToSlash(rel)), Reason: "not a regular file"})
	}, func(o Object) error {
		if why := validKey(ref + "/" + o.Path); why != "" {
			skipped = append(skipped, Skipped{Path: quote(ref + "/" + o.Path), Reason: why})
			return nil
		}
		inv = append(inv, fileState{Path: o.Path, Size: o.Size, MTime: o.ModTime.UnixNano()})
		return nil
	})
	sort.Slice(inv, func(i, j int) bool { return inv[i].Path < inv[j].Path })
	return inv, skipped, err
}

// listTenant lists the bucket's copy of one project, in key order whatever order the service used.
func (e *Engine) listTenant(ctx context.Context, b Bucket, ref string) ([]Entry, error) {
	var out []Entry
	if err := b.List(ctx, ref+"/", func(en Entry) error { out = append(out, en); return nil }); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// syncTenant brings the bucket's copy of one project up to date with its files and returns the
// inventory it saw. prev is the inventory of the pass before; nil means there is none (the first
// pass, or a run that was continued), and the bucket's listing stands in for it.
func (e *Engine) syncTenant(ctx context.Context, b Bucket, ref string, prev inventory) (inventory, passResult, error) {
	cur, skipped, err := e.walkTenant(ctx, ref)
	if err != nil {
		return nil, passResult{}, fmt.Errorf("%s: %w", ref, err)
	}
	res := passResult{Files: len(cur), Skipped: skipped}
	var changed []fileState
	var gone []string
	if prev == nil {
		listed, err := e.listTenant(ctx, b, ref)
		if err != nil {
			return nil, res, fmt.Errorf("%s: %w", ref, err)
		}
		changed = diffBucket(cur, listed, ref+"/")
	} else {
		changed, gone = diff(prev, cur)
	}
	if err := e.upload(ctx, b, ref, changed, &res); err != nil {
		return nil, res, fmt.Errorf("%s: %w", ref, err)
	}
	if len(gone) > 0 {
		keys := make([]string, len(gone))
		for i, p := range gone {
			keys[i] = ref + "/" + p
		}
		if err := b.Delete(ctx, keys...); err != nil {
			return nil, res, fmt.Errorf("%s: %w", ref, err)
		}
		res.Deleted = len(gone)
	}
	// An empty, non-nil inventory means "known to be empty", not "unknown".
	if cur == nil {
		cur = inventory{}
	}
	return cur, res, nil
}

// upload copies the files with the configured number of workers.
func (e *Engine) upload(ctx context.Context, b Bucket, ref string, files []fileState, res *passResult) error {
	if len(files) == 0 {
		return nil
	}
	var n, bytes atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(e.workers)
	var mu sync.Mutex
	var firstErr error
	for _, f := range files {
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error {
			size, err := e.copyFile(gctx, b, ref, f)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				return nil // deleted since the walk; the next pass sees that
			case errors.Is(err, errChanged) && !e.fence:
				return nil // being written; its modification time moves, so the next pass sends it again
			case err != nil:
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return err
			}
			n.Add(1)
			bytes.Add(size)
			return nil
		})
	}
	err := g.Wait()
	res.Uploaded.Files += int(n.Load())
	res.Uploaded.Bytes += bytes.Load()
	if firstErr != nil {
		return firstErr
	}
	return err
}

// copyFile sends one file. The size it sends is the file's size now, which can differ from the
// walk's if the file changed since; the inventory keeps the walk's values, so the next pass sees
// the difference and sends the file again.
func (e *Engine) copyFile(ctx context.Context, b Bucket, ref string, f fileState) (int64, error) {
	root := e.paths.StorageObjects(ref)
	abs := filepath.Join(root, filepath.FromSlash(f.Path))
	fh, err := e.files.Open(root, f.Path)
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%s: %w", quote(abs), fs.ErrNotExist)
	}
	meta, err := e.files.MetaOf(fh)
	if err != nil {
		return 0, fmt.Errorf("%s: read extended attributes: %w", quote(abs), err)
	}
	if err := b.Put(ctx, ref+"/"+f.Path, fh, fi.Size(), meta); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("%s: %w", quote(abs), errChanged) // shorter than it was when the copy began
		}
		return 0, err
	}
	return fi.Size(), nil
}

// errChanged marks a file that changed while it was read.
var errChanged = errors.New("the file changed while it was copied")

// holdWrites asks the edge proxy to answer Storage's writes with 503 and keeps asking until the
// returned function is called. A run that dies leaves a marker that expires on its own.
func (e *Engine) holdWrites(ctx context.Context) (release func()) {
	write := func() {
		now := e.now()
		m := hold.Marker{PID: os.Getpid(), Since: now.UTC(), Until: now.Add(e.holdFor).UTC(), Reason: "Storage migration"}
		if err := hold.Write(e.paths, m); err != nil {
			e.log.Warn("could not write the Storage write hold; writes are fenced by stopping Storage instead", "err", err)
		}
	}
	write()
	hctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(e.holdFor / 4)
		defer t.Stop()
		for {
			select {
			case <-hctx.Done():
				return
			case <-t.C:
				write()
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
		_ = hold.Remove(e.paths)
	}
}
