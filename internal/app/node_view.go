package app

import (
	"context"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// homedHere is a registry that lists only the projects homed on one node. The node's health checks
// (internal/health) read every project of the registry and ask the node's plane about its units; on a
// node of a cluster the registry also names the projects that run on other nodes, which have no units
// here, and the checks would call each of them unhealthy and raise an alert for it. Projects with no
// home (a registry from before the cluster work) belong to every node.
type homedHere struct {
	registry.Registry
	self string
}

func (h homedHere) ListProjects(ctx context.Context) ([]registry.Project, error) {
	ps, err := h.Registry.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := ps[:0:0]
	for _, p := range ps {
		if p.NodeID == "" || p.NodeID == h.self {
			out = append(out, p)
		}
	}
	return out, nil
}

// nodeSeenBy is n as the node's own health sees it: on a node that belongs to a cluster, with the
// projects homed here only. A server with no cluster is returned as it is.
func nodeSeenBy(n *lifecycle.Node, boot cluster.BootDecision) *lifecycle.Node {
	if !boot.Joined || boot.SelfID == "" {
		return n
	}
	c := *n
	c.Registry = homedHere{Registry: n.Registry, self: boot.SelfID}
	return &c
}
