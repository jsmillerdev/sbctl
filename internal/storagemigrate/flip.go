package storagemigrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/secrets"
)

var (
	flipSteps     = []string{stepHold, stepFinal, stepStopped, stepFence, stepSwitched, stepLive, stepStarted}
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
// airtight. Until Storage has started on the bucket, a failure puts it back on the files. From then
// on it may hold writes that only the bucket has, so a failure leaves it where it is: --resume
// finishes the switch and --rollback copies those writes back.
func (e *Engine) flip(ctx context.Context, b Bucket) error {
	st := e.st
	done := func(step string) bool { return reached(flipSteps, st.Step, step) }
	if !done(stepStopped) {
		// Storage is still up: what would fail after the stop fails now instead.
		if err := e.checkSwitch(ctx, st.Dest, e.creds); err != nil {
			return err
		}
	}
	release := e.holdWrites(ctx)
	defer release()
	defer e.unthrottle()()
	if st.Step == "" {
		if err := e.mark(stepHold); err != nil {
			return err
		}
	}
	stopped := done(stepStopped) && !done(stepLive)
	switched := done(stepSwitched) && !done(stepLive)
	var stoppedAt time.Time // when this process stopped Storage; a run that continues one cannot say

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
		e.say("stopping supavise-storage: reads fail until it runs again, for a time that grows with the number of files")
		stopped = true // a stop that fails may still have stopped it
		stoppedAt = e.now()
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
	if !done(stepLive) {
		e.say("starting supavise-storage on the bucket")
		if err := e.d.Storage.Start(ctx); err != nil {
			return e.abortFlip(ctx, err, stopped, switched)
		}
		if err := e.mark(stepLive); err != nil {
			return fmt.Errorf("Storage runs on the bucket, but the run could not record it: %w", err)
		}
		if !stoppedAt.IsZero() {
			e.say("supavise-storage was stopped for %s: the pass over the files, the check of the rows, the new configuration and the start", e.now().Sub(stoppedAt).Round(100*time.Millisecond))
		}
	} else if err := e.d.Storage.Healthy(ctx); err != nil {
		// An earlier attempt got Storage onto the bucket and it is not answering now.
		e.say("starting supavise-storage on the bucket")
		if err := e.d.Storage.Start(ctx); err != nil {
			return fmt.Errorf("%w (Storage keeps its objects in the bucket now: fix the cause and --resume, or --rollback)", err)
		}
	}
	if !done(stepStarted) {
		if err := e.probe(ctx, "the bucket"); err != nil {
			return fmt.Errorf("%w (Storage runs on the bucket and may have taken writes that the files lack: fix the cause and --resume, or --rollback to copy them back)%s", err, e.roleHint())
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
	e.say("restart supavise.service now so that the daemon reads the new setting too: until then a backup that it starts itself looks for the files, finds none, and records an empty snapshot of them")
	return nil
}

// roleHint is what to add to the failure of Storage's first reads from the bucket when the run uses a
// role: the daemon serves the role's credentials to supavise-storage, and reads [fleet]
// storage_s3_role_arn when it starts. A daemon that started before this run wrote the role does not
// serve them, and Storage cannot sign a request until it is restarted.
func (e *Engine) roleHint() string {
	if !e.creds.assumesARole() && e.st.RoleARN == "" {
		return ""
	}
	return " With a role the daemon serves Storage's credentials, and it reads the role when it starts: if supavise.service started before the role was in the configuration, run sudo systemctl restart supavise.service, then --resume."
}

// abortFlip puts Storage back on the files after a failure before Storage has started on the bucket.
// The run goes back to the catch-up phase, so --resume copies, verifies and tries the switch again.
func (e *Engine) abortFlip(ctx context.Context, cause error, stopped, switched bool) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	if switched {
		if err := e.d.Settings.UseFiles(cctx, e.st.PrevBackend, e.st.WroteCredentials); err != nil {
			// The configuration still names the bucket, and a Storage started now would come up on
			// it, with writes that only the bucket would have. It stays stopped, and the run stays
			// in the switch: --resume writes the bucket into the configuration again and finishes it.
			e.fence = false
			return fmt.Errorf("%w; and the configuration could not be put back on the files: %v. supavise-storage is left stopped, because it would start on the bucket. Fix the cause and run --resume to finish the switch, or set [fleet] storage_backend back to %q yourself and start supavise-storage (sudo systemctl start supavise-storage.service)", cause, err, e.st.PrevBackend)
		}
		e.st.WroteCredentials = false
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
		_, pending, _, err := e.checkRows(ctx, p.Ref, inv)
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

// probe reads a few objects through Storage, which now runs on the backend named by from. It says what
// it read, because a store without rows gives it nothing to read.
func (e *Engine) probe(ctx context.Context, from string) error {
	if e.d.Reader == nil {
		return nil
	}
	projects, err := e.d.Tenants.Projects(ctx)
	if err != nil {
		e.say("nothing was read back through Storage: the projects could not be listed (%v)", err)
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
	samples := e.samples(ctx, refs, 3)
	for ref, row := range samples {
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
			return fmt.Errorf("Storage could not read %s back from %s: %w", quote(ref+"/"+rowPath(row)), from, last)
		}
	}
	if len(samples) == 0 {
		e.say("nothing was read back through Storage: no project has an object with a recorded size, and a write by Storage to %s is untested", from)
	} else {
		e.say("Storage served %d sampled objects from %s", len(samples), from)
	}
	return nil
}

// keepFiles renames the objects directory to objects.migrated-<date> and returns the new name; ""
// when there is no objects directory and no earlier attempt of this run renamed it (nothing was ever
// stored). The name is recorded before the rename, and only a directory with that name is taken for
// the files of this run.
func (e *Engine) keepFiles() (string, error) {
	st := e.st
	if st.KeepAs == "" {
		day := e.now().UTC().Format("20060102")
		name := retainedPrefix + day
		for i := 2; ; i++ {
			if _, err := os.Lstat(filepath.Join(serviceDir(e.paths), name)); errors.Is(err, fs.ErrNotExist) {
				break
			}
			name = fmt.Sprintf("%s%s-%d", retainedPrefix, day, i)
		}
		st.KeepAs = name
		if err := e.save(); err != nil {
			return "", err
		}
	}
	src, dst := objectsDir(e.paths), filepath.Join(serviceDir(e.paths), st.KeepAs)
	_, serr := os.Lstat(src)
	_, derr := os.Lstat(dst)
	switch {
	case errors.Is(serr, fs.ErrNotExist) && derr == nil:
		return st.KeepAs, nil // an earlier attempt of this run renamed it
	case errors.Is(serr, fs.ErrNotExist):
		return "", nil
	case serr != nil:
		return "", serr
	case derr == nil:
		return "", fmt.Errorf("both %s and %s exist; move one of them aside, then --resume", src, dst)
	}
	return st.KeepAs, os.Rename(src, dst)
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
// deleted since are deleted from the files. It also undoes a switch that stopped after Storage
// started on the bucket.
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
	// A run whose switch stopped after Storage started on the bucket can be rolled back too: Storage
	// may have written there, and the rollback copies that back.
	live := st != nil && st.Phase == PhaseFlipping && reached(flipSteps, st.Step, stepLive)
	if st == nil || (st.Phase != PhaseDone && st.Phase != PhaseRollingBack && !live) {
		return errors.New("there is no finished Storage migration to roll back")
	}
	if err := e.checkState(st); err != nil {
		return err
	}
	e.st, e.inv = st, map[string]inventory{}
	if st.Phase == PhaseDone || live {
		st.Phase, st.Step = PhaseRollingBack, ""
	}
	st.Error = ""
	e.begin(req)
	var b Bucket
	if st.NeedsBucket() {
		if b, err = e.connect(ctx, st.Dest, req.Credentials); err != nil {
			return err
		}
	}
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
		if err := e.probe(ctx, "the files"); err != nil {
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
	// The directory the switch renamed the files to is recorded as Retained once the rename is done
	// and saved. A run that was killed between the rename and that save names it only as KeepAs,
	// which is written before the rename: the files are there all the same.
	name := e.st.Retained
	if name == "" {
		name = e.st.KeepAs
	}
	if name != "" && !e.st.Cleaned {
		kept = filepath.Join(serviceDir(e.paths), name)
		if fi, err := os.Lstat(kept); err != nil || !fi.IsDir() {
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
	removed, refused := 0, 0
	for _, ref := range refs {
		r, err := e.pullTenant(ctx, b, ref)
		if err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
		got.Files += r.Files
		got.Bytes += r.Bytes
		removed += r.removed
		refused += len(r.refused)
		if len(r.refused) > 0 {
			e.say("project %s: %d keys in the bucket cannot be files and were left there: %s", ref, len(r.refused), strings.Join(first(r.refused, 3), ", "))
		}
	}
	e.st.Downloaded.Files += got.Files
	e.st.Downloaded.Bytes += got.Bytes
	e.st.Removed += removed
	e.st.RefusedKeys = refused // what a pass leaves out is the same every time
	e.say("%s: %d objects (%s) copied back, %d removed from the files", what, got.Files, bytesString(got.Bytes), removed)
	return e.save()
}

// pulled is what pullTenant did for one project: the objects fetched, the files removed and the keys
// it would not write.
type pulled struct {
	Counts
	removed int
	refused []string
}

// fileUnder is where the object with the key rel (the part after <ref>/) lies below root, and false
// when no file there could be that object. A key comes from the bucket and is not trusted: one that
// is empty, has an empty, "." or ".." segment or holds a NUL would, once joined, leave root or name
// another file than the object's.
func fileUnder(root, rel string) (string, bool) {
	if rel == "" || strings.ContainsRune(rel, 0) {
		return "", false
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", false
	}
	return abs, true
}

// pullTenant makes the files of one project match the bucket's copy.
func (e *Engine) pullTenant(ctx context.Context, b Bucket, ref string) (pulled, error) {
	var res pulled
	all, err := e.listTenant(ctx, b, ref)
	if err != nil {
		return res, err
	}
	cur, _, err := e.walkTenant(ctx, ref)
	if err != nil {
		return res, err
	}
	root := e.paths.StorageObjects(ref)
	prefix := ref + "/"
	listed := all[:0:0]
	for _, en := range all {
		if _, ok := fileUnder(root, en.Key[len(prefix):]); !ok {
			res.refused = append(res.refused, quote(en.Key))
			continue
		}
		listed = append(listed, en)
	}
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
			res.Files++
			res.Bytes += n
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return res, err
	}
	for _, p := range drop {
		abs, ok := fileUnder(root, p)
		if !ok {
			continue
		}
		if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, err
		}
		pruneEmpty(filepath.Dir(abs), root)
		res.removed++
	}
	return res, nil
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
	dst, ok := fileUnder(e.paths.StorageObjects(ref), en.Key[len(ref)+1:])
	if !ok {
		return 0, fmt.Errorf("%s: the key cannot be a file below the project's directory", quote(en.Key))
	}
	if err := e.mkdirBelow(objectsDir(e.paths), filepath.Dir(dst)); err != nil {
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
		return 0, fmt.Errorf("%s: %w", quote(en.Key), err)
	}
	if ok, err := e.files.SetMeta(tmp.Name(), meta); err != nil {
		return 0, fmt.Errorf("%s: write extended attributes: %w", quote(en.Key), err)
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

// mkdirBelow creates the directories of dir that are missing below top, which exists, each owned like
// the service directory when this process runs as root. A directory that is a link or a file is an
// error: nothing Storage's file backend makes is one, and writing through it could leave top.
func (e *Engine) mkdirBelow(top, dir string) error {
	rel, err := filepath.Rel(top, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is not below %s", dir, top)
	}
	cur := top
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		switch {
		case err == nil && fi.IsDir():
			continue
		case err == nil:
			return fmt.Errorf("%s is not a directory (a link or a file): a rollback does not write through it", cur)
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		if err := os.Mkdir(cur, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if f, err := os.Open(cur); err == nil {
			e.chownLikeParent(f)
			f.Close()
		}
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
