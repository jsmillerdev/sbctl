// Package placement says where a project's pieces run and routes every operation there. A
// project has one home node, where its primary Postgres, GoTrue and PostgREST run
// (registry.Project.NodeID), and zero or more replicas, each a standby Postgres with its own
// PostgREST on another node (registry.Replica). This file is the contract the other packages are
// written against; the implementation (the plane router, the remote plane, the node agent) lives
// in the other files.
//
// The leader drives every project through a lifecycle.Plane. For a project homed on the leader
// that is the local plane. For a project homed elsewhere it is a remote plane that sends each
// method to the home node over the peer API, and the node's agent runs it there. Replica
// operations (create, observe, promote, remove) go to the node that holds the replica the same way.
package placement

import (
	"context"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// Resolver answers where a project is, from the registry (the replicated copy on a follower).
type Resolver interface {
	// HomeOf is the id of the node that is the project's home. registry.ErrNotFound for an unknown ref.
	HomeOf(ctx context.Context, ref string) (node string, err error)
}

// PlaneRouter is the lifecycle.Plane the leader's engine drives. It sends each call for a
// project to the plane of the project's home: the local one, or a remote plane that marshals the
// call over peerapi.PathPlane. A test fails when lifecycle.Plane gains a method that the remote
// plane lacks (peerapi.PlaneMethods).
type PlaneRouter interface {
	lifecycle.Plane
	// Local is the plane of this node, which serves the projects homed here.
	Local() lifecycle.Plane
	// For returns the plane that serves ref: Local when ref is homed here, a remote plane
	// otherwise. registry.ErrNotFound for an unknown ref.
	For(ctx context.Context, ref string) (lifecycle.Plane, error)
}

// InstanceOps performs replica operations on any node: the node runs them itself when it is this
// one and sends them over the peer API otherwise. The replica controller and the failover
// orchestrator code against it; its fake is a map of nodes.
type InstanceOps interface {
	// Ensure creates the instance on node, or finds it already there, and returns what the node observes.
	// ErrNoRoom when the node has no room for it: nothing is recorded there, and asking again later starts it.
	Ensure(ctx context.Context, node string, spec peerapi.InstanceSpec) (peerapi.InstanceStatus, error)
	// Observe returns what node observes about the instance.
	Observe(ctx context.Context, node, identifier string) (peerapi.InstanceStatus, error)
	// Remove stops the instance, deletes its data and forwarders. Removing one that is not there succeeds.
	Remove(ctx context.Context, node, identifier string) error
	// Do runs a replica action (restart, stop, start, promote, demote).
	Do(ctx context.Context, node, identifier string, a peerapi.Action, req peerapi.InstanceAction) (peerapi.InstanceStatus, error)
}

// BackupOps performs the backup operations that need a project's data directory on its home node.
type BackupOps interface {
	// BaseBackup takes a base backup of ref on its home node.
	BaseBackup(ctx context.Context, node, ref string, req peerapi.BackupRequest) (peerapi.BackupResult, error)
	// Restore restores ref in place on its home node.
	Restore(ctx context.Context, node, ref string, req peerapi.BackupRequest) (peerapi.BackupResult, error)
}

// Agent is what a node runs to serve the instance requests of the peer API. The handlers the
// placement package registers with mesh.Handle call it; InstanceOps calls it directly for
// this node.
type Agent interface {
	Ensure(ctx context.Context, spec peerapi.InstanceSpec) (peerapi.InstanceStatus, error)
	Observe(ctx context.Context, identifier string) (peerapi.InstanceStatus, error)
	Remove(ctx context.Context, identifier string) error
	Do(ctx context.Context, identifier string, a peerapi.Action, req peerapi.InstanceAction) (peerapi.InstanceStatus, error)
}

// RegistryResolver is the Resolver over a registry.
type RegistryResolver struct{ Reg registry.Registry }

var _ Resolver = RegistryResolver{}

func (r RegistryResolver) HomeOf(ctx context.Context, ref string) (string, error) {
	p, err := r.Reg.GetProject(ctx, ref)
	if err != nil {
		return "", err
	}
	return p.NodeID, nil
}
