package placement

import (
	"context"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh/peerapi"
)

// SystemStandby is how a server that joins a cluster builds its standby of the system cluster, before
// its daemon runs and before it has a registry: Seed is cluster.JoinOptions.Seed and Preflight is
// cluster.JoinOptions.Preflight. Plane is a plane over this server's supervisor and artifacts (its
// registry is never read), Seeder the backup service's SeedReplica (SeederFrom). The work is
// lifecycle.PostgresPlane.SeedSystemStandby; this adapts it to the join's types.
type SystemStandby struct {
	Plane  *lifecycle.PostgresPlane
	Seeder lifecycle.ReplicaSeeder
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
	return s.Plane.SystemStandbyPreflight(ctx)
}
