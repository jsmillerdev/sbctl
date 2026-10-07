package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/OWNER/sbctl/deploy/systemd"
	"github.com/OWNER/sbctl/internal/backup"
	"github.com/OWNER/sbctl/internal/lifecycle"
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
cluster (sb-postgres@system), creates the sbctl, _supavisor, _realtime and _storage
databases, applies the registry migrations, records the system project and its sealed
credentials, and starts GoTrue (sb-gotrue@system) configured for Studio sign-in.
Running it again on an initialized node starts what is stopped and re-applies the
configuration. Run it as the user that owns the state directory (sbctl).`,
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
		Long:  "Starts the system cluster and GoTrue, then every project the registry lists as active. Use it after `sbctl system stop`; on a server the daemon and systemd do this at boot.",
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

	install := &cobra.Command{
		Use:   "install-units",
		Short: "Install the systemd units and the polkit rule, and enable the system units (needs root)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			// The embedded timer carries the default schedule; the node's own comes from
			// config and is written in the same pass, so a second run changes nothing.
			timer, err := backup.RenderBackupTimerChecked(cfg.Backup.BaseBackupOnCalendar)
			if err != nil {
				return fmt.Errorf("config backup.base_backup_on_calendar: %w", err)
			}
			changed, err := systemd.InstallWith(sysUnitDir, sysPolkitDir, map[string][]byte{backup.BackupTimerUnit: []byte(timer)})
			if err != nil {
				return err
			}
			for _, f := range changed {
				fmt.Fprintln(cmd.OutOrStdout(), "installed", f)
			}
			if len(changed) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "units are up to date")
			}
			apply, err := lifecycle.OpenOptions{Log: newLogger(cfg)}.UnitInstaller(cfg)
			if err != nil {
				fmt.Fprintln(os.Stderr, "run `systemctl daemon-reload` and enable the system units yourself:", err)
				return nil
			}
			return apply(cmd.Context(), len(changed) > 0)
		},
	}
	install.Flags().StringVar(&sysUnitDir, "unit-dir", "/etc/systemd/system", "where to write unit files")
	install.Flags().StringVar(&sysPolkitDir, "polkit-dir", "/etc/polkit-1/rules.d", "where to write the polkit rule (empty skips it)")

	systemCmd.AddCommand(initCmd, status, start, stop, install)
	rootCmd.AddCommand(systemCmd)
}
