package placement

import (
	"context"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh/peerapi"
)

// StandbyPlane is the part of lifecycle.PostgresPlane that builds a server's standby of the system
// cluster; the tests of the commands that use it replace it.
type StandbyPlane interface {
	SeedSystemStandby(ctx context.Context, plan lifecycle.SystemStandbyPlan, seeder lifecycle.ReplicaSeeder) error
	SystemStandbyPreflight(ctx context.Context) error
	SystemStandbyJoinPreflight(ctx context.Context) error
}

var _ StandbyPlane = (*lifecycle.PostgresPlane)(nil)

// SystemStandby is how a server that joins a cluster builds its standby of the system cluster, before
// its daemon runs and before it has a registry: Seed is cluster.JoinOptions.Seed and Preflight is
// cluster.JoinOptions.Preflight. Plane is a plane over this server's supervisor and artifacts
// (lifecycle.OpenStandbyPlane; its registry is never read), Seeder the backup service's SeedReplica
// (SeederFrom). The work is lifecycle.PostgresPlane.SeedSystemStandby; this adapts it to the join's types.
type SystemStandby struct {
	Plane  StandbyPlane
	Seeder lifecycle.ReplicaSeeder
	// Joining is set for a server that has not joined yet. Its Preflight leaves the backup backend alone:
	// the leader's settings replace this server's own with the join.
	Joining bool
}

var _ cluster.SeedFunc = SystemStandby{}.Seed

// Seed builds the standby of b's base backup and starts it; it can be repeated (see SeedSystemStandby).
func (s SystemStandby) Seed(ctx context.Context, b peerapi.SystemBootstrap) error {
	return s.Plane.SeedSystemStandby(ctx, lifecycle.SystemStandbyPlan{
		Identifier: b.Identifier, BackupID: b.BackupID, ReplicationPassword: b.ReplicationPassword,
	}, s.Seeder)
}

// Preflight checks what Seed needs, before anything is changed.
func (s SystemStandby) Preflight(ctx context.Context) error {
	if s.Joining {
		return s.Plane.SystemStandbyJoinPreflight(ctx)
	}
	return s.Plane.SystemStandbyPreflight(ctx)
}
