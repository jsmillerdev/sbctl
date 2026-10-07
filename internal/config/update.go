package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Update is the [update] section: how the node learns about new Supavise releases and, when the
// operator opts in, installs them. Hosted Supabase upgrades its platform itself; here the
// operator owns the node, so the default is to say that a release exists and let a person run
// `supavise upgrade`. Mode "auto" makes the node run the same upgrade by itself, inside the
// maintenance window only. `supavise update config` and the installer's --auto-upgrade and
// --maintenance-window flags set it; internal/update is the code that reads it.
type Update struct {
	// Mode is "notify" (the default: log that a release is available, change nothing) or "auto"
	// (run `supavise upgrade --unattended` inside Window).
	Mode string `toml:"mode"`
	// Window is the weekly maintenance window in the node's time zone, "Sun 04:00-06:00"
	// (ParseWindow documents the format). Only auto upgrades and the OS reboot wait for it.
	Window string `toml:"window"`
	// Channel is the release stream to follow. "stable" (GitHub's latest non-prerelease) is the
	// only channel.
	Channel string `toml:"channel"`
	// CheckInterval is how often the daemon asks GitHub for the latest release (internal/health
	// records the answer in <state_dir>/system/update.json): a duration such as "12h", whole days
	// such as "2d", or a bare number of seconds; "off", "never" or 0 stop the check. The default
	// is 24h and the floor one hour. ParseCheckInterval is the grammar. Auto mode still upgrades
	// in the window with the check off, because `supavise upgrade` looks for itself.
	CheckInterval string `toml:"check_interval"`
	// OSSecurityUpdates turns on unattended-upgrades for the distribution's security updates only
	// (`supavise install` writes the apt configuration). A new install sets it; an existing
	// node keeps what its config says (false when the key is absent), so a Supavise release never
	// starts patching a host that was not patched before.
	OSSecurityUpdates bool `toml:"os_security_updates"`
	// OSReboot is "window" (reboot the node inside the maintenance window when a patch needs it)
	// or "never" (leave the reboot to the operator). It applies only when OSSecurityUpdates is on.
	OSReboot string `toml:"os_reboot"`
}

const (
	UpdateNotify = "notify"
	UpdateAuto   = "auto"

	// DefaultCheckInterval and MinCheckInterval are the default and the floor of
	// update.check_interval: an unauthenticated client may ask GitHub 60 times an hour.
	DefaultCheckInterval = 24 * time.Hour
	MinCheckInterval     = time.Hour
)

// DefaultUpdate returns the built-in [update] defaults. The window opens after the 03:00
// nightly base backups and the unattended upgrade refuses to start without a recent backup of
// every project anyway.
func DefaultUpdate() Update {
	return Update{
		Mode:          UpdateNotify,
		Window:        "Sun 04:00-06:00",
		Channel:       "stable",
		CheckInterval: "24h",
		OSReboot:      "window",
	}
}

// ParsedWindow is the maintenance window, parsed. Validate has already accepted it.
func (u Update) ParsedWindow() (Window, error) { return ParseWindow(u.Window) }

// ParseCheckInterval reads an update.check_interval value: a Go duration ("12h"), whole days
// ("2d"), a bare number of seconds ("7200"), or "off", "never" or "0" for no check. An empty value
// is the default. A value under MinCheckInterval is raised to it. It is the one grammar for the
// setting: Validate, the daemon's release check and the timer all go through it.
func ParseCheckInterval(raw string) (d time.Duration, on bool, err error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "":
		return DefaultCheckInterval, true, nil
	case "off", "never", "0":
		return 0, false, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, perr := strconv.Atoi(days)
		if perr != nil || n <= 0 {
			return 0, false, fmt.Errorf("update.check_interval %q: want a duration such as 12h, whole days such as 2d, seconds, or off", raw)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else if n, perr := strconv.ParseInt(s, 10, 64); perr == nil {
		d = time.Duration(n) * time.Second
	} else if d, perr = time.ParseDuration(s); perr != nil {
		return 0, false, fmt.Errorf("update.check_interval %q: want a duration such as 12h, whole days such as 2d, seconds, or off", raw)
	}
	if d <= 0 {
		return 0, false, fmt.Errorf("update.check_interval %q: must be positive (or off)", raw)
	}
	return max(d, MinCheckInterval), true, nil
}

// CheckEvery is the interval between release checks; ok is false when checks are off.
func (u Update) CheckEvery() (d time.Duration, ok bool, err error) {
	return ParseCheckInterval(u.CheckInterval)
}

// RebootsInWindow reports whether the node reboots itself inside the maintenance window for OS
// patches: only a node whose OS updates Supavise manages does.
func (u Update) RebootsInWindow() bool { return u.OSSecurityUpdates && u.OSReboot == "window" }

// TimerWanted reports whether supavise-upgrade.timer should run: for auto upgrades or for the
// reboot in the window. With neither the timer has nothing to do and install-units disables it.
// (The release check belongs to the daemon, not to the timer.)
func (u Update) TimerWanted() bool { return u.Mode == UpdateAuto || u.RebootsInWindow() }

func (u Update) validate() error {
	switch u.Mode {
	case UpdateNotify, UpdateAuto:
	default:
		return fmt.Errorf("config: update.mode %q: want %q or %q", u.Mode, UpdateNotify, UpdateAuto)
	}
	if u.Channel != "stable" {
		return fmt.Errorf("config: update.channel %q: stable is the only channel", u.Channel)
	}
	switch u.OSReboot {
	case "never", "window":
	default:
		return fmt.Errorf("config: update.os_reboot %q: want never or window", u.OSReboot)
	}
	if _, err := ParseWindow(u.Window); err != nil {
		return fmt.Errorf("config: update.window: %w", err)
	}
	if _, _, err := u.CheckEvery(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// DecodeTOML decodes config.toml text into c, accepting every documented form of
// update.check_interval. Every reader of the file goes through it, so the daemon, the
// installer and `supavise update config` agree on what a file means.
func DecodeTOML(b []byte, c *Config) error {
	return toml.Unmarshal(quoteBareCheckInterval(b), c)
}

// quoteBareCheckInterval rewrites a bare TOML number in update.check_interval (seconds, as
// `check_interval = 7200` or `= 0`) to the string the field holds, so that the number form
// loads too. Any other file comes back unchanged.
func quoteBareCheckInterval(b []byte) []byte {
	var doc map[string]any
	if toml.Unmarshal(b, &doc) != nil {
		return b // the real decode reports it
	}
	tbl, _ := doc["update"].(map[string]any)
	n, ok := tbl["check_interval"].(int64)
	if !ok {
		return b
	}
	tbl["check_interval"] = strconv.FormatInt(n, 10)
	out, err := toml.Marshal(doc)
	if err != nil {
		return b
	}
	return out
}
