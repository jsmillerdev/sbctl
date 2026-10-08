package main

import (
	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/app"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "serve",
		Short: "Run the daemon: Management API, HTTPS edge, project lifecycle (supavise.service)",
		Long: `Runs everything supavise.service runs: the Management API (on the loopback admin
listener and at api.<domain>), the edge proxy with automatic TLS, and the lifecycle engine.
It starts every active project at boot, finishes operations an earlier crash interrupted,
and shuts down gracefully on SIGTERM. Project units belong to systemd and keep running when
the daemon stops.

The system project must exist (run ` + "`supavise system init`" + ` once). Run as the supavise user.
Listen addresses, ports and TLS come from the config file and SUPAVISE_* variables.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return app.Serve(cmd.Context(), cfg, appOptions(cfg))
		},
	})
}
