package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ErrReadOnly is returned by a write on a registry that cannot write: one opened with
// OpenReadOnly, or the system cluster of a node that is a standby (invariant I1).
var ErrReadOnly = errors.New("registry: read-only")

// Join token errors. Both wrap ErrConflict, so a caller that only cares that the token cannot be
// used checks ErrConflict.
var (
	ErrTokenUsed    = fmt.Errorf("%w: join token already used", ErrConflict)
	ErrTokenExpired = fmt.Errorf("%w: join token expired", ErrConflict)
)

// NodeState is where a node is in its life in the cluster.
type NodeState string

const (
	// NodeJoining: the node holds a certificate but has not confirmed that its system standby
	// streams. A node stuck here for an hour is removed.
	NodeJoining NodeState = "joining"
	NodeActive  NodeState = "active"
	// NodeFenced: the node lost the leadership to a higher epoch and must not start a primary.
	NodeFenced NodeState = "fenced"
	// NodeLeft: removed with `supavise node rm`; its certificate no longer authorizes a peer.
	// The row stays so that an id is never reused.
	NodeLeft NodeState = "left"
)

func (s NodeState) valid() bool {
	switch s {
	case NodeJoining, NodeActive, NodeFenced, NodeLeft:
		return true
	}
	return false
}

// FounderNodeID is the node every registry starts with (migration 1300): the server that was
// installed before any cluster existed. projects.node_id defaults to it.
const FounderNodeID = "n1"

// Node is one supavise server of the cluster.
type Node struct {
	// ID is "n1", "n2", ...; CreateNode assigns the next free one when empty.
	ID string
	// Name is the operator's name for the node: lower case letters, digits and hyphens, unique.
	Name string
	// Region is a code from config.Regions.
	Region string
	// PublicHost is what clients use for this node's pooler endpoints.
	PublicHost string
	// PeerAddr is a host:port hint for dialing the node's mesh port. A session works from
	// whichever side can connect, so the hint may be stale.
	PeerAddr   string
	Provider   NodeProvider
	Version    string
	State      NodeState
	CertSerial string
	JoinedAt   time.Time
}

// NodeProvider says where a node runs when that matters to the cluster. The keys it does not
// know are not kept.
type NodeProvider struct {
	AWS *NodeAWS `json:"aws,omitempty"`
}

// NodeAWS identifies an EC2 instance for fencing and address takeover.
type NodeAWS struct {
	InstanceID string `json:"instance_id"`
	Zone       string `json:"zone,omitempty"`
	Region     string `json:"region,omitempty"`
	// AllocationID is the node's own Elastic IP, when it has one.
	AllocationID string `json:"allocation_id,omitempty"`
}

// ServiceAddress is the address clients use for the cluster (DNS points at it).
type ServiceAddress struct {
	IP string `json:"ip,omitempty"`
	// AllocationID is the Elastic IP the survivor of a failover takes over on AWS.
	AllocationID string `json:"allocation_id,omitempty"`
}

// Maintenance announces that a node is down on purpose; automatic failover is suppressed
// until Until. The zero value means none.
type Maintenance struct {
	Node   string    `json:"node,omitempty"`
	Until  time.Time `json:"until,omitzero"`
	Reason string    `json:"reason,omitempty"`
}

// Active reports whether the announcement still holds at now.
func (m Maintenance) Active(now time.Time) bool { return m.Node != "" && now.Before(m.Until) }

// Cluster is the single cluster row.
type Cluster struct {
	Name string
	// Epoch counts leader changes, starting at 1. A leader that learns of a higher epoch fences itself.
	Epoch int64
	// Leader is the id of the node whose system cluster is not in recovery.
	Leader         string
	ServiceAddress ServiceAddress
	Maintenance    Maintenance
	// ChangeSeq moves with every change to projects, routes, project secrets, nodes and
	// replicas. A standby cannot LISTEN, so OpenReadOnly polls it.
	ChangeSeq int64
	UpdatedAt time.Time
}

// JoinToken is a one-time credential for `supavise node join`. The registry keeps only the hash
// of the secret.
type JoinToken struct {
	ID         string
	SecretHash []byte
	// NodeName, when set, is the only name the token admits.
	NodeName  string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
}

// Replica origins (Replica.Origin).
const (
	ReplicaManual  = "manual"  // `replicas add` or Studio's Add read replica
	ReplicaDefault = "default" // created by [replicas] default = "all"
	ReplicaSystem  = "system"  // the standby of the system cluster, created at join
)

