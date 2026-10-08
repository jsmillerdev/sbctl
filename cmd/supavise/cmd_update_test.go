package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/health"
	"github.com/supavise/supavise/internal/update"
)

func TestInstallUpdateFlags(t *testing.T) {
	// A new install: notify mode, the default window, OS security updates on.
	cfg := config.Default()
	if err := applyInstall(cfg, installOptions{Fresh: true}, changedSet()); err != nil {
		t.Fatal(err)
	}
	if cfg.Update.Mode != config.UpdateNotify || !cfg.Update.OSSecurityUpdates || cfg.Update.OSReboot != "window" {
		t.Fatalf("a new install: %+v", cfg.Update)
	}

	// --no-os-updates opts out of the OS patching of a new install.
	cfg = config.Default()
	if err := applyInstall(cfg, installOptions{Fresh: true, NoOSUpdates: true}, changedSet()); err != nil {
		t.Fatal(err)
	}
	if cfg.Update.OSSecurityUpdates {
		t.Fatal("--no-os-updates left OS security updates on")
	}

	// --auto-upgrade and --maintenance-window set the mode and the window; --os-reboot the reboot.
	cfg = config.Default()
	o := installOptions{Fresh: true, AutoUpgrade: true, MaintenanceWindow: "Sat 02:00-04:00", OSReboot: "never"}
	if err := applyInstall(cfg, o, changedSet("auto-upgrade", "maintenance-window", "os-reboot")); err != nil {
		t.Fatal(err)
	}
	if cfg.Update.Mode != "auto" || cfg.Update.Window != "Sat 02:00-04:00" || cfg.Update.OSReboot != "never" {
		t.Fatalf("%+v", cfg.Update)
	}
	// --auto-upgrade=false goes back to notify.
	if err := applyInstall(cfg, installOptions{AutoUpgrade: false}, changedSet("auto-upgrade")); err != nil {
		t.Fatal(err)
	}
	if cfg.Update.Mode != "notify" {
		t.Fatalf("--auto-upgrade=false: %+v", cfg.Update)
	}

	// Bad values are refused with the flag's name.
	for flag, bad := range map[string]installOptions{
		"maintenance-window": {MaintenanceWindow: "Sunday morning"},
		"os-reboot":          {OSReboot: "maybe"},
	} {
		if err := applyInstall(config.Default(), bad, changedSet(flag)); err == nil {
			t.Errorf("--%s with a bad value was accepted", flag)
		}
	}
	if err := applyInstall(config.Default(), installOptions{MaintenanceWindow: "nonsense"}, changedSet("maintenance-window")); err == nil ||
		!strings.Contains(err.Error(), "--maintenance-window") {
		t.Errorf("error does not name the flag: %v", err)
	}
}

// The [update] settings survive a re-run of the installer: a re-run with no flags keeps them, a
// re-run with one flag changes that setting only, and an existing node's file (no [update]
// section) is not patched by a re-run.
func TestUpdateSettingsSurviveAnInstallerRerun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := config.Default()
	o := installOptions{Fresh: true, AutoUpgrade: true, MaintenanceWindow: "Sat,Sun 01:00-03:00"}
	if err := applyInstall(cfg, o, changedSet("auto-upgrade", "maintenance-window")); err != nil {
		t.Fatal(err)
	}
	body, err := renderConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	// Only differences from the defaults are written.
	if !strings.Contains(string(body), "[update]") || strings.Contains(string(body), "channel") || strings.Contains(string(body), "check_interval") {
		t.Fatalf("rendered config:\n%s", body)
	}

	// Re-run, no flags.
	again, existed, err := readConfigFile(path)
	if err != nil || !existed {
		t.Fatal(err)
	}
	if err := applyInstall(again, installOptions{Fresh: !existed}, changedSet()); err != nil {
		t.Fatal(err)
	}
	if again.Update.Mode != "auto" || again.Update.Window != "Sat,Sun 01:00-03:00" || !again.Update.OSSecurityUpdates {
		t.Fatalf("a re-run lost the settings: %+v", again.Update)
	}
	// Re-run with one flag: the window changes, the mode stays.
	if err := applyInstall(again, installOptions{MaintenanceWindow: "Sun 05:00-06:00"}, changedSet("maintenance-window")); err != nil {
		t.Fatal(err)
	}
	if again.Update.Mode != "auto" || again.Update.Window != "Sun 05:00-06:00" {
		t.Fatalf("%+v", again.Update)
	}
	// Opting out of OS updates on a re-run sticks.
	if err := applyInstall(again, installOptions{NoOSUpdates: true}, changedSet()); err != nil {
		t.Fatal(err)
	}
	b2, _ := renderConfig(again)
	next, _, _ := readConfigFile(writeTemp(t, b2))
	if err := applyInstall(next, installOptions{}, changedSet()); err != nil {
		t.Fatal(err)
	}
	if next.Update.OSSecurityUpdates {
		t.Fatal("a re-run turned OS updates back on")
	}

	// A node installed before the section existed: its file has no [update], and a re-run (not Fresh)
	// leaves OS patching off and notify mode.
	old := writeTemp(t, []byte("domain = \"example.com\"\n"))
	oc, existed, err := readConfigFile(old)
	if err != nil || !existed {
		t.Fatal(err)
	}
	if err := applyInstall(oc, installOptions{Fresh: false}, changedSet()); err != nil {
		t.Fatal(err)
	}
	if oc.Update.OSSecurityUpdates || oc.Update.Mode != "notify" || oc.Update.RebootsInWindow() {
		t.Fatalf("an existing node was changed by a re-run: %+v", oc.Update)
	}
}

