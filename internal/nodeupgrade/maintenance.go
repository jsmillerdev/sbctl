package nodeupgrade

import (
	"context"
	"fmt"
	"time"
)

// MaintenanceReasonPrefix starts the reason of the announcement an upgrade or a rollback makes
// (registry.Maintenance.Reason). A leftover announcement with this prefix, from a run that was killed, is
// replaced by the next run; one with another reason (a planned failover is under way) is not.
const MaintenanceReasonPrefix = "supavise "

// MaintenanceTTL is how long an announcement holds without being renewed. The host renews it while the
// run lives, so a run that dies leaves an announcement that is gone in this time.
const MaintenanceTTL = 15 * time.Minute

// MaintenanceAnnouncer is the optional Host capability behind design 2.10.7: the leader of a cluster
// announces maintenance in the registry (cluster.maintenance) before an upgrade or a rollback stops
// anything, so that the follower's automatic failover never fires while the daemon restarts, and clears
// it when the run ends, whatever way it ends.
type MaintenanceAnnouncer interface {
	// AnnounceMaintenance records that node is down on purpose for reason, for MaintenanceTTL, and keeps
	// the announcement alive until EndMaintenance. It fails when an announcement of another reason is
	// active.
	AnnounceMaintenance(ctx context.Context, node, reason string) error
	// EndMaintenance clears the announcement AnnounceMaintenance made, and only that one.
	EndMaintenance(ctx context.Context, node string) error
}

// announce announces maintenance on this node when it leads a cluster and the host can; it returns the
// function that ends it (nil when nothing was announced). An error means nothing was announced and the
// run must not go on: a failover could fire in the middle of it.
func announce(ctx context.Context, h Host, o *Options, node *Node, reason string) (end func(), err error) {
	a, ok := h.(MaintenanceAnnouncer)
	if !ok || node.Cluster == nil || !node.Cluster.Leader {
		return nil, nil
	}
	self := node.Cluster.Self
	if err := a.AnnounceMaintenance(ctx, self, reason); err != nil {
		return nil, fmt.Errorf("cannot announce the maintenance that keeps an automatic failover from firing: %w", err)
	}
	o.say("announced maintenance on %s (%s): no automatic failover while it runs", self, reason)
	return func() {
		// The run may end because its context was cancelled; the announcement must still go.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := a.EndMaintenance(cctx, self); err != nil {
			o.log().Warn("could not clear the maintenance announcement", "error", err.Error())
			o.say("warning: could not clear the maintenance announcement (%v); it ends by itself in %s", err, MaintenanceTTL)
		}
	}, nil
}

// MaintenanceReason is the reason an upgrade to version gives.
func MaintenanceReason(version string) string {
	return MaintenanceReasonPrefix + "upgrade to " + version
}

// RollbackMaintenanceReason is the reason a rollback to version gives.
func RollbackMaintenanceReason(version string) string {
	return MaintenanceReasonPrefix + "rollback to " + version
}