// Replica statuses (Replica.Status): the Management API's project status values a replica uses.
const (
	ReplicaInit      = "INIT_READ_REPLICA"
	ReplicaInitError = "INIT_READ_REPLICA_FAILED"
)

// Replica setup steps (Replica.InitStep), in order; Studio shows the seven.
const (
	ReplicaStepRequested = "0_requested"
	ReplicaStepDone      = "6_completed_read_replica_setup"
)

// Replica is a standby Postgres of a project on a node that is not the project's home, with its PostgREST.
type Replica struct {
	// Identifier is "<ref>-rr-<region>-<id6>" (see ReplicaIdentifier).
	Identifier string
	Ref        string
	NodeID     string
	Origin     string
	// Status is a Management API project status (ACTIVE_HEALTHY, INIT_READ_REPLICA, ...).
	Status string
	// InitStep is the setup step reached; InitError is empty or the failed step's spec value.
	InitStep  string
	InitError string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ReplicaOptout records that the default reconciler must not create a replica of Ref on NodeID.
type ReplicaOptout struct{ Ref, NodeID string }

// Scope, kind and state of a Move.
type (
	MoveScope string
	MoveKind  string
	MoveState string
)

const (
	MoveProject MoveScope = "project"
	MoveServer  MoveScope = "server"

	MoveSwitchover MoveKind = "switchover" // planned: the old primary is alive and stops cleanly
	MoveFailover   MoveKind = "failover"   // unplanned: the old primary is fenced first

	MoveRunning MoveState = "running"
	MoveDone    MoveState = "done"
	MoveFailed  MoveState = "failed"
	MoveAborted MoveState = "aborted"
)

func (s MoveState) valid() bool {
	switch s {
	case MoveRunning, MoveDone, MoveFailed, MoveAborted:
		return true
	}
	return false
}

// Move is the audit and resume log of one switchover or failover.
type Move struct {
	ID    int64
	Scope MoveScope
	Kind  MoveKind
	// Ref is the project of a project move; empty for a server move.
	Ref       string
	FromNode  string
	ToNode    string
	Epoch     int64
	State     MoveState
	Steps     []MoveStep
	Error     string
	StartedAt time.Time
	EndedAt   *time.Time
}

// MoveStep is one finished step of a Move.
type MoveStep struct {
	Name   string    `json:"name"`
	At     time.Time `json:"at"`
	Detail string    `json:"detail,omitempty"`
}

// Patterns the database enforces and the in-memory registry mirrors.
var (
	nodeIDRe    = regexp.MustCompile(`^n[0-9]{1,4}$`)
	nodeNameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	replicaIDRe = regexp.MustCompile(`^(system|[a-z]{20})-rr-[a-z0-9-]+-[a-z0-9]{6}$`)
)

// ValidNodeName reports whether s can be the name of a node.
func ValidNodeName(s string) bool { return nodeNameRe.MatchString(s) }

// ValidReplicaIdentifier reports whether s has the shape of a replica identifier.
func ValidReplicaIdentifier(s string) bool { return replicaIDRe.MatchString(s) }

// ReplicaIdentifier builds "<ref>-rr-<region>-<id6>". Studio requires "-rr-" in it and shows the
// last "-" segment as the short id.
func ReplicaIdentifier(ref, region, id6 string) string { return ref + "-rr-" + region + "-" + id6 }

// ParseReplicaIdentifier splits an identifier into its project ref, region and six character id.
func ParseReplicaIdentifier(identifier string) (ref, region, id6 string, ok bool) {
	if !ValidReplicaIdentifier(identifier) {
		return "", "", "", false
	}
	ref, rest, _ := strings.Cut(identifier, "-rr-")
	i := strings.LastIndexByte(rest, '-')
	return ref, rest[:i], rest[i+1:], true
}

// ClusterStore holds the cluster membership, the placement of projects and replicas, join
// tokens and the log of moves. Both registries implement it and Registry embeds it, so anything
// that holds a Registry reaches it.
//
// A registry opened with OpenReadOnly (a standby's) answers every Get* and List* and refuses
// the rest with ErrReadOnly. Get* return ErrNotFound; Create* return ErrConflict on duplicates;
// a write that names a node, project or replica that does not exist returns ErrNotFound.
type ClusterStore interface {
	// CreateNode inserts n. An empty ID gets the next free "n<k>" and an empty State is
	// NodeJoining: a node is not trusted until it confirms. ErrConflict when the name is taken.
	CreateNode(ctx context.Context, n *Node) error
	GetNode(ctx context.Context, id string) (*Node, error)
	GetNodeByName(ctx context.Context, name string) (*Node, error)
	// ListNodes returns every node, the left ones included, by id.
	ListNodes(ctx context.Context) ([]Node, error)
	// UpdateNode writes name, region, public host, peer address, provider and version, and
	// nothing else; it is a no-op (no write) when none of them changed. ErrConflict when the
	// name belongs to another node.
	UpdateNode(ctx context.Context, n *Node) error
	// SetNodeState moves the node to s. It does not check that the move is a legal transition.
	SetNodeState(ctx context.Context, id string, s NodeState) error
	// SetNodeCert records the serial of the certificate that authorizes the node.
	SetNodeCert(ctx context.Context, id, serial string) error
	// DeleteNode removes a node row. ErrConflict while the cluster leader, a project's home or a
	// replica refers to it; `node rm` sets NodeLeft instead.
	DeleteNode(ctx context.Context, id string) error

	GetCluster(ctx context.Context) (*Cluster, error)
	SetClusterName(ctx context.Context, name string) error
	SetServiceAddress(ctx context.Context, a ServiceAddress) error
	// SetMaintenance announces m; the zero Maintenance clears it.
	SetMaintenance(ctx context.Context, m Maintenance) error
	// SetLeader makes node the leader at epoch, a compare-and-set: ErrConflict unless epoch is
	// higher than the stored one (or equal with the same leader, which changes nothing).
	SetLeader(ctx context.Context, node string, epoch int64) error

	// SetProjectNode moves the home of ref to node. epoch must be the cluster's current epoch
	// (ErrConflict otherwise: the caller is a stale leader) and node must be active. In the same
	// transaction it deletes the replica row of ref on node, so a replica is never on its own
	// project's home (I2). The caller writes the replica row of the old home afterwards.
	SetProjectNode(ctx context.Context, ref, node string, epoch int64) error

	// CreateReplica inserts r. Origin, Status and InitStep default to "manual",
	// INIT_READ_REPLICA and "0_requested". ErrConflict when the project already has a replica on
	// the node or r.NodeID is the project's home (I2); ErrNotFound when the project or node is unknown.
	CreateReplica(ctx context.Context, r *Replica) error
	GetReplica(ctx context.Context, identifier string) (*Replica, error)
	// ListReplicas returns the replicas of ref, or of every project when ref is empty, by
	// project and creation time.
	ListReplicas(ctx context.Context, ref string) ([]Replica, error)
	// ListReplicasOn returns the replicas on node.
	ListReplicasOn(ctx context.Context, node string) ([]Replica, error)
	// SetReplicaStatus writes the status, the setup step and the failed step's code. It writes
	// nothing when all three are unchanged, so a status report every 10 seconds costs no WAL.
	SetReplicaStatus(ctx context.Context, identifier, status, initStep, initError string) error
	DeleteReplica(ctx context.Context, identifier string) error

	// PutReplicaOptout records an opt-out; putting one that exists changes nothing.
	PutReplicaOptout(ctx context.Context, ref, node string) error
	// DeleteReplicaOptout clears an opt-out; clearing one that does not exist is not an error.
	DeleteReplicaOptout(ctx context.Context, ref, node string) error
	ListReplicaOptouts(ctx context.Context) ([]ReplicaOptout, error)

	// CreateJoinToken inserts t; ErrConflict on a duplicate id.
	CreateJoinToken(ctx context.Context, t *JoinToken) error
	GetJoinToken(ctx context.Context, id string) (*JoinToken, error)
	// UseJoinToken marks the token used at the given time, once: ErrTokenUsed for a second
	// use, ErrTokenExpired when at is not before ExpiresAt, ErrNotFound for an unknown id.
	UseJoinToken(ctx context.Context, id string, at time.Time) (*JoinToken, error)
	// DeleteExpiredJoinTokens removes the tokens that expired before the given time and returns how many.
	DeleteExpiredJoinTokens(ctx context.Context, before time.Time) (int, error)

	// CreateMove inserts m as MoveRunning with no steps and sets ID and StartedAt.
	CreateMove(ctx context.Context, m *Move) error
	GetMove(ctx context.Context, id int64) (*Move, error)
	// ListMoves returns the newest moves first; an empty state lists every state, a limit
	// of zero or less means 100.
	ListMoves(ctx context.Context, state MoveState, limit int) ([]Move, error)
	// AppendMoveStep adds a finished step to the move's log.
	AppendMoveStep(ctx context.Context, id int64, s MoveStep) error
	// FinishMove sets the final state (done, failed or aborted), the error text and EndedAt.
	FinishMove(ctx context.Context, id int64, state MoveState, errText string) error
}

func marshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil { // the types marshaled here are plain structs
		panic(err)
	}
	return b
}
