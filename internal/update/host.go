package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
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

// HostReboot restarts the machine through systemd, which stops each Postgres cleanly (its unit
// waits up to 90 s). supavise.service drains running lifecycle operations for up to 10 minutes,
// but a project's units are not ordered after it, so they stop at the same time: the reboot waits
// for HostRebootBlocker to find the node quiet instead of counting on the drain.
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
	if o.HTTP == nil {
		// A release check that cannot reach GitHub fails in half a minute; the service is not
		// kept waiting for the library's ten minutes, which suit a binary download.
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
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

// Gate says how HostRebootBlocker reaches the node's project list.
type Gate struct {
	Exe        string // the supavise binary
	ConfigPath string // its config file
	User       string // the user that owns the state directory and may open the registry
	StateDir   string
}

// HostRebootBlocker returns the check that stands in front of the OS reboot. The node may restart
// only when:
//
//   - supavise.service runs and no supavise-* unit has failed (supavise-upgrade.service itself
//     is not counted: it fails when an upgrade rolls back);
//   - no base backup is running (supavise-basebackup@*.service, the prune service);
//   - no project is in a transitional status (a lifecycle operation is in flight: a Postgres
//     upgrade an Owner started from Studio, a restore, a pause) or in a failed or unhealthy one.
//
// The project statuses come from `supavise projects list --json` run as the supavise user, the
// way the installer runs the other commands that open the registry. A check that cannot run
// blocks the reboot: it must be asked for, never guessed.
func HostRebootBlocker(g Gate) func(ctx context.Context) string {
	return func(ctx context.Context) string {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		systemctl := func(args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, "systemctl", append([]string{"--no-pager", "--plain", "--no-legend"}, args...)...).Output()
		}
		if err := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "supavise.service").Run(); err != nil {
			return "supavise.service is not running"
		}
		out, err := systemctl("list-units", "--state=failed", "supavise*")
		if err != nil {
			return "cannot list failed units: " + err.Error()
		}
		if failed := FailedUnits(string(out)); len(failed) > 0 {
			return "failed units: " + strings.Join(failed, ", ")
		}
		out, err = systemctl("list-units", "--state=activating,active,deactivating,reloading", "supavise-basebackup@*.service", "supavise-basebackup-prune.service")
		if err != nil {
			return "cannot list running backups: " + err.Error()
		}
		if busy := UnitNames(string(out)); len(busy) > 0 {
			return "a base backup is running: " + strings.Join(busy, ", ")
		}
		args := []string{"-u", g.User, "--", "env", "HOME=" + g.StateDir, g.Exe, "--config", g.ConfigPath, "projects", "list", "--json"}
		out, err = exec.CommandContext(ctx, "runuser", args...).Output()
		if err != nil {
			return "cannot read the project list: " + err.Error()
		}
		return ProjectBlocker(out)
	}
}

// UnitNames returns the unit names in the output of `systemctl list-units --plain --no-legend`.
func UnitNames(out string) []string {
	var names []string
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(strings.TrimLeft(l, "●* ")); len(f) > 0 {
			names = append(names, f[0])
		}
	}
	return names
}

// FailedUnits is UnitNames without supavise-upgrade.service.
func FailedUnits(out string) []string {
	var names []string
	for _, n := range UnitNames(out) {
		if n != ServiceUnit {
			names = append(names, n)
		}
	}
	return names
}

// ProjectBlocker reads the JSON of `supavise projects list --json` and returns why the node must
// not reboot, or "". Only projects that should be running count: a paused (INACTIVE) project is
// quiet, a removed one is gone.
func ProjectBlocker(listJSON []byte) string {
	var ps []struct {
		Ref    string `json:"ref"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(listJSON, &ps); err != nil {
		return "cannot read the project list: " + err.Error()
	}
	var busy, bad []string
	for _, p := range ps {
		switch p.Status {
		case "COMING_UP", "PAUSING", "RESTORING", "RESTARTING", "UPGRADING", "GOING_DOWN":
			busy = append(busy, p.Ref+" "+p.Status)
		case "ACTIVE_UNHEALTHY", "RESTORE_FAILED", "INIT_FAILED", "UNKNOWN":
			bad = append(bad, p.Ref+" "+p.Status)
		}
	}
	sort.Strings(busy)
	sort.Strings(bad)
	switch {
	case len(busy) > 0:
		return "a lifecycle operation is in flight: " + strings.Join(busy, ", ")
	case len(bad) > 0:
		return "projects are not healthy: " + strings.Join(bad, ", ")
	}
	return ""
}
