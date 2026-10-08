package replicas

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// stuckAfter is how long a removal may fail before the operator hears about it.
const stuckAfter = 15 * time.Minute

// replicaOf returns the replica identifier if it belongs to ref, else ErrNotFound.
func (c *Controller) replicaOf(ctx context.Context, ref, identifier string) (*registry.Replica, error) {
	r, err := c.reg.GetReplica(ctx, identifier)
	if errors.Is(err, registry.ErrNotFound) || (err == nil && r.Ref != ref) {
		return nil, ErrNotFound
	}
	return r, err
}

// Remove implements Service: the replica is GOING_DOWN at once and the controller stops its
// units, drops its Supavisor tenant and deletes its directory and row. Removing a replica the
// default made records an opt-out, so the reconciler does not make it again; a later Setup clears it.
func (c *Controller) Remove(ctx context.Context, ref, identifier string) error {
	r, err := c.replicaOf(ctx, ref, identifier)
	if err != nil {
		return err
	}
	if r.Origin == registry.ReplicaSystem {
		return refuse("The standby of the system cluster goes away with its server: use `supavise node rm`.")
	}
	if r.Status == statusGoingDown {
		return nil
	}
	if r.Origin == registry.ReplicaDefault || c.cfg.Replicas.AllByDefault() {
		if err := c.reg.PutReplicaOptout(ctx, r.Ref, r.NodeID); err != nil {
			return err
		}
	}
	if !c.markGoingDown(ctx, r) {
		return ErrNotFound
	}
	c.kick()
	return nil
}

// markGoingDown writes GOING_DOWN, keeping the step the setup reached.
func (c *Controller) markGoingDown(ctx context.Context, r *registry.Replica) bool {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.reg.GetReplica(ctx, r.Identifier); err != nil {
		return false
	}
	return c.reg.SetReplicaStatus(ctx, r.Identifier, statusGoingDown, r.InitStep, r.InitError) == nil
}

// RemoveAll implements Remover.
func (c *Controller) RemoveAll(ctx context.Context, ref string) error {
	rows, err := c.reg.ListReplicas(ctx, ref)
	if err != nil {
		return err
	}
	return c.removeNow(ctx, rows)
}

// RemoveOn implements Remover.
func (c *Controller) RemoveOn(ctx context.Context, node string) error {
	rows, err := c.reg.ListReplicasOn(ctx, node)
	if err != nil {
		return err
	}
	return c.removeNow(ctx, rows)
}

// removeNow marks the rows GOING_DOWN and tries each removal once, here. The ones that cannot
// finish stay GOING_DOWN for the controller to retry; so does one a worker is acting on, because
// the worker may be creating the instance this very moment.
func (c *Controller) removeNow(ctx context.Context, rows []registry.Replica) error {
	var pending []string
	for i := range rows {
		r := rows[i]
		if r.Status != statusGoingDown && !c.markGoingDown(ctx, &r) {
			continue
		}
		r.Status = statusGoingDown
		if c.isBusy(r.Identifier) || !c.removeOnce(ctx, &r) {
			pending = append(pending, r.Identifier)
		}
	}
	if len(pending) > 0 {
		c.kick()
		return &PendingError{Identifiers: pending}
	}
	return nil
}

func (c *Controller) isBusy(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.busy[id]
}

// removeStep is the controller's retry of a removal that did not finish.
func (c *Controller) removeStep(ctx context.Context, r *registry.Replica) {
	if !c.due(r.Identifier, "remove") {
		return
	}
	if c.removeOnce(ctx, r) {
		return
	}
	c.mu.Lock()
	s := c.st(r.Identifier)
	if s.removeSince.IsZero() {
		s.removeSince = c.now()
	}
	stuck := c.now().Sub(s.removeSince) >= stuckAfter && !s.alerted["remove"]
	if stuck {
		s.alerted["remove"] = true
	}
	c.mu.Unlock()
	if stuck {
		c.alert(ctx, alerts.Event{
			Kind: alerts.KindReplicaUnhealthy, Ref: r.Ref, Key: "replica_remove/" + r.Identifier,
			Title:  "Read replica removal is stuck",
			Detail: fmt.Sprintf("The read replica %s of project %s could not be removed from node %s for %s: %s. The controller keeps trying.", r.Identifier, r.Ref, r.NodeID, stuckAfter, c.lastError(r.Identifier, "remove")),
		})
	}
}

