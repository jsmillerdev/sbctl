package failover

import (
	"context"

	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// The orchestrator does nothing to a database, a unit or a file itself. Everything it does to the
// world goes through the ports in this file, so that every branch of a move (a fence that fails, a
// promotion that fails halfway, a crash after any step) is tested with fakes. The daemon wires
// them to the real parts in internal/app/wire_failover.go: placement for the plane and the
// instances, the mesh for the peers, fleet for the shared services, backup for the epoch marker.
//
// A node is named by its id ("n2"). A port that takes a node runs the operation itself when
// the node is this one and sends it over the peer API otherwise; the orchestrator does not care.

// Store is the registry as the orchestrator uses it. registry.Registry implements it. On a
// follower it answers reads from the replicated registry and refuses writes with registry.ErrReadOnly
// until the node's system cluster is promoted; Deps.Store therefore returns the node's current
// registry each time it is called.
type Store interface {
	GetProject(ctx context.Context, ref string) (*registry.Project, error)
	ListProjects(ctx context.Context) ([]registry.Project, error)
	SetProjectStatus(ctx context.Context, ref string, s registry.Status) error

	GetNode(ctx context.Context, id string) (*registry.Node, error)
	ListNodes(ctx context.Context) ([]registry.Node, error)
	SetNodeState(ctx context.Context, id string, s registry.NodeState) error
	GetCluster(ctx context.Context) (*registry.Cluster, error)
	SetLeader(ctx context.Context, node string, epoch int64) error
	SetMaintenance(ctx context.Context, m registry.Maintenance) error
	SetProjectNode(ctx context.Context, ref, node string, epoch int64) error

	CreateReplica(ctx context.Context, r *registry.Replica) error
	GetReplica(ctx context.Context, identifier string) (*registry.Replica, error)
	ListReplicas(ctx context.Context, ref string) ([]registry.Replica, error)

	CreateMove(ctx context.Context, m *registry.Move) error
	ListMoves(ctx context.Context, state registry.MoveState, limit int) ([]registry.Move, error)
	AppendMoveStep(ctx context.Context, id int64, s registry.MoveStep) error
	FinishMove(ctx context.Context, id int64, state registry.MoveState, errText string) error
}

var _ Store = registry.Registry(nil)

// Instances are the replica operations on any node: ensure, observe and the actions promote,
// demote, stop, start and restart. placement.InstanceOps implements it. The system cluster of a
// node is an instance too (its replica row has origin "system"), so promoting a node's standby of
// the system cluster is a Do with ActionPromote on its identifier.
type Instances interface {
	Ensure(ctx context.Context, node string, spec peerapi.InstanceSpec) (peerapi.InstanceStatus, error)
	Observe(ctx context.Context, node, identifier string) (peerapi.InstanceStatus, error)
	Do(ctx context.Context, node, identifier string, a peerapi.Action, req peerapi.InstanceAction) (peerapi.InstanceStatus, error)
}

// Primaries are the operations on the primary of a project (or of "system") on a node. The
// local half is LocalPrimaries; the orchestrator reaches another node through the peer
// endpoints of peer.go.
type Primaries interface {
	// Stop stops the project's units on node, PostgREST and GoTrue first and Postgres last with
	// a fast shutdown, and returns when the cluster has stopped. The LSN is the cluster's
	// shutdown checkpoint ("0/3000060"), which is the last WAL the old primary wrote. Stopping a
	// project that is not running returns the checkpoint of its control file. The daemon, and
	// with it the WAL relay the cluster archives through, stays up: a cluster that cannot
	// archive its last segments cannot finish its shutdown.
	Stop(ctx context.Context, node, ref string) (lsn string, err error)
	// Start starts the project's units on node as a primary and its backup timer. It is
	// idempotent: units that run are left alone.
	Start(ctx context.Context, node, ref string) error
	// Healthy asks whether the project's primary on node answers. An error means the node could
	// not be asked; ok false with a nil error means it was asked and the project is not well.
	Healthy(ctx context.Context, node, ref string) (ok bool, detail string, err error)
	// SetAside moves the data of the project's old primary on node to data.diverged-<epoch>,
	// where `supavise node rejoin` and the janitor of [failover] keep_diverged_days find it, and
	// clears the project's fence record, so that a replica can be built in its place.
	SetAside(ctx context.Context, node, ref string, epoch int64) error
}

// LocalPrimaries is Primaries for this node, implemented over the node's plane (lifecycle).
type LocalPrimaries interface {
	Stop(ctx context.Context, ref string) (lsn string, err error)
	Start(ctx context.Context, ref string) error
	Healthy(ctx context.Context, ref string) (ok bool, detail string, err error)
	SetAside(ctx context.Context, ref string, epoch int64) error
}

// BaseBackups takes a base backup of a project on its home node. placement.BackupOps implements it.
type BaseBackups interface {
	BaseBackup(ctx context.Context, node, ref string, req peerapi.BackupRequest) (peerapi.BackupResult, error)
}

// Fleet is the shared services of the leader (Supavisor, Realtime and the rest of internal/fleet).
type Fleet interface {
	// QuiesceTenant makes Supavisor and Realtime let go of the project's database and keeps their
	// clients out until the stop is over. Realtime's logical walsender otherwise holds the
	// shutdown of the cluster until it times out.
	QuiesceTenant(ctx context.Context, ref string) error
	// EnsureTenant registers the project with Supavisor and Realtime again once its database
	// answers at its new home, which makes Realtime create its slot, and refreshes the pooler's
	// cache on every node that runs Supavisor.
	EnsureTenant(ctx context.Context, ref string) error
}

// Peers are the calls to other nodes that are not one of the ports above.
type Peers interface {
	// Ping asks a node for its own view of the cluster. An error means no answer.
	Ping(ctx context.Context, node string) (peerapi.Ping, error)
	// Fence asks a node to stop acting as a primary (the cooperative fence). The call fails when
	// the node does not answer; a node that answers has fenced itself or says why it did not.
	Fence(ctx context.Context, node string, req FenceCall) (peerapi.FenceResponse, error)
}

// Leader is what the current leader does for a planned switchover of the whole server. The
// survivor calls it; the leader's own handler (peer.go) does the work with LocalPrimaries and
// LocalServices.
type Leader interface {
	// Quiesce announces maintenance, stops the projects' clusters, the system project's GoTrue
	// and the shared services, and last the system cluster, and returns the final checkpoint LSN
	// of each cluster by ref.
	Quiesce(ctx context.Context, node string, req QuiesceRequest) (QuiesceResult, error)
	// Resume starts again what Quiesce stopped, and clears the maintenance announcement. The
	// survivor calls it when it aborts before it fenced and promoted anything.
	Resume(ctx context.Context, node string) error
}

// LocalServices are the shared services this node runs as the leader, started and stopped as one.
type LocalServices interface {
	// Stop stops the system project's GoTrue and the shared services (Studio, Realtime, Storage,
	// Edge Runtime and the rest) in the reverse order of their start. The system cluster stays.
	Stop(ctx context.Context) error
	// Start starts them again.
	Start(ctx context.Context) error
}

// Takeover is the node's side of becoming the leader.
type Takeover interface {
	// BecomeLeader returns when the system cluster of this node has been promoted and the daemon
	// runs as the leader at epoch: the registry accepts writes, the proxy manages the
	// certificates, the Management API and the shared services answer, the Edge Functions are
	// synced and the forwarders are rebound. The orchestrator promotes the cluster; the daemon
	// notices the role change and does the rest (invariant I1), and this call waits for it.
	BecomeLeader(ctx context.Context, epoch int64) error
}

// ReplicaSetup builds a replica where there is none. replicas.Service implements it.
type ReplicaSetup interface {
	SetupOn(ctx context.Context, ref, nodeID string) error
}

// Locker serializes a move with the lifecycle operations on the same project (pause, resume,
// delete, upgrade): the engine's lock for the project.
type Locker interface {
	Lock(ctx context.Context, ref string) (unlock func(), err error)
}

// Cloud is implemented by a Provider that can say what the cloud reports about a node: AWS knows
// whether the instance runs and whether its status checks pass. The automatic server mode needs
// it, because a peer that does not answer is not a peer that is down.
type Cloud interface {
	PeerState(ctx context.Context, node registry.Node) (PeerState, error)
}

// PeerState is what the cloud says about a node's machine.
type PeerState struct {
	// State is the machine's state in the cloud's words ("running", "stopped", "terminated").
	State string
	// Impaired is true when the cloud's status checks fail (system or instance).
	Impaired bool
	Detail   string
}

// Down reports whether the machine is not running, or is running and failing its checks. A
// pending or stopping machine is not down: it has not finished changing state.
func (s PeerState) Down() bool {
	switch s.State {
	case "stopped", "terminated", "shutting-down":
		return true
	case "running":
		return s.Impaired
	}
	return false
}
