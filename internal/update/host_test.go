//go:build unix

package update

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A cancelled context (systemd stops the service) reaches `supavise upgrade` as SIGTERM, and the
// upgrade gets to end on its own terms: its exit status comes back, not a kill.
func TestRunUpgradePassesSIGTERMAndWaitsForTheExit(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	exe := filepath.Join(dir, "fake-supavise")
	script := "#!/bin/sh\n" +
		"trap 'echo got-term; exit 3' TERM\n" +
		"echo \"$@\" > " + started + "\n" +
		"while :; do sleep 0.05; done\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	var out bytes.Buffer
	exit, err := RunUpgrade(ctx, exe, "/etc/supavise/config.toml", &out, &out)
	if err != nil || exit != ExitRolledBack {
		t.Fatalf("RunUpgrade = %d, %v; want the script's own exit status 3 after SIGTERM", exit, err)
	}
	if !strings.Contains(out.String(), "got-term") {
		t.Errorf("the command never saw SIGTERM: %q", out.String())
	}
	if b, _ := os.ReadFile(started); !strings.Contains(string(b), "--config /etc/supavise/config.toml upgrade --unattended") {
		t.Errorf("arguments: %q", b)
	}
}

func TestRunUpgradeReportsExitStatusesAndMissingCommands(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "fake")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if exit, err := RunUpgrade(context.Background(), exe, "", nil, nil); err != nil || exit != ExitRefused {
		t.Errorf("RunUpgrade = %d, %v", exit, err)
	}
	if exit, err := RunUpgrade(context.Background(), filepath.Join(dir, "missing"), "", nil, nil); err == nil || exit != -1 {
		t.Errorf("a command that cannot run: %d, %v", exit, err)
	}
}

func TestTryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "host.lock")
	release, held, err := tryLock(path)
	if err != nil || held {
		t.Fatalf("first lock: held %v, %v", held, err)
	}
	if _, held, err := tryLock(path); err != nil || !held {
		t.Errorf("second lock: held %v, %v; want held", held, err)
	}
	release()
	release2, held, err := tryLock(path)
	if err != nil || held {
		t.Fatalf("after release: held %v, %v", held, err)
	}
	release2()
}
