package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/diskquota"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/update"
)

var systemCmd = &cobra.Command{
	Use:   "system",
	Short: "Set up and inspect the system project (registry and dashboard auth)",
}

var (
	sysNoFetch   bool
	sysUnitDir   string
	sysPolkitDir string
)

func init() {
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Create or repair the system project",
		Long: `Creates the master key, fetches the artifacts, initializes and starts the system
cluster (supavise-postgres@system), creates the supavise, _supavisor, _realtime and _storage
databases, applies the registry migrations, records the system project and its sealed
credentials, and starts GoTrue (supavise-gotrue@system) configured for Studio sign-in.
Running it again on an initialized node starts what is stopped and re-applies the
configuration. Run it as the user that owns the state directory (supavise).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			n, err := lifecycle.InitSystem(cmd.Context(), cfg, openOptions(cfg), !sysNoFetch)
			if err != nil {
				return err
			}
			defer n.Close()
			hs, _ := n.Engine.Health(cmd.Context(), "system")
			healthTable(cmd.OutOrStdout(), hs)
			fmt.Fprintf(cmd.OutOrStdout(), "system project ready (postgres 127.0.0.1:%d, gotrue 127.0.0.1:%d)\n", cfg.Ports.SystemPostgres, cfg.Ports.SystemGoTrue)
			return nil
		},
	}
	initCmd.Flags().BoolVar(&sysNoFetch, "no-fetch", false, "do not download artifacts; use what is in place")

	status := &cobra.Command{
		Use:   "status",
		Short: "Check the system project's services",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			hs, err := lifecycle.SystemStatus(cmd.Context(), cfg, openOptions(cfg))
			if err != nil {
				return err
			}
			healthTable(cmd.OutOrStdout(), hs)
			if !healthy(hs) {
				return fmt.Errorf("system project is not healthy")
			}
			return nil
		},
	}

	start := &cobra.Command{
		Use:   "start",
		Short: "Start the system project and every active project",
		Long:  "Starts the system cluster and GoTrue, then every project the registry lists as active. Use it after `supavise system stop`; on a server the daemon and systemd do this at boot.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			n, err := lifecycle.InitSystem(cmd.Context(), cfg, openOptions(cfg), false)
			if err != nil {
				return err
			}
			defer n.Close()
			errs := n.Engine.StartActive(cmd.Context())
			for ref, err := range errs {
				fmt.Fprintf(os.Stderr, "project %s: %v\n", ref, err)
			}
			if len(errs) > 0 {
				return fmt.Errorf("%d project(s) failed to start", len(errs))
			}
			fmt.Fprintln(cmd.OutOrStdout(), "started")
			return nil
		},
	}

	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop every project's units, then the system project's",
		Long:  "Stops all units found under the state directory without touching the data or the registry. Use it to shut a development node down; on a server use systemctl.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lifecycle.StopAll(cmd.Context(), cfg, openOptions(cfg))
		},
	}

	var (
		convergeCheck, convergeJSON bool
	)
	converge := &cobra.Command{
		Use:   "converge",
		Short: "Bring this server's units, directories, mounts, firewall and packages to what this release expects (needs root)",
		Long: `Runs the host layer of a release: a list of idempotent steps that bring the server to what this
binary expects. A second run changes nothing. The steps, in order:

  units        the systemd units and the polkit rule (the backup and upgrade timers from config.toml)
  directories  /etc/supavise/cluster and /etc/supavise/config.d, mode 0750, owned by supavise
  mounts       RequiresMountsFor drop-ins for every supavise unit whose state directory or config
               directory is a mount, so that a unit never starts against an empty directory
               when its volume is missing
  ufw          the mesh port (7443/tcp) when ufw is active
  packages     the packages the release declares, if any
  config.d     on a node that is in a cluster, the cluster settings from the leader

Each step prints what it changed. When every step succeeded, converge writes the revision it
completed to <state_dir>/converged; the daemon raises host_not_converged while that file is behind
the binary, and ` + "`supavise upgrade`" + ` runs converge after it swaps the binary and fails, and rolls back,
when it does not succeed.

--check changes nothing and needs no root: it prints, for every step, whether it has something to
do. --json prints a list of {id, title, pending, needs_root, detail} for --check and the same with
what changed after a run. Exit status 0 means the command ran; --check reports pending steps in its
output and does not signal them in the status.

` + "`install-units`" + ` is an alias: an older ` + "`supavise upgrade`" + ` runs it on the new binary right after the swap,
which is how a node reaches this command from a release that does not know it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConverge(cmd, convergeCheck, convergeJSON)
		},
	}
	install := &cobra.Command{
		Use:   "install-units",
		Short: "Alias of converge: install the systemd units and the polkit rule, and everything else the release expects (needs root)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConverge(cmd, convergeCheck, convergeJSON)
		},
	}
	for _, c := range []*cobra.Command{converge, install} {
		c.Flags().BoolVar(&convergeCheck, "check", false, "change nothing: print, for every step, whether it has something to do (needs no root)")
		c.Flags().BoolVar(&convergeJSON, "json", false, "print the steps as JSON")
		c.Flags().StringVar(&sysUnitDir, "unit-dir", defaultUnitDir, "where to write unit files; any other directory is a test: only the units are installed there, nothing is reloaded and the host is not marked converged")
		c.Flags().StringVar(&sysPolkitDir, "polkit-dir", "/etc/polkit-1/rules.d", "where to write the polkit rule (empty skips it)")
	}

	quota := &cobra.Command{
		Use:    "set-disk-quota <ref>",
		Short:  "Apply a project's stored disk limit as an XFS project quota (run by supavise-diskquota@<ref>, as root)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !secrets.ValidRef(args[0]) {
				return fmt.Errorf("%q is not a project ref", args[0])
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return diskquota.New(cfg, nil).ApplyStored(cmd.Context(), args[0])
		},
	}
	systemCmd.AddCommand(initCmd, status, start, stop, converge, install, quota)
	rootCmd.AddCommand(systemCmd)
}

const defaultUnitDir = "/etc/systemd/system"

// applyUpgradeTimer starts supavise-upgrade.timer, or stops it, according to [update]: it runs for
// auto upgrades and for the OS reboot in the window, and not at all when neither is on. A timer
// whose file changed is restarted, so that systemd schedules it again from the new file.
func applyUpgradeTimer(ctx context.Context, cfg *config.Config, fileChanged bool, out, errOut io.Writer) error {
	if cfg.Supervisor != config.SupervisorSystemd {
		return nil
	}
	systemctl := func(args ...string) error {
		o, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(o)))
		}
		return nil
	}
	if !cfg.Update.TimerWanted() {
		if err := systemctl("disable", "--now", update.TimerUnit); err != nil {
			// Not enabled is the normal case here; a real failure shows in `supavise update status`.
			fmt.Fprintf(errOut, "note: %v\n", err)
		}
		return nil
	}
	if err := systemctl("enable", "--now", update.TimerUnit); err != nil {
		return err
	}
	if fileChanged {
		if err := systemctl("restart", update.TimerUnit); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "%s is enabled (%s mode, window %s)\n", update.TimerUnit, cfg.Update.Mode, cfg.Update.Window)
	return nil
}
