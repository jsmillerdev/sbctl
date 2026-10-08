package replicas

import (
	"context"

	"github.com/supavise/supavise/internal/registry"
)

// reconcileDefaults makes [replicas] default = "all" true: every project that can be copied
// has a replica on every active node other than its home, unless an opt-out row says the
// operator removed that one (design 2.7.9). It only writes the missing rows, at 0_requested with
// origin "default"; admission and the concurrency limit are the setup's, so a node without room
// leaves its rows waiting rather than missing. It returns how many rows it wrote.
func (c *Controller) reconcileDefaults(ctx context.Context, nodes []registry.Node, projects []registry.Project, rows []registry.Replica) int {
	if !c.cfg.Replicas.AllByDefault() {
		return 0
	}
	if c.cfg.FileBackup() {
		c.warnOnce("backend", "replicas.default = \"all\" needs S3-compatible backup storage; no replicas are created")
		return 0
	}
	if err := c.cfg.CheckReplicaPorts(); err != nil {
		c.warnOnce("ports", err.Error())
		return 0
	}
	optouts, err := c.reg.ListReplicaOptouts(ctx)
	if err != nil {
		c.log.Warn("replicas: list opt-outs", "error", err)
		return 0
	}
	skip := make(map[registry.ReplicaOptout]bool, len(optouts)+len(rows))
	for _, o := range optouts {
		skip[o] = true
	}
	for _, r := range rows {
		skip[registry.ReplicaOptout{Ref: r.Ref, NodeID: r.NodeID}] = true
	}
	created := 0
	for i := range projects {
		p := &projects[i]
		if !replicable(p) {
			continue
		}
		if p.Seq > c.cfg.MaxReplicaSeq() {
			c.warnOnce("seq/"+p.Ref, "project "+p.Ref+" has a port number too high for a replica; it gets none")
			continue
		}
		for _, n := range nodes {
			if n.State != registry.NodeActive || n.ID == p.NodeID || skip[registry.ReplicaOptout{Ref: p.Ref, NodeID: n.ID}] {
				continue
			}
			r, err := c.insert(ctx, p.Ref, n, registry.ReplicaDefault)
			if err != nil {
				c.log.Warn("replicas: default replica", "ref", p.Ref, "node", n.ID, "error", err)
				continue
			}
			c.log.Info("replicas: default replica requested", "identifier", r.Identifier, "ref", p.Ref, "node", n.ID)
			created++
		}
	}
	return created
}

// warnOnce logs a condition that holds on every pass only the first time.
func (c *Controller) warnOnce(key, msg string) {
	c.mu.Lock()
	seen := c.warned[key]
	c.warned[key] = true
	c.mu.Unlock()
	if !seen {
		c.log.Warn("replicas: " + msg)
	}
}
