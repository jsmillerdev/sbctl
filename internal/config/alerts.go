package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Alerts is the [alerts] config section: where internal/alerts sends what the node raises
// (a disk running low, a stale backup, an unhealthy project, a certificate near its end, a new
// release). Without a webhook or a recipient nothing leaves the node; every alert is still
// logged under the message key "alert".
type Alerts struct {
	// Webhooks are the endpoints that receive each alert as a JSON POST. Write them as
	// [[alerts.webhooks]] tables. The list has no environment override; the secrets sit in
	// config.toml in plain text, like the backup keys: keep the file mode 0600.
	Webhooks []AlertWebhook `toml:"webhooks"`
	// EmailTo is a comma-separated list of recipients. The mail goes through the [mail] SMTP
	// relay (smtp_host and smtp_from are required).
	EmailTo string `toml:"email_to"`
	// MinSeverity drops alerts below it: "info", "warning" or "critical". Empty means "info".
	MinSeverity string `toml:"min_severity"`
	// CheckIntervalSeconds is how often the daemon looks for new problems. 0 means 60.
	CheckIntervalSeconds int `toml:"check_interval_seconds"`
	// UnhealthyAfterSeconds is how long a project (or any other health condition) must stay
	// bad before it is raised, so that a restart or an upgrade does not page anyone. 0 means 180.
	UnhealthyAfterSeconds int `toml:"unhealthy_after_seconds"`
	// RepeatHours is how long an alert that is still active stays quiet before it is sent
	// again. 0 means 12. A recovery is sent once.
	RepeatHours int `toml:"repeat_hours"`
	// MaxPerHour caps the notifications sent in any hour, whatever their kind. What exceeds it
	// is counted and reported in the next one that goes out. 0 means 20.
	MaxPerHour int `toml:"max_per_hour"`
}

// AlertWebhook is one webhook endpoint.
type AlertWebhook struct {
	// URL is the endpoint (http or https).
	URL string `toml:"url"`
	// Secret, when set, signs the body: the X-Supavise-Signature header carries
	// "sha256=" and the hex HMAC-SHA256 of the body under this key.
	Secret string `toml:"secret"`
}

const (
	defaultAlertCheck     = time.Minute
	defaultAlertDebounce  = 3 * time.Minute
	defaultAlertRepeat    = 12 * time.Hour
	defaultAlertMaxPerHr  = 20
	maxAlertWebhookURLLen = 2048
)

// Check is the pause between checks.
func (a Alerts) Check() time.Duration {
	if a.CheckIntervalSeconds <= 0 {
		return defaultAlertCheck
	}
	return time.Duration(a.CheckIntervalSeconds) * time.Second
}

// Debounce is how long a condition must last before it is raised.
func (a Alerts) Debounce() time.Duration {
	if a.UnhealthyAfterSeconds <= 0 {
		return defaultAlertDebounce
	}
	return time.Duration(a.UnhealthyAfterSeconds) * time.Second
}

// Repeat is the quiet period of an alert that is still active.
func (a Alerts) Repeat() time.Duration {
	if a.RepeatHours <= 0 {
		return defaultAlertRepeat
	}
	return time.Duration(a.RepeatHours) * time.Hour
}

// HourlyCap is the most notifications sent per hour.
func (a Alerts) HourlyCap() int {
	if a.MaxPerHour <= 0 {
		return defaultAlertMaxPerHr
	}
	return a.MaxPerHour
}

// Recipients returns EmailTo split, trimmed and with empty entries removed.
func (a Alerts) Recipients() []string {
	var out []string
	for _, e := range strings.Split(a.EmailTo, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// Severity returns the minimum severity as a rank (0 info, 1 warning, 2 critical).
func (a Alerts) Severity() int {
	n, _ := SeverityRank(a.MinSeverity)
	return n
}

// SeverityRank maps "info", "warning" and "critical" (empty means info) to 0, 1 and 2.
func SeverityRank(s string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return 0, true
	case "warning":
		return 1, true
	case "critical":
		return 2, true
	}
	return 0, false
}

func (c *Config) validateAlerts() error {
	a := c.Alerts
	if _, ok := SeverityRank(a.MinSeverity); !ok {
		return fmt.Errorf("config: alerts.min_severity must be info, warning or critical, not %q", a.MinSeverity)
	}
	for i, w := range a.Webhooks {
		u, err := url.Parse(strings.TrimSpace(w.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(w.URL) > maxAlertWebhookURLLen {
			return fmt.Errorf("config: alerts.webhooks[%d].url must be an http or https URL", i)
		}
	}
	if len(a.Recipients()) > 0 && !c.Mail.Enabled() {
		return fmt.Errorf("config: alerts.email_to needs the [mail] section (smtp_host and smtp_from)")
	}
	for name, v := range map[string]int{
		"check_interval_seconds": a.CheckIntervalSeconds, "unhealthy_after_seconds": a.UnhealthyAfterSeconds,
		"repeat_hours": a.RepeatHours, "max_per_hour": a.MaxPerHour,
	} {
		if v < 0 {
			return fmt.Errorf("config: alerts.%s must not be negative, not %d", name, v)
		}
	}
	return nil
}
