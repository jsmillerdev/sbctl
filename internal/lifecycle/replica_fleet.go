package lifecycle

import (
	"context"
	"fmt"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// ReplicaFleet reaches the replicas of a project on the nodes that hold them. internal/placement
// implements it over the peer API; the Engine uses it to restart a project's replicas in the right
// order when the project is resized.
type ReplicaFleet interface {
	// Replicas lists the replicas of ref.
	Replicas(ctx context.Context, ref string) ([]registry.Replica, error)
	// Restart restarts the replica on its node so that it runs on class, the compute size to
	// render: the node's copy of the registry may not have the new size yet.
	Restart(ctx context.Context, r registry.Replica, class string) error
	// Failed is told of a replica that did not come back after the Engine restarted it, so that the
	// node can raise the alert; it must not block.
	Failed(ctx context.Context, r registry.Replica, cause error)
}

// EventReplicaResizeFailed is the event a replica that did not come back on a new size leaves
// on its project.
const EventReplicaResizeFailed = "replica.resize_failed"

// resizeReplicas restarts every replica of p on the size p has now. A replica that is still being
// set up or removed is left alone (it renders from the project row when it starts). A failure marks
// the replica ACTIVE_UNHEALTHY, records an event and tells the fleet; it never returns an error,
// because a replica must not stop the primary's resize.
func (e *Engine) resizeReplicas(ctx context.Context, p *registry.Project) {
	if e.opts.Replicas == nil || p.Ref == config.SystemRef {
		return
	}
	rs, err := e.opts.Replicas.Replicas(ctx, p.Ref)
	if err != nil {
		e.log.Warn("resize: listing the replicas", "ref", p.Ref, "error", err)
		return
	}
	for _, r := range rs {
		if r.Status != string(registry.StatusActiveHealthy) && r.Status != string(registry.StatusActiveUnhealthy) {
			continue
		}
		if err := e.reg.SetReplicaStatus(ctx, r.Identifier, string(registry.StatusResizing), r.InitStep, r.InitError); err != nil {
			e.log.Warn("resize: marking a replica RESIZING", "replica", r.Identifier, "error", err)
		}
		status := registry.StatusActiveHealthy
		if err := e.opts.Replicas.Restart(ctx, r, p.Class); err != nil {
			status = registry.StatusActiveUnhealthy
			e.log.Warn("resize: a replica did not come back on the new size", "replica", r.Identifier, "node", r.NodeID, "error", err)
			e.event(ctx, p.Ref, EventReplicaResizeFailed, map[string]any{"replica": r.Identifier, "node": r.NodeID, "class": p.Class, "error": err.Error()})
			e.opts.Replicas.Failed(ctx, r, fmt.Errorf("resize to %s: %w", p.Class, err))
		}
		if err := e.reg.SetReplicaStatus(ctx, r.Identifier, string(status), r.InitStep, r.InitError); err != nil {
			e.log.Warn("resize: recording a replica's status", "replica", r.Identifier, "error", err)
		}
	}
}
