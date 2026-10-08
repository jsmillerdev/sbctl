package replicas

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas/replicaid"
)

// The refusals of a setup request (design 2.7.2). The Management API returns the text as a 400
// and Studio shows it as it is.
const (
	msgNoNode       = "No Supavise server is joined in %s."
	msgSameServer   = "Read replicas on the same server as the primary are not offered."
	msgHasReplica   = "This project already has a replica on %s."
	msgGoingDown    = "The replica on %s is being removed; try again when it is gone."
	msgSizeTooSmall = "Read replicas need a compute size of small or larger."
	msgMaxReplicas  = "The project already has the maximum of %d read replicas."
	msgNoCapacity   = "Not enough capacity on %s."
	msgNeedS3       = "Read replicas need S3-compatible backup storage."
	msgBranch       = "Read replicas are not offered for branches."
	msgSystem       = "The system project has no read replicas of its own: its standby is made when a server joins."
	msgNodeJoining  = "The server %s has not finished joining the cluster."
	msgNodeFenced   = "The server %s is fenced and cannot take a replica."
	msgNodeLeft     = "The server %s has left the cluster."
)

func refuse(format string, args ...any) error { return &UserError{Msg: fmt.Sprintf(format, args...)} }

// Setup implements Service: the replica goes to the least loaded server joined in region.
//
// A request holds setupMu from the first read to the check after the insert, so two requests to
// this process (Studio's double click, the API beside the daemon's own) take turns. Requests of
// two processes meet only in the registry; create covers them.
func (c *Controller) Setup(ctx context.Context, ref, region string) error {
	c.setupMu.Lock()
	defer c.setupMu.Unlock()
	p, nodes, err := c.setupContext(ctx, ref)
	if err != nil {
		return err
	}
	var inRegion []registry.Node
	for _, n := range nodes {
		if n.State == registry.NodeActive && replicaid.Region(n) == region {
			inRegion = append(inRegion, n)
		}
	}
	if len(inRegion) == 0 {
		return refuse(msgNoNode, region)
	}
	existing, err := c.reg.ListReplicas(ctx, ref)
	if err != nil {
		return err
	}
	var candidates []registry.Node
	for _, n := range inRegion {
		if n.ID != p.NodeID {
			candidates = append(candidates, n)
		}
	}
	if len(candidates) == 0 {
		return refuse(msgSameServer)
	}
	var free []registry.Node
	var taken, leaving []string
	for _, n := range candidates {
		switch r := replicaOn(existing, n.ID); {
		case r == nil:
			free = append(free, n)
		case r.Status == statusGoingDown:
			leaving = append(leaving, nodeLabel(n))
		default:
			taken = append(taken, nodeLabel(n))
		}
	}
	if len(free) == 0 {
		if len(taken) == 0 {
			return refuse(msgGoingDown, strings.Join(leaving, ", "))
		}
		return refuse(msgHasReplica, strings.Join(taken, ", "))
	}
	target, err := c.leastLoaded(ctx, free)
	if err != nil {
		return err
	}
	return c.create(ctx, p, nodes, existing, target)
}

// SetupOn implements Service: the replica goes to the named server (its id, or its name).
func (c *Controller) SetupOn(ctx context.Context, ref, node string) error {
	c.setupMu.Lock()
	defer c.setupMu.Unlock()
	p, nodes, err := c.setupContext(ctx, ref)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(nodes, func(n registry.Node) bool { return n.ID == node })
	if i < 0 {
		i = slices.IndexFunc(nodes, func(n registry.Node) bool { return n.Name == node })
	}
	if i < 0 {
		return refuse("There is no Supavise server %q in this cluster.", node)
	}
	target := nodes[i]
	if target.State != registry.NodeActive {
		return refuse(nodeStateMsg(target.State), nodeLabel(target))
	}
	if target.ID == p.NodeID {
		return refuse(msgSameServer)
	}
	existing, err := c.reg.ListReplicas(ctx, ref)
	if err != nil {
		return err
	}
	if r := replicaOn(existing, target.ID); r != nil {
		if r.Status == statusGoingDown {
			return refuse(msgGoingDown, nodeLabel(target))
		}
		return refuse(msgHasReplica, nodeLabel(target))
	}
	return c.create(ctx, p, nodes, existing, target)
}

// setupContext loads the project and the nodes and applies the refusals that do not depend on
// where the replica would go.
func (c *Controller) setupContext(ctx context.Context, ref string) (*registry.Project, []registry.Node, error) {
	p, err := c.reg.GetProject(ctx, ref)
	if err != nil {
		return nil, nil, fmt.Errorf("replicas: project %s: %w", ref, err)
	}
	switch {
	case ref == config.SystemRef:
		return nil, nil, refuse(msgSystem)
	case p.Branch != nil:
		return nil, nil, refuse(msgBranch)
	case MaxReplicas(p.Class) == 0:
		return nil, nil, refuse(msgSizeTooSmall)
	case c.cfg.FileBackup():
		return nil, nil, refuse(msgNeedS3)
	}
	if err := c.cfg.CheckReplicaPorts(); err != nil {
		return nil, nil, &UserError{Msg: err.Error()}
	}
	if last := c.cfg.MaxReplicaSeq(); p.Seq > last {
		return nil, nil, refuse("This project cannot have a read replica: its port number %d is above %d, the last one that fits between ports.replica_base and ports.project_base.", p.Seq, last)
	}
	nodes, err := c.reg.ListNodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	return p, nodes, nil
}

