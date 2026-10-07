package alerts

import (
	"strings"
	"time"
)

// Kinds of alert. The ones the daemon's checker raises are listed first; the others are
// raised by the commands and engines that own the event (`supavise upgrade` and its rollout).
// Kind and Severity are plain strings, so a caller can pass a literal.
const (
	KindDiskLow             = "disk_low"
	KindBackupFailed        = "backup_failed" // the newest backup failed, or none is fresh
	KindProjectUnhealthy    = "project_unhealthy"
	KindCertificateExpiring = "certificate_expiring"
	KindNodeUnhealthy       = "node_unhealthy" // a shared service, the system cluster or the registry
	KindUpdateAvailable     = "update_available"

	KindUpgradeStarted   = "upgrade_started"
	KindUpgradeSucceeded = "upgrade_succeeded"
	KindUpgradeFailed    = "upgrade_failed"

	KindTest = "test"
)

// Severities.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Event is one thing the operator should hear about.
type Event struct {
	// Kind is one of the Kind constants.
	Kind string
	// Severity is "info", "warning" or "critical". Empty means "warning".
	Severity string
	// Title is one line, stable for a given problem (put numbers in Detail), because a
	// recovery message repeats it.
	Title string
	// Detail is the sentence or two that say what is wrong and what to do.
	Detail string
	// Ref is the project the event is about; empty for the node.
	Ref string

	// Key identifies the problem for de-duplication: the same Key is sent again only after
	// [alerts] repeat_hours. Empty means Kind and Ref together.
	Key string
	// Resolved reports that the problem Key names is over. It is sent once, and only if the
	// problem itself was.
	Resolved bool
	// Time is when it happened; empty means now.
	Time time.Time
}

func (e Event) key() string {
	if e.Key != "" {
		return e.Key
	}
	return e.Kind + "/" + e.Ref
}

func (e Event) severity() string {
	switch strings.ToLower(e.Severity) {
	case SeverityInfo, SeverityCritical:
		return strings.ToLower(e.Severity)
	}
	return SeverityWarning
}

// oneShot kinds are announcements, not conditions: each is sent when it happens, subject only to
// the hourly cap. A second failed upgrade is news; so is the next release.
func oneShot(kind string) bool {
	switch kind {
	case KindUpgradeStarted, KindUpgradeSucceeded, KindUpgradeFailed, KindUpdateAvailable, KindTest:
		return true
	}
	return false
}
