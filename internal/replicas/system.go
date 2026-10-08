package replicas

import (
	"context"
	"errors"
	"fmt"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// CreateSystemReplica records the standby of the system cluster on a node that is joining
// (design 2.3): a replica of the project "system", origin "system", hidden from the platform
// listings. The cluster join calls it when it admits the node, and returns the identifier in the
// join response. The joining node seeds the standby itself, before its daemon runs, so the
// controller only watches the row (it reaches ACTIVE_HEALTHY once the node reports the standby
// streaming). Calling it again for the same node returns the row it made, which is what a
// resumed join needs. An error wrapping registry.ErrNotFound means the node is unknown.
func CreateSystemReplica(ctx context.Context, reg registry.Registry, nodeID string) (*registry.Replica, error) {
	n, err := reg.GetNode(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("replicas: node %s: %w", nodeID, err)
	}
	rows, err := reg.ListReplicas(ctx, config.SystemRef)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].NodeID == nodeID {
			return &rows[i], nil
		}
	}
	for range 5 {
		r := &registry.Replica{Identifier: registry.ReplicaIdentifier(config.SystemRef, nodeRegion(*n), randomID6()), Ref: config.SystemRef, NodeID: nodeID, Origin: registry.ReplicaSystem}
		if err = reg.CreateReplica(ctx, r); err == nil {
			return r, nil
		}
		if _, e := reg.GetReplica(ctx, r.Identifier); e != nil {
			break // not an identifier clash
		}
	}
	return nil, errors.Join(fmt.Errorf("replicas: create the system standby of node %s", nodeID), err)
}
