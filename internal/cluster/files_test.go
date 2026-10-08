package cluster

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileCreatesDirectoriesAndLeavesNoTemporaryFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "etc", "config.d", "10-cluster.toml")
	if err := writeFile(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(p, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "two" {
		t.Fatalf("%q, %v", b, err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v, %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Dir(p)); err != nil || fi.Mode().Perm() != 0o750 {
		t.Fatalf("directory: %v, %v", fi, err)
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Fatalf("leftovers: %v", ents)
	}
	// A path component that is a file is an error, not a panic.
	if err := writeFile(filepath.Join(p, "child"), []byte("x"), 0o600); err == nil {
		t.Fatal("wrote below a file")
	}
}
