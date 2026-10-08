// Package replicas is the read-replica controller: it turns the replicas rows the Management API
// and the server-wide default write into running standbys on joined servers, reports their
// setup steps, status and lag, and removes them (design 2.7).
package replicas

import (
	"context"
	"errors"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// Service is what the Management API (internal/api) and `supavise replicas` ask of the
// controller. Every method is safe to call on the leader only; a follower's API forwards to it.
type Service interface {
	// Setup records a replica of ref in region and returns once the row exists; the controller
	// runs the steps afterwards. A refusal is a *UserError whose text the API returns as a 400.
	Setup(ctx context.Context, ref, region string) error
	// SetupOn is Setup on a named node (`supavise replicas add --node`).
	SetupOn(ctx context.Context, ref, nodeID string) error
	// Remove starts removing a replica of ref; ErrNotFound when ref has no such identifier.
	Remove(ctx context.Context, ref, identifier string) error
	// Restart restarts one replica's units on its node; ErrNotFound as for Remove.
	Restart(ctx context.Context, ref, identifier string) error
	// List returns ref's replicas, oldest first.
	List(ctx context.Context, ref string) ([]Replica, error)
	// Statuses returns one Status per replica of ref, in List's order.
	Statuses(ctx context.Context, ref string) ([]Status, error)
	// Lag returns the replica's lag samples since the given time, oldest first (1-minute points
	// kept in memory for 24 hours; empty after a leader restart).
	Lag(ctx context.Context, identifier string, since time.Time) ([]LagPoint, error)
}

// Replica is a replicas row with where it runs.
type Replica struct {
	registry.Replica
	// Region is the region of the node the replica runs on.
	Region string
	// PublicHost is that node's public host (its pooler endpoint).
	PublicHost string
}

// Status is a replica's status as GET /platform/projects/{ref}/databases-statuses reports it.
type Status struct {
	Identifier string
	// Status is a Management API project status: ACTIVE_HEALTHY, ACTIVE_UNHEALTHY, RESTARTING,
	// RESIZING, GOING_DOWN, INIT_READ_REPLICA or INIT_READ_REPLICA_FAILED.
	Status string
	// Init is set while the replica sets up, and after a failed setup.
	Init *InitStatus
	// LagSeconds is the latest lag, or -1 when unknown.
	LagSeconds float64
}

// InitStatus is replicaInitializationStatus in the spec.
type InitStatus struct {
	// Status is "in_progress", "completed" or "failed".
	Status string
	// Progress is the step reached, a spec value such as "4_downloaded_base_backup".
	Progress string
	// Error is the failed step's spec value, such as "1_read_replica_instance_launch_failed".
	Error string
	// Estimates in seconds; zero when unknown.
	BaseBackupDownloadEstimateSeconds int
	WALArchiveReplayEstimateSeconds   int
}

// LagPoint is one lag sample.
type LagPoint struct {
	At      time.Time
	Seconds float64
}

// UserError is a refusal the API shows verbatim (design 2.7.2).
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

// ErrNotFound is returned for an identifier that is not a replica of the project.
var ErrNotFound = errors.New("replicas: no such read replica")
