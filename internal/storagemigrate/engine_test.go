package storagemigrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/storagemigrate/hold"
)

var ctx = context.Background()

func wantKeys(refKeys ...string) []string { return refKeys }

func TestMigrateCopiesVerifiesAndSwitches(t *testing.T) {
	e := newEnv(t)
	e.populate()
	heldAtStop := false
	e.svc.onStop = func() { heldAtStop = hold.Held(e.paths, time.Now()) }
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")

	want := wantKeys(
		refA+"/avatars/dir/sub/nested.bin/v2", refA+"/avatars/top.txt/v1", refA+"/docs/empty/v3",
		refB+"/pics/a+b=c.txt/v5", refB+"/pics/sp ace/ü-é.txt/v4")
	if got := e.keyList(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("keys\n got: %q\nwant: %q", got, want)
	}
	o := e.bucket.objs[refA+"/avatars/top.txt/v1"]
	if o.meta.ContentType != "text/plain" || o.meta.CacheControl != "max-age=3600" || string(o.data) != "top level object\n" {
		t.Errorf("top.txt: %+v", o)
	}
	if n := len(e.bucket.objs[refA+"/avatars/dir/sub/nested.bin/v2"].data); n != 300_000 {
		t.Errorf("nested.bin has %d bytes", n)
	}

	st := e.state()
	if st.Phase != PhaseDone || st.Step != "" || st.Error != "" || st.Uploaded.Files != 5 || !st.WroteCredentials {
		t.Errorf("state: %+v", st)
	}
	if !strings.HasPrefix(st.Retained, retainedPrefix) || !e.exists(st.Retained+"/stub/"+refA+"/avatars/top.txt/v1") || e.exists("objects") {
		t.Errorf("the files were not kept aside: retained %q", st.Retained)
	}
	if d := st.RetainUntil.Sub(st.FlippedAt); d != RetainFor {
		t.Errorf("kept for %s", d)
	}
	if got := e.svc.log(); got != "stop,start" {
		t.Errorf("service calls: %s", got)
	}
	if e.set.useBucket != 1 || e.set.backend != "s3" || e.set.dest.Bucket != "objects" || e.set.dest.Region != "us-east-1" || e.set.creds.Source != CredFile {
		t.Errorf("settings: %+v", e.set)
	}
	if !heldAtStop || hold.Held(e.paths, time.Now()) {
		t.Errorf("write hold: during the stop %v, afterwards %v", heldAtStop, hold.Held(e.paths, time.Now()))
	}
	if len(e.rd.reads) == 0 || !strings.Contains(e.out.String(), "Storage served 2 sampled objects from the bucket") {
		t.Errorf("nothing was read back through Storage:\n%s", e.out.String())
	}
	if len(st.Tenants) != 2 || st.Tenants[0].Rows != 3 || st.Tenants[1].Rows != 2 || st.Tenants[0].Bytes != 300_017 {
		t.Errorf("verified: %+v", st.Tenants)
	}
	if d := Describe(st, time.Now()); !strings.Contains(d, "phase:") || !strings.Contains(d, "done") || !strings.Contains(d, "kept as objects.migrated-") {
		t.Errorf("status:\n%s", d)
	}
	// A second run has nothing to do while Storage uses the bucket.
	e.cfg.Fleet.StorageBackend = "s3"
	if err := e.engine().Migrate(ctx, e.req()); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("second run: %v", err)
	}
}

func TestCatchUpSendsWhatChangedDuringTheCopy(t *testing.T) {
	e := newEnv(t)
	e.populate()
	var once sync.Once
	// The first object of the second project goes out when the first project is done: a file
	// of the first is written and another deleted, after they were walked and sent.
	e.bucket.onPut = func(key string) {
		if !strings.HasPrefix(key, refB+"/") {
			return
		}
		once.Do(func() {
			e.put(refA, "avatars", "late.txt", "v9", []byte("written while copying"), "text/plain")
			e.remove(refA, "avatars", "top.txt", "v1")
		})
	}
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	keys := strings.Join(e.keyList(), "\n")
	if !strings.Contains(keys, refA+"/avatars/late.txt/v9") {
		t.Errorf("the file written during the copy is not in the bucket:\n%s", keys)
	}
	if strings.Contains(keys, "top.txt") {
		t.Errorf("the file deleted during the copy is still in the bucket:\n%s", keys)
	}
	if st := e.state(); st.Passes < 3 || st.Phase != PhaseDone || st.Deleted != 1 {
		t.Errorf("passes %d, phase %s, deleted %d", st.Passes, st.Phase, st.Deleted)
	}
}

func TestKeysThatNoPassOfTheRunSentAreLeftAlone(t *testing.T) {
	e := newEnv(t)
	e.populate()
	// Something else's, or from an earlier use of the bucket: no file matches it.
	foreign := refA + "/avatars/from-before.txt/v0"
	e.bucket.objs[foreign] = memObj{data: []byte("keep me"), mod: time.Now()}
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	if _, ok := e.bucket.objs[foreign]; !ok {
		t.Error("an object no file matches was deleted")
	}
	if st := e.state(); st.Tenants[0].Extra != 1 || st.Deleted != 0 {
		t.Errorf("tenants %+v, deleted %d", st.Tenants, st.Deleted)
	}
	if !strings.Contains(e.out.String(), "have no file and were left alone") {
		t.Errorf("the run did not say so:\n%s", e.out.String())
	}
}

func TestSwitchCopiesWhatWasWrittenAfterTheLastCatchUp(t *testing.T) {
	e := newEnv(t)
	e.populate()
	// Storage keeps taking writes until it is stopped: this one lands after the verification and
	// is only caught by the pass over the stopped service's files.
	e.svc.onStop = func() {
		e.put(refA, "avatars", "straggler.txt", "v8", []byte("one more"), "text/plain")
	}
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	if _, ok := e.bucket.objs[refA+"/avatars/straggler.txt/v8"]; !ok {
		t.Errorf("the file written when Storage stopped was not copied:\n%v", e.keyList())
	}
}

