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
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/secrets"
)

var (
	flipSteps     = []string{stepHold, stepFinal, stepStopped, stepFence, stepSwitched, stepStarted}
	rollbackSteps = []string{stepHold, stepRestore, stepFinal, stepStopped, stepFence, stepSwitched, stepStarted}
)

// reached reports whether a run whose last finished step is last has finished step too.
func reached(steps []string, last, step string) bool {
	idx := func(s string) int {
		for i, x := range steps {
			if x == s {
				return i
			}
		}
		return -1
	}
	return idx(last) >= idx(step)
}

func (e *Engine) mark(step string) error {
	e.st.Step = step
	return e.save()
}

// flip is the switch (design 2.11, step 5). Storage's writes are held, a pass copies what changed,
// supavise-storage is stopped, and a pass over files that nothing writes to any more copies the
// rest; only then does the configuration name the bucket and Storage start on it. The stop is
// what makes the copy complete: the hold keeps clients from seeing errors, it does not have to be
// airtight. Until the configuration changes, a failure puts Storage back on the files.
func (e *Engine) flip(ctx context.Context, b Bucket) error {
	st := e.st
	release := e.holdWrites(ctx)
	defer release()
	done := func(step string) bool { return reached(flipSteps, st.Step, step) }
	if st.Step == "" {
		if err := e.mark(stepHold); err != nil {
			return err
		}
	}
	stopped := done(stepStopped) && !done(stepStarted)
	switched := done(stepSwitched) && !done(stepStarted)

	if !done(stepFinal) {
		e.say("holding Storage's writes; copying what changed")
		if _, err := e.pass(ctx, b, "final pass"); err != nil {
			return e.abortFlip(ctx, err, stopped, switched)
		}
		if err := e.mark(stepFinal); err != nil {
			return err
		}
	}
	if !done(stepStopped) {
		e.say("stopping supavise-storage")
		stopped = true // a stop that fails may still have stopped it
		if err := e.d.Storage.Stop(ctx); err != nil {
			return e.abortFlip(ctx, err, stopped, switched)
		}
		if err := e.mark(stepStopped); err != nil {
			return err
		}
	}
	if !done(stepFence) {
		e.fence = true
		_, err := e.pass(ctx, b, "fence pass")
		if err == nil {
			err = e.checkAllRows(ctx)
		}
		e.fence = false
		if err != nil {
			return e.abortFlip(ctx, err, stopped, switched)
		}
		if err := e.mark(stepFence); err != nil {
			return err
		}
	}
	if !done(stepSwitched) {
		wrote, err := e.d.Settings.UseBucket(ctx, st.Dest, e.creds)
		st.WroteCredentials = st.WroteCredentials || wrote
		switched = true
		if err != nil {
			return e.abortFlip(ctx, err, stopped, switched)
		}
		if err := e.mark(stepSwitched); err != nil {
			return err
		}
	}
	if !done(stepStarted) {
		e.say("starting supavise-storage on the bucket")
		if err := e.d.Storage.Start(ctx); err != nil {
			return e.abortFlip(ctx, err, stopped, switched)
		}
		if err := e.probe(ctx); err != nil {
			return e.abortFlip(ctx, err, true, switched)
		}
		if err := e.mark(stepStarted); err != nil {
			return err
		}
	}

	kept, err := e.keepFiles()
	if err != nil {
		return fmt.Errorf("Storage runs on the bucket, but the files could not be moved aside: %w", err)
	}
	now := e.now().UTC()
	st.Phase, st.Step, st.FlippedAt, st.Error = PhaseDone, "", now, ""
	if kept != "" {
		st.Retained, st.RetainUntil = kept, now.Add(RetainFor)
	}
	if err := e.save(); err != nil {
		return err
	}
	e.say("Storage now keeps its objects in the bucket %s", st.Dest.Bucket)
	if kept != "" {
		e.say("the files are kept in %s: --rollback goes back to them and --cleanup deletes them (keep them for about 14 days)", filepath.Join(serviceDir(e.paths), kept))
	}
	e.say("restart supavise.service when convenient so that the daemon reads the new setting too: until then, a backup it starts itself still looks for the files")
	return nil
}

