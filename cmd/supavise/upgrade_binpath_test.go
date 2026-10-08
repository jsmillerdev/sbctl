package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/selfupdate"
)

// binTree makes <top>/bin/supavise, owned by the test's own uid, the stand-in for root on a node:
// requireRootOwned takes the uid it trusts and the directory it stops at.
func binTree(t *testing.T) (top, bin string) {
	t.Helper()
	top, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(top, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(top, "bin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "supavise")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return top, bin
}

func TestRequireRootOwnedAcceptsAProtectedBinary(t *testing.T) {
	top, bin := binTree(t)
	if err := requireRootOwned(bin, uint32(os.Getuid()), top); err != nil {
		t.Fatal(err)
	}
}

// The check runs before anything is executed: a binary that any of these could change is
// refused, and the error says why.
func TestRequireRootOwnedRefuses(t *testing.T) {
	me := uint32(os.Getuid())
	cases := []struct {
		name  string
		setup func(t *testing.T, top, bin string) (path string, owner uint32)
		want  string
	}{
		{"another uid owns the file", func(t *testing.T, top, bin string) (string, uint32) { return bin, me + 1 }, "belongs to uid"},
		{"the file is writable by its group", func(t *testing.T, top, bin string) (string, uint32) {
			must(t, os.Chmod(bin, 0o775))
			return bin, me
		}, "group or by others"},
		{"the file is writable by others", func(t *testing.T, top, bin string) (string, uint32) {
			must(t, os.Chmod(bin, 0o757))
			return bin, me
		}, "group or by others"},
		{"its directory is writable by others", func(t *testing.T, top, bin string) (string, uint32) {
			must(t, os.Chmod(filepath.Dir(bin), 0o777))
			return bin, me
		}, "group or by others"},
		{"a directory higher up is writable by its group", func(t *testing.T, top, bin string) (string, uint32) {
			must(t, os.Chmod(top, 0o775))
			return bin, me
		}, "group or by others"},
		{"it is a directory", func(t *testing.T, top, bin string) (string, uint32) {
			return filepath.Dir(bin), me
		}, "not a regular file"},
		{"it is a symlink to a file in a writable directory", func(t *testing.T, top, bin string) (string, uint32) {
			loose := filepath.Join(top, "loose")
			must(t, os.Mkdir(loose, 0o777))
			must(t, os.Chmod(loose, 0o777))
			target := filepath.Join(loose, "supavise")
			must(t, os.WriteFile(target, []byte("x"), 0o755))
			link := filepath.Join(filepath.Dir(bin), "link")
			must(t, os.Symlink(target, link))
			return link, me
		}, "group or by others"},
		{"it does not exist", func(t *testing.T, top, bin string) (string, uint32) {
			return filepath.Join(filepath.Dir(bin), "missing"), me
		}, "no such file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			top, bin := binTree(t)
			path, owner := c.setup(t, top, bin)
			err := requireRootOwned(path, owner, top)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestRequireRootOwnedFollowsAProtectedSymlink(t *testing.T) {
	top, bin := binTree(t)
	link := filepath.Join(top, "bin", "link")
	must(t, os.Symlink(bin, link))
	if err := requireRootOwned(link, uint32(os.Getuid()), top); err != nil {
		t.Fatal(err)
	}
}

// The path to the binary is the installer's, not the config's: the supavise user writes the config.
func TestInstalledBinaryIgnoresBinPathFromTheConfig(t *testing.T) {
	_, bin := binTree(t)
	cfg := &config.Config{BinPath: bin}
	got, err := installedBinary(cfg)
	if err == nil || got != "" {
		t.Fatalf("a bin_path of the config's own choosing was accepted: %q, %v", got, err)
	}
	if !strings.Contains(err.Error(), config.DefaultBinPath) {
		t.Fatalf("the error does not name the path that is trusted: %v", err)
	}
}

// A node host is not built, and nothing is run, from a config that names another binary.
func TestNewNodeHostRefusesAForeignBinPath(t *testing.T) {
	_, bin := binTree(t)
	cfg := config.Default()
	cfg.BinPath = bin
	h, err := newNodeHost(cobraIO{Out: os.Stderr, Err: os.Stderr}, cfg, 0, selfupdate.Options{})
	if err == nil || h != nil {
		t.Fatalf("newNodeHost = %v, %v", h, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