func TestVerifyRefusesToSwitchWhenAnObjectIsMissing(t *testing.T) {
	e := newEnv(t)
	e.populate()
	e.tenants.rows[refA] = append(e.tenants.rows[refA], Row{Bucket: "avatars", Name: "ghost.txt", Version: "v0", Size: 3, HasSize: true})
	err := e.engine().Migrate(ctx, e.req())
	var verr *VerifyError
	if !errors.As(err, &verr) || verr.Ref != refA || len(verr.Missing) != 1 || !strings.Contains(err.Error(), "ghost.txt") {
		t.Fatalf("Migrate = %v", err)
	}
	st := e.state()
	if st.Phase != PhaseVerifying || !strings.Contains(st.Error, "nothing was switched") {
		t.Errorf("state: %s, %q", st.Phase, st.Error)
	}
	if e.svc.log() != "" || e.set.useBucket != 0 || !e.exists("objects") {
		t.Errorf("something was switched: %s %+v", e.svc.log(), e.set)
	}
	// Fixing the cause lets --resume go on from the verification.
	e.tenants.rows[refA] = e.tenants.rows[refA][:3]
	mustf(t, e.engine().Resume(ctx, e.req()), "resume")
	if st := e.state(); st.Phase != PhaseDone {
		t.Errorf("phase %s", st.Phase)
	}
}

func TestVerifyComparesSizes(t *testing.T) {
	e := newEnv(t)
	e.populate()
	e.tenants.rows[refA][0].Size++
	err := e.engine().Migrate(ctx, e.req())
	var verr *VerifyError
	if !errors.As(err, &verr) || len(verr.MismatchedSize) != 1 {
		t.Fatalf("Migrate = %v", err)
	}
}

func TestVerifyToleratesFilesWithoutRowsAndProjectsWithoutDatabase(t *testing.T) {
	e := newEnv(t)
	e.populate()
	e.write(refA, "avatars/orphan.txt/v7", []byte("no row"), "text/plain")
	e.tenants.offline[refB] = true
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	st := e.state()
	if st.Tenants[0].Orphans != 1 || st.Tenants[0].Rows != 3 || !st.Tenants[1].Offline || st.Tenants[1].Files != 2 {
		t.Errorf("tenants: %+v", st.Tenants)
	}
	if _, ok := e.bucket.objs[refA+"/avatars/orphan.txt/v7"]; !ok {
		t.Error("the file without a row was not copied")
	}
	if d := Describe(st, time.Now()); !strings.Contains(d, "no running database") {
		t.Errorf("status:\n%s", d)
	}
}

func TestFileNamesThatCannotBeKeysAreReported(t *testing.T) {
	e := newEnv(t)
	e.populate()
	bad := filepath.Join(e.paths.StorageObjects(refA), "avatars", "bad\xff.txt", "v1")
	if err := os.MkdirAll(filepath.Dir(bad), 0o750); err != nil {
		t.Skipf("this file system refuses a name that is not UTF-8: %v", err)
	}
	if err := os.WriteFile(bad, []byte("x"), 0o640); err != nil {
		t.Skipf("this file system refuses a name that is not UTF-8: %v", err)
	}
	long := strings.Repeat(strings.Repeat("l", 200)+"/", 6)
	e.write(refA, "avatars/"+long+"v1", []byte("long"), "")
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	st := e.state()
	if st.SkippedTotal != 2 || len(st.Skipped) != 2 {
		t.Fatalf("skipped: %+v", st.Skipped)
	}
	reasons := st.Skipped[0].Reason + "|" + st.Skipped[1].Reason
	if !strings.Contains(reasons, "UTF-8") || !strings.Contains(reasons, "1,024") {
		t.Errorf("reasons: %s", reasons)
	}
	if !strings.Contains(e.out.String(), "cannot be S3 keys") {
		t.Errorf("the run did not say so:\n%s", e.out.String())
	}
	for _, k := range e.keyList() {
		if strings.Contains(k, "bad") || strings.Contains(k, "llll") {
			t.Errorf("copied %q", k)
		}
	}
}

func TestPreflight(t *testing.T) {
	t.Run("storage is not healthy", func(t *testing.T) {
		e := newEnv(t)
		e.svc.health = errors.New("supavise-storage.service is STOPPED")
		err := e.engine().Migrate(ctx, e.req())
		if err == nil || !strings.Contains(err.Error(), "STOPPED") {
			t.Fatalf("Migrate = %v", err)
		}
		if st, _ := ReadState(e.paths); st != nil {
			t.Errorf("a failed preflight left a state: %+v", st)
		}
	})
	t.Run("the bucket takes no write", func(t *testing.T) {
		e := newEnv(t)
		e.bucket.fail = func(op, key string) error { return errors.New("AccessDenied") }
		err := e.engine().Migrate(ctx, e.req())
		if err == nil || !strings.Contains(err.Error(), "does not take a write") {
			t.Fatalf("Migrate = %v", err)
		}
	})
	t.Run("no bucket", func(t *testing.T) {
		e := newEnv(t)
		e.cfg.Fleet.StorageS3Bucket = ""
		if err := e.engine().Migrate(ctx, e.req()); err == nil || !strings.Contains(err.Error(), "--bucket") {
			t.Fatalf("Migrate = %v", err)
		}
		r := e.req()
		r.Bucket = "other"
		mustf(t, e.engine().Migrate(ctx, r), "migrate with --bucket")
		if e.state().Dest.Bucket != "other" {
			t.Errorf("destination %+v", e.state().Dest)
		}
	})
	t.Run("a run is in progress", func(t *testing.T) {
		e := newEnv(t)
		e.populate()
		e.tenants.failOn = func(string) error { return errors.New("boom") }
		if err := e.engine().Migrate(ctx, e.req()); err == nil {
			t.Fatal("Migrate worked")
		}
		err := e.engine().Migrate(ctx, e.req())
		var ip ErrInProgress
		if !errors.As(err, &ip) || ip.Phase != PhaseVerifying || !strings.Contains(err.Error(), "--resume") {
			t.Errorf("second Migrate = %v", err)
		}
	})
	t.Run("another process holds the lock", func(t *testing.T) {
		e := newEnv(t)
		release, err := lock(e.paths)
		mustf(t, err, "lock")
		defer release()
		if err := e.engine().Migrate(ctx, e.req()); err == nil || !strings.Contains(err.Error(), "another") {
			t.Errorf("Migrate = %v", err)
		}
	})
	t.Run("nothing to resume", func(t *testing.T) {
		e := newEnv(t)
		if err := e.engine().Resume(ctx, e.req()); err == nil || !strings.Contains(err.Error(), "no Storage migration") {
			t.Errorf("Resume = %v", err)
		}
	})
}

