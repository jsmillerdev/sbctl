package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jsmillerdev/supavise/internal/notice"
)

var maintenanceCmd = &cobra.Command{
	Use:   "maintenance",
	Short: "Announce a maintenance window in the dashboard",
	Long: `An announced window shows a banner to every signed-in dashboard user while it is open
(and, with --notice, for that long before it opens). It changes nothing else: projects keep
running, and the alerts stay quiet for the length of the window so planned downtime does not
page anyone. An upgrade shows its own "upgrade in progress" banner while it runs.

Studio draws the banner with its own wording ("We are investigating a technical issue");
the message you give is kept for 'supavise status' and for anything else that reads
/api/incident-banner.`,
}

func init() {
	var at, until, duration, lead, message string
	announce := &cobra.Command{
		Use:   "announce --at <time> --message <text> [--duration 2h | --until <time>] [--notice 24h]",
		Short: "Announce a maintenance window",
		Long: `Announces a window that starts at --at and lasts --duration (default 2h) or ends at --until.
Times are RFC 3339 ("2026-10-12T22:00:00+02:00") or "2006-01-02 15:04" in UTC, or "now".
A new announcement replaces the old one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			now := time.Now()
			m, err := buildMaintenance(now, at, until, duration, lead, message)
			if err != nil {
				return err
			}
			got, err := notice.WriteMaintenance(cfg.Paths(), *m, now)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "announced: %s\n", describeMaintenance(got))
			return nil
		},
	}
	announce.Flags().StringVar(&at, "at", "", "when the window starts")
	announce.Flags().StringVar(&until, "until", "", "when the window ends (instead of --duration)")
	announce.Flags().StringVar(&duration, "duration", "2h", "how long the window lasts")
	announce.Flags().StringVar(&lead, "notice", "0", "show the banner this long before the window opens (for example 24h)")
	announce.Flags().StringVar(&message, "message", "", "what the dashboard's users should know")
	_ = announce.MarkFlagRequired("at")
	_ = announce.MarkFlagRequired("message")

	clearCmd := &cobra.Command{
		Use:   "clear",
		Short: "Remove the announced maintenance window",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			had, err := notice.ClearMaintenance(cfg.Paths())
			if err != nil {
				return err
			}
			if had {
				fmt.Fprintln(cmd.OutOrStdout(), "cleared")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "no maintenance window was announced")
			}
			return nil
		},
	}

	show := &cobra.Command{
		Use:   "show",
		Short: "Show the announced maintenance window",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			m, err := notice.ReadMaintenance(cfg.Paths())
			if err != nil {
				return err
			}
			if m == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "no maintenance window is announced")
				return nil
			}
			state := "announced"
			now := time.Now()
			switch {
			case now.After(m.EndsAt):
				state = "over (`supavise maintenance clear` removes it)"
			case m.InProgress(now):
				state = "in progress"
			case m.Active(now):
				state = "announced, banner showing"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s [%s]\n", describeMaintenance(m), state)
			return nil
		},
	}
	maintenanceCmd.AddCommand(announce, clearCmd, show)
	rootCmd.AddCommand(maintenanceCmd)
}

func buildMaintenance(now time.Time, at, until, duration, leadArg, message string) (*notice.Maintenance, error) {
	start, err := parseWhen(at, now)
	if err != nil {
		return nil, fmt.Errorf("--at: %w", err)
	}
	var end time.Time
	if until != "" {
		if end, err = parseWhen(until, now); err != nil {
			return nil, fmt.Errorf("--until: %w", err)
		}
	} else {
		d, err := time.ParseDuration(duration)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("--duration %q: want a duration such as 90m or 2h", duration)
		}
		end = start.Add(d)
	}
	var leadSecs int
	if leadArg != "" && leadArg != "0" {
		d, err := time.ParseDuration(leadArg)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("--notice %q: want a duration such as 24h", leadArg)
		}
		leadSecs = int(d.Seconds())
	}
	m := &notice.Maintenance{Message: strings.TrimSpace(message), StartsAt: start, EndsAt: end, LeadSeconds: leadSecs}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if !end.After(now) {
		return nil, errors.New("the window has already ended")
	}
	return m, nil
}

// parseWhen reads "now", RFC 3339, or a date and time without a zone, which is UTC.
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "now") {
		return now.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time: use RFC 3339 (2026-10-12T22:00:00Z), \"2026-10-12 22:00\" (UTC) or now", s)
}

func describeMaintenance(m *notice.Maintenance) string {
	s := fmt.Sprintf("%q from %s to %s", m.Message, m.StartsAt.Format(time.RFC3339), m.EndsAt.Format(time.RFC3339))
	if m.LeadSeconds > 0 {
		s += fmt.Sprintf(", banner %s ahead", (time.Duration(m.LeadSeconds) * time.Second).String())
	}
	return s
}
