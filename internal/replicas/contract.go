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
	//
	// Setup is where the limits are enforced, with the texts of design 2.7.2: the size cap, the
	// limit of joined servers minus one and the home-node rule (a replica is never on its
	// project's home, invariant I2). The checks an API handler makes before it calls are a fast
	// path and prove nothing: two requests can pass them together. The home rule is the
	// registry's (CreateReplica refuses the home in the transaction that inserts the row, and a
	// failover that lands between the check and the insert is refused there). The cap is
	// checked again after the insert, and a row that put the project over it is deleted and
	// refused, so concurrent requests, from the daemon or from `supavise replicas add`, never
	// leave a project over its cap; when they collide, both may be refused and a retry succeeds.
	Setup(ctx context.Context, ref, region string) error
	// SetupOn is Setup on a named node (`supavise replicas add --node`).
	SetupOn(ctx context.Context, ref, nodeID string) error
	// Remove starts removing a replica of ref; ErrNotFound when ref has no such identifier.
	Remove(ctx context.Context, ref, identifier string) error
	// Restart restarts one replica's units on its node; ErrNotFound as for Remove. It does not
	// block until the replica is back: it returns once the replica is RESTARTING and the call to
	// the node is under way in the background, and the next healthy observation made after that
	// makes the replica ACTIVE_HEALTHY again. The call to the node gets Timeouts.Restart (5
	// minutes); when it fails the replica becomes ACTIVE_UNHEALTHY and an alert is raised. A
	// replica that is not up (setting up, failed, going down) is refused with a *UserError. A
	// caller that wants to know when it is back follows the status.
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
	// RemoveAll removes every replica of ref (a project delete or an in-place restore). It waits
	// a short while for a setup that is acting on a replica, then removes it. A removal that cannot
	// finish now, because the node does not answer, leaves its row GOING_DOWN for the controller to
	// retry and is returned in a *PendingError.
	//
	// A project row owns its replica rows (the registry deletes them with it), so the caller must
	// not delete the project while RemoveAll returns a *PendingError: the retry would go with the
	// row and the instance would stay on the node. It refuses the delete (the node can be tried
	// again) or retries RemoveAll until it returns nil. An in-place restore may go on only
	// when RemoveAll returns nil, too.
	RemoveAll(ctx context.Context, ref string) error
	// RemoveOn removes every replica on node, the standby of the system cluster included
	// (`supavise node rm`). The caller first moves the node out of the active state (left): while
	// it is active the default reconciler makes the replicas again, so RemoveOn refuses an active
	// node. A node whose state is left has nothing to reach: its rows are deleted. The row of the
	// system standby is only deleted, never sent to the node: the node's system cluster goes with
	// the node's retirement (internal/cluster), not with this call.
	RemoveOn(ctx context.Context, node string) error
}

// ReportSink takes the observed state a node reports to the leader (peerapi.PathReport). The
// mesh's intake handler calls it; the controller also polls the nodes, so a report only makes
// the leader's picture fresher. The sink trusts Report.Node (it takes an instance only from the
// node whose row it is), so the intake must set Node from the node's authenticated identity.
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
	// completed once it served. A replica going down keeps the outcome of its setup.
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
