package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/deploy/systemd"
	"github.com/jsmillerdev/supavise/internal/backup"
	"github.com/jsmillerdev/supavise/internal/config"
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
		{"backups", "restore-files"}, {"backups", "status"},
		{"system", "export-key"}, {"system", "escrow-key"}, {"system", "restore-key"},
	} {
		c, _, err := rootCmd.Find(path)
		if err != nil || c == nil || c.Name() != path[len(path)-1] {
			t.Errorf("supavise %s is not registered: %v", strings.Join(path, " "), err)
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

func TestRestoreFilesValidatesBeforeTouchingAnything(t *testing.T) {
	const ref = "abcdefghijklmnopqrst"
	if _, err := runRoot(t, "backups", "restore-files", ref); err == nil || !strings.Contains(err.Error(), "--to") {
		t.Errorf("no target = %v", err)
	}
	if _, err := runRoot(t, "backups", "restore-files", ref, "--to", "yesterday"); err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Errorf("bad --to = %v", err)
	}
	// In place replaces the project's files, which needs --force.
	if _, err := runRoot(t, "backups", "restore-files", ref, "--to", "latest"); !errors.Is(err, backup.ErrForceRequired) {
		t.Errorf("in place without --force = %v, want ErrForceRequired", err)
	}
}

func TestCreateRejectsConflictingFileFlags(t *testing.T) {
	if _, err := runRoot(t, "backups", "create", "abcdefghijklmnopqrst", "--skip-files", "--files-only"); err == nil || !strings.Contains(err.Error(), "exclude each other") {
		t.Errorf("err = %v", err)
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
	p := filepath.Join(dir, "supavise.env")
	var out bytes.Buffer
	warnEnvFileMode(&out, p) // missing: silent
	if out.Len() != 0 {
		t.Fatalf("missing file warned: %q", out.String())
	}
	for _, tc := range []struct {
		mode os.FileMode
		warn bool
	}{{0o600, false}, {0o640, true}, {0o644, true}, {0o400, false}} {
		if err := os.WriteFile(p, []byte("SUPAVISE_REGISTRY_DSN=x\n"), 0o600); err != nil {
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

func TestRefuseRoot(t *testing.T) {
	if err := refuseRoot(0); !errors.Is(err, errRunAsRoot) || !strings.Contains(err.Error(), "sudo -u supavise") {
		t.Fatalf("refuseRoot(0) = %v", err)
	}
	for _, euid := range []int{1, 100, 1000} {
		if err := refuseRoot(euid); err != nil {
			t.Errorf("refuseRoot(%d) = %v", euid, err)
		}
	}
	// Both command groups carry the check, so no subcommand can forget it.
	for _, name := range []string{"backups", "wal"} {
		c, _, err := rootCmd.Find([]string{name})
		if err != nil || c.PersistentPreRunE == nil {
			t.Errorf("supavise %s has no root check: %v", name, err)
		}
	}
}

func TestCreateRejectsUnknownReason(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is refused before flags are looked at")
	}
	if _, err := runRoot(t, "backups", "create", "abcdefghijklmnopqrst", "--reason", "because"); err == nil || !strings.Contains(err.Error(), "--reason") {
		t.Fatalf("unknown --reason = %v", err)
	}
	for _, r := range []string{"manual", "scheduled", "final", "post-restore"} {
		if !validReason(r) {
			t.Errorf("%s must be valid", r)
		}
	}
}

func TestWarnConfigFileMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cfg := &config.Config{}
	warnConfigFileMode(&out, p, cfg)
	if out.Len() != 0 {
		t.Fatalf("no secret in config, no warning: %q", out.String())
	}
	cfg.Backup.S3SecretAccessKey = "secret"
	warnConfigFileMode(&out, p, cfg)
	if !strings.Contains(out.String(), "0600") || strings.Contains(out.String(), "secret\n") {
		t.Fatalf("warning = %q", out.String())
	}
	out.Reset()
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	warnConfigFileMode(&out, p, cfg)
	if out.Len() != 0 {
		t.Fatalf("0600 file warned: %q", out.String())
	}
}

func TestInstallUnitsIsIdempotentWithACustomBackupSchedule(t *testing.T) {
	dir := t.TempDir()
	cal := "*-*-* 01:30:00"
	timer, err := backup.RenderBackupTimerChecked(cal)
	if err != nil {
		t.Fatal(err)
	}
	ov := map[string][]byte{backup.BackupTimerUnit: []byte(timer)}
	first, err := systemd.InstallWith(dir, "", ov)
	if err != nil || len(first) == 0 {
		t.Fatalf("first install: %v %v", first, err)
	}
	if second, err := systemd.InstallWith(dir, "", ov); err != nil || len(second) != 0 {
		t.Fatalf("second install with a custom schedule changed %v (err %v)", second, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "supavise-basebackup@.timer"))
	if !strings.Contains(string(b), "OnCalendar=*-*-* 01:30:00\n") {
		t.Errorf("timer file:\n%s", b)
	}
	if _, err := backup.RenderBackupTimerChecked("bad\nExecStart=/bin/sh"); err == nil {
		t.Error("an expression with a newline must be rejected, not written into the unit")
	}
}

func TestServeIsRegistered(t *testing.T) {
	if c, _, err := rootCmd.Find([]string{"serve"}); err != nil || c == nil || c.Name() != "serve" {
		t.Fatalf("supavise serve is not registered: %v", err)
	}
}
