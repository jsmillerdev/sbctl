package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/selfupdate"
	"github.com/jsmillerdev/supavise/internal/update"
)

func init() {
	updateCmd := &cobra.Command{
		Use:   "update",
		Short: "Release checks, the maintenance window and automatic upgrades",
		Long: `Supavise tells you when a release exists and, only if you opt in, installs it by itself
inside a maintenance window. The setting is the [update] section of config.toml:

  mode                "notify" (the default) logs that a release is available and changes nothing;
                      "auto" runs ` + "`supavise upgrade --unattended`" + ` inside the window
  window              the weekly maintenance window in the node's time zone, "Sun 04:00-06:00"
                      (days, then HH:MM-HH:MM; "Sat,Sun", "Mon-Fri" and "daily" work; a window may
                      cross midnight)
  channel             "stable", the only channel
  check_interval      how often the node asks GitHub for the latest release, "6h" ("off" stops it)
  os_security_updates unattended-upgrades for security updates only (a new install sets it)
  os_reboot           "window" reboots the node inside the window when an OS patch needs it, "never"

` + "`supavise update config`" + ` changes these; ` + "`supavise update status`" + ` shows them and what the node did last.
A systemd timer (supavise-upgrade.timer) wakes the node at the window; nothing starts an
upgrade outside it, and notify mode never starts one.`,
	}

	// ---- config ------------------------------------------------------------
	var (
		fMode, fWindow, fChannel, fInterval, fReboot string
		fOSUpdates                                   bool
	)
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Show or change the [update] settings (needs root to change)",
		Long: `Without flags it prints the current [update] settings. With flags it changes those settings
only, writes config.toml, rewrites supavise-upgrade.timer for the new window and enables or
disables it, and, when --os-security-updates changed, installs or removes the unattended-upgrades
configuration. A re-run of the installer keeps what this sets.

  sudo supavise update config --mode auto --window "Sun 03:00-05:00"
  sudo supavise update config --mode notify
  sudo supavise update config --os-security-updates=false`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := selfUpdateConfigPath()
			cfg, existed, err := readConfigFile(path)
			if err != nil {
				return err
			}
			f := cmd.Flags()
			anyChange := false
			for _, n := range []string{"mode", "window", "channel", "check-interval", "os-security-updates", "os-reboot"} {
				anyChange = anyChange || f.Changed(n)
			}
			if !anyChange {
				printUpdateSettings(cmd.OutOrStdout(), cfg.Update)
				return nil
			}
			if runtime.GOOS != "linux" {
				return errors.New("update config configures a Linux server install")
			}
			if os.Geteuid() != 0 {
				return errors.New("run as root: sudo supavise update config ...")
			}
			if !existed {
				return fmt.Errorf("%s does not exist: this node is not installed (supavise install)", path)
			}
			if err := applyUpdateFlags(&cfg.Update, f.Changed, fMode, fWindow, fChannel, fInterval, fReboot, fOSUpdates); err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			body, err := renderConfig(cfg)
			if err != nil {
				return err
			}
			prev, _ := os.ReadFile(path)
			if !bytes.Equal(prev, body) {
				uid, gid := supaviseOwner()
				if err := writeFileAtomic(path, body, 0o600, uid, gid); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
			}
			// The timer follows the settings: a new window rewrites it. install-units is the one
			// place that renders and enables it, so the installer and this command cannot disagree.
			if err := runInstallUnits(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return err
			}
			if f.Changed("os-security-updates") {
				if err := applyOSUpdates(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), cfg.Update.OSSecurityUpdates); err != nil {
					return err
				}
			}
			printUpdateSettings(cmd.OutOrStdout(), cfg.Update)
			return nil
		},
	}
	cf := configCmd.Flags()
	cf.StringVar(&fMode, "mode", "", `"notify" (log that a release exists) or "auto" (upgrade inside the window)`)
	cf.StringVar(&fWindow, "window", "", `maintenance window in the node's time zone, for example "Sun 03:00-05:00"`)
	cf.StringVar(&fChannel, "channel", "", `release channel; "stable" is the only one`)
	cf.StringVar(&fInterval, "check-interval", "", `how often to ask GitHub for the latest release, "6h"; "off" stops the check`)
	cf.BoolVar(&fOSUpdates, "os-security-updates", false, "unattended OS security updates (Ubuntu and Debian)")
	cf.StringVar(&fReboot, "os-reboot", "", `"window" reboots inside the maintenance window when an OS patch needs it; "never"`)

	// ---- status ------------------------------------------------------------
	var asJSON bool
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show the update settings, the next maintenance window and the last unattended upgrade",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			st, err := (update.Store{Path: update.StatePath(cfg.StateDir)}).Load()
			if err != nil {
				return err
			}
			rep, err := buildUpdateReport(cmd.Context(), cfg, st, time.Now())
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			rep.print(cmd.OutOrStdout())
			return nil
		},
	}
	statusCmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	// ---- run (the timer's command) ----------------------------------------
	var at string
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "One pass of the release check, the unattended upgrade and the OS reboot (what supavise-upgrade.timer runs)",
		Long: `Checks for a release (at most every update.check_interval) and logs update_available once per
release. In auto mode, while the maintenance window is open, runs ` + "`supavise upgrade --unattended`" + `: once
per window, except that a refusal (exit 2, nothing changed) is tried again at the next tick. With
update.os_security_updates and update.os_reboot = "window", reboots the node inside the window when an
OS patch needs it, once per window. It does nothing else, and nothing outside the window except the
check. Needs root. The timer runs it; run it by hand to see what it would do now.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return errors.New("update run belongs to a Linux server install")
			}
			if os.Geteuid() != 0 {
				return errors.New("run as root: sudo supavise update run")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return err
			}
			now := time.Now
			if at != "" {
				t, err := time.Parse(time.RFC3339, at)
				if err != nil {
					return fmt.Errorf("--at: %w", err)
				}
				now = func() time.Time { return t }
			}
			return update.Run(cmd.Context(), update.Deps{
				Update:  cfg.Update,
				Version: version,
				Store:   update.Store{Path: update.StatePath(cfg.StateDir)},
				Log:     newLogger(cfg).With("component", "update"),
				Now:     now,
				Latest: func(ctx context.Context) (string, error) {
					return update.LatestStable(ctx, selfupdate.Options{})
				},
				Upgrade: func(ctx context.Context) (int, error) {
					return update.RunUpgrade(ctx, exe, configPath, cmd.OutOrStdout(), cmd.ErrOrStderr())
				},
				RebootRequired: update.HostRebootRequired,
				Reboot:         update.HostReboot,
			})
		},
	}
	runCmd.Flags().StringVar(&at, "at", "", "pretend it is this time (RFC 3339), to see what a run would do (tests)")
	_ = runCmd.Flags().MarkHidden("at")

	// ---- resume ------------------------------------------------------------
	resumeCmd := &cobra.Command{
		Use:   "resume",
		Short: "Let automatic upgrades run again after a failure that needed you",
		Long: `After an unattended upgrade fails in a way that needs the operator (` + "`supavise upgrade`" + ` exit 4),
