package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/nodeupgrade"
	"github.com/supavise/supavise/internal/registry"
)

// clusterView finds this node's place in its cluster and keeps, of the registry's projects, the
// ones homed here. A server that is not in a cluster (its registry has no other node, or none of the
// cluster tables yet: a v0.1.x registry that the new binary reads before migration 1300) gets no view
// and every project, as before.
//
// A project homed on another node is that node's to back up, upgrade and restart: its data and its
// units are there, and this node's engine refuses to act on it. The upgrade of this node leaves it
// out and says so in the plan.
func clusterView(ctx context.Context, cfg *config.Config, reg registry.Registry, rows []registry.Project) (*nodeupgrade.ClusterView, []registry.Project, error) {
	nodes, err := reg.ListNodes(ctx)
	if err != nil {
		return nil, rows, nil // a registry that predates the cluster tables
	}
	var live []registry.Node
	for _, n := range nodes {
		if n.State != registry.NodeLeft {
			live = append(live, n)
		}
	}
	if len(live) <= 1 {
		return nil, rows, nil
	}
	self, err := lifecycle.SelfNode(ctx, reg, cfg, true)
	if err != nil {
		return nil, nil, fmt.Errorf("this server is one of %d in a cluster, but the registry does not know it by the name %q: %w", len(live), cfg.NodeName(), err)
	}
	cl, err := reg.GetCluster(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read the cluster: %w", err)
	}
	view := &nodeupgrade.ClusterView{Self: self, Leader: cl.Leader == self}
	var here []registry.Project
	homed := map[string]bool{}
	for _, p := range rows {
		if p.NodeID == "" || p.NodeID == self {
			here = append(here, p)
			homed[p.Ref] = true
		} else if p.Ref != config.SystemRef {
			view.Elsewhere = append(view.Elsewhere, p.Ref)
		}
	}
	slices.Sort(view.Elsewhere)
	for _, n := range live {
		if n.ID == self || n.State != registry.NodeActive {
			continue
		}
		var refs []string
		if view.Leader {
			refs = append(refs, config.SystemRef) // every node follows the leader's system cluster
		}
		reps, err := reg.ListReplicasOn(ctx, n.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot read the replicas on %s: %w", n.ID, err)
		}
		for _, r := range reps {
			if homed[r.Ref] && !slices.Contains(refs, r.Ref) {
				refs = append(refs, r.Ref)
			}
		}
		if len(refs) > 0 {
			slices.Sort(refs)
			view.Standbys = append(view.Standbys, nodeupgrade.Standby{Node: n.ID, Name: n.Name, Version: n.Version, Refs: refs})
		}
	}
	return view, here, nil
}

// readableRegistryDSN is the DSN of the registry this server can read as the user it runs as: the
// socket of its system cluster, else the socket of the hot standby a follower keeps of the leader's
// (lifecycle.FollowerRegistryDSN). The first that answers a query wins; when none does the first is
// returned, so that the caller's error is the one it has always shown. A follower's registry is the
// replicated copy, which `supavise upgrade` only reads.
func readableRegistryDSN(ctx context.Context, cfg *config.Config) string {
	cands := []string{lifecycle.SystemSocketDSN(cfg, "supavise"), lifecycle.FollowerRegistryDSN(cfg)}
	for _, c := range cands {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := registry.AppliedMigrations(cctx, strings.TrimSuffix(c, " pool_max_conns=6"))
		cancel()
		if err == nil {
			return c
		}
	}
	return cands[0]
}

// registryDSN is readableRegistryDSN, looked up once for the run.
func (h *nodeHost) registryDSN(ctx context.Context) string {
	h.dsnMu.Lock()
	defer h.dsnMu.Unlock()
	if h.dsn == "" {
		h.dsn = readableRegistryDSN(ctx, h.cfg)
	}
	return h.dsn
}
