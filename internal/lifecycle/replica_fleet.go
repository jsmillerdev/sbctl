package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"

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

// SetReplicaFleet sets Options.Replicas on an Engine that was built without it. The daemon calls it
// while it wires the cluster features, before the Engine serves a request.
func (e *Engine) SetReplicaFleet(f ReplicaFleet) { e.opts.Replicas = f }

// EventReplicaResizeFailed is the event a replica that did not come back on a new size leaves
// on its project.
const EventReplicaResizeFailed = "replica.resize_failed"

// EventReplicaRestartFailed is the event a replica that did not come back after the project's
// Postgres settings made the primary restart leaves on its project.
const EventReplicaRestartFailed = "replica.restart_failed"

// resizeReplicas restarts every replica of p on the size p has now. A replica that is still being
// set up or removed is left alone (it renders from the project row when it starts). A failure marks
// the replica ACTIVE_UNHEALTHY, records an event and tells the fleet; it never returns an error,
// because a replica must not stop the primary's resize.
func (e *Engine) resizeReplicas(ctx context.Context, p *registry.Project) {
	e.restartReplicas(ctx, p, "resize to "+p.Class, EventReplicaResizeFailed, registry.StatusResizing)
}

// replicaRestartConcurrency is how many replicas restart at once, and defaultReplicaRestartTimeout how
// long one may take (Options.ReplicaRestartTimeout): a standby waits for its base of recovery, up to
// ten minutes, and a node that does not answer would hold a resize or a settings save for that long.
const (
	replicaRestartConcurrency    = 4
	defaultReplicaRestartTimeout = 5 * time.Minute
)

// restartReplicas restarts every replica of p on the size p has now and tells the fleet of the ones
// that do not come back; why says what made the restart necessary. During is the status the registry
// shows while a replica restarts ("" leaves it as it is). The replicas restart together, a few at a
// time, and each has a deadline, so that a slow or unreachable node holds neither the primary's resize
// nor the save of a setting for long: a replica must not stop the primary.
func (e *Engine) restartReplicas(ctx context.Context, p *registry.Project, why, failEvent string, during registry.Status) {
	if e.opts.Replicas == nil || p.Ref == config.SystemRef {
		return
	}
	rs, err := e.opts.Replicas.Replicas(ctx, p.Ref)
	if err != nil {
		e.log.Warn("restarting the replicas: listing them", "ref", p.Ref, "why", why, "error", err)
		return
	}
	sem := make(chan struct{}, replicaRestartConcurrency)
	var wg sync.WaitGroup
	for _, r := range rs {
		if r.Status != string(registry.StatusActiveHealthy) && r.Status != string(registry.StatusActiveUnhealthy) {
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			e.restartReplica(ctx, p, r, why, failEvent, during)
		}()
	}
	wg.Wait()
}

// restartReplica restarts one replica of p and records how it went.
func (e *Engine) restartReplica(ctx context.Context, p *registry.Project, r registry.Replica, why, failEvent string, during registry.Status) {
	// The rows are written after the restart, which may have taken the whole deadline or the caller's
	// context: the outcome is recorded either way.
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	if during != "" {
		if err := e.reg.SetReplicaStatus(cctx, r.Identifier, string(during), r.InitStep, r.InitError); err != nil {
			e.log.Warn("restarting the replicas: marking a replica "+string(during), "replica", r.Identifier, "error", err)
		}
	}
	timeout := e.opts.ReplicaRestartTimeout
	if timeout <= 0 {
		timeout = defaultReplicaRestartTimeout
	}
	rctx, stop := context.WithTimeout(ctx, timeout)
	err := e.opts.Replicas.Restart(rctx, r, p.Class)
	stop()
	status := registry.StatusActiveHealthy
	switch {
	case err == nil:
	case ctx.Err() != nil:
		// The caller gave up (a stopped daemon, a cancelled request): the replica was not judged, so it
		// goes back to the status it had and nobody is told.
		status = registry.Status(r.Status)
		e.log.Warn("restarting the replicas: stopped before a replica came back", "replica", r.Identifier, "why", why, "error", err)
	default:
		status = registry.StatusActiveUnhealthy
		e.log.Warn("a replica did not come back after its restart", "replica", r.Identifier, "node", r.NodeID, "why", why, "error", err)
		e.event(cctx, p.Ref, failEvent, map[string]any{"replica": r.Identifier, "node": r.NodeID, "class": p.Class, "error": err.Error()})
		e.opts.Replicas.Failed(cctx, r, fmt.Errorf("%s: %w", why, err))
	}
	if err := e.reg.SetReplicaStatus(cctx, r.Identifier, string(status), r.InitStep, r.InitError); err != nil {
		e.log.Warn("restarting the replicas: recording a replica's status", "replica", r.Identifier, "error", err)
	}
}
