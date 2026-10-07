package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// storeContract is the behavior every Store must have. TestFileStore runs it
// locally; the S3 tests (gated on SUPAVISE_TEST_S3_*) run the same function.
func storeContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()

	if _, err := st.Get(ctx, "a/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
	if _, err := st.Stat(ctx, "a/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat missing = %v, want ErrNotFound", err)
	}
	if err := st.Delete(ctx, "a/missing"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}

	for key, body := range map[string]string{
		"p1/wal/one.zst": "1", "p1/wal/two.zst": "22", "p1/base/b1/data": "333", "p1/base/b1/backup.json": "{}",
		"p1/base/b2/backup.json": "{}", "p2/wal/x.zst": "x",
	} {
		if err := st.Put(ctx, key, strings.NewReader(body)); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}
	if b := readAll(t, st, "p1/wal/two.zst"); string(b) != "22" {
		t.Fatalf("Get = %q", b)
	}
	if info, err := st.Stat(ctx, "p1/base/b1/data"); err != nil || info.Size != 3 || info.Key != "p1/base/b1/data" {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	// Replace.
	if err := st.Put(ctx, "p1/wal/one.zst", strings.NewReader("replaced")); err != nil {
		t.Fatal(err)
	}
	if b := readAll(t, st, "p1/wal/one.zst"); string(b) != "replaced" {
		t.Fatalf("after replace Get = %q", b)
	}

	objs, err := st.List(ctx, "p1/")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	want := []string{"p1/base/b1/backup.json", "p1/base/b1/data", "p1/base/b2/backup.json", "p1/wal/one.zst", "p1/wal/two.zst"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("List(p1/) = %v, want %v", keys, want)
	}
	if objs, _ := st.List(ctx, "p1/wal/t"); len(objs) != 1 || objs[0].Key != "p1/wal/two.zst" {
		t.Fatalf("List(p1/wal/t) = %v", objs)
	}
	dirs, err := st.ListDirs(ctx, "p1/base/")
	if err != nil || !reflect.DeepEqual(dirs, []string{"b1", "b2"}) {
		t.Fatalf("ListDirs = %v, %v", dirs, err)
	}
	if dirs, _ := st.ListDirs(ctx, ""); !reflect.DeepEqual(dirs, []string{"p1", "p2"}) {
		t.Fatalf("ListDirs(root) = %v", dirs)
	}
	if dirs, err := st.ListDirs(ctx, "nope/"); err != nil || len(dirs) != 0 {
		t.Fatalf("ListDirs(nope/) = %v, %v", dirs, err)
	}

	if err := st.Delete(ctx, "p1/base/b1/backup.json", "p1/base/b1/data", "p1/base/b2/backup.json", "p1/wal/one.zst"); err != nil {
		t.Fatal(err)
	}
	if dirs, _ := st.ListDirs(ctx, "p1/base/"); len(dirs) != 0 {
		t.Fatalf("empty directories still listed after Delete: %v", dirs)
	}
	if _, err := st.Stat(ctx, "p1/wal/one.zst"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted key still there: %v", err)
	}

	// A reader that fails mid-stream must leave nothing behind and keep any old object.
	boom := errors.New("boom")
	err = st.Put(ctx, "p1/wal/two.zst", io.MultiReader(strings.NewReader("partial"), errReader{boom}))
	if !errors.Is(err, boom) {
		t.Fatalf("Put with failing reader = %v, want boom", err)
	}
	if b := readAll(t, st, "p1/wal/two.zst"); string(b) != "22" {
		t.Fatalf("failed Put damaged the old object: %q", b)
	}
	err = st.Put(ctx, "p1/wal/new.zst", io.MultiReader(strings.NewReader("partial"), errReader{boom}))
	if err == nil {
		t.Fatal("Put with failing reader succeeded")
	}
	if _, err := st.Stat(ctx, "p1/wal/new.zst"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed Put left an object: %v", err)
	}

	// Keys that could escape the root are refused.
	for _, bad := range []string{"", "/abs", "../x", "a/../b", "a//b", "a\\b"} {
		if err := st.Put(ctx, bad, strings.NewReader("x")); err == nil {
			t.Errorf("Put(%q) succeeded", bad)
		}
	}
	if u := st.URL("p1/wal/two.zst"); !strings.HasSuffix(u, "p1/wal/two.zst") {
		t.Errorf("URL = %q", u)
	}

	// Larger than one S3 part (tests lower PartSize) and byte-exact.
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<14*5) // 4 MiB
	if err := st.Put(ctx, "p2/big", bytes.NewReader(big)); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, st, "p2/big"); !bytes.Equal(got, big) {
		t.Fatalf("big object differs (%d vs %d bytes)", len(got), len(big))
	}
	_ = st.Delete(ctx, "p1/wal/two.zst", "p2/wal/x.zst", "p2/big")
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestFileStoreContract(t *testing.T) {
	st, err := NewFileStore(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	storeContract(t, st)
}

func TestFileStoreHidesInFlightFiles(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A crashed Put leaves a temp file; it must not be listed as an object.
	if err := os.MkdirAll(filepath.Join(dir, "r", "wal"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "r", "wal", "X.zst"+tmpMarker+"123"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	objs, err := st.List(context.Background(), "r/")
	if err != nil || len(objs) != 0 {
		t.Fatalf("List = %v, %v", objs, err)
	}
}

func TestFileStoreKeysCannotEscapeRoot(t *testing.T) {
	// validKey forbids ".." so a key can never leave the root, whatever the layout holds.
	st, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(context.Background(), "../../etc/passwd"); err == nil {
		t.Fatal("Get escaped the root")
	}
}

func TestOpenStoreRejectsBadBackends(t *testing.T) {
	for _, tc := range []struct {
		backend, want string
	}{
		{"", "not configured"},
		{"ftp://x/y", "unsupported scheme"},
		{"file://relative/dir", "want file:///absolute/dir"},
		{"s3:///prefix-only", "want s3://bucket/prefix"},
	} {
		cfg := cfgWithBackend(tc.backend)
		_, err := OpenStore(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("OpenStore(%q) = %v, want error containing %q", tc.backend, err, tc.want)
		}
	}
	st, err := OpenStore(context.Background(), cfgWithBackend("file://"+t.TempDir()+"/b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.(*FileStore); !ok {
		t.Fatalf("OpenStore(file) = %T", st)
	}
}
