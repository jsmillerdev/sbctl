// Package replicas is the read-replica controller: it turns the replicas rows the Management API
// and the server-wide default write into running standbys on joined servers, reports their
// setup steps, status and lag, and removes them (design 2.7).
package replicas

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/mesh/peerapi"
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
	// List returns ref's replicas, oldest first. An empty ref lists every project's, the
	// standbys of the system cluster included (`supavise replicas ls`).
	List(ctx context.Context, ref string) ([]Replica, error)
	// Statuses returns one Status per replica of ref, in List's order.
	Statuses(ctx context.Context, ref string) ([]Status, error)
	// Lag returns the replica's lag samples since the given time, oldest first (1-minute points
	// kept in memory for 24 hours; empty after a leader restart).
	Lag(ctx context.Context, identifier string, since time.Time) ([]LagPoint, error)
}

// Remover is what the code that retires projects and nodes asks of the controller, beside
// Service. The controller implements both. Neither method records an opt-out: the default
// reconciler may create the replicas again when the project or the node comes back.
type Remover interface {
	// RemoveAll removes every replica of ref (a project delete or an in-place restore). A removal
	// that cannot finish now, because the node does not answer, leaves its row GOING_DOWN for the
	// controller to retry and is returned in a *PendingError.
	RemoveAll(ctx context.Context, ref string) error
	// RemoveOn removes every replica on node, the standby of the system cluster included
	// (`supavise node rm`). A node whose state is left has nothing to reach: its rows are deleted.
	RemoveOn(ctx context.Context, node string) error
}

// ReportSink takes the observed state a node reports to the leader (peerapi.PathReport). The
// mesh's intake handler calls it; the controller also polls the nodes, so a report only makes
// the leader's picture fresher.
type ReportSink interface {
	HandleReport(ctx context.Context, rep peerapi.Report)
}

// Pooler keeps the Supavisor tenant of a replica: external id the replica's identifier, the
// replica's Postgres port, the project's pool settings (design 2.7.6). Fleet provides it.
type Pooler interface {
	// EnsureReplicaTenant creates or updates the tenant; a second call with the same inputs changes nothing.
	EnsureReplicaTenant(ctx context.Context, ref, identifier string) error
	// RemoveReplicaTenant drops it; dropping one that is not there succeeds.
	RemoveReplicaTenant(ctx context.Context, identifier string) error
}

// PendingError is returned by Remover when some removals did not finish.
type PendingError struct{ Identifiers []string }

func (e *PendingError) Error() string {
	return "replicas: removal pending for " + strings.Join(e.Identifiers, ", ")
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
	// Init is set on every replica: in progress while it sets up, failed after a failed setup,
	// completed once it served.
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
