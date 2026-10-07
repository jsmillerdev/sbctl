package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/fleet"
	"github.com/jsmillerdev/supavise/internal/registry"
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
