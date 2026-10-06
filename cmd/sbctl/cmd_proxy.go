package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/OWNER/sbctl/internal/proxy"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// The production binary runs the proxy inside its main service next to the
// Management API. This command runs the edge alone against an existing registry,
// for development and for debugging routing and certificates.
func init() {
	var dsn string
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Run only the HTTPS edge (development)",
		Long: `Runs the edge proxy without the Management API: host to project routing, apikey
handling and TLS, against the registry in $SBCTL_REGISTRY_DSN (recommended) or
--registry-dsn (the DSN, password included, is visible in the process list).
The master key at key_path must already exist. api.<domain> answers 503.
Listen addresses, ports and TLS come from the config file and SBCTL_* variables,
for example SBCTL_TLS_MODE=off SBCTL_LISTEN_HTTP=127.0.0.1:33080 SBCTL_LISTEN_HTTPS=127.0.0.1:33443.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if dsn == "" {
				dsn = os.Getenv("SBCTL_REGISTRY_DSN")
			}
			if dsn == "" {
				return fmt.Errorf("proxy: --registry-dsn or SBCTL_REGISTRY_DSN is required")
			}
			ctx := cmd.Context()
			reg, err := registry.Open(ctx, dsn)
			if err != nil {
				return err
			}
			defer reg.Close()
			// Load, never create: a wrong key_path must not mint a new master key that
			// cannot open any project's sealed secrets.
			keyBody, err := os.ReadFile(cfg.KeyPath)
			if err != nil {
				return fmt.Errorf("proxy: reading the master key (key_path): %w", err)
			}
			sec, err := secrets.Load(keyBody)
			if err != nil {
				return err
			}
			var lvl slog.LevelVar
			if err := lvl.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
				return fmt.Errorf("proxy: log_level: %w", err)
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &lvl}))
			srv, err := proxy.New(proxy.Options{
				Config: cfg, Registry: reg, Keys: proxy.RegistryKeys{Registry: reg, Secrets: sec}, Logger: log,
			})
			if err != nil {
				return err
			}
			return srv.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&dsn, "registry-dsn", "", "Postgres DSN of the sbctl registry database; prefer $SBCTL_REGISTRY_DSN, a flag is visible in the process list")
	rootCmd.AddCommand(cmd)
}