// failOnce returns an error the first time it is called with a matching key and nil afterwards.
func failOnce(match string, err error) func(op, key string) error {
	var mu sync.Mutex
	done := false
	return func(op, key string) error {
		mu.Lock()
		defer mu.Unlock()
		if !done && op == "put" && strings.Contains(key, match) && !strings.HasPrefix(key, ".supavise") {
			done = true
			return err
		}
		return nil
	}
}

func TestResumeAfterAFailureInEachPhase(t *testing.T) {
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		arm   func(e *env)
		phase Phase
		clean func(e *env)
	}{
		"copying": {phase: PhaseCopying, arm: func(e *env) { e.bucket.fail = failOnce("top.txt", boom) }},
		"catching up": {phase: PhaseCatchingUp, arm: func(e *env) {
			var once sync.Once
			e.bucket.onPut = func(key string) {
				if !strings.HasPrefix(key, ".supavise") {
					once.Do(func() { e.put(refA, "avatars", "late.txt", "v9", []byte("late"), "text/plain") })
				}
			}
			e.bucket.fail = failOnce("late.txt", boom)
		}},
		"verifying": {phase: PhaseVerifying, arm: func(e *env) {
			n := 0
			e.tenants.failOn = func(string) error {
				if n++; n == 1 {
					return boom
				}
				return nil
			}
		}},
		"stopping":  {phase: PhaseCatchingUp, arm: func(e *env) { e.svc.stopErr = boom }, clean: func(e *env) { e.svc.stopErr = nil }},
		"switching": {phase: PhaseCatchingUp, arm: func(e *env) { e.set.err = boom }, clean: func(e *env) { e.set.err = nil }},
		"starting": {phase: PhaseCatchingUp, arm: func(e *env) {
			e.svc.onStart = func() {
				if e.set.backend == "s3" {
					e.svc.startErr = boom
				}
			}
		}, clean: func(e *env) { e.svc.startErr, e.svc.onStart = nil, nil }},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.populate()
			tc.arm(e)
			err := e.engine().Migrate(ctx, e.req())
			if err == nil || !errors.Is(err, boom) && !strings.Contains(err.Error(), "boom") {
				t.Fatalf("Migrate = %v", err)
			}
			st := e.state()
			if st.Phase != tc.phase || !strings.Contains(st.Error, "boom") {
				t.Fatalf("after the failure: phase %s (want %s), error %q", st.Phase, tc.phase, st.Error)
			}
			if e.exists("objects") == false {
				t.Fatal("the files were moved although the switch did not happen")
			}
			// A failure of the switch leaves Storage running on the files again.
			if strings.Contains(e.svc.log(), "stop") && !strings.HasSuffix(e.svc.log(), "start") {
				t.Errorf("Storage was left stopped: %s", e.svc.log())
			}
			if e.set.backend == "s3" {
				t.Errorf("the configuration still names the bucket")
			}
			if tc.clean != nil {
				tc.clean(e)
			}
			e.bucket.fail, e.bucket.onPut, e.tenants.failOn = nil, nil, nil
			mustf(t, e.engine().Resume(ctx, e.req()), "resume")
			st = e.state()
			if st.Phase != PhaseDone || st.Error != "" || e.set.backend != "s3" {
				t.Errorf("after the resume: %+v", st)
			}
			if len(e.keyList()) < 5 {
				t.Errorf("keys: %q", e.keyList())
			}
		})
	}
}

// killed makes the code under test stop where it is, as a process that is killed does, except that
// deferred cleanups still run.
type killed struct{}

func crash(t *testing.T, f func() error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(killed); !ok {
				panic(r)
			}
		}
	}()
	_ = f()
}

func TestResumeAfterTheProcessDiesInTheMiddleOfTheSwitch(t *testing.T) {
	t.Run("storage stopped", func(t *testing.T) {
		e := newEnv(t)
		e.populate()
		first := true
		e.svc.onStop = func() {
			if first {
				first = false
				panic(killed{})
			}
		}
		crash(t, func() error { return e.engine().Migrate(ctx, e.req()) })
		if st := e.state(); st.Phase != PhaseFlipping || st.Step != stepFinal {
			t.Fatalf("state after the crash: %s/%s", st.Phase, st.Step)
		}
		mustf(t, e.engine().Resume(ctx, e.req()), "resume")
		if st := e.state(); st.Phase != PhaseDone || e.svc.log() != "stop,stop,start" || e.set.useBucket != 1 {
			t.Errorf("phase %s, service %s, settings %+v", st.Phase, e.svc.log(), e.set)
		}
	})
	t.Run("configuration switched", func(t *testing.T) {
		e := newEnv(t)
		e.populate()
		first := true
		e.svc.onStart = func() {
			if first {
				first = false
				panic(killed{})
			}
		}
		crash(t, func() error { return e.engine().Migrate(ctx, e.req()) })
		if st := e.state(); st.Phase != PhaseFlipping || st.Step != stepSwitched || e.set.backend != "s3" {
			t.Fatalf("state after the crash: %s/%s, backend %q", st.Phase, st.Step, e.set.backend)
		}
		before := len(e.bucket.puts)
		mustf(t, e.engine().Resume(ctx, e.req()), "resume")
		if st := e.state(); st.Phase != PhaseDone || e.set.useBucket != 1 || len(e.bucket.puts) != before {
			t.Errorf("phase %s, UseBucket %d, puts %d -> %d", st.Phase, e.set.useBucket, before, len(e.bucket.puts))
		}
	})
}

func TestResumeSkipsWhatTheBucketAlreadyHas(t *testing.T) {
	e := newEnv(t)
	e.populate()
	e.bucket.fail = failOnce("nested.bin", errors.New("boom"))
	if err := e.engine().Migrate(ctx, e.req()); err == nil {
		t.Fatal("Migrate worked")
	}
	e.bucket.fail = nil
	have := len(e.bucket.keys())
	e.bucket.clear()
	mustf(t, e.engine().Resume(ctx, e.req()), "resume")
	// The resumed run sends only the rest.
	if got := len(e.bucket.puts); got != 5-have {
		t.Errorf("the resumed run sent %d objects, want %d (the bucket had %d)", got, 5-have, have)
	}
}

