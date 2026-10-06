package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OWNER/sbctl/internal/backup"
)

func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	err := rootCmd.Execute()
	rootCmd.SetArgs(nil)
	return out.String(), err
}

func TestBackupsCommandsAreRegistered(t *testing.T) {
	for _, path := range [][]string{
		{"wal", "push"}, {"wal", "fetch"},
		{"backups", "create"}, {"backups", "list"}, {"backups", "prune"}, {"backups", "restore"}, {"backups", "finish-restore"},
	} {
		c, _, err := rootCmd.Find(path)
		if err != nil || c == nil || c.Name() != path[len(path)-1] {
			t.Errorf("sbctl %s is not registered: %v", strings.Join(path, " "), err)
		}
	}
}

func TestRestoreValidatesBeforeTouchingAnything(t *testing.T) {
	const ref = "abcdefghijklmnopqrst"
	if _, err := runRoot(t, "backups", "restore", ref, "--to", "yesterday"); err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Errorf("bad --to = %v", err)
	}
	// In place (no --as, or --as the same ref) needs --force.
	for _, args := range [][]string{
		{"backups", "restore", ref, "--to", "2026-10-06T14:30:00Z"},
		{"backups", "restore", ref, "--to", "2026-10-06T14:30:00Z", "--as", ref},
	} {
		if _, err := runRoot(t, args...); !errors.Is(err, backup.ErrForceRequired) {
			t.Errorf("%v = %v, want ErrForceRequired", args, err)
		}
	}
	// latest and backup are targets of their own; in place still needs --force.
	for _, to := range []string{"latest", "backup"} {
		if _, err := runRoot(t, "backups", "restore", ref, "--to", to); !errors.Is(err, backup.ErrForceRequired) {
			t.Errorf("--to %s in place = %v, want ErrForceRequired", to, err)
		}
	}
	if _, err := runRoot(t, "backups", "restore", ref); err == nil {
		t.Error("--to is required")
	}
}

func TestWALPushNeedsRef(t *testing.T) {
	if _, err := runRoot(t, "wal", "push", "pg_wal/000000010000000000000001"); err == nil || !strings.Contains(err.Error(), "ref") {
		t.Errorf("wal push without --ref = %v", err)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 20: "5.0 MiB", 3 << 30: "3.0 GiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestWarnEnvFileMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sbctl.env")
	var out bytes.Buffer
	warnEnvFileMode(&out, p) // missing: silent
	if out.Len() != 0 {
		t.Fatalf("missing file warned: %q", out.String())
	}
	for _, tc := range []struct {
		mode os.FileMode
		warn bool
	}{{0o600, false}, {0o640, true}, {0o644, true}, {0o400, false}} {
		if err := os.WriteFile(p, []byte("SBCTL_REGISTRY_DSN=x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, tc.mode); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		warnEnvFileMode(&out, p)
		if (out.Len() > 0) != tc.warn {
			t.Errorf("mode %04o: output %q, want warning=%v", tc.mode, out.String(), tc.warn)
		}
	}
}
