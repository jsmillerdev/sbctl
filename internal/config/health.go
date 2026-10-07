package config

import (
	"fmt"
	"strings"
	"time"
)

// Health is the [health] config section: the thresholds `supavise status`, the public and
// operator health endpoints and the alert checker (internal/health, internal/alerts) judge
// the node by. Everything has a working default.
type Health struct {
	// DiskLowPercent is the free space, in percent of the state directory's disk, below which
	// the node is degraded and a disk_low alert is raised. Half of it is critical. 0 means 10.
	DiskLowPercent int `toml:"disk_low_percent"`
	// DiskLowGB is an absolute floor next to DiskLowPercent: less free space than this many
	// GiB is low too, whatever the percentage (a 2 TB volume at 10% still has 200 GB; a 40 GB
	// one at 10% has 4). 0 means 5.
	DiskLowGB int `toml:"disk_low_gb"`
	// BackupStaleHours is how old a project's newest completed base backup may be before it
	// counts as stale. 0 means the period of [backup] base_backup_on_calendar plus 12 hours
	// (36 for the default nightly timer): one missed run is a warning, not a flap. Set it for a
	// calendar the node cannot read the period from (see BackupStale).
	BackupStaleHours int `toml:"backup_stale_hours"`
	// CertificateWarnDays is how many days before a certificate expires the node is degraded
	// and a certificate_expiring alert is raised. CertMagic renews at 30 days, so a certificate
	// this close to its end means renewal is failing. 0 means 14.
	CertificateWarnDays int `toml:"certificate_warn_days"`
	// CacheSeconds is how long the daemon reuses a health report for /healthz and the alert
	// checker before it probes again. 0 means 20.
	CacheSeconds int `toml:"cache_seconds"`
}

const (
	defaultDiskLowPercent = 10
	defaultDiskLowGB      = 5
	backupStaleSlack      = 12 * time.Hour
	defaultCertWarnDays   = 14
	defaultHealthCache    = 20 * time.Second
)

// DiskLow returns the warning thresholds: free percent and free bytes.
func (h Health) DiskLow() (percent int, bytes int64) {
	percent, gb := h.DiskLowPercent, h.DiskLowGB
	if percent <= 0 {
		percent = defaultDiskLowPercent
	}
	if gb <= 0 {
		gb = defaultDiskLowGB
	}
	return percent, int64(gb) << 30
}

// BackupStale is the age past which a project's newest base backup is stale: [health]
// backup_stale_hours, or the period of the base backup timer plus 12 hours.
func (c *Config) BackupStale() time.Duration {
	if h := c.Health.BackupStaleHours; h > 0 {
		return time.Duration(h) * time.Hour
	}
	return calendarPeriod(c.Backup.BaseBackupOnCalendar) + backupStaleSlack
}

// calendarPeriod is how long apart the runs of a systemd OnCalendar expression are, read from
// the common forms and rounded up when unsure: a day for a daily time of day (and for what it
// does not recognize), an hour for an hourly one, a week for a weekday list or "weekly", a
// month for "monthly". A longer period only makes the stale check slower to speak, never wrong.
func calendarPeriod(expr string) time.Duration {
	e := strings.ToLower(strings.TrimSpace(expr))
	switch {
	case e == "":
		return 24 * time.Hour
	case e == "hourly" || strings.Contains(e, "*-*-* *:"):
		return time.Hour
	case e == "yearly" || e == "annually" || e == "quarterly" || e == "semiannually":
		return 366 * 24 * time.Hour
	case e == "monthly" || (strings.HasPrefix(e, "*-*-") && !strings.HasPrefix(e, "*-*-*")) || strings.HasPrefix(e, "*-*~"):
		return 31 * 24 * time.Hour
	case e == "weekly" || hasWeekday(e):
		return 7 * 24 * time.Hour
	}
	return 24 * time.Hour
}

func hasWeekday(e string) bool {
	for _, d := range []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"} {
		if strings.Contains(e, d) {
			return true
		}
	}
	return false
}

// CertificateWarn is the remaining lifetime under which a certificate is reported.
func (h Health) CertificateWarn() time.Duration {
	if h.CertificateWarnDays <= 0 {
		return defaultCertWarnDays * 24 * time.Hour
	}
	return time.Duration(h.CertificateWarnDays) * 24 * time.Hour
}

// Cache is how long a health report is reused.
func (h Health) Cache() time.Duration {
	if h.CacheSeconds <= 0 {
		return defaultHealthCache
	}
	return time.Duration(h.CacheSeconds) * time.Second
}

func (h Health) validate() error {
	if h.DiskLowPercent < 0 || h.DiskLowPercent > 90 {
		return fmt.Errorf("config: health.disk_low_percent must be between 0 and 90, not %d", h.DiskLowPercent)
	}
	for name, v := range map[string]int{
		"disk_low_gb": h.DiskLowGB, "backup_stale_hours": h.BackupStaleHours,
		"certificate_warn_days": h.CertificateWarnDays, "cache_seconds": h.CacheSeconds,
	} {
		if v < 0 {
			return fmt.Errorf("config: health.%s must not be negative, not %d", name, v)
		}
	}
	return nil
}
