package main

import (
	"github.com/spf13/cobra"

	"github.com/OWNER/sbctl/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// configPath is the --config flag; empty means $SBCTL_CONFIG or /etc/sbctl/config.toml.
var configPath string

// rootCmd is the CLI root. Each area adds its subcommands from its own file
// (cmd_<area>.go) in an init function; do not register commands here.
var rootCmd = &cobra.Command{
	Use:           "sbctl",
	Short:         "Multi-project Supabase on one machine",
	Version:       version,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "", "config file (default $SBCTL_CONFIG or "+config.DefaultPath+")")
}

// loadConfig is shared by every subcommand.
func loadConfig() (*config.Config, error) { return config.Load(configPath) }