func TestRollbackCopiesBackWhatChangedAndDeletesWhatWasDeleted(t *testing.T) {
	e := newEnv(t)
	// The attributes of a file that is copied back are checked on the real file system, when it
	// has extended attributes.
	realAttrs := e.realAttrs()
	e.populate()
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	kept := e.state().Retained

	// What Storage does on the bucket after the switch: a new object (with its attributes) and a delete.
	e.bucket.objs[refA+"/avatars/new.bin/v10"] = memObj{data: []byte("written on the bucket"), meta: FileMeta{ContentType: "image/png", CacheControl: "no-cache"}, mod: time.Now()}
	delete(e.bucket.objs, refA+"/avatars/top.txt/v1")
	e.svc.mu.Lock()
	e.svc.calls = nil
	e.svc.mu.Unlock()

	mustf(t, e.engine().Rollback(ctx, e.req()), "rollback")
	st := e.state()
	if st.Phase != PhaseRolledBack || st.Downloaded.Files != 1 || st.Removed != 1 || st.Retained != "" || st.WroteCredentials {
		t.Errorf("state: %+v", st)
	}
	if e.exists(kept) || !e.exists("objects") {
		t.Error("the kept files were not moved back")
	}
	newFile := filepath.Join(e.paths.StorageObjects(refA), "avatars", "new.bin", "v10")
	if b, err := os.ReadFile(newFile); err != nil || string(b) != "written on the bucket" {
		t.Errorf("new object: %q, %v", b, err)
	}
	if m, err := e.files.Meta(newFile); realAttrs && (err != nil || m.ContentType != "image/png" || m.CacheControl != "no-cache") {
		t.Errorf("attributes of the new object: %+v, %v", m, err)
	}
	if _, err := os.Stat(filepath.Join(e.paths.StorageObjects(refA), "avatars", "top.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the deleted object is still a file: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(e.paths.StorageObjects(refB), "pics", "a+b=c.txt", "v5")); err != nil || string(b) != "plus and equals\n" {
		t.Errorf("an object that did not change: %q, %v", b, err)
	}
	if e.set.backend != "" || e.set.useFiles != 1 || e.svc.log() != "stop,start" {
		t.Errorf("settings %+v, service %s", e.set, e.svc.log())
	}
	if !strings.Contains(e.out.String(), "copied back") {
		t.Errorf("the command did not say what it copied back:\n%s", e.out.String())
	}
	if err := e.engine().Rollback(ctx, e.req()); err == nil {
		t.Error("a second rollback worked")
	}

	// Migrating again sends only what the bucket lacks.
	for k, o := range e.bucket.objs {
		o.mod = time.Now().Add(time.Hour) // as if the copies were made after the files were written
		e.bucket.objs[k] = o
	}
	e.tenants.rows[refA] = []Row{
		{Bucket: "avatars", Name: "dir/sub/nested.bin", Version: "v2", Size: 300_000, HasSize: true},
		{Bucket: "docs", Name: "empty", Version: "v3", Size: 0, HasSize: true},
		{Bucket: "avatars", Name: "new.bin", Version: "v10", Size: int64(len("written on the bucket")), HasSize: true},
	}
	e.bucket.clear()
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate again")
	var sent []string
	for _, k := range e.bucket.puts {
		if !strings.HasPrefix(k, ".supavise") { // the preflight's probe
			sent = append(sent, k)
		}
	}
	if len(sent) != 0 {
		t.Errorf("the second migration sent %q", sent)
	}
	if e.state().Phase != PhaseDone {
		t.Errorf("phase %s", e.state().Phase)
	}
}

func TestRollbackKeepsFilesWhenTheBucketListsNothing(t *testing.T) {
	e := newEnv(t)
	e.populate()
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	// The bucket has lost everything of one project (the wrong bucket, a listing that said nothing).
	for k := range e.bucket.objs {
		if strings.HasPrefix(k, refB+"/") {
			delete(e.bucket.objs, k)
		}
	}
	mustf(t, e.engine().Rollback(ctx, e.req()), "rollback")
	if b, err := os.ReadFile(filepath.Join(e.paths.StorageObjects(refB), "pics", "a+b=c.txt", "v5")); err != nil || string(b) != "plus and equals\n" {
		t.Errorf("a file was removed because the bucket lists nothing: %q, %v", b, err)
	}
	if st := e.state(); st.Removed != 0 || !strings.Contains(e.out.String(), "lists nothing") {
		t.Errorf("removed %d; output:\n%s", st.Removed, e.out.String())
	}
}

func TestRollbackAfterCleanupFetchesEverything(t *testing.T) {
	e := newEnv(t)
	realAttrs := e.realAttrs()
	e.populate()
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	kept := e.state().Retained
	mustf(t, e.engine().Cleanup(ctx), "cleanup")
	if e.exists(kept) || !e.state().Cleaned {
		t.Fatalf("cleanup left %s (cleaned %v)", kept, e.state().Cleaned)
	}
	if err := e.engine().Cleanup(ctx); err == nil {
		t.Error("a second cleanup worked")
	}
	if d := Describe(e.state(), time.Now()); !strings.Contains(d, "files:") || !strings.Contains(d, "deleted") {
		t.Errorf("status:\n%s", d)
	}
	mustf(t, e.engine().Rollback(ctx, e.req()), "rollback")
	st := e.state()
	if st.Phase != PhaseRolledBack || st.Downloaded.Files != 5 {
		t.Errorf("state: %+v", st)
	}
	f := filepath.Join(e.paths.StorageObjects(refB), "pics", "sp ace", "ü-é.txt", "v4")
	if b, err := os.ReadFile(f); err != nil || string(b) != "space and unicode\n" {
		t.Errorf("%q, %v", b, err)
	}
	if m, err := e.files.Meta(f); realAttrs && (err != nil || m.ContentType != "text/plain" || m.CacheControl != "max-age=3600") {
		t.Errorf("attributes: %+v, %v", m, err)
	}
}

func TestCleanupNeedsAFinishedMigration(t *testing.T) {
	e := newEnv(t)
	if err := e.engine().Cleanup(ctx); err == nil {
		t.Error("cleanup without a migration worked")
	}
	e.populate()
	e.tenants.failOn = func(string) error { return errors.New("boom") }
	_ = e.engine().Migrate(ctx, e.req())
	if err := e.engine().Cleanup(ctx); err == nil {
		t.Error("cleanup during a migration worked")
	}
	if !e.exists("objects") {
		t.Error("the files are gone")
	}
}

func TestCleanupRefusesADirectoryTheStateShouldNotName(t *testing.T) {
	e := newEnv(t)
	e.populate()
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	st := e.state()
	st.Retained = "../../etc"
	mustf(t, saveState(e.paths, st, time.Now()), "save")
	if err := e.engine().Cleanup(ctx); err == nil || !strings.Contains(err.Error(), "not one this command keeps") {
		t.Errorf("Cleanup = %v", err)
	}
}

func TestWriteHold(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	if hold.Held(e.paths, now) {
		t.Fatal("held before anything")
	}
	release := e.engine().holdWrites(ctx)
	if !hold.Held(e.paths, time.Now()) {
		t.Error("not held")
	}
	// The run keeps the hold alive for longer than HoldFor.
	time.Sleep(450 * time.Millisecond)
	if !hold.Held(e.paths, time.Now()) {
		t.Error("the hold expired while the run lives")
	}
	release()
	if hold.Held(e.paths, time.Now()) || fileExists(hold.Path(e.paths)) {
		t.Error("held after release")
	}
	// A run that died leaves a marker that expires.
	mustf(t, hold.Write(e.paths, hold.Marker{PID: 1, Since: now, Until: now.Add(time.Minute)}), "write")
	if !hold.Held(e.paths, now) || hold.Held(e.paths, now.Add(2*time.Minute)) {
		t.Error("expiry")
	}
	if hold.ClearStale(e.paths, now) || !hold.ClearStale(e.paths, now.Add(2*time.Minute)) || fileExists(hold.Path(e.paths)) {
		t.Error("ClearStale")
	}
}

func TestStatusWithoutARun(t *testing.T) {
	e := newEnv(t)
	st, err := e.engine().Status()
	if err != nil || st != nil {
		t.Errorf("Status = %v, %v", st, err)
	}
	var buf bytes.Buffer
	buf.WriteString(Describe(&State{Phase: PhaseCatchingUp, Dest: Destination{Bucket: "b"}, Error: "x"}, time.Now()))
	if !strings.Contains(buf.String(), "supavise storage migrate --resume") {
		t.Errorf("an unfinished run does not say how to continue:\n%s", buf.String())
	}
}

// storageStartsOnTheBucket arms e so that Storage, as soon as it starts on the bucket, takes a write
// that only the bucket has, and the read-back through it fails.
func storageStartsOnTheBucket(e *env) {
	e.svc.onStart = func() {
		if e.set.backend == "s3" {
			e.bucket.mu.Lock()
			e.bucket.objs[refA+"/avatars/written.bin/v20"] = memObj{data: []byte("only in the bucket"),
				meta: FileMeta{ContentType: "image/png", CacheControl: "no-cache"}, mod: time.Now()}
			e.bucket.mu.Unlock()
		}
	}
	e.rd.err = errors.New("boom")
}

func TestAFailureAfterStorageStartedOnTheBucketIsNotUndone(t *testing.T) {
	e := newEnv(t)
	e.populate()
	storageStartsOnTheBucket(e)
	err := e.engine().Migrate(ctx, e.req())
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "--rollback") {
		t.Fatalf("Migrate = %v", err)
	}
	st := e.state()
	if st.Phase != PhaseFlipping || st.Step != stepLive {
		t.Fatalf("state: %s/%s", st.Phase, st.Step)
	}
	// Storage keeps running on the bucket, with the configuration that names it.
	if e.set.backend != "s3" || e.set.useFiles != 0 || e.svc.log() != "stop,start" || !e.exists("objects") {
		t.Errorf("backend %q, UseFiles %d, service %s", e.set.backend, e.set.useFiles, e.svc.log())
	}
	if d := Describe(st, time.Now()); !strings.Contains(d, "--rollback") || !strings.Contains(d, "--resume") {
		t.Errorf("status:\n%s", d)
	}

	// The write Storage took on the bucket comes back with the rollback.
	e.rd.err = nil
	mustf(t, e.engine().Rollback(ctx, e.req()), "rollback")
	st = e.state()
	if st.Phase != PhaseRolledBack || st.Downloaded.Files != 1 || e.set.backend != "" || e.set.useFiles != 1 {
		t.Errorf("state: %+v, backend %q", st, e.set.backend)
	}
	f := filepath.Join(e.paths.StorageObjects(refA), "avatars", "written.bin", "v20")
	if b, err := os.ReadFile(f); err != nil || string(b) != "only in the bucket" {
		t.Errorf("the write on the bucket: %q, %v", b, err)
	}
}

