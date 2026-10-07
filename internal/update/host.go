package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/jsmillerdev/supavise/internal/selfupdate"
)

// rebootMarkers are the files that packages write when an update needs a reboot.
var rebootMarkers = []string{"/run/reboot-required", "/var/run/reboot-required"}

// HostRebootRequired reports whether an installed OS update waits for a reboot: the marker file
// Ubuntu's packages write, or needrestart's kernel check, which covers Debian (it has no marker)
// and an Ubuntu kernel installed without one. A needrestart that is missing or fails counts as
// "no reboot due": a reboot must be asked for, never guessed.
func HostRebootRequired(ctx context.Context) bool {
	for _, m := range rebootMarkers {
		if _, err := os.Stat(m); err == nil {
			return true
		}
	}
	path, err := exec.LookPath("needrestart")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// -b is batch mode: it prints, and never restarts anything. -k checks the kernel only.
	out, err := exec.CommandContext(ctx, path, "-b", "-k").Output()
	if err != nil {
		return false
	}
	return KernelRebootDue(KernelStatus(string(out)))
}

// HostReboot restarts the machine through systemd, which stops the units in order: Postgres
// shuts down cleanly and supavise.service drains its operations first.
func HostReboot(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "systemctl", "reboot").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl reboot: %v: %s", err, out)
	}
	return nil
}

// LatestStable asks GitHub for the tag of the newest stable release of o's repository.
func LatestStable(ctx context.Context, o selfupdate.Options) (string, error) {
	o.Tag = ""
	rel, err := selfupdate.Latest(ctx, o)
	if err != nil {
		return "", err
	}
	return rel.Tag, nil
}

// RunUpgrade runs `<exe> upgrade --unattended` and returns its exit status. An exit status is
// not an error here: the contract gives each one a meaning. The error is for a command that did
// not run.
func RunUpgrade(ctx context.Context, exe, configPath string, stdout, stderr io.Writer) (int, error) {
	args := []string{}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	args = append(args, "upgrade", "--unattended")
	c := exec.CommandContext(ctx, exe, args...)
	c.Stdout, c.Stderr = stdout, stderr
	err := c.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		return ee.ExitCode(), nil
	}
	return -1, err
}