func writeTemp(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUpdateConfigFlagsPersistThroughTheConfigFile(t *testing.T) {
	cfg := config.Default()
	changed := changedSet("mode", "window", "check-interval", "os-security-updates", "os-reboot")
	if err := applyUpdateFlags(&cfg.Update, changed, "auto", "Mon-Fri 01:00-02:00", "", "12h", "never", true); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := renderConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := config.Default()
	if err := toml.Unmarshal(body, got); err != nil {
		t.Fatal(err)
	}
	u := got.Update
	if u.Mode != "auto" || u.Window != "Mon-Fri 01:00-02:00" || u.CheckInterval != "12h" || u.OSReboot != "never" || !u.OSSecurityUpdates {
		t.Fatalf("persisted %+v", u)
	}
	if u.Channel != "stable" {
		t.Fatalf("untouched keys keep their defaults: %+v", u)
	}

	// A bad window is refused before anything is written.
	if err := applyUpdateFlags(&config.Default().Update, changedSet("window"), "", "someday", "", "", "", false); err == nil {
		t.Error("a bad window was accepted")
	}
	// Flags that were not given change nothing.
	c2 := config.Default()
	if err := applyUpdateFlags(&c2.Update, changedSet(), "auto", "x", "y", "z", "q", true); err != nil || c2.Update != config.DefaultUpdate() {
		t.Errorf("unchanged flags changed the settings: %+v, %v", c2.Update, err)
	}
}

func TestUpdateReport(t *testing.T) {
	cfg := config.Default()
	cfg.Update.Mode = "auto"
	cfg.Update.Window = "Sun 03:00-05:00"
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) // a Wednesday
	st := update.State{Result: &update.Result{At: now.Add(-72 * time.Hour), Exit: 0, Meaning: "upgraded, or nothing to do"}}
	rec := &health.UpdateRecord{Latest: "v9.9.9", CheckedAt: now.Add(-time.Hour)} // the daemon's check
	r, err := buildUpdateReport(context.Background(), cfg, st, rec, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.WindowOpen || r.NextWindow != "2026-10-11T03:00:00Z" || !r.Available {
		t.Fatalf("%+v", r)
	}
	var out bytes.Buffer
	r.print(&out)
	for _, want := range []string{"Mode        auto", "Sun 03:00-05:00", "next opens 2026-10-11T03:00:00Z", "v9.9.9 is available", "exit 0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output lacks %q:\n%s", want, out.String())
		}
	}
	st.Blocked = "needs the operator"
	r, _ = buildUpdateReport(context.Background(), cfg, st, rec, now)
	out.Reset()
	r.print(&out)
	if !strings.Contains(out.String(), "PAUSED") || !strings.Contains(out.String(), "supavise update resume") {
		t.Errorf("a paused node must say so:\n%s", out.String())
	}
}

// install-units writes supavise-upgrade.timer from the node's [update] settings, and a second run
// finds nothing to change.
func TestInstallUnitsRendersTheUpgradeTimerFromTheConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	body := "supervisor = \"exec\"\nstate_dir = " + `"` + dir + `"` + "\n[update]\nmode = \"auto\"\nwindow = \"Sat 02:00-03:00\"\ncheck_interval = \"off\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	units := filepath.Join(dir, "units")
	args := []string{"--config", cfgPath, "system", "install-units", "--unit-dir", units, "--polkit-dir", ""}
	out, err := runRoot(t, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(units, update.TimerUnit))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"OnCalendar=Sat *-*-* 02:00:00", "OnCalendar=Sat *-*-* 02:45:00"} {
		if !strings.Contains(got, want) {
			t.Errorf("timer lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "OnBootSec") {
		t.Errorf("the daemon checks for releases; the timer has no boot check:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(units, update.ServiceUnit)); err != nil {
		t.Errorf("the service unit was not installed: %v", err)
	}
	out, err = runRoot(t, args...)
	if err != nil || !strings.Contains(out, "units are up to date") {
		t.Errorf("second run: %v\n%s", err, out)
	}
	// A bad window never reaches a unit file.
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(body, "Sat 02:00-03:00", "someday", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runRoot(t, args...); err == nil {
		t.Errorf("a bad window was accepted:\n%s", out)
	}
}

// Every reader of config.toml accepts the documented forms of check_interval, a bare number
// of seconds included: the installer, `update config` and `self-update` all read through
// readConfigFile, and a file that loads for the daemon must load for them.
func TestReadConfigFileAcceptsABareCheckInterval(t *testing.T) {
	for body, want := range map[string]struct {
		every time.Duration
		on    bool
	}{
		"[update]\ncheck_interval = 7200\n":    {2 * time.Hour, true},
		"[update]\ncheck_interval = 0\n":       {0, false},
		"[update]\ncheck_interval = \"2d\"\n":  {48 * time.Hour, true},
		"[update]\ncheck_interval = \"off\"\n": {0, false},
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, existed, err := readConfigFile(path)
		if err != nil || !existed {
			t.Errorf("%q: %v", body, err)
			continue
		}
		if d, on, err := cfg.Update.CheckEvery(); err != nil || on != want.on || d != want.every {
			t.Errorf("%q: CheckEvery() = %v, %v, %v", body, d, on, err)
		}
		// printInstallConfig renders what readConfigFile read; the value survives that.
		out, err := renderConfig(cfg)
		if err != nil {
			t.Errorf("%q: render: %v", body, err)
			continue
		}
		again := config.Default()
		if err := config.DecodeTOML(out, again); err != nil || again.Update.CheckInterval != cfg.Update.CheckInterval {
			t.Errorf("%q: rendered %q read back as %q (%v)", body, out, again.Update.CheckInterval, err)
		}
	}
}
