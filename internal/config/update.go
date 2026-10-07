package config

import (
	"fmt"
	"strings"
	"time"
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
	// CheckInterval is how often the node asks GitHub for the latest release: a duration such as
	// "6h" (at least 15m, at most 168h), or "off" for no check (auto mode still upgrades in the
	// window, because the upgrade command looks for itself).
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

	// MinCheckInterval and MaxCheckInterval bound update.check_interval.
	MinCheckInterval = 15 * time.Minute
	MaxCheckInterval = 7 * 24 * time.Hour
)

// DefaultUpdate returns the built-in [update] defaults. The window opens after the 03:00
// nightly base backups and the unattended upgrade refuses to start without a recent backup of
// every project anyway.
func DefaultUpdate() Update {
	return Update{
		Mode:          UpdateNotify,
		Window:        "Sun 04:00-06:00",
		Channel:       "stable",
		CheckInterval: "6h",
		OSReboot:      "window",
	}
}

// ParsedWindow is the maintenance window, parsed. Validate has already accepted it.
func (u Update) ParsedWindow() (Window, error) { return ParseWindow(u.Window) }

// CheckEvery is the interval between release checks; ok is false when checks are off.
func (u Update) CheckEvery() (d time.Duration, ok bool, err error) {
	s := strings.TrimSpace(u.CheckInterval)
	if s == "" || strings.EqualFold(s, "off") {
		return 0, false, nil
	}
	d, err = time.ParseDuration(s)
	if err != nil {
		return 0, false, fmt.Errorf("update.check_interval %q: want a duration such as 6h, or off", u.CheckInterval)
	}
	if d < MinCheckInterval || d > MaxCheckInterval {
		return 0, false, fmt.Errorf("update.check_interval %q: want between %s and %s, or off", u.CheckInterval, MinCheckInterval, MaxCheckInterval)
	}
	return d, true, nil
}

// RebootsInWindow reports whether the node reboots itself inside the maintenance window for OS
// patches: only a node whose OS updates Supavise manages does.
func (u Update) RebootsInWindow() bool { return u.OSSecurityUpdates && u.OSReboot == "window" }

// TimerWanted reports whether supavise-upgrade.timer should run: for the release check, for
// auto upgrades or for the reboot in the window. With none of the three the timer has nothing
// to do and install-units disables it.
func (u Update) TimerWanted() bool {
	_, checks, _ := u.CheckEvery()
	return checks || u.Mode == UpdateAuto || u.RebootsInWindow()
}

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
