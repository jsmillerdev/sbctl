package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/alerts"
)

var alertsCmd = &cobra.Command{
	Use:   "alerts",
	Short: "Test the alert destinations and list the alerts that are still active",
	Long: `The daemon tells the operator when the node needs them (a disk running low, a failed or stale
backup, a project that does not answer, a certificate that will not renew, a release to
install). Destinations are webhooks and email, set in config.toml under [alerts]; email goes
through the [mail] relay.`,
}

func init() {
	test := &cobra.Command{
		Use:   "test",
		Short: "Send a test alert to every destination",
		Long: `Sends one test alert to every webhook and recipient in [alerts] and prints the result of each.
It ignores de-duplication and the hourly limit. The exit status is 1 when no destination is
configured or any delivery failed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			res, err := alerts.New(cfg, alerts.Options{Log: newLogger(cfg)}).Test(cmd.Context())
			if err != nil {
				return err
			}
			failed := 0
			for _, r := range res {
				if r.Err != nil {
					failed++
					fmt.Fprintf(cmd.OutOrStdout(), "FAILED  %s: %v\n", r.Destination, r.Err)
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "ok      %s\n", r.Destination)
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d destination(s) failed", failed, len(res))
			}
			return nil
		},
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List the alerts that were sent and are not resolved",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			active, err := alerts.New(cfg, alerts.Options{}).Active()
			if err != nil {
				return err
			}
			if len(active) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no active alerts")
				return nil
			}
			keys := make([]string, 0, len(active))
			for k := range active {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			t := newTable(cmd.OutOrStdout())
			fmt.Fprintln(t, "KIND\tSEVERITY\tPROJECT\tSINCE\tTITLE")
			for _, k := range keys {
				a := active[k]
				ref := a.Ref
				if ref == "" {
					ref = "-"
				}
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", a.Kind, a.Severity, ref, a.FirstSent.Local().Format(time.DateTime), a.Title)
			}
			return t.Flush()
		},
	}
	alertsCmd.AddCommand(test, list)
	rootCmd.AddCommand(alertsCmd)
}