// replicaOn returns the row of the replica on node, or nil.
func replicaOn(rows []registry.Replica, node string) *registry.Replica {
	if i := slices.IndexFunc(rows, func(r registry.Replica) bool { return r.NodeID == node }); i >= 0 {
		return &rows[i]
	}
	return nil
}

// nodeStateMsg is the refusal for a node that is not active.
func nodeStateMsg(s registry.NodeState) string {
	switch s {
	case registry.NodeFenced:
		return msgNodeFenced
	case registry.NodeLeft:
		return msgNodeLeft
	}
	return msgNodeJoining
}

// leastLoaded picks the node that holds the fewest projects and replicas; the lower id wins a tie.
func (c *Controller) leastLoaded(ctx context.Context, nodes []registry.Node) (registry.Node, error) {
	projects, err := c.reg.ListProjects(ctx)
	if err != nil {
		return registry.Node{}, err
	}
	load := map[string]int{}
	for _, p := range projects {
		load[p.NodeID]++
	}
	best := nodes[0]
	for _, n := range nodes {
		rs, err := c.reg.ListReplicasOn(ctx, n.ID)
		if err != nil {
			return registry.Node{}, err
		}
		load[n.ID] += len(rs)
		if load[n.ID] < load[best.ID] {
			best = n
		}
	}
	return best, nil
}

// create applies the refusals that depend on the target, then writes the row and checks the cap
// once more. The count in front of the insert is a fast path: a request in another process, the
// daemon beside `supavise replicas add`, can pass it at the same time. The registry has no
// multi-statement transaction to hold the count and the insert together, so the check is made after
// the insert instead: a row that leaves the project over its limit is deleted again and refused.
// The request that inserts last always sees the others' rows, so the limit is never exceeded; two
// that insert together may both see the excess and both withdraw, and a retry succeeds.
func (c *Controller) create(ctx context.Context, p *registry.Project, nodes []registry.Node, existing []registry.Replica, target registry.Node) error {
	if target.ID == p.NodeID {
		return refuse(msgSameServer)
	}
	active := 0
	for _, n := range nodes {
		if n.State == registry.NodeActive {
			active++
		}
	}
	limit := min(MaxReplicas(p.Class), active-1)
	if liveCount(existing) >= limit {
		return refuse(msgMaxReplicas, limit)
	}
	if c.o.Admit != nil {
		projects, err := c.reg.ListProjects(ctx)
		if err != nil {
			return err
		}
		rows, err := c.reg.ListReplicas(ctx, "")
		if err != nil {
			return err
		}
		req := AdmitRequest{Node: target, Project: *p, Hosted: hostedOn(target.ID, projects, rows, ""), SeedBytes: c.seedSize(ctx, p.Ref)}
		if err := c.o.Admit.Admit(ctx, req); err != nil {
			c.log.Info("replicas: no room for a replica", "ref", p.Ref, "node", target.ID, "reason", err.Error())
			return refuse(msgNoCapacity, nodeLabel(target))
		}
	}
	r, err := replicaid.Create(ctx, c.reg, p.Ref, target, registry.ReplicaManual, c.o.NewID)
	if err != nil {
		if errors.Is(err, registry.ErrConflict) {
			return c.conflict(ctx, p.Ref, target)
		}
		return err
	}
	if err := c.checkCap(ctx, r, limit); err != nil {
		return err
	}
	// Asking for the replica again after removing a default one is a change of mind.
	if err := c.reg.DeleteReplicaOptout(ctx, p.Ref, target.ID); err != nil {
		return err
	}
	c.log.Info("replicas: replica requested", "identifier", r.Identifier, "ref", p.Ref, "node", target.ID)
	c.kick()
	return nil
}

// liveCount is how many of the rows are replicas that are not going down.
func liveCount(rows []registry.Replica) int {
	n := 0
	for _, r := range rows {
		if r.Status != statusGoingDown {
			n++
		}
	}
	return n
}

// conflict words the registry's refusal of a replica on target: the project's home is target (a
// failover moved it since the request began) or target already holds a replica.
func (c *Controller) conflict(ctx context.Context, ref string, target registry.Node) error {
	if cur, err := c.reg.GetProject(ctx, ref); err == nil && cur.NodeID == target.ID {
		return refuse(msgSameServer)
	}
	return refuse(msgHasReplica, nodeLabel(target))
}

// checkCap withdraws the row r when the project has more replicas than limit now, and says why.
// When it cannot count, it withdraws the row too: a replica nobody could check is not kept.
func (c *Controller) checkCap(ctx context.Context, r *registry.Replica, limit int) error {
	rows, err := c.reg.ListReplicas(ctx, r.Ref)
	if err == nil && liveCount(rows) <= limit {
		return nil
	}
	if derr := c.reg.DeleteReplica(ctx, r.Identifier); derr != nil && !errors.Is(derr, registry.ErrNotFound) {
		c.log.Warn("replicas: a replica over the limit could not be withdrawn", "identifier", r.Identifier, "error", derr)
		if err == nil {
			err = derr
		}
		return err
	}
	if err != nil {
		return err
	}
	c.log.Info("replicas: a concurrent request took the last place, the replica is withdrawn", "identifier", r.Identifier, "ref", r.Ref, "limit", limit)
	return refuse(msgMaxReplicas, limit)
}
