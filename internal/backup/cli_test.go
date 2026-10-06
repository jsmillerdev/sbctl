package backup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cliEnv runs the real sbctl binary against a file backend, as Postgres does.
type cliEnv struct {
	t       *testing.T
	bin     string
	cfg     string
	archive string
	root    string
}

func newCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	root := t.TempDir()
	c := &cliEnv{t: t, bin: sbctlBinary(t), root: root, archive: filepath.Join(root, "archive"), cfg: filepath.Join(root, "sbctl.toml")}
	writeFile(t, c.cfg, []byte(fmt.Sprintf("state_dir = %q\n\n[backup]\nbackend = %q\n", filepath.Join(root, "state"), "file://"+c.archive)))
	return c
}

// run returns the exit status, stderr and the elapsed time.
func (c *cliEnv) run(dir string, args ...string) (int, string, time.Duration) {
	c.t.Helper()
	cmd := exec.Command(c.bin, append([]string{"--config", c.cfg}, args...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	took := time.Since(start)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String(), took
	case errors.As(err, &ee):
		return ee.ExitCode(), stderr.String(), took
	}
	c.t.Fatalf("running sbctl: %v", err)
	return -1, "", 0
}

// TestWALCommandsExitStatuses pins the archive_command and restore_command contract
// of the real binary: what Postgres sees as success, "not there" and "stop recovery".
func TestWALCommandsExitStatuses(t *testing.T) {
	c := newCLIEnv(t)
	datadir := filepath.Join(c.root, "pgdata")
	name := walName(1, 5)
	writeFile(t, filepath.Join(datadir, "pg_wal", name), bytes.Repeat([]byte("wal"), 100000))

	// archive_command runs with the data directory as cwd and a relative %p.
	if code, errs, took := c.run(datadir, "wal", "push", "--ref", testRef, "pg_wal/"+name); code != 0 {
		t.Fatalf("push exit %d: %s", code, errs)
	} else {
		t.Logf("wal push took %s", took)
		if took > 5*time.Second {
			t.Errorf("wal push took %s: archive_command must start fast", took)
		}
	}
	if code, errs, _ := c.run(datadir, "wal", "push", "--ref", testRef, "pg_wal/"+name); code != 0 {
		t.Fatalf("identical re-push exit %d: %s", code, errs)
	}
	writeFile(t, filepath.Join(datadir, "pg_wal", name), []byte("different"))
	if code, errs, _ := c.run(datadir, "wal", "push", "--ref", testRef, "pg_wal/"+name); code == 0 || !strings.Contains(errs, "different content") {
		t.Fatalf("push of different content: exit %d, stderr %q", code, errs)
	}
	if code, _, _ := c.run(datadir, "wal", "push", "--ref", testRef, "pg_wal/"+name+".tmp"); code == 0 {
		t.Fatal("push of a missing file succeeded")
	}
	if code, _, _ := c.run(datadir, "wal", "push", "pg_wal/"+name); code == 0 {
		t.Fatal("push without --ref succeeded")
	}

	// restore_command: present, absent, unreadable.
	dest := filepath.Join(c.root, "RECOVERYXLOG")
	if code, errs, _ := c.run(datadir, "wal", "fetch", "--ref", testRef, name, dest); code != 0 {
		t.Fatalf("fetch exit %d: %s", code, errs)
	}
	if b, _ := os.ReadFile(dest); len(b) != 300000 {
		t.Fatalf("fetched %d bytes", len(b))
	}
	os.Remove(dest)
	code, errs, took := c.run(datadir, "wal", "fetch", "--ref", testRef, walName(1, 6), dest)
	if code != 1 {
		t.Fatalf("fetch of a missing file: exit %d (%s); Postgres treats 1 as end of archive and above 125 as fatal", code, errs)
	}
	t.Logf("wal fetch of a missing file took %s", took)
	if took > 3*time.Second {
		t.Errorf("a missing file must fail fast, took %s", took)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("fetch of a missing file created the destination")
	}
	if code, _, _ := c.run(datadir, "wal", "fetch", "--ref", testRef, "00000002.history", dest); code != 1 {
		t.Fatalf("fetch of a missing history file: exit %d", code)
	}
	// An object that cannot be read is not the end of the archive: exit 126 aborts recovery.
	obj := filepath.Join(c.archive, testRef, "wal", walName(1, 7)+".zst")
	writeFile(t, obj, []byte("this is not zstd"))
	if code, _, _ := c.run(datadir, "wal", "fetch", "--ref", testRef, walName(1, 7), dest); code != WALExitFatal {
		t.Fatalf("fetch of a corrupt object: exit %d, want %d", code, WALExitFatal)
	}
	if err := os.Chmod(obj, 0); err == nil && os.Getuid() != 0 {
		if code, _, _ := c.run(datadir, "wal", "fetch", "--ref", testRef, walName(1, 7), dest); code != WALExitFatal {
			t.Fatalf("fetch of an unreadable object: exit %d, want %d", code, WALExitFatal)
		}
	}
	// A backend that is not configured is also fatal for fetch.
	bad := filepath.Join(c.root, "bad.toml")
	writeFile(t, bad, []byte("[backup]\nbackend = \"ftp://nowhere\"\n"))
	cmd := exec.Command(c.bin, "--config", bad, "wal", "fetch", "--ref", testRef, name, dest)
	if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != WALExitFatal {
		t.Fatalf("fetch with a bad backend: %v", err)
	}
}
