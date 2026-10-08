package replicas

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/supavise/supavise/internal/registry"
)

// The refusals of a setup request (design 2.7.2). The Management API returns the text as a 400
// and Studio shows it as it is.
const (
	msgNoNode        = "No Supavise server is joined in %s."
	msgSameServer    = "Read replicas on the same server as the primary are not offered."
	msgHasReplica    = "This project already has a replica on %s."
	msgSizeTooSmall  = "Read replicas need a compute size of small or larger."
	msgMaxReplicas   = "The project already has the maximum of %d read replicas."
	msgNoCapacity    = "Not enough capacity on %s."
	msgNeedS3        = "Read replicas need S3-compatible backup storage."
	msgBranch        = "Read replicas are not offered for branches."
	msgNodeNotActive = "The server %s has not finished joining the cluster."
)

func refuse(format string, args ...any) error { return &UserError{Msg: fmt.Sprintf(format, args...)} }

// Setup implements Service: the replica goes to the least loaded server joined in region.
func (c *Controller) Setup(ctx context.Context, ref, region string) error {
	p, nodes, err := c.setupContext(ctx, ref)
	if err != nil {
		return err
	}
	var inRegion []registry.Node
	for _, n := range nodes {
		if n.State == registry.NodeActive && nodeRegion(n) == region {
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
	var taken []string
	for _, n := range candidates {
		if hasReplicaOn(existing, n.ID) {
			taken = append(taken, nodeLabel(n))
		} else {
			free = append(free, n)
		}
	}
	if len(free) == 0 {
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
		return refuse(msgNodeNotActive, nodeLabel(target))
	}
	if target.ID == p.NodeID {
		return refuse(msgSameServer)
	}
	existing, err := c.reg.ListReplicas(ctx, ref)
	if err != nil {
		return err
	}
	if hasReplicaOn(existing, target.ID) {
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

func hasReplicaOn(rows []registry.Replica, node string) bool {
	return slices.ContainsFunc(rows, func(r registry.Replica) bool { return r.NodeID == node })
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

// create applies the refusals that depend on the target, then writes the row.
func (c *Controller) create(ctx context.Context, p *registry.Project, nodes []registry.Node, existing []registry.Replica, target registry.Node) error {
	live := 0
	for _, r := range existing {
		if r.Status != statusGoingDown {
			live++
		}
	}
	active := 0
	for _, n := range nodes {
		if n.State == registry.NodeActive {
			active++
		}
	}
	if limit := min(MaxReplicas(p.Class), active-1); live >= limit {
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
	r, err := c.insert(ctx, p.Ref, target, registry.ReplicaManual)
	if err != nil {
		if errors.Is(err, registry.ErrConflict) {
			return refuse(msgHasReplica, nodeLabel(target))
		}
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

// insert writes a replica row for ref on node, retrying with another identifier when the six
// random characters collide.
func (c *Controller) insert(ctx context.Context, ref string, node registry.Node, origin string) (*registry.Replica, error) {
	var err error
	for range 5 {
		r := &registry.Replica{Identifier: c.newIdentifier(ref, node), Ref: ref, NodeID: node.ID, Origin: origin}
		if err = c.reg.CreateReplica(ctx, r); err == nil {
			return r, nil
		}
		if _, e := c.reg.GetReplica(ctx, r.Identifier); e != nil {
			return nil, err // not an identifier clash
		}
	}
	return nil, err
}
