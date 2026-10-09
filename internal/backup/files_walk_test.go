package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/storageattr"
)

func walkAll(t *testing.T, root string, o WalkStorageOptions) []StorageObject {
	t.Helper()
	var got []StorageObject
	if err := WalkStorage(context.Background(), root, o, func(so StorageObject) error { got = append(got, so); return nil }); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestWalkStorageReportsObjectsInPathOrder(t *testing.T) {
	e := newTestEnv(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	e.putObject(t, testRef, "avatars/me.png/v2", []byte("two"), base)
	typed := e.putObject(t, testRef, "avatars/me.png/v1", []byte("one"), base.Add(time.Hour))
	e.putObject(t, testRef, "docs/Résumé.pdf/v1", []byte("pdf!"), base.Add(2*time.Hour))
	hasAttrs := setContentType(t, typed, "image/png")
	if ok, err := writeStorageAttrs(typed, map[string][]byte{storageattr.Prefixes[0] + "cache-control": []byte("max-age=3600")}); err != nil || ok != hasAttrs {
		t.Fatalf("cache-control attribute: %v, %v", ok, err)
	}

	got := walkAll(t, e.objectsDir(testRef), WalkStorageOptions{})
	var paths []string
	for _, o := range got {
		paths = append(paths, o.Path)
	}
	if want := []string{"avatars/me.png/v1", "avatars/me.png/v2", "docs/Résumé.pdf/v1"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %q, want %q", paths, want)
	}
	o := got[0]
	if o.Size != 3 || !o.ModTime.Equal(base.Add(time.Hour)) || o.Abs != filepath.Join(e.objectsDir(testRef), "avatars", "me.png", "v1") {
		t.Errorf("object = %+v", o)
	}
	if hasAttrs {
		if v, ok := o.Attr("content-type"); !ok || v != "image/png" {
			t.Errorf("content-type = %q, %v; Attrs %v", v, ok, o.Attrs)
		}
		if v, ok := o.Attr("cache-control"); runtime.GOOS == "linux" && (!ok || v != "max-age=3600") {
			t.Errorf("cache-control = %q, %v", v, ok)
		}
	}
	if v, ok := got[1].Attr("content-type"); ok || v != "" || got[1].Attrs != nil {
		t.Errorf("object without attributes: %q, %v, %v", v, ok, got[1].Attrs)
	}
	f, err := o.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if b, _ := io.ReadAll(f); string(b) != "one" {
		t.Errorf("content = %q", b)
	}
}

func TestWalkStorageSinceLeavesOutOlderObjects(t *testing.T) {
	e := newTestEnv(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	e.putObject(t, testRef, "b/old/v1", []byte("x"), base)
	e.putObject(t, testRef, "b/edge/v1", []byte("x"), base.Add(time.Hour))
	e.putObject(t, testRef, "b/new/v1", []byte("x"), base.Add(2*time.Hour))
	got := walkAll(t, e.objectsDir(testRef), WalkStorageOptions{Since: base.Add(time.Hour)})
	if len(got) != 1 || got[0].Path != "b/new/v1" {
		t.Fatalf("objects newer than the pass start = %+v", got)
	}
}

func TestWalkStorageSkipsWhatIsNotAFile(t *testing.T) {
	e := newTestEnv(t)
	root := e.objectsDir(testRef)
	e.putObject(t, testRef, "b/real/v1", []byte("x"), time.Now())
	outside := writeFile(t, filepath.Join(t.TempDir(), "secret"), []byte("not an object"))
	if err := os.Symlink(outside, filepath.Join(root, "b", "link")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "b", "dirlink")); err != nil {
		t.Fatal(err)
	}
	var skipped []string
	got := walkAll(t, root, WalkStorageOptions{Skipped: func(p string) { skipped = append(skipped, filepath.Base(p)) }})
	if len(got) != 1 || got[0].Path != "b/real/v1" {
		t.Fatalf("objects = %+v; a link must never be followed", got)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %v; want both links reported", skipped)
	}
	// A link that is handed to Open anyway is refused.
	if f, err := (StorageObject{Abs: filepath.Join(root, "b", "link")}).Open(); err == nil {
		f.Close()
		t.Fatal("Open followed a symbolic link")
	}
	// Without a Skipped hook the walk is the same.
	if got := walkAll(t, root, WalkStorageOptions{}); len(got) != 1 {
		t.Fatalf("objects = %+v", got)
	}
}

func TestWalkStorageRootsAndErrors(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	// A project that never stored an object has no directory.
	if got := walkAll(t, e.objectsDir(testRef), WalkStorageOptions{}); len(got) != 0 {
		t.Fatalf("objects of a project without a directory = %+v", got)
	}
	file := writeFile(t, filepath.Join(t.TempDir(), "x"), []byte("x"))
	if err := WalkStorage(ctx, file, WalkStorageOptions{}, func(StorageObject) error { return nil }); err == nil {
		t.Error("a root that is a file was walked")
	}
	// fn's error stops the walk and comes back as it is.
	e.putObject(t, testRef, "b/a/v1", []byte("x"), time.Now())
	e.putObject(t, testRef, "b/b/v1", []byte("x"), time.Now())
	stop := errors.New("stop here")
	n := 0
	if err := WalkStorage(ctx, e.objectsDir(testRef), WalkStorageOptions{}, func(StorageObject) error { n++; return stop }); !errors.Is(err, stop) || n != 1 {
		t.Fatalf("walk with a failing fn = %v after %d calls", err, n)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := WalkStorage(cctx, e.objectsDir(testRef), WalkStorageOptions{}, func(StorageObject) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("walk with a cancelled context = %v", err)
	}
}

// The nightly copy walks the same tree through the same walker.
func TestBackupStorageUsesTheSharedWalker(t *testing.T) {
	e := newTestEnv(t)
	root := e.objectsDir(testRef)
	e.putObject(t, testRef, "b/real/v1", []byte("x"), time.Now().Add(-time.Hour))
	outside := writeFile(t, filepath.Join(t.TempDir(), "secret"), []byte("not an object"))
	if err := os.Symlink(outside, filepath.Join(root, "b", "link")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	snap, err := e.svc.backupStorage(context.Background(), testRef, ReasonManual)
	if err != nil || snap == nil || snap.Files != 1 {
		t.Fatalf("snapshot = %+v, %v; the link must be left out", snap, err)
	}
}
