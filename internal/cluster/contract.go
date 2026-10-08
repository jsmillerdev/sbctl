// Package cluster is a node's knowledge of the cluster it belongs to: which nodes exist, which
// one leads, at what epoch, and whether this node may act as a primary. This file is the
// contract the other packages are written against; the implementation (the derived certificate
// authority, join and rejoin, leader detection, the epoch marker) lives in the other files.
//
// A node is the leader exactly when the system cluster on that node is not in recovery
// (invariant I1). Leadership is not a flag anyone sets: promoting the system cluster makes the
// node the leader, and the daemon then records itself in the registry at a higher epoch. A
// leader that learns of a higher epoch, from a peer or from the leader marker in the backup
// store, fences itself: it starts no primary and says so in its status.
package cluster

import (
	"context"
	"errors"

	"github.com/supavise/supavise/internal/registry"
)

// ErrNotLeader is returned by an operation that only the leader performs, on a node that is not
// the leader (or is fenced).
var ErrNotLeader = errors.New("cluster: this node is not the leader")

// Role is what this node does in the cluster right now.
type Role string

const (
	// RoleLeader: the system cluster is a primary here. The node runs the writable registry, the
	// Management API, Studio and the shared services.
	RoleLeader Role = "leader"
	// RoleFollower: the system cluster is a hot standby. The node reads the registry from it and
	// runs the projects and replicas that are homed here.
	RoleFollower Role = "follower"
	// RoleFenced: a higher epoch or a different leader was seen. No cluster starts as a primary,
	// the proxy answers 503 with the reason, and `supavise node rejoin` is the way back.
	RoleFenced Role = "fenced"
)

// Snapshot is one consistent view of the cluster.
type Snapshot struct {
	// Self is this node's row.
	Self registry.Node
	// Nodes are all rows, the left ones included, by id.
	Nodes []registry.Node
	// Leader is the id of the leader ("" while none is known).
	Leader string
	Epoch  int64
	Role   Role
	// Maintenance is the announcement that suppresses automatic failover; the zero value when none.
	Maintenance registry.Maintenance
}

// Membership is what the rest of the daemon asks about the cluster. It answers from memory, so it
// is cheap to call per request; Watch says when the answers may have changed.
type Membership interface {
	// Self is this node's row (the registry's copy, as the daemon last read it).
	Self() registry.Node
	// Nodes are all rows by id, the left ones included.
	Nodes() []registry.Node
	// Leader is the leader's row; ok is false while no leader is known.
	Leader() (n registry.Node, ok bool)
	Epoch() int64
	// IsLeader reports whether this node is the leader and not fenced.
	IsLeader() bool
	Role() Role
	// Watch delivers a Snapshot after every change of the leader, the epoch, the role, a node row
	// or the maintenance announcement, and one at once when the call is made. It coalesces: a slow
	// reader gets the latest Snapshot, not every intermediate one. The channel closes when ctx ends.
	Watch(ctx context.Context) <-chan Snapshot
}
