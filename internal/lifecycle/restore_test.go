package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/fleet"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// fakeRestorer stands in for backup.Service: like the real one it pauses the project and
// resumes it through the Manager it was given, and it records the status it saw each time.
type fakeRestorer struct {
	fakeBackup
	h        *harness
	err      error
	seen     []registry.Status
	reqs     []RestoreRequest
	duringFn func(ctx context.Context)
}

func (f *fakeRestorer) RestoreInPlace(ctx context.Context, ref string, req RestoreRequest) error {
	f.reqs = append(f.reqs, req)
	look := func() {
		p, _ := f.h.reg.GetProject(context.Background(), ref)
		f.seen = append(f.seen, p.Status)
	}
	if err := f.h.e.Pause(ctx, ref); err != nil {
		return err
	}
	look()
	if f.duringFn != nil {
		f.duringFn(ctx)
	}
	if err := f.h.e.Resume(ctx, ref); err != nil {
		return err
	}
	look()
	return f.err
}

func newRestoreHarness(t *testing.T) (*harness, *fakeRestorer) {
	t.Helper()
	h := newHarness(t)
	fr := &fakeRestorer{h: h}
	h.e = NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, h.plane, Options{Fleet: fleet.Fleet{h.tenant}, Backup: fr})
	return h, fr
}