func TestResumeAfterTheReadBackFailedFinishesTheSwitch(t *testing.T) {
	e := newEnv(t)
	e.populate()
	storageStartsOnTheBucket(e)
	if err := e.engine().Migrate(ctx, e.req()); err == nil {
		t.Fatal("Migrate worked")
	}
	e.rd.err = nil
	mustf(t, e.engine().Resume(ctx, e.req()), "resume")
	st := e.state()
	if st.Phase != PhaseDone || st.Retained == "" || e.set.useBucket != 1 || e.set.useFiles != 0 {
		t.Errorf("state %+v, settings %+v", st, e.set)
	}
	// Storage was running: it is not started again.
	if got := e.svc.log(); got != "stop,start" {
		t.Errorf("service calls: %s", got)
	}
}

func TestStorageThatIsDownAfterItWasOnTheBucketIsNotPutBackOnTheFiles(t *testing.T) {
	e := newEnv(t)
	e.populate()
	storageStartsOnTheBucket(e)
	if err := e.engine().Migrate(ctx, e.req()); err == nil {
		t.Fatal("Migrate worked")
	}
	e.rd.err = nil
	e.svc.health, e.svc.startErr = errors.New("down"), errors.New("does not start")
	err := e.engine().Resume(ctx, e.req())
	if err == nil || !strings.Contains(err.Error(), "does not start") || !strings.Contains(err.Error(), "--rollback") {
		t.Fatalf("Resume = %v", err)
	}
	if st := e.state(); st.Phase != PhaseFlipping || st.Step != stepLive || e.set.backend != "s3" || e.set.useFiles != 0 {
		t.Errorf("state %s/%s, backend %q, UseFiles %d", st.Phase, st.Step, e.set.backend, e.set.useFiles)
	}
	e.svc.health, e.svc.startErr = nil, nil
	mustf(t, e.engine().Resume(ctx, e.req()), "resume")
	if e.state().Phase != PhaseDone {
		t.Errorf("phase %s", e.state().Phase)
	}
}

