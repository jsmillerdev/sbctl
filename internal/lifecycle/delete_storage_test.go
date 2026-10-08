package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/supavise/supavise/internal/registry"
)

// objectsBackup is a BaseBackuper that records whether the project's Storage objects were still
// on disk when the final backup ran, which is when the backup copies them.
type objectsBackup struct {
	dir     func(ref string) string
	err     error
	present []bool
}

func (b *objectsBackup) BaseBackup(_ context.Context, ref string) (*registry.Backup, error) {
	_, err := os.Stat(b.dir(ref))
	b.present = append(b.present, err == nil)
	return &registry.Backup{Ref: ref}, b.err
}

// storageObject writes one object of ref the way Storage's file backend does.
func storageObject(t *testing.T, h *harness, ref string) string {
	t.Helper()
	f := filepath.Join(h.cfg.Paths().StorageObjects(ref), "avatars", "me.png", "v1")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestDeleteRemovesTheStorageObjectsAfterTheFinalBackup(t *testing.T) {
	ctx := context.Background()
	newCase := func(t *testing.T) (*harness, *objectsBackup, *registry.Project, string, string) {
		h := newHarness(t)
		h.cfg.StateDir = t.TempDir()
		ob := &objectsBackup{dir: h.cfg.Paths().StorageObjects}
		h.e.opts.Backup = ob
		p, other := h.create(t), h.create(t)
		return h, ob, p, storageObject(t, h, p.Ref), storageObject(t, h, other.Ref)
	}

	t.Run("a delete removes them once the backup has seen them, and only its own", func(t *testing.T) {
		h, ob, p, obj, otherObj := newCase(t)
		if err := h.e.Delete(ctx, p.Ref); err != nil {
			t.Fatal(err)
		}
		if len(ob.present) != 1 || !ob.present[0] {
			t.Fatalf("the objects were gone when the final backup ran: %v", ob.present)
		}
		if exists(obj) || exists(h.cfg.Paths().StorageObjects(p.Ref)) {
			t.Fatal("the deleted project's Storage objects are still on disk")
		}
		if !exists(otherObj) {
			t.Fatal("another project's Storage objects were removed")
		}
	})

	t.Run("a failed final backup keeps them with the project", func(t *testing.T) {
		h, ob, p, obj, _ := newCase(t)
		ob.err = errors.New("s3 down")
		if err := h.e.Delete(ctx, p.Ref); err == nil {
			t.Fatal("expected the backup failure")
		}
		if !exists(obj) {
			t.Fatal("the Storage objects were removed although the final backup failed")
		}
		if _, err := h.reg.GetProject(ctx, p.Ref); err != nil {
			t.Fatalf("the project did not stay: %v", err)
		}
	})

	t.Run("a delete that stops at the data plane keeps them, and the retry removes them", func(t *testing.T) {
		h, _, p, obj, _ := newCase(t)
		h.plane.failOn["Delete"] = errors.New("units would not go")
		if err := h.e.Delete(ctx, p.Ref); err == nil {
			t.Fatal("expected the data plane failure")
		}
		if !exists(obj) {
			t.Fatal("the Storage objects were removed before the project's data")
		}
		delete(h.plane.failOn, "Delete")
		if err := h.e.Delete(ctx, p.Ref); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if exists(obj) {
			t.Fatal("the retry left the Storage objects")
		}
	})

	t.Run("a delete without a final backup removes them too", func(t *testing.T) {
		h, ob, p, obj, _ := newCase(t)
		if err := h.e.DeleteWith(ctx, p.Ref, DeleteOptions{SkipFinalBackup: true}); err != nil {
			t.Fatal(err)
		}
		if len(ob.present) != 0 || exists(obj) {
			t.Fatalf("backups = %v, objects present = %v", ob.present, exists(obj))
		}
	})

	t.Run("a delete that keeps the record leaves them for the restore", func(t *testing.T) {
		h, _, p, obj, _ := newCase(t)
		if err := h.e.DeleteWith(ctx, p.Ref, DeleteOptions{KeepRecord: true}); err != nil {
			t.Fatal(err)
		}
		if !exists(obj) {
			t.Fatal("a KeepRecord delete removed the Storage objects")
		}
	})

	t.Run("a project that never stored an object deletes as before", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.StateDir = t.TempDir()
		p := h.create(t)
		if err := h.e.Delete(ctx, p.Ref); err != nil {
			t.Fatal(err)
		}
	})
}