func status(t *testing.T, h *harness, ref string) registry.Status {
	t.Helper()
	p, err := h.reg.GetProject(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return p.Status
}

func TestRestoreHoldsRestoringThroughPauseAndResume(t *testing.T) {
	h, fr := newRestoreHarness(t)
	ctx := context.Background()
	p := h.create(t)

	r, err := h.e.BeginRestore(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if got := status(t, h, p.Ref); got != registry.StatusRestoring {
		t.Fatalf("after BeginRestore status = %s", got)
	}
	target := time.Date(2026, 10, 6, 14, 30, 0, 0, time.UTC)
	if err := r.Run(ctx, RestoreRequest{Target: target}); err != nil {
		t.Fatal(err)
	}
	for i, s := range fr.seen {
		if s != registry.StatusRestoring {
			t.Errorf("status seen inside the restore, step %d = %s, want RESTORING", i, s)
		}
	}
	if len(fr.seen) != 2 || len(fr.reqs) != 1 || !fr.reqs[0].Target.Equal(target) {
		t.Fatalf("seen %v, requests %+v", fr.seen, fr.reqs)
	}
	if got := status(t, h, p.Ref); got != registry.StatusActiveHealthy {
		t.Fatalf("after the restore status = %s", got)
	}
	if !h.plane.has("Stop "+p.Ref) || h.plane.calls[len(h.plane.calls)-1] != "Start "+p.Ref {
		t.Fatalf("the restore did not stop and start the units: %v", h.plane.calls)
	}
	// The project takes the next operation as usual.
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatalf("pause after a restore: %v", err)
	}
}

func TestRestoreRefusesOverlappingOperations(t *testing.T) {
	h, fr := newRestoreHarness(t)
	ctx := context.Background()
	p := h.create(t)
	r, err := h.e.BeginRestore(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	// Between BeginRestore and Run, and while Run is working, nothing else may start.
	check := func(when string) {
		t.Helper()
		if _, err := h.e.BeginRestore(ctx, p.Ref); !errors.Is(err, ErrInvalidState) {
			t.Errorf("%s: a second restore: %v", when, err)
		}
		if err := h.e.Pause(ctx, p.Ref); !errors.Is(err, ErrInvalidState) {
			t.Errorf("%s: pause: %v", when, err)
		}
		if err := h.e.Resume(ctx, p.Ref); !errors.Is(err, ErrInvalidState) {
			t.Errorf("%s: resume: %v", when, err)
		}
		if err := h.e.Delete(ctx, p.Ref); !errors.Is(err, ErrInvalidState) {
			t.Errorf("%s: delete: %v", when, err)
		}
		if got := status(t, h, p.Ref); got != registry.StatusRestoring {
			t.Errorf("%s: status = %s", when, got)
		}
	}
	check("before Run")
	fr.duringFn = func(context.Context) { check("during Run") }
	if err := r.Run(ctx, RestoreRequest{BackupID: "20261006T030000Z-3f9a1c"}); err != nil {
		t.Fatal(err)
	}
	if fr.reqs[0].BackupID != "20261006T030000Z-3f9a1c" {
		t.Fatalf("request = %+v", fr.reqs[0])
	}
	// A delete is refused only while a restore runs in this process.
	if err := h.e.Delete(ctx, p.Ref); err != nil {
		t.Fatalf("delete after the restore: %v", err)
	}
}

func TestRestoreFailureSettlesTheStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		healthy bool
		want    registry.Status
	}{
		{"original back and healthy", true, registry.StatusActiveHealthy},
		{"nothing runs", false, registry.StatusActiveUnhealthy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fr := newRestoreHarness(t)
			ctx := context.Background()
			p := h.create(t)
			h.plane.healthy = tc.healthy
			fr.err = errors.New("recovery ended before configured recovery target was reached")
			r, err := h.e.BeginRestore(ctx, p.Ref)
			if err != nil {
				t.Fatal(err)
			}
			err = r.Run(ctx, RestoreRequest{Target: time.Now()})
			if err == nil || !errors.Is(err, fr.err) {
				t.Fatalf("Run = %v, want the restorer's error", err)
			}
			if got := status(t, h, p.Ref); got != tc.want {
				t.Fatalf("status = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestBeginRestoreRefusals(t *testing.T) {
	ctx := context.Background()
	h, _ := newRestoreHarness(t)
	p := h.create(t)
	if err := h.e.Pause(ctx, p.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.BeginRestore(ctx, p.Ref); !errors.Is(err, ErrInvalidState) {
		t.Errorf("restore of a paused project: %v", err)
	}
	if _, err := h.e.BeginRestore(ctx, "system"); !errors.Is(err, ErrInvalidState) {
		t.Errorf("restore of the system project: %v", err)
	}
	if _, err := h.e.BeginRestore(ctx, "nosuchproject"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("restore of a missing project: %v", err)
	}

	// A backup service that cannot restore (the fake without the capability) is refused
	// before the project moves.
	h2 := newHarness(t)
	p2 := h2.create(t)
	if _, err := h2.e.BeginRestore(ctx, p2.Ref); !errors.Is(err, ErrNoRestorer) {
		t.Errorf("no restorer: %v", err)
	}
	if got := status(t, h2, p2.Ref); got != registry.StatusActiveHealthy {
		t.Errorf("status after a refused restore = %s", got)
	}
}

func TestLateBackuperReportsAMissingBackend(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	lb := &lateBackuper{node: &Node{}, factory: func(*Node) (BaseBackuper, error) { return nil, errors.New("backup.backend is not set") }}
	h.e = NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, h.plane, Options{Backup: lb})
	p := h.create(t)
	_, err := h.e.BeginRestore(ctx, p.Ref)
	if !errors.Is(err, ErrNoRestorer) {
		t.Fatalf("BeginRestore = %v, want ErrNoRestorer", err)
	}
	if got := status(t, h, p.Ref); got != registry.StatusActiveHealthy {
		t.Fatalf("status = %s", got)
	}

	// With a working service the call reaches it and the Engine is handed over as Manager.
	fr := &fakeRestorer{h: h}
	lb = &lateBackuper{node: &Node{}, factory: func(*Node) (BaseBackuper, error) { return fr, nil }}
	h.e = NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, h.plane, Options{Backup: lb})
	r, err := h.e.BeginRestore(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx, RestoreRequest{Target: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if len(fr.reqs) != 1 {
		t.Fatalf("the late backuper did not pass the request on: %+v", fr.reqs)
	}
}

func TestInterruptedRestoreCanBeDeleted(t *testing.T) {
	ctx := context.Background()
	h, _ := newRestoreHarness(t)
	p := h.create(t)
	// The daemon died during a restore: RESTORING in the registry, nothing running here.
	if err := h.reg.SetProjectStatus(ctx, p.Ref, registry.StatusRestoring); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Delete(ctx, p.Ref); err != nil {
		t.Fatalf("delete of a project left RESTORING: %v", err)
	}
}

// pwPlane is a data plane that can set the service roles' passwords, as PostgresPlane can.
type pwPlane struct {
	*fakePlane
	set []string
	err error
}

func (p *pwPlane) SetRolePasswords(_ context.Context, _ *registry.Project, keys *secrets.ProjectKeys) error {
	p.set = append(p.set, keys.DBPassword)
	return p.err
}

func eventKinds(t *testing.T, h *harness, ref string) []string {
	t.Helper()
	evs, err := h.reg.ListEvents(context.Background(), ref, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}
	return out
}

func TestRestoreReappliesTheCurrentRolePasswords(t *testing.T) {
	ctx := context.Background()
	h, fr := newRestoreHarness(t)
	pl := &pwPlane{fakePlane: h.plane}
	h.e = NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, pl, Options{Fleet: fleet.Fleet{h.tenant}, Backup: fr})
	p := h.create(t)
	keys, err := h.e.Keys(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}

	run := func() error {
		r, err := h.e.BeginRestore(ctx, p.Ref)
		if err != nil {
			t.Fatal(err)
		}
		return r.Run(ctx, RestoreRequest{Target: time.Now()})
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if len(pl.set) != 1 || pl.set[0] != keys.DBPassword {
		t.Fatalf("role passwords set after the restore: %v, want the registry's", pl.set)
	}

	// A restore that failed put the original back: its passwords are the registry's already.
	fr.err = errors.New("recovery ended before configured recovery target was reached")
	if err := run(); err == nil {
		t.Fatal("the failed restore reported success")
	}
	if len(pl.set) != 1 {
		t.Errorf("a failed restore set role passwords: %v", pl.set)
	}

	// A cluster that refuses the passwords does not undo a restore that worked; the event says so.
	fr.err = nil
	pl.err = errors.New("cluster is read-only")
	if err := run(); err != nil {
		t.Fatalf("restore with failing role passwords: %v", err)
	}
	if got := status(t, h, p.Ref); got != registry.StatusActiveHealthy {
		t.Errorf("status = %s", got)
	}
	var seen bool
	for _, k := range eventKinds(t, h, p.Ref) {
		seen = seen || k == EventRestorePasswordsFailed
	}
	if !seen {
		t.Error("no restore.passwords_failed event")
	}
}

// diskHarness is a restore harness whose state directory is a temp directory with a project
// data directory of size bytes, and a disk reporting free bytes.
func diskHarness(t *testing.T, size, free int64) (*harness, *fakeRestorer, *registry.Project) {
	t.Helper()
	h, fr := newRestoreHarness(t)
	h.cfg.StateDir = t.TempDir()
	p := h.create(t)
	data := h.cfg.Paths().PostgresData(p.Ref)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "base"), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	h.e.freeBytes = func(string) int64 { return free }
	return h, fr, p
}

func TestBeginRestoreRefusesWhenTheDiskCannotHoldTheCopy(t *testing.T) {
	ctx := context.Background()
	const size = 4 << 20
	need := restoreNeed(size)

	h, fr, p := diskHarness(t, size, need-1)
	_, err := h.e.BeginRestore(ctx, p.Ref)
	if !errors.Is(err, ErrInsufficientDisk) {
		t.Fatalf("BeginRestore with %d free, %d needed: %v", need-1, need, err)
	}
	if got := status(t, h, p.Ref); got != registry.StatusActiveHealthy {
		t.Errorf("a refused restore left the status %s", got)
	}
	if len(fr.reqs) != 0 {
		t.Errorf("a refused restore ran: %+v", fr.reqs)
	}
	// Enough room, and an unreadable disk, both go ahead; a status refusal still comes first.
	h.e.freeBytes = func(string) int64 { return need }
	r, err := h.e.BeginRestore(ctx, p.Ref)
	if err != nil {
		t.Fatalf("BeginRestore with enough room: %v", err)
	}
	if err := r.Run(ctx, RestoreRequest{Target: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h.e.freeBytes = func(string) int64 { return -1 }
	if _, err := h.e.BeginRestore(ctx, p.Ref); err != nil {
		t.Fatalf("BeginRestore with an unreadable disk: %v", err)
	}
	h.e.freeBytes = func(string) int64 { return 0 }
	if _, err := h.e.BeginRestore(ctx, p.Ref); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("restore of a project already RESTORING on a full disk: %v", err)
	}
}

func TestRestoreKeepsOnlyTheNewestOldDataDirectory(t *testing.T) {
	ctx := context.Background()
	h, fr, p := diskHarness(t, 1024, 1<<40)
	data := h.cfg.Paths().PostgresData(p.Ref)
	mk := func(suffix string) string {
		d := data + suffix
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		return d
	}
	oldPre1 := mk(".pre-restore-20261001T030000Z")
	oldPre2 := mk(".pre-restore-20261002T030000Z")
	oldFailed := mk(".failed-restore-20261002T040000Z")
	// The restore moves the data aside as the backup service does.
	aside := data + ".pre-restore-20261003T030000Z"
	fr.duringFn = func(context.Context) {
		if err := os.MkdirAll(aside, 0o700); err != nil {
			t.Error(err)
		}
	}
	exists := func(d string) bool { _, err := os.Stat(d); return err == nil }
	run := func() error {
		r, err := h.e.BeginRestore(ctx, p.Ref)
		if err != nil {
			t.Fatal(err)
		}
		return r.Run(ctx, RestoreRequest{Target: time.Now()})
	}

	// A failed restore keeps the newest failed directory and every pre-restore one: a failed
	// rollback leaves the original data in one of them.
	failedNew := mk(".failed-restore-20261003T040000Z")
	fr.err = errors.New("recovery failed")
	if err := run(); err == nil {
		t.Fatal("the failed restore reported success")
	}
	if exists(oldFailed) || !exists(failedNew) || !exists(oldPre1) || !exists(oldPre2) || !exists(aside) {
		t.Errorf("after a failure: older failed %v, newest failed %v, pre-restore %v %v %v", exists(oldFailed), exists(failedNew), exists(oldPre1), exists(oldPre2), exists(aside))
	}

	// A restore that worked keeps the directory it just set aside and nothing older.
	fr.err = nil
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if !exists(aside) || exists(oldPre1) || exists(oldPre2) || exists(failedNew) {
		t.Errorf("after a restore: newest pre-restore %v, older %v %v, failed %v", exists(aside), exists(oldPre1), exists(oldPre2), exists(failedNew))
	}
	if !exists(data) {
		t.Error("the data directory itself was removed")
	}
}
