package failover

import (
	"errors"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/registry"
)

// refusals are the errors a node answers with when it turns a request away before it changes
// anything: the caller is not the leader, or acts under an older epoch; the project is not homed there
// or the replica is not; the state does not allow the action (the node is fenced, the cluster is no
// standby, the replica is still being set up, a standby has not replayed as far as asked); there is no
// room, no backup engine, nothing to restore. The placement layer gives each of them back through
// errors.Is, from the peer API and from the node's own agent alike.
var refusals = []error{
	cluster.ErrNotLeader, placement.ErrStaleEpoch, placement.ErrNotHome, placement.ErrNoRoom, registry.ErrNotFound,
	lifecycle.ErrInvalidState, lifecycle.ErrNotStandby, lifecycle.ErrReplayBehind,
	lifecycle.ErrClusterExists, lifecycle.ErrNoRestorableState, lifecycle.ErrNoSnapshot,
}

// refusedByTheNode reports whether err is such an answer. Anything else (a status 500, a timeout, a
// lost answer, a failure in the middle of the work) says nothing about what the node did, and a
// promotion that ends in one may have happened.
func refusedByTheNode(err error) bool {
	for _, target := range refusals {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
