package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/api"
)

func init() {
	apiCmd := &cobra.Command{
		Use:   "api",
		Short: "Management API helpers",
	}
	var format, name string
	profile := &cobra.Command{
		Use:   "profile",
		Short: "Print the profile file the Supabase CLI reads with --profile",
		Long: `Print the profile that points the Supabase CLI at this installation's Management API:

  supavise api profile --format yaml > supavise-profile.yaml
  supabase --profile ./supavise-profile.yaml login --token sbp_...
  supabase --profile ./supavise-profile.yaml projects list

The CLI chooses its parser from the file extension, so use the matching --format.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			out, err := api.NewCLIProfile(cfg, name).Render(format)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), out)
			return err
		},
	}
	profile.Flags().StringVar(&format, "format", "yaml", "yaml, toml or json")
	profile.Flags().StringVar(&name, "name", "supavise", "profile name (the CLI keeps one saved token per name)")
	apiCmd.AddCommand(profile)
	rootCmd.AddCommand(apiCmd)
}
