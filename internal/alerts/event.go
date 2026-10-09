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

	// Read replicas and the second server. A replica's Ref is the project it copies.
	KindReplicaUnhealthy = "replica_unhealthy" // a replica is behind the limit, its receiver is down or its PostgREST does not answer
	KindReplicaLag       = "replica_lag"       // replication lag is high but the replica is still within its limit
	KindReplicaCapacity  = "replica_capacity"  // a node has no room for a replica that [replicas] default = "all" wants
	KindNodeUnreachable  = "node_unreachable"  // a peer node does not answer the mesh
	KindNodeVersionSkew  = "node_version_skew" // a peer runs a release outside the window this one can work with

	// Failover. These three are announcements, like the upgrade ones.
	KindFailoverStarted   = "failover_started"
	KindFailoverCompleted = "failover_completed"
	KindFailoverFailed    = "failover_failed"
	// KindFailoverAutoOff is a condition: automatic failover is configured and the node cannot fence.
	// It has a recovery message and the cap holds it back, so a probe that flaps cannot flood.
	KindFailoverAutoOff = "failover_auto_off"
	// KindFenced is critical: this node lost the leadership to a higher epoch and starts no primary.
	KindFenced = "fenced"

	// Things a release or a host change left undone.
	KindInfraBehind      = "infra_behind"       // the AWS stack lacks resources this release needs
	KindHostNotConverged = "host_not_converged" // `supavise system converge` has not run for this release
	KindStandbyBehind    = "standby_behind"     // the registry is newer than this binary, which leaves running instances alone

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
	case KindUpgradeStarted, KindUpgradeSucceeded, KindUpgradeFailed, KindUpdateAvailable, KindTest,
		KindFailoverStarted, KindFailoverCompleted, KindFailoverFailed:
		return true
	}
	return false
}