the node stops trying until you have looked. Check the node, then run this.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if os.Geteuid() != 0 {
				return errors.New("run as root: sudo supavise update resume")
			}
			was, err := update.Resume(update.Store{Path: update.StatePath(cfg.StateDir)})
			if err != nil {
				return err
			}
			if was == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "automatic upgrades were not paused")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "automatic upgrades resume at the next maintenance window (they were paused: %s)\n", was)
			return nil
		},
	}

	updateCmd.AddCommand(configCmd, statusCmd, runCmd, resumeCmd)
	rootCmd.AddCommand(updateCmd)

	// ---- system os-updates -------------------------------------------------
	var osEnable, osDisable bool
	osCmd := &cobra.Command{
		Use:   "os-updates",
		Short: "Set up (or remove) unattended security updates for the operating system (needs root)",
		Long: `Writes the apt configuration that turns on unattended-upgrades for security updates only
(Ubuntu and Debian), installs unattended-upgrades and needrestart when they are missing, and keeps
needrestart from restarting supavise-* units. Without a flag it follows update.os_security_updates in
config.toml: on, it sets everything up; off, it removes the files Supavise wrote and leaves the packages.
The installer calls it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if osEnable && osDisable {
				return errors.New("--enable and --disable exclude each other")
			}
			if os.Geteuid() != 0 {
				return errors.New("run as root: sudo supavise system os-updates")
			}
			enable := osEnable
			if !osEnable && !osDisable {
				cfg, _, err := readConfigFile(selfUpdateConfigPath())
				if err != nil {
					return err
				}
				enable = cfg.Update.OSSecurityUpdates
			}
			return applyOSUpdates(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), enable)
		},
	}
	osCmd.Flags().BoolVar(&osEnable, "enable", false, "set up unattended security updates whatever config.toml says")
	osCmd.Flags().BoolVar(&osDisable, "disable", false, "remove the Supavise configuration for them")
	systemCmd.AddCommand(osCmd)
}

// applyUpdateFlags puts the [update] flags that were given into u.
func applyUpdateFlags(u *config.Update, changed func(string) bool, mode, window, channel, interval, reboot string, osUpdates bool) error {
	if changed("mode") {
		u.Mode = mode
	}
	if changed("window") {
		if _, err := config.ParseWindow(window); err != nil {
			return fmt.Errorf("--window: %w", err)
		}
		u.Window = window
	}
	if changed("channel") {
		u.Channel = channel
	}
	if changed("check-interval") {
		u.CheckInterval = interval
	}
	if changed("os-reboot") {
		u.OSReboot = reboot
	}
	if changed("os-security-updates") {
		u.OSSecurityUpdates = osUpdates
	}
	return nil
}

func printUpdateSettings(w io.Writer, u config.Update) {
	fmt.Fprintf(w, "[update]\nmode = %q\nwindow = %q\nchannel = %q\ncheck_interval = %q\nos_security_updates = %v\nos_reboot = %q\n",
		u.Mode, u.Window, u.Channel, u.CheckInterval, u.OSSecurityUpdates, u.OSReboot)
}

// supaviseOwner is the uid and gid of the supavise user, or -1 (leave the owner) when the user
// does not exist.
func supaviseOwner() (uid, gid int) {
	u, err := user.Lookup(installUser)
	if err != nil {
		return -1, -1
	}
	uid, _ = strconv.Atoi(u.Uid)
	gid, _ = strconv.Atoi(u.Gid)
	return uid, gid
}

// runInstallUnits runs this binary's `system install-units`, which renders and enables the
// upgrade timer from the config. It runs the file on disk, as the installer does.
func runInstallUnits(ctx context.Context, out, errOut io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	c := exec.CommandContext(ctx, exe, append(args, "system", "install-units")...)
	c.Stdout, c.Stderr = out, errOut
	if err := c.Run(); err != nil {
		return fmt.Errorf("supavise system install-units: %w", err)
	}
	return nil
}

// ---- status report ------------------------------------------------------------------

type updateReport struct {
	Installed    string         `json:"installed"`
	Settings     config.Update  `json:"settings"`
	TimeZone     string         `json:"time_zone"`
	WindowOpen   bool           `json:"window_open"`
	NextWindow   string         `json:"next_window,omitempty"`
	Latest       string         `json:"latest,omitempty"`
	CheckedAt    string         `json:"checked_at,omitempty"`
	Available    bool           `json:"update_available"`
	LastResult   *update.Result `json:"last_unattended_upgrade,omitempty"`
	Blocked      string         `json:"automatic_upgrades_paused,omitempty"`
	Timer        string         `json:"timer,omitempty"`
	RebootNeeded bool           `json:"reboot_needed"`
}

func buildUpdateReport(ctx context.Context, cfg *config.Config, st update.State, now time.Time) (*updateReport, error) {
	win, err := cfg.Update.ParsedWindow()
	if err != nil {
		return nil, err
	}
	r := &updateReport{Installed: version, Settings: cfg.Update, TimeZone: now.Location().String(),
		WindowOpen: win.Contains(now), Latest: st.Latest, LastResult: st.Result, Blocked: st.Blocked}
	if t, ok := win.Next(now); ok {
		r.NextWindow = t.Format(time.RFC3339)
	}
	if !st.CheckedAt.IsZero() {
		r.CheckedAt = st.CheckedAt.Format(time.RFC3339)
	}
	r.Available = st.Latest != "" && selfupdate.Newer(st.Latest, version)
	if runtime.GOOS == "linux" {
		if out, _ := exec.CommandContext(ctx, "systemctl", "is-active", update.TimerUnit).Output(); len(out) > 0 {
			r.Timer = strings.TrimSpace(string(out))
		}
		r.RebootNeeded = cfg.Update.OSSecurityUpdates && update.HostRebootRequired(ctx)
	}
	return r, nil
}

func (r *updateReport) print(w io.Writer) {
	fmt.Fprintf(w, "Supavise %s\n", r.Installed)
	fmt.Fprintf(w, "Mode        %s", r.Settings.Mode)
	if r.Settings.Mode == config.UpdateNotify {
		fmt.Fprint(w, " (logs that a release is available; `sudo supavise update config --mode auto` upgrades inside the window)")
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Window      %s (%s)", r.Settings.Window, r.TimeZone)
	switch {
	case r.WindowOpen:
		fmt.Fprint(w, ", open now")
	case r.NextWindow != "":
		fmt.Fprintf(w, ", next opens %s", r.NextWindow)
	}
	fmt.Fprintln(w)
	check := "every " + r.Settings.CheckInterval
	if strings.EqualFold(r.Settings.CheckInterval, "off") {
		check = "off"
	}
	fmt.Fprintf(w, "Release     %s check %s", r.Settings.Channel, check)
	switch {
	case r.Latest == "":
		fmt.Fprint(w, "; not checked yet")
	case r.Available:
		fmt.Fprintf(w, "; %s is available (checked %s)", r.Latest, r.CheckedAt)
	default:
		fmt.Fprintf(w, "; %s is the latest (checked %s)", r.Latest, r.CheckedAt)
	}
	fmt.Fprintln(w)
	osLine := "off"
	if r.Settings.OSSecurityUpdates {
		osLine = "security updates on, reboot " + r.Settings.OSReboot
		if r.RebootNeeded {
			osLine += "; a reboot is waiting"
		}
	}
	fmt.Fprintf(w, "OS updates  %s\n", osLine)
	if r.Timer != "" {
		fmt.Fprintf(w, "Timer       %s %s\n", update.TimerUnit, r.Timer)
	}
	if r.LastResult != nil {
		fmt.Fprintf(w, "Last auto   exit %d at %s: %s\n", r.LastResult.Exit, r.LastResult.At.Format(time.RFC3339), r.LastResult.Meaning)
	}
	if r.Blocked != "" {
		fmt.Fprintf(w, "PAUSED      automatic upgrades wait for you (%s); check `supavise status`, then `sudo supavise update resume`\n", r.Blocked)
	}
}

// ---- OS updates -----------------------------------------------------------------------

// applyOSUpdates sets up unattended security updates, or removes what Supavise wrote.
func applyOSUpdates(ctx context.Context, out, errOut io.Writer, enable bool) error {
	if !enable {
		for _, f := range []string{update.AptConfigFile, update.NeedrestartFile} {
			b, err := os.ReadFile(f)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if !bytes.Contains(b, []byte("Managed by Supavise")) {
				fmt.Fprintf(out, "%s is not Supavise's file; left alone\n", f)
				continue
			}
			if err := os.Remove(f); err != nil {
				return err
			}
			fmt.Fprintf(out, "removed %s\n", f)
		}
		return nil
	}
	rel, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return fmt.Errorf("cannot read /etc/os-release: %w", err)
	}
	osr := parseOSRelease(rel)
	distro := update.Distro(osr)
	body, err := update.RenderAptConfig(distro)
	if err != nil {
		return fmt.Errorf("%s: %w; patch it yourself or pass --no-os-updates", osr["PRETTY_NAME"], err)
	}
	if err := ensureOSPackages(ctx, out, errOut); err != nil {
		return err
	}
	if err := writeIfChanged(update.AptConfigFile, []byte(body), out); err != nil {
		return err
	}
	if st, err := os.Stat(filepath.Dir(update.NeedrestartFile)); err == nil && st.IsDir() {
		if err := writeIfChanged(update.NeedrestartFile, []byte(update.RenderNeedrestart()), out); err != nil {
			return err
		}
	}
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		// The distribution's own timers do the patching; make sure they run.
		if o, err := exec.CommandContext(ctx, "systemctl", "enable", "--now", "apt-daily.timer", "apt-daily-upgrade.timer").CombinedOutput(); err != nil {
			fmt.Fprintf(errOut, "warning: could not enable apt-daily.timer and apt-daily-upgrade.timer: %v: %s\n", err, strings.TrimSpace(string(o)))
		}
	}
	return nil
}

func writeIfChanged(path string, body []byte, out io.Writer) error {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, body) {
		fmt.Fprintf(out, "%s is up to date\n", path)
		return nil
	}
	if err := writeFileAtomic(path, body, 0o644, 0, 0); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s\n", path)
	return nil
}

// ensureOSPackages installs the packages that are missing. needrestart's hook on apt is switched
// off for the run, so that installing it never restarts a service.
func ensureOSPackages(ctx context.Context, out, errOut io.Writer) error {
	var missing []string
	for _, p := range update.OSPackages {
		if !dpkgInstalled(ctx, p) {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	fmt.Fprintf(out, "installing %s\n", strings.Join(missing, ", "))
	env := append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_SUSPEND=1")
	apt := func(args ...string) error {
		c := exec.CommandContext(ctx, "apt-get", args...)
		c.Env, c.Stdout, c.Stderr = env, out, errOut
		return c.Run()
	}
	if err := apt("update", "-qq"); err != nil {
		fmt.Fprintf(errOut, "warning: apt-get update failed: %v\n", err)
	}
	if err := apt(append([]string{"install", "-y", "-qq"}, missing...)...); err != nil {
		return fmt.Errorf("apt-get install %s: %w", strings.Join(missing, " "), err)
	}
	return nil
}

func dpkgInstalled(ctx context.Context, pkg string) bool {
	b, err := exec.CommandContext(ctx, "dpkg-query", "-W", "-f=${Status}", pkg).Output()
	return err == nil && strings.TrimSpace(string(b)) == "install ok installed"
}