func TestPreflightTriesTheSwitchBeforeAnythingIsCopied(t *testing.T) {
	t.Run("the change would not take effect", func(t *testing.T) {
		e := newEnv(t)
		e.populate()
		e.set.previewErr = errors.New("a file in config.d overrides config.toml")
		err := e.engine().Migrate(ctx, e.req())
		if err == nil || !strings.Contains(err.Error(), "would not take effect") || !strings.Contains(err.Error(), "overrides") {
			t.Fatalf("Migrate = %v", err)
		}
		if st, _ := ReadState(e.paths); st != nil || e.svc.log() != "" || len(e.bucket.keys()) != 0 {
			t.Errorf("something happened: state %v, service %q, keys %q", st, e.svc.log(), e.bucket.keys())
		}
	})
	t.Run("Storage could not start", func(t *testing.T) {
		e := newEnv(t)
		e.populate()
		e.svc.renderErr = errors.New("needs storage_s3_access_key_id")
		err := e.engine().Migrate(ctx, e.req())
		if err == nil || !strings.Contains(err.Error(), "could not start on the bucket") || !strings.Contains(err.Error(), "storage_s3_access_key_id") {
			t.Fatalf("Migrate = %v", err)
		}
		if st, _ := ReadState(e.paths); st != nil || e.svc.log() != "" {
			t.Errorf("something happened: state %v, service %q", st, e.svc.log())
		}
	})
	t.Run("it broke during the copy", func(t *testing.T) {
		e := newEnv(t)
		e.populate()
		var once sync.Once
		e.bucket.onPut = func(key string) {
			if !strings.HasPrefix(key, ".supavise") {
				once.Do(func() { e.svc.setRenderErr(errors.New("the key is gone")) })
			}
		}
		err := e.engine().Migrate(ctx, e.req())
		if err == nil || !strings.Contains(err.Error(), "the key is gone") {
			t.Fatalf("Migrate = %v", err)
		}
		// The check came before the stop, so Storage was never touched.
		if e.svc.log() != "" || hold.Held(e.paths, time.Now()) || !e.exists("objects") {
			t.Errorf("service %q", e.svc.log())
		}
		e.svc.setRenderErr(nil)
		mustf(t, e.engine().Resume(ctx, e.req()), "resume")
		if e.state().Phase != PhaseDone {
			t.Errorf("phase %s", e.state().Phase)
		}
	})
}

func TestPassesOfTheSwitchRunAtFullSpeed(t *testing.T) {
	e := newEnv(t)
	e.populate()
	var mu sync.Mutex
	var seen []string
	e.bucket.onPut = func(key string) {
		if strings.HasPrefix(key, ".supavise") {
			return
		}
		l := e.bucket.lim
		l.mu.Lock()
		limited := l.rate > 0
		l.mu.Unlock()
		mu.Lock()
		seen = append(seen, fmt.Sprintf("held=%v limited=%v", hold.Held(e.paths, time.Now()), limited))
		mu.Unlock()
	}
	// A write that lands when Storage stops is sent by the pass of the switch.
	e.svc.onStop = func() { e.put(refA, "avatars", "straggler.txt", "v8", []byte("one more"), "text/plain") }
	req := e.req()
	req.RateMiB = 1
	mustf(t, e.engine().Migrate(ctx, req), "migrate")
	count := map[string]int{}
	for _, s := range seen {
		count[s]++
	}
	if count["held=false limited=true"] != 5 || count["held=true limited=false"] != 1 || len(seen) != 6 {
		t.Errorf("puts: %v", count)
	}
	if rate := e.bucket.lim.rate; rate != 1<<20 {
		t.Errorf("the limit after the switch is %v bytes a second", rate)
	}
}

func TestRecordThatDoesNotFitTheNodeIsRefused(t *testing.T) {
	unfinished := func(t *testing.T) *env {
		e := newEnv(t)
		e.populate()
		e.tenants.failOn = func(string) error { return errors.New("boom") }
		if err := e.engine().Migrate(ctx, e.req()); err == nil {
			t.Fatal("Migrate worked")
		}
		e.tenants.failOn = nil
		return e
	}
	tamper := func(t *testing.T, e *env, f func(*State)) {
		st := e.state()
		f(st)
		mustf(t, saveState(e.paths, st, time.Now()), "save")
	}
	t.Run("a backend to restore that is not one", func(t *testing.T) {
		e := unfinished(t)
		tamper(t, e, func(st *State) { st.PrevBackend = "file'\n[ports]" })
		if err := e.engine().Resume(ctx, e.req()); err == nil || !strings.Contains(err.Error(), "never records") {
			t.Errorf("Resume = %v", err)
		}
	})
	t.Run("a directory outside the service directory", func(t *testing.T) {
		e := unfinished(t)
		for _, name := range []string{"../../etc", "objects", retainedPrefix + "x/../../y"} {
			tamper(t, e, func(st *State) { st.KeepAs = name })
			if err := e.engine().Resume(ctx, e.req()); err == nil || !strings.Contains(err.Error(), "not one this command keeps") {
				t.Errorf("%q: Resume = %v", name, err)
			}
		}
	})
	t.Run("another service than the objects went to", func(t *testing.T) {
		e := unfinished(t)
		e.cfg.Fleet.StorageS3Endpoint = "http://elsewhere.test"
		if err := e.engine().Resume(ctx, e.req()); err == nil || !strings.Contains(err.Error(), "another service") {
			t.Errorf("Resume = %v", err)
		}
	})
	t.Run("a finished run", func(t *testing.T) {
		e := newEnv(t)
		e.populate()
		mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
		e.cfg.Fleet.StorageS3ForcePathStyle = true
		if err := e.engine().Rollback(ctx, e.req()); err == nil || !strings.Contains(err.Error(), "another service") {
			t.Errorf("Rollback = %v", err)
		}
		e.cfg.Fleet.StorageS3ForcePathStyle = false
		tamper(t, e, func(st *State) { st.Retained = "../../etc" })
		if err := e.engine().Rollback(ctx, e.req()); err == nil || !strings.Contains(err.Error(), "not one this command keeps") {
			t.Errorf("Rollback = %v", err)
		}
	})
}