// removeOnce does the removal: the Supavisor tenant, then the instance (units, directory,
// forwarders) on its node, then the row. A node that left the cluster has nothing to remove. It
// reports whether the row is gone.
func (c *Controller) removeOnce(ctx context.Context, r *registry.Replica) bool {
	gone := false
	if n, err := c.reg.GetNode(ctx, r.NodeID); err == nil && n.State == registry.NodeLeft {
		gone = true
	}
	if c.o.Pooler != nil && r.Origin != registry.ReplicaSystem {
		if err := c.o.Pooler.RemoveReplicaTenant(ctx, r.Identifier); err != nil {
			c.removeFailed(r, fmt.Errorf("drop the Supavisor tenant: %w", err))
			return false
		}
	}
	if !gone {
		if c.o.Ops == nil {
			c.removeFailed(r, errNoOps)
			return false
		}
		if err := c.o.Ops.Remove(ctx, r.NodeID, r.Identifier); err != nil {
			c.removeFailed(r, fmt.Errorf("remove the instance on %s: %w", r.NodeID, err))
			return false
		}
	}
	if err := c.reg.DeleteReplica(ctx, r.Identifier); err != nil && !errors.Is(err, registry.ErrNotFound) {
		c.removeFailed(r, err)
		return false
	}
	c.mu.Lock()
	var open []string
	if s := c.state[r.Identifier]; s != nil {
		for name, on := range s.alerted {
			if on {
				open = append(open, name)
			}
		}
	}
	delete(c.state, r.Identifier)
	c.mu.Unlock()
	c.log.Info("replicas: replica removed", "identifier", r.Identifier, "node", r.NodeID)
	c.resolveAlerts(ctx, r, open)
	return true
}

func (c *Controller) removeFailed(r *registry.Replica, err error) {
	c.failedCall(r.Identifier, "remove", err)
	c.log.Warn("replicas: removal will be tried again", "identifier", r.Identifier, "node", r.NodeID, "error", err)
}

// resolveAlerts closes the alerts (by name, as healthAlerts keeps them) a removed replica had open.
func (c *Controller) resolveAlerts(ctx context.Context, r *registry.Replica, open []string) {
	for _, name := range open {
		kind := alerts.KindReplicaUnhealthy
		if name == "replica_lag" {
			kind = alerts.KindReplicaLag
		}
		c.alert(ctx, alerts.Event{Kind: kind, Ref: r.Ref, Key: name + "/" + r.Identifier, Resolved: true, Severity: alerts.SeverityInfo,
			Title: "Read replica removed"})
	}
}

// Restart implements Service: the replica is RESTARTING at once, its units restart on its node in
// the background, and the next healthy observation makes it ACTIVE_HEALTHY again.
func (c *Controller) Restart(ctx context.Context, ref, identifier string) error {
	r, err := c.replicaOf(ctx, ref, identifier)
	if err != nil {
		return err
	}
	switch {
	case !active(r):
		return refuse("The read replica %s cannot restart now: it is %s.", identifier, r.Status)
	case c.o.Ops == nil:
		return errNoOps
	}
	if !c.setStatus(ctx, r.Identifier, statusRestart, StepDone, "") {
		return ErrNotFound
	}
	c.mu.Lock()
	s := c.st(r.Identifier)
	s.transient, s.restarting = c.now(), true
	c.mu.Unlock()
	epoch, _ := c.leader()
	c.mu.Lock()
	bg := c.runCtx
	c.mu.Unlock()
	if bg == nil { // not running: the call outlives the request that made it
		bg = context.WithoutCancel(ctx)
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		bg, cancel := context.WithTimeout(bg, 5*time.Minute)
		defer cancel()
		st, err := c.o.Ops.Do(bg, r.NodeID, r.Identifier, peerapi.ActionRestart, peerapi.InstanceAction{Epoch: epoch})
		c.mu.Lock()
		c.st(r.Identifier).restarting = false
		c.mu.Unlock()
		if err != nil {
			c.log.Error("replicas: restart failed", "identifier", r.Identifier, "node", r.NodeID, "error", err)
			c.setStatus(bg, r.Identifier, statusUnhealthy, StepDone, "")
			c.alert(bg, alerts.Event{
				Kind: alerts.KindReplicaUnhealthy, Ref: r.Ref, Key: "replica_unhealthy/" + r.Identifier,
				Title:  "Read replica is unhealthy",
				Detail: fmt.Sprintf("Restarting the read replica %s of project %s on node %s failed: %v.", r.Identifier, r.Ref, r.NodeID, err),
			})
			return
		}
		c.observed(r.Identifier, st)
		c.kick()
	}()
	return nil
}
