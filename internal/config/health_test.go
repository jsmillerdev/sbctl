package config

import (
	"testing"
	"time"
)

// A node whose base backup runs weekly must not report every project stale after 36 hours.
func TestBackupStaleFollowsTheBackupSchedule(t *testing.T) {
	for _, tc := range []struct {
		calendar string
		hours    int
		want     time.Duration
	}{
		{"*-*-* 03:00:00", 0, 36 * time.Hour},
		{"", 0, 36 * time.Hour},
		{"daily", 0, 36 * time.Hour},
		{"hourly", 0, 13 * time.Hour},
		{"*-*-* *:00:00", 0, 13 * time.Hour},
		{"weekly", 0, 7*24*time.Hour + 12*time.Hour},
		{"Sun *-*-* 02:00:00", 0, 7*24*time.Hour + 12*time.Hour},
		{"Mon..Fri *-*-* 03:00:00", 0, 7*24*time.Hour + 12*time.Hour}, // a weekend gap is not stale
		{"monthly", 0, 31*24*time.Hour + 12*time.Hour},
		{"*-*-01 03:00:00", 0, 31*24*time.Hour + 12*time.Hour},
		{"weekly", 48, 48 * time.Hour}, // an explicit value wins
	} {
		c := Default()
		c.Backup.BaseBackupOnCalendar = tc.calendar
		c.Health.BackupStaleHours = tc.hours
		if got := c.BackupStale(); got != tc.want {
			t.Errorf("calendar %q, backup_stale_hours %d: %s, want %s", tc.calendar, tc.hours, got, tc.want)
		}
	}
}