func TestOnlyTheDirectoryThisRunNamedIsTakenForItsFiles(t *testing.T) {
	e := newEnv(t) // Storage never stored an object
	other := filepath.Join(e.paths.System("storage"), retainedPrefix+"20000101", "stub", "keep-me")
	mustf(t, os.MkdirAll(filepath.Dir(other), 0o750), "mkdir")
	mustf(t, os.WriteFile(other, []byte("an operator's copy"), 0o640), "write")
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	st := e.state()
	if st.Phase != PhaseDone || st.Retained != "" || st.KeepAs == "" {
		t.Errorf("state: %+v", st)
	}
	if err := e.engine().Cleanup(ctx); err == nil {
		t.Error("Cleanup worked on a run that kept nothing")
	}
	if !fileExists(other) {
		t.Error("a directory this run did not name was deleted")
	}
	if !strings.Contains(e.out.String(), "nothing was read back") {
		t.Errorf("a store without rows did not say so:\n%s", e.out.String())
	}
}

func TestFileUnder(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "state", "objects", "stub", refA)
	for rel, ok := range map[string]bool{
		"docs/top.txt/v1": true, "docs/..hidden/v1": true, "docs/a b/ü.txt/v1": true, "docs/a\\b/v1": true,
		"": false, ".": false, "..": false, "../x": false, "docs/../../x": false, "docs/./x": false, "docs//x": false,
		"/etc/passwd": false, "docs/x/": false, "docs/\x00x/v1": false, "../" + refB + "/x": false,
	} {
		abs, got := fileUnder(root, rel)
		if got != ok || (got && !strings.HasPrefix(abs, root+string(filepath.Separator))) {
			t.Errorf("fileUnder(%q) = %q, %v; want %v", rel, abs, got, ok)
		}
	}
}

// treeOf lists the files below dir.
func treeOf(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return err
	})
	mustf(t, err, "walk %s", dir)
	return out
}

func TestRollbackRefusesKeysThatLeaveTheProject(t *testing.T) {
	e := newEnv(t)
	e.populate()
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	for _, k := range []string{
		refA + "/../../../../escape-marker",          // out of the state directory
		refA + "/avatars/../../" + refB + "/planted", // into another project's directory
		refA + "/./dot/v1", refA + "//empty/v1", refA + "/a/../../../y", refA + "/",
	} {
		e.bucket.objs[k] = memObj{data: []byte("planted"), mod: time.Now()}
	}
	mustf(t, e.engine().Rollback(ctx, e.req()), "rollback")
	for _, p := range treeOf(t, e.cfg.StateDir) {
		if b := filepath.Base(p); b == "escape-marker" || b == "planted" || b == "y" || b == "empty" || b == "dot" {
			t.Errorf("a hostile key was written: %s", p)
		}
	}
	st := e.state()
	if st.Phase != PhaseRolledBack || st.RefusedKeys != 6 || st.Downloaded.Files != 0 {
		t.Errorf("state: %+v", st)
	}
	if out := e.out.String(); !strings.Contains(out, "cannot be files") || !strings.Contains(out, "escape-marker") {
		t.Errorf("the rollback did not report the keys:\n%s", out)
	}
	if d := Describe(st, time.Now()); !strings.Contains(d, "refused") {
		t.Errorf("status:\n%s", d)
	}
	// They are still in the bucket.
	if _, ok := e.bucket.objs[refA+"/../../../../escape-marker"]; !ok {
		t.Error("a refused key was removed from the bucket")
	}
}

func TestRollbackDoesNotWriteThroughALink(t *testing.T) {
	e := newEnv(t)
	e.populate()
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	outside := t.TempDir()
	link := filepath.Join(e.paths.System("storage"), e.state().Retained, "stub", refA, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("no links here: %v", err)
	}
	e.bucket.objs[refA+"/linked/name/v1"] = memObj{data: []byte("planted"), mod: time.Now()}
	err := e.engine().Rollback(ctx, e.req())
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Rollback = %v", err)
	}
	if got := treeOf(t, outside); len(got) != 0 {
		t.Errorf("a file was written through the link: %v", got)
	}
}

func TestLinksAreReportedByTheirPathBelowTheProject(t *testing.T) {
	e := newEnv(t)
	e.populate()
	link := filepath.Join(e.paths.StorageObjects(refA), "avatars", "link")
	if err := os.Symlink("/etc/passwd", link); err != nil {
		t.Skipf("no links here: %v", err)
	}
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	st := e.state()
	if st.SkippedTotal != 1 || st.Skipped[0].Path != `"`+refA+`/avatars/link"` {
		t.Errorf("skipped: %+v", st.Skipped)
	}
}

// A run killed after it renamed the files and before it saved the new name has only KeepAs, which
// is written before the rename. A rollback finds the files there and puts them back, and does not
// fetch every object from the bucket into an empty directory and leave the files orphaned.
func TestRollbackFindsTheFilesByKeepAsWhenTheRenameWasNotRecorded(t *testing.T) {
	e := newEnv(t)
	e.populate()
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	st := e.state()
	kept := st.Retained
	if kept == "" || st.KeepAs != kept || !e.exists(kept) {
		t.Fatalf("state: %+v", st)
	}
	st.Phase, st.Step, st.Retained, st.RetainUntil = PhaseFlipping, stepStarted, "", time.Time{}
	mustf(t, saveState(e.paths, st, time.Now()), "save")

	mustf(t, e.engine().Rollback(ctx, e.req()), "rollback")
	after := e.state()
	if after.Phase != PhaseRolledBack || after.Downloaded.Files != 0 {
		t.Fatalf("the rollback fetched %d objects although the files were there: %+v", after.Downloaded.Files, after)
	}
	if e.exists(kept) {
		t.Errorf("%s is still there: the files were not moved back", kept)
	}
	if len(treeOf(t, e.paths.StorageObjects(refA))) != 3 || len(treeOf(t, e.paths.StorageObjects(refB))) != 2 {
		t.Errorf("files after the rollback: %v", treeOf(t, filepath.Join(e.paths.System("storage"), "objects")))
	}
}