// abortFlip puts Storage back on the files after a failure before the switch is complete. The
// run goes back to the catch-up phase, so --resume copies, verifies and tries the switch again.
func (e *Engine) abortFlip(ctx context.Context, cause error, stopped, switched bool) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	if switched {
		if err := e.d.Settings.UseFiles(cctx, e.st.PrevBackend, e.st.WroteCredentials); err != nil {
			cause = fmt.Errorf("%w; and the configuration could not be put back on the files: %v", cause, err)
		} else {
			e.st.WroteCredentials = false
		}
	}
	if stopped || switched {
		if err := e.d.Storage.Start(cctx); err != nil {
			cause = fmt.Errorf("%w; and supavise-storage did not start again: %v (start it: sudo systemctl start supavise-storage.service)", cause, err)
		}
	}
	e.st.Phase, e.st.Step = PhaseCatchingUp, ""
	e.fence = false
	return cause
}

// checkAllRows reads every online project's rows once more against the inventories of the fence
// pass, when nothing writes: a row without its file now is a real loss.
func (e *Engine) checkAllRows(ctx context.Context) error {
	projects, err := e.d.Tenants.Projects(ctx)
	if err != nil {
		return err
	}
	for _, p := range projects {
		if !p.Online || !secrets.ValidRef(p.Ref) {
			continue
		}
		inv := e.inv[p.Ref]
		_, pending, err := e.checkRows(ctx, p.Ref, inv)
		if errors.Is(err, ErrOffline) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: read storage.objects: %w", p.Ref, err)
		}
		if len(pending) > 0 {
			verr := &VerifyError{Ref: p.Ref, Rows: len(pending)}
			for _, r := range pending {
				if f, ok := inv.find(rowPath(r)); ok {
					verr.MismatchedSize = append(verr.MismatchedSize, quote(rowPath(r))+fmt.Sprintf(" (%d bytes, the row says %d)", f.Size, r.Size))
				} else {
					verr.Missing = append(verr.Missing, quote(rowPath(r)))
				}
			}
			return verr
		}
	}
	return nil
}

// probe reads a few objects through Storage, which now runs on the new backend.
func (e *Engine) probe(ctx context.Context) error {
	if e.d.Reader == nil {
		return nil
	}
	projects, err := e.d.Tenants.Projects(ctx)
	if err != nil {
		return nil // the checks that mattered are done; a sample needs the databases
	}
	byRef := map[string]Project{}
	for _, p := range projects {
		byRef[p.Ref] = p
	}
	refs, err := e.refs(byRef)
	if err != nil {
		return err
	}
	for ref, row := range e.samples(ctx, refs, 3) {
		var last error
		for try := 0; try < 5; try++ {
			n, err := e.d.Reader.Read(ctx, ref, row)
			switch {
			case err != nil:
				last = err
			case row.HasSize && n != row.Size:
				last = fmt.Errorf("it came back with %d bytes, the database says %d", n, row.Size)
			default:
				last = nil
			}
			if last == nil {
				break
			}
			if serr := e.sleep(ctx, time.Second); serr != nil {
				return serr
			}
		}
		if last != nil {
			return fmt.Errorf("Storage could not read %s back from the bucket: %w", quote(ref+"/"+rowPath(row)), last)
		}
	}
	return nil
}

// keepFiles renames the objects directory to objects.migrated-<date> and returns the new name; ""
// when there is no objects directory (nothing was ever stored, or an earlier attempt renamed it).
func (e *Engine) keepFiles() (string, error) {
	src := objectsDir(e.paths)
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return e.findKept(), nil
	} else if err != nil {
		return "", err
	}
	name := retainedPrefix + e.now().UTC().Format("20060102")
	for i := 2; ; i++ {
		if _, err := os.Lstat(filepath.Join(serviceDir(e.paths), name)); errors.Is(err, fs.ErrNotExist) {
			break
		}
		name = fmt.Sprintf("%s%s-%d", retainedPrefix, e.now().UTC().Format("20060102"), i)
	}
	return name, os.Rename(src, filepath.Join(serviceDir(e.paths), name))
}

