package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileReplacesAtomicallyWithTheMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "f.json")
	if err := WriteFile(path, []byte("x"), 0o640, Options{}); err == nil {
		t.Fatal("a missing directory without MkdirMode should fail")
	}
	for _, o := range []Options{{MkdirMode: 0o750, Sync: true, SyncDir: true}, {MkdirMode: 0o750, OwnerLike: Dir}} {
		if err := WriteJSON(path, map[string]int{"a": 1}, 0o640, o); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil || string(b) != "{\n  \"a\": 1\n}\n" {
			t.Fatalf("contents %q, %v", b, err)
		}
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o640 {
			t.Fatalf("mode %v", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o750 {
			t.Fatalf("dir mode %v", fi.Mode().Perm())
		}
	}
	left, _ := filepath.Glob(filepath.Join(dir, "a", "b", ".*"))
	if len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}