// A row without a version has no key the migration can look for. The verification does not count it
// against the project and does not stop at it, its file is copied by path like every other, and the
// run says how many rows it could not match.
func TestRowsWithoutAVersionAreCountedNotVerified(t *testing.T) {
	e := newEnv(t)
	e.populate()
	e.write(refA, "avatars/legacy.txt", []byte("an object that predates versions"), "text/plain")
	e.tenants.mu.Lock()
	e.tenants.rows[refA] = append(e.tenants.rows[refA], Row{Bucket: "avatars", Name: "legacy.txt", Version: "", Size: 32, HasSize: true})
	e.tenants.mu.Unlock()
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	st := e.state()
	if st.Phase != PhaseDone {
		t.Fatalf("phase %s: %s", st.Phase, st.Error)
	}
	var tn Tenant
	for _, x := range st.Tenants {
		if x.Ref == refA {
			tn = x
		}
	}
	if tn.Unversioned != 1 || tn.Orphans != 0 || tn.Rows != 3 || tn.Files != 4 {
		t.Fatalf("tenant: %+v", tn)
	}
	if _, ok := e.bucket.objs[refA+"/avatars/legacy.txt"]; !ok {
		t.Errorf("the file of the row without a version was not copied: %v", e.keyList())
	}
	if out := e.out.String(); !strings.Contains(out, "1 rows have no version") {
		t.Errorf("the run did not say so:\n%s", out)
	}
	if d := Describe(st, time.Now()); !strings.Contains(d, "no version") {
		t.Errorf("status:\n%s", d)
	}
}

// A rollback that stopped after it had copied everything back and switched the configuration only has
// Storage to start. It asks for no credentials and does not open the bucket, so a credentials file that
// was deleted since the first run is not in the way.
func TestResumeOfARollbackPastTheCopyNeedsNoCredentials(t *testing.T) {
	e := newEnv(t)
	e.populate()
	mustf(t, e.engine().Migrate(ctx, e.req()), "migrate")
	e.svc.mu.Lock()
	e.svc.startErr = errors.New("systemd is busy")
	e.svc.mu.Unlock()
	if err := e.engine().Rollback(ctx, e.req()); err == nil {
		t.Fatal("Rollback worked although Storage did not start")
	}
	st := e.state()
	if st.Phase != PhaseRollingBack || st.Step != stepSwitched || st.NeedsBucket() {
		t.Fatalf("state: %+v (needs the bucket: %v)", st, st.NeedsBucket())
	}
	e.svc.mu.Lock()
	e.svc.startErr = nil
	e.svc.mu.Unlock()
	opens := e.opens
	mustf(t, e.engine().Resume(ctx, Request{RateMiB: -1}), "resume with no credentials")
	if e.opens != opens {
		t.Errorf("the bucket was opened %d more times", e.opens-opens)
	}
	if after := e.state(); after.Phase != PhaseRolledBack {
		t.Fatalf("phase %s", after.Phase)
	}

	// Before the copy back is done the bucket is needed.
	for _, s := range []*State{
		{Phase: PhaseRollingBack, Step: stepHold}, {Phase: PhaseRollingBack, Step: stepFinal}, {Phase: PhaseRollingBack, Step: stepFence},
		{Phase: PhaseRollingBack}, {Phase: PhaseCopying}, {Phase: PhaseFlipping, Step: stepSwitched}, {Phase: PhaseDone},
	} {
		if !s.NeedsBucket() {
			t.Errorf("%s/%s does not need the bucket", s.Phase, s.Step)
		}
	}
	if (&State{Phase: PhaseRollingBack, Step: stepStarted}).NeedsBucket() {
		t.Error("a rollback past the switch needs the bucket")
	}
}

// When Storage did not start on the bucket and the configuration cannot be put back on the files,
// starting Storage would bring it up on the bucket. It stays stopped, the run stays in the switch, and
// --resume finishes it once the cause is gone.
func TestStorageStaysStoppedWhenTheConfigurationCannotBePutBack(t *testing.T) {
	e := newEnv(t)
	e.populate()
	boom := errors.New("boom")
	e.svc.onStart = func() {
		if e.set.backend == "s3" {
			e.svc.startErr = boom
		}
	}
	e.set.filesErr = errors.New("config.toml is read-only")
	err := e.engine().Migrate(ctx, e.req())
	if err == nil || !strings.Contains(err.Error(), "left stopped") || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("Migrate = %v", err)
	}
	if got := e.svc.log(); got != "stop,start" { // the one start that failed on the bucket; none after it
		t.Fatalf("service calls: %s", got)
	}
	st := e.state()
	if st.Phase != PhaseFlipping || st.Step != stepSwitched && st.Step != stepFence {
		t.Fatalf("state: %s/%s", st.Phase, st.Step)
	}
	e.svc.onStart, e.svc.startErr, e.set.filesErr = nil, nil, nil
	mustf(t, e.engine().Resume(ctx, e.req()), "resume")
	if after := e.state(); after.Phase != PhaseDone || e.set.backend != "s3" {
		t.Fatalf("after the resume: %+v, backend %s", after, e.set.backend)
	}
}

// With a role, a daemon that started before the role was in the configuration serves Storage no
// credentials; the first read through Storage fails, and the error says what to do. With a key it
// says nothing about it.
func TestAFailedFirstReadWithARoleSaysToRestartTheDaemon(t *testing.T) {
	e := newEnv(t)
	e.populate()
	e.rd.err = errors.New("HTTP 500: could not load credentials")
	req := e.req()
	req.Credentials = Credentials{Source: CredRole, RoleARN: "arn:aws:iam::1:role/storage"}
	err := e.engine().Migrate(ctx, req)
	if err == nil || !strings.Contains(err.Error(), "systemctl restart supavise.service") || !strings.Contains(err.Error(), "--resume") {
		t.Fatalf("Migrate with a role = %v", err)
	}
	e = newEnv(t)
	e.populate()
	e.rd.err = errors.New("HTTP 500")
	if err := e.engine().Migrate(ctx, e.req()); err == nil || strings.Contains(err.Error(), "systemctl restart") {
		t.Fatalf("Migrate with a key = %v", err)
	}
}