// findKept is the newest directory that an earlier attempt of this run may have kept.
func (e *Engine) findKept() string {
	ents, _ := os.ReadDir(serviceDir(e.paths))
	var kept []string
	for _, en := range ents {
		if en.IsDir() && strings.HasPrefix(en.Name(), retainedPrefix) {
			kept = append(kept, en.Name())
		}
	}
	if len(kept) == 0 {
		return ""
	}
	sort.Strings(kept)
	return kept[len(kept)-1]
}

// Cleanup deletes the files a finished migration kept.
func (e *Engine) Cleanup(ctx context.Context) error {
	release, err := lock(e.paths)
	if err != nil {
		return err
	}
	defer release()
	st, err := ReadState(e.paths)
	if err != nil {
		return err
	}
	switch {
	case st == nil || st.Phase != PhaseDone:
		return errors.New("there is no finished Storage migration whose files could be deleted")
	case st.Cleaned || st.Retained == "":
		return errors.New("the files of the migration are already deleted")
	case !strings.HasPrefix(st.Retained, retainedPrefix) || strings.ContainsRune(st.Retained, filepath.Separator):
		return fmt.Errorf("the state names %q as the directory to delete, which is not one this command keeps", st.Retained)
	}
	dir := filepath.Join(serviceDir(e.paths), st.Retained)
	e.say("deleting %s", dir)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	e.st = st
	st.Cleaned = true
	return e.save()
}

// Rollback goes back to the files: it copies what the bucket gained since the switch into the
// files that were kept (all of it when they were deleted), stops Storage, copies once more and
// starts Storage on the files. Objects uploaded after the switch come back with it, and objects
// deleted since are deleted from the files.
func (e *Engine) Rollback(ctx context.Context, req Request) error {
	release, err := lock(e.paths)
	if err != nil {
		return err
	}
	defer release()
	st, err := ReadState(e.paths)
	if err != nil {
		return err
	}
	if st == nil || (st.Phase != PhaseDone && st.Phase != PhaseRollingBack) {
		return errors.New("there is no finished Storage migration to roll back")
	}
	e.st, e.inv = st, map[string]inventory{}
	if st.Phase == PhaseDone {
		st.Phase, st.Step = PhaseRollingBack, ""
	}
	st.Error = ""
	b, err := e.open(ctx, st.Dest, req.Credentials)
	if err != nil {
		return err
	}
	e.begin(req)
	return e.rollback(ctx, b)
}

func (e *Engine) rollback(ctx context.Context, b Bucket) error {
	if err := e.doRollback(ctx, b); err != nil {
		return e.fail(err)
	}
	return nil
}

