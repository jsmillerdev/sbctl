package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/notice"
)

var maintenanceCmd = &cobra.Command{
	Use:   "maintenance",
	Short: "Announce a maintenance window to the operator and the alerts",
	Long: `An announced window is shown by 'supavise status' and /healthz/detail while it is
open, and the alerts stay quiet about the project and shared-service problems the window
restarts, so planned downtime does not page anyone. A critical problem with the system cluster or
the registry, a low disk, a failed backup and an expiring certificate are still sent. Projects
keep running. An upgrade is shown the same way while it runs.

The dashboard banner stays empty for now: Studio draws any incident as "We are investigating a
technical issue" with a link to Supabase's status page, which would present planned work as an
outage. The message you give is kept for 'supavise status' and for the day Studio can show it.`,
}

func init() {
	var at, until, duration, lead, message string
	var allowLong bool
	announce := &cobra.Command{
		Use:   "announce --at <time> --message <text> [--duration 2h | --until <time>] [--notice 24h]",
		Short: "Announce a maintenance window",
		Long: `Announces a window that starts at --at and lasts --duration (default 2h) or ends at --until.
Times are RFC 3339 ("2026-10-12T22:00:00+02:00") or "2006-01-02 15:04" in UTC, or "now".
A window longer than 24h needs --allow-long, because alerts stay quiet for all of it.
A new announcement replaces the old one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			now := time.Now()
			m, err := buildMaintenance(now, at, until, duration, lead, message, allowLong)
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
	announce.Flags().StringVar(&lead, "notice", "0", "announce the window this long before it opens (for example 24h); only affects a dashboard banner, which is not shown yet")
	announce.Flags().StringVar(&message, "message", "", "what the operators should know (shown by 'supavise status'; the dashboard does not show it yet)")
	announce.Flags().BoolVar(&allowLong, "allow-long", false, "allow a window longer than 24h; alerts stay quiet for all of it")
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
				state = "announced, notice period"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s [%s]\n", describeMaintenance(m), state)
			return nil
		},
	}
	maintenanceCmd.AddCommand(announce, clearCmd, show)
	rootCmd.AddCommand(maintenanceCmd)
}

func buildMaintenance(now time.Time, at, until, duration, leadArg, message string, allowLong bool) (*notice.Maintenance, error) {
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
	m := &notice.Maintenance{Message: strings.TrimSpace(message), StartsAt: start, EndsAt: end, LeadSeconds: leadSecs, Extended: allowLong}
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
		s += fmt.Sprintf(", notice %s ahead", (time.Duration(m.LeadSeconds) * time.Second).String())
	}
	return s
}