func (e *Engine) doRollback(ctx context.Context, b Bucket) error {
	st := e.st
	release := e.holdWrites(ctx)
	defer release()
	done := func(step string) bool { return reached(rollbackSteps, st.Step, step) }
	if st.Step == "" {
		if err := e.mark(stepHold); err != nil {
			return err
		}
	}
	stopped := done(stepStopped) && !done(stepStarted)

	if !done(stepRestore) {
		if err := e.restoreFiles(); err != nil {
			return err
		}
		if err := e.mark(stepRestore); err != nil {
			return err
		}
	}
	if !done(stepFinal) {
		e.say("holding Storage's writes; copying what the bucket gained back into the files")
		if err := e.pullAll(ctx, b, "final pass"); err != nil {
			return err
		}
		if err := e.mark(stepFinal); err != nil {
			return err
		}
	}
	// A failure from here to the switch puts Storage back on the bucket, which is where it was.
	revive := func(cause error) error {
		if !stopped {
			return cause
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if err := e.d.Storage.Start(cctx); err != nil {
			return fmt.Errorf("%w; and supavise-storage did not start again: %v (start it: sudo systemctl start supavise-storage.service)", cause, err)
		}
		return cause
	}
	if !done(stepStopped) {
		e.say("stopping supavise-storage")
		stopped = true
		if err := e.d.Storage.Stop(ctx); err != nil {
			return revive(err)
		}
		if err := e.mark(stepStopped); err != nil {
			return err
		}
	}
	if !done(stepFence) {
		e.fence = true
		err := e.pullAll(ctx, b, "fence pass")
		e.fence = false
		if err != nil {
			return revive(err)
		}
		if err := e.mark(stepFence); err != nil {
			return err
		}
	}
	if !done(stepSwitched) {
		if err := e.d.Settings.UseFiles(ctx, st.PrevBackend, st.WroteCredentials); err != nil {
			return revive(err)
		}
		st.WroteCredentials = false
		if err := e.mark(stepSwitched); err != nil {
			return err
		}
	}
	if !done(stepStarted) {
		e.say("starting supavise-storage on the files")
		if err := e.d.Storage.Start(ctx); err != nil {
			return err
		}
		if err := e.probe(ctx); err != nil {
			return err
		}
		if err := e.mark(stepStarted); err != nil {
			return err
		}
	}
	st.Phase, st.Step, st.Retained, st.Cleaned, st.Error = PhaseRolledBack, "", "", false, ""
	st.RetainUntil = time.Time{}
	if err := e.save(); err != nil {
		return err
	}
	e.say("Storage keeps its objects as files again: %d objects (%s) were copied back and %d removed", st.Downloaded.Files, bytesString(st.Downloaded.Bytes), st.Removed)
	e.say("restart supavise.service when convenient so that the daemon reads the setting too")
	return nil
}

// restoreFiles moves the kept files back to where Storage's file backend reads them. When they
// were deleted the directory starts empty and the passes below fetch everything.
func (e *Engine) restoreFiles() error {
	objs := objectsDir(e.paths)
	kept := ""
	if e.st.Retained != "" && !e.st.Cleaned {
		kept = filepath.Join(serviceDir(e.paths), e.st.Retained)
		if _, err := os.Lstat(kept); err != nil {
			kept = ""
		}
	}
	if _, err := os.Lstat(objs); err == nil {
		empty := true
		_ = filepath.WalkDir(objs, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				empty = false
			}
			return nil
		})
		switch {
		case kept == "":
			return nil // the directory is there: something put it back, or an earlier attempt did
		case !empty:
			return fmt.Errorf("both %s and %s hold files; move one of them aside, then --resume", objs, kept)
		}
		if err := os.RemoveAll(objs); err != nil {
			return err
		}
	}
	if kept == "" {
		return os.MkdirAll(objs, 0o750)
	}
	return os.Rename(kept, objs)
}

// pullAll runs pullTenant for every project.
func (e *Engine) pullAll(ctx context.Context, b Bucket, what string) error {
	projects, err := e.d.Tenants.Projects(ctx)
	if err != nil {
		return err
	}
	byRef := map[string]Project{}
	for _, p := range projects {
		byRef[p.Ref] = p
	}
	refs, err := e.refs(byRef)
	if err != nil {
		return err
	}
	var got Counts
	removed := 0
	for _, ref := range refs {
		c, r, err := e.pullTenant(ctx, b, ref)
		if err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
		got.Files += c.Files
		got.Bytes += c.Bytes
		removed += r
	}
	e.st.Downloaded.Files += got.Files
	e.st.Downloaded.Bytes += got.Bytes
	e.st.Removed += removed
	e.say("%s: %d objects (%s) copied back, %d removed from the files", what, got.Files, bytesString(got.Bytes), removed)
	return e.save()
}

// pullTenant makes the files of one project match the bucket's copy.
func (e *Engine) pullTenant(ctx context.Context, b Bucket, ref string) (Counts, int, error) {
	listed, err := e.listTenant(ctx, b, ref)
	if err != nil {
		return Counts{}, 0, err
	}
	cur, _, err := e.walkTenant(ctx, ref)
	if err != nil {
		return Counts{}, 0, err
	}
	prefix := ref + "/"
	var fetch []Entry
	var drop []string
	i, j := 0, 0
	for i < len(listed) || j < len(cur) {
		var key string
		if i < len(listed) {
			key = listed[i].Key[len(prefix):]
		}
		switch {
		case j == len(cur) || (i < len(listed) && key < cur[j].Path):
			fetch = append(fetch, listed[i])
			i++
		case i == len(listed) || cur[j].Path < key:
			drop = append(drop, cur[j].Path)
			j++
		default:
			if listed[i].Size != cur[j].Size {
				fetch = append(fetch, listed[i])
			}
			i++
			j++
		}
	}
	if len(listed) == 0 && len(drop) > 0 {
		// A bucket that lists nothing for a project that has files is more likely the wrong bucket
		// or a listing that failed quietly than a project whose objects were all deleted.
		e.say("project %s: the bucket lists nothing for it, so its %d files were not removed", ref, len(drop))
		drop = nil
	}
	var got Counts
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(e.workers)
	for _, en := range fetch {
		g.Go(func() error {
			n, err := e.download(gctx, b, ref, en)
			if err != nil {
				return err
			}
			mu.Lock()
			got.Files++
			got.Bytes += n
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return got, 0, err
	}
	root := e.paths.StorageObjects(ref)
	for _, p := range drop {
		abs := filepath.Join(root, filepath.FromSlash(p))
		if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return got, 0, err
		}
		pruneEmpty(filepath.Dir(abs), root)
	}
	return got, len(drop), nil
}

// pruneEmpty removes the empty directories from dir up to, not including, root.
func pruneEmpty(dir, root string) {
	for dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// download writes one object to its file: aside first, with its attributes and modification time,
// then renamed into place, so Storage never reads half a file.
func (e *Engine) download(ctx context.Context, b Bucket, ref string, en Entry) (int64, error) {
	rel := en.Key[len(ref)+1:]
	dst := filepath.Join(e.paths.StorageObjects(ref), filepath.FromSlash(rel))
	if err := e.mkdirAll(filepath.Dir(dst)); err != nil {
		return 0, err
	}
	rc, meta, err := b.Get(ctx, en.Key)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".supavise-pull-")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, rc)
	if err == nil && n != en.Size {
		err = fmt.Errorf("%d bytes came back, the listing says %d", n, en.Size)
	}
	if err == nil {
		err = tmp.Chmod(0o640)
	}
	if err == nil {
		e.chownLikeParent(tmp)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, fmt.Errorf("%s: %w", en.Key, err)
	}
	if ok, err := e.files.SetMeta(tmp.Name(), meta); err != nil {
		return 0, fmt.Errorf("%s: write extended attributes: %w", en.Key, err)
	} else if !ok && (meta.ContentType != "" || meta.CacheControl != "") {
		e.log.Warn("this file system has no extended attributes: objects copied back lose their content type and cache control", "path", dst)
	}
	if !en.ModTime.IsZero() {
		_ = os.Chtimes(tmp.Name(), en.ModTime, en.ModTime)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return 0, err
	}
	return n, nil
}

// mkdirAll creates dir and the directories above it that are missing, owned like the
// service directory when this process runs as root.
func (e *Engine) mkdirAll(dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		return nil
	}
	if err := e.mkdirAll(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if f, err := os.Open(dir); err == nil {
		e.chownLikeParent(f)
		f.Close()
	}
	return nil
}

// chownLikeParent gives f the owner of the service directory, which belongs to the supavise
// user, when the command runs as root: Storage runs as that user and must be able to replace
// what a rollback wrote.
func (e *Engine) chownLikeParent(f *os.File) {
	if os.Geteuid() != 0 {
		return
	}
	fi, err := os.Stat(serviceDir(e.paths))
	if err != nil {
		return
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = f.Chown(int(st.Uid), int(st.Gid))
	}
}
