// Package peerapi defines the peer API: the HTTP requests nodes send each other over rpc streams
// of the mesh (internal/mesh), as paths and JSON bodies. It holds types only. The server side
// registers handlers with mesh.Handle (each owner registers its own endpoints); the client side
// calls mesh.RPC.Call with these paths and types.
//
// Every request carries the caller's node id in its client certificate (mesh.PeerFrom), so no
// body repeats it. Every non-2xx answer carries an Error. Fields that name a time are RFC 3339.
// LSNs are the text form of pg_lsn ("0/3000100"). Instances and projects are named as in the
// registry: an identifier is "<ref>-rr-<region>-<id6>", a ref is "system" or twenty letters.
package peerapi

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// Paths, in the pattern syntax of net/http's ServeMux, with the method in front where the
// endpoint has one. The functions below fill the wildcards in for a client.
const (
	// PathPing: any node to any node, every 5 seconds. Answers a Ping.
	PathPing = "/peer/v1/ping"
	// PathInstance: leader to the node that runs (or will run) a replica. PUT ensures the
	// instance (InstanceSpec in, InstanceStatus out), GET observes it (InstanceStatus out),
	// DELETE removes it and its data (204).
	PathInstance = "/peer/v1/instances/{identifier}"
	// PathInstanceAction: POST, leader to the node. {action} is an Action; the body is an
	// InstanceAction and the answer an InstanceStatus.
	PathInstanceAction = "/peer/v1/instances/{identifier}/{action}"
	// PathPlane: POST, leader to a project's home, for a project not homed on the leader. {method}
	// is a PlaneMethod; the body is a PlaneCall and the answer a PlaneResult.
	PathPlane = "/peer/v1/projects/{ref}/plane/{method}"
	// PathBackup: POST, leader to a project's home, for the backup operations that need the data
	// directory. {op} is a BackupOp; the body is a BackupRequest and the answer a BackupResult.
	PathBackup = "/peer/v1/projects/{ref}/backup/{op}"
	// PathFleetRefresh: POST, leader to a node that runs Supavisor. Drops the node's cached copy
	// of the pooler tenant ({tenant} is the external id: a ref or a replica identifier). No
	// body; the answer is a RefreshResult.
	PathFleetRefresh = "/peer/v1/fleet/refresh/{tenant}"
	// PathReport: POST, node to leader, every 10 seconds. A Report in, 204 out.
	PathReport = "/peer/v1/report"
	// PathCerts: GET, follower to leader. The CertMagic store as a CertSnapshot; the answer
	// carries an ETag and honors If-None-Match with 304.
	PathCerts = "/peer/v1/certs"
	// PathConfig: GET, follower to leader. The cluster-scoped configuration as a ClusterConfig.
	PathConfig = "/peer/v1/config"
	// PathFence: POST, any node to a node. A FenceRequest in, a FenceResponse out.
	PathFence = "/peer/v1/fence"
	// PathCertsRenew: POST, node to leader. A CertRenewRequest in, a CertRenewResponse out.
	PathCertsRenew = "/peer/v1/certs/renew"
	// PathJoin: joiner to leader, over TLS with server authentication only (no client
	// certificate yet). GET answers a JoinChallenge; POST takes a JoinRequest and answers a JoinResponse.
	PathJoin = "/peer/v1/join"
	// PathJoinConfirm: POST, joiner to leader, once its system standby streams. A JoinConfirm in, 204 out.
	PathJoinConfirm = "/peer/v1/join/confirm"
	// PathRejoin: POST, a fenced node to the leader, over mutual TLS with the certificate the node
	// keeps. A RejoinRequest in, a RejoinResponse out. The node's row goes back to joining and
	// PathJoinConfirm makes it active.
	PathRejoin = "/peer/v1/rejoin"
)

func InstancePath(identifier string) string {
	return "/peer/v1/instances/" + url.PathEscape(identifier)
}

func InstanceActionPath(identifier string, a Action) string {
	return InstancePath(identifier) + "/" + url.PathEscape(string(a))
}

func PlanePath(ref string, m PlaneMethod) string {
	return "/peer/v1/projects/" + url.PathEscape(ref) + "/plane/" + url.PathEscape(string(m))
}

func BackupPath(ref string, op BackupOp) string {
	return "/peer/v1/projects/" + url.PathEscape(ref) + "/backup/" + url.PathEscape(string(op))
}

func FleetRefreshPath(tenant string) string {
	return "/peer/v1/fleet/refresh/" + url.PathEscape(tenant)
}

// Error is the body of every non-2xx answer.
type Error struct {
	Message string `json:"message"`
	// Code is a short machine-readable reason when the caller can act on it ("stale_epoch",
	// "not_leader", "not_home", "lag_too_high").
	Code string `json:"code,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Ping is what a node answers to PathPing: its own view of the cluster, which is how nodes
// notice a higher epoch, a version skew or a schema they cannot read.
type Ping struct {
	Node string `json:"node"`
	// Epoch and Leader are the cluster epoch and leader (a node id, "" when unknown) as the
	// sender believes them.
	Epoch  int64  `json:"epoch"`
	Leader string `json:"leader"`
	// Version is the release; Schema is the registry migrations the node's database has applied
	// (registry.AppliedMigrations, joined with commas), not the label registry.SchemaVersion()
	// gives: two nodes of different releases compare what their databases hold.
	Version string `json:"version"`
	Schema  string `json:"schema"`
	// Health is the node's own verdict: "healthy", "degraded" or "down" (health.Verdict).
	Health string    `json:"health"`
	Time   time.Time `json:"time"`
}

// SchemaString writes a set of applied registry migrations as Ping.Schema: the names, sorted, joined
// by commas.
func SchemaString(names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// ParseSchema reads Ping.Schema back into the set of migration names.
func ParseSchema(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// SchemaAhead lists the migrations that remote has applied and local has not: what a node whose
// binary knows only local cannot run on remote's database.
func SchemaAhead(remote, local []string) []string {
	have := make(map[string]bool, len(local))
	for _, n := range local {
		have[n] = true
	}
	var out []string
	for _, n := range remote {
		if !have[n] {
			out = append(out, n)
		}
	}
	return out
}

// Action is a replica operation of PathInstanceAction.
type Action string

const (
	// ActionRestart restarts the replica's Postgres and PostgREST.
	ActionRestart Action = "restart"
	ActionStop    Action = "stop"
	ActionStart   Action = "start"
	// ActionPromote turns the standby into a primary on the canonical port. See InstanceAction.
	ActionPromote Action = "promote"
	// ActionDemote turns a primary into a replica of the project's new home, in place.
	ActionDemote Action = "demote"
)

// InstanceSpec asks a node to create or keep a replica instance (PUT PathInstance). The node
// renders everything else from its own copy of the registry: the project row, its sealed
// secrets and its compute size. A repeated PUT with the same spec changes nothing.
type InstanceSpec struct {
	Identifier string `json:"identifier"`
	Ref        string `json:"ref"`
	// BackupID is the base backup to seed from; empty means the newest complete one.
	BackupID string `json:"backup_id,omitempty"`
	// NoUpstream seeds from the archive only, with no primary_conninfo: the standby that
	// `supavise failover --restore-missing` drains and promotes.
	NoUpstream bool `json:"no_upstream,omitempty"`
	// Epoch is the cluster epoch the leader acts in; a node that knows a higher one refuses.
	Epoch int64 `json:"epoch"`
}

// InstanceStatus is what a node observes about one replica instance (answer of GET, PUT and
// POST on PathInstance and PathInstanceAction, and the items of a Report).
type InstanceStatus struct {
	Identifier string `json:"identifier"`
	Ref        string `json:"ref"`
	// Role is "replica", "primary" (promoted, not yet re-registered) or "absent".
	Role string `json:"role"`
	// Step is the setup step reached (registry.ReplicaStep*, "0_requested" ...
	// "6_completed_read_replica_setup"). Error is empty or the failed step's spec value
	// ("1_read_replica_instance_launch_failed" ... "5_complete_read_replica_setup_failed") and
	// Detail the cause in words.
	Step   string `json:"step"`
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`

	PostgresUp     bool `json:"postgres_up"`
	PostgRESTReady bool `json:"postgrest_ready"`
	// InRecovery is pg_is_in_recovery(); ReceiverStatus is pg_stat_wal_receiver.status ("streaming",
	// "" when there is no receiver); the LSNs are pg_last_wal_receive_lsn() and pg_last_wal_replay_lsn().
	InRecovery     bool   `json:"in_recovery"`
	ReceiverStatus string `json:"receiver_status,omitempty"`
	ReceiveLSN     string `json:"receive_lsn,omitempty"`
	ReplayLSN      string `json:"replay_lsn,omitempty"`
	// LagSeconds is computed as Studio does: 0 when receive equals replay and the receiver is
	// streaming, else now() - pg_last_xact_replay_timestamp(). Nil when unknown.
	LagSeconds *float64  `json:"lag_seconds,omitempty"`
	At         time.Time `json:"at"`
}

// InstanceAction is the body of PathInstanceAction.
type InstanceAction struct {
	// Epoch is the cluster epoch the leader acts in. Promote writes it to promote.ok; any
	// action under an epoch lower than the node's is refused with code "stale_epoch".
	Epoch int64 `json:"epoch"`
	// WaitLSN (promote): wait until replay reaches it before promoting. A planned switchover
	// passes the old primary's final checkpoint LSN.
	WaitLSN string `json:"wait_lsn,omitempty"`
	// DrainArchive (promote): let restore_command drain the WAL archive first, polling until
	// replay is stable and the archive returns not-found, bounded by 10 seconds. An unplanned
	// failover sets it.
	DrainArchive bool `json:"drain_archive,omitempty"`
	// TimeoutSeconds bounds the waits of the action. 0 means the node's default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// PlaneMethod names a method of lifecycle.Plane. placement's reflection test fails when the
// interface gains a method that has no constant here.
type PlaneMethod string

const (
	PlaneCreate        PlaneMethod = "create"
	PlaneDelete        PlaneMethod = "delete"
	PlaneSnapshot      PlaneMethod = "snapshot"
	PlaneRoute         PlaneMethod = "route"
	PlaneUsage         PlaneMethod = "usage"
	PlaneStart         PlaneMethod = "start"
	PlaneStartDatabase PlaneMethod = "start_database"
	PlaneStop          PlaneMethod = "stop"
	PlaneReconfigure   PlaneMethod = "reconfigure"
	PlaneHealth        PlaneMethod = "health"
)

// PlaneMethods lists every PlaneMethod.
var PlaneMethods = []PlaneMethod{PlaneCreate, PlaneDelete, PlaneSnapshot, PlaneRoute, PlaneUsage,
	PlaneStart, PlaneStartDatabase, PlaneStop, PlaneReconfigure, PlaneHealth}

// PlaneCall is the body of PathPlane. Args is the JSON of the method's arguments after the
// context and the ref: the project row and keys for start, the seed for create, nothing for
// stop. placement defines each method's argument and result shape in one place and tests the
// round trip.
type PlaneCall struct {
	Epoch int64           `json:"epoch"`
	Args  json.RawMessage `json:"args,omitempty"`
}

// PlaneResult is the answer of PathPlane: the JSON of the method's results.
type PlaneResult struct {
	Result json.RawMessage `json:"result,omitempty"`
}

// BackupOp is the {op} of PathBackup.
type BackupOp string

const (
	// BackupBase takes a base backup of the project on its home node.
	BackupBase BackupOp = "base"
	// BackupRestore restores the project on its home node (an in-place restore).
	BackupRestore BackupOp = "restore"
)

// BackupRequest is the body of PathBackup.
type BackupRequest struct {
	Epoch int64 `json:"epoch"`
	// Reason is the manifest reason of a base backup ("manual", "scheduled", "pre-upgrade", ...).
	Reason string `json:"reason,omitempty"`
	// BackupID and Target choose what a restore recovers to; empty means the end of the archive.
	BackupID string     `json:"backup_id,omitempty"`
	Target   *time.Time `json:"target,omitempty"`
}

// BackupResult describes the base backup an operation took or recovered from.
type BackupResult struct {
	ID        string `json:"id"`
	Timeline  int    `json:"timeline"`
	StartLSN  string `json:"start_lsn"`
	StopLSN   string `json:"stop_lsn"`
	SizeBytes int64  `json:"size_bytes"`
}

// RefreshResult is the answer of PathFleetRefresh.
type RefreshResult struct {
	Refreshed bool `json:"refreshed"`
}

// Report is what a node tells the leader about what it observes (PathReport). The leader keeps
// lag and receiver state in memory and writes only status transitions to the registry (I5).
type Report struct {
	Node string    `json:"node"`
	At   time.Time `json:"at"`
	// Epoch is the epoch the node believes; a higher one than the leader's fences the leader.
	Epoch     int64            `json:"epoch"`
	Instances []InstanceStatus `json:"instances,omitempty"`
	// Projects is the health of the projects homed on the node when it is not the leader.
	Projects []ProjectHealth `json:"projects,omitempty"`
}

// ProjectHealth is one project's health as its home node sees it.
type ProjectHealth struct {
	Ref     string `json:"ref"`
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail,omitempty"`
}

// CertSnapshot is the CertMagic store of the leader (PathCerts), file by file.
type CertSnapshot struct {
	Files []CertFile `json:"files"`
}

// CertFile is one file of the CertMagic store: Path relative to its root, Data and the permission bits.
type CertFile struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Data []byte `json:"data"`
}

// ClusterConfig is the cluster-scoped configuration (PathConfig): the text config.MarshalCluster
// renders for config.d/10-cluster.toml, and a revision (the SHA-256 of that text, hex) that lets a
// node skip a write when nothing changed.
type ClusterConfig struct {
	Revision string `json:"revision"`
	TOML     string `json:"toml"`
}

// FenceRequest asks a node to stop acting as a primary (PathFence): the cooperative fence. The
// node stops its project clusters and the system cluster, removes their *.run files and records
// that epoch Epoch belongs to Leader. A node that knows a higher epoch answers with it and does
// nothing.
type FenceRequest struct {
	Epoch  int64  `json:"epoch"`
	Leader string `json:"leader"`
	Reason string `json:"reason,omitempty"`
}

// FenceResponse tells what the node did.
type FenceResponse struct {
	// Epoch is the node's epoch after the request.
	Epoch int64 `json:"epoch"`
	// Fenced is true when the node is now fenced (it was, or it has just become so).
	Fenced bool `json:"fenced"`
	// Stopped lists the refs whose primary the node stopped.
	Stopped []string `json:"stopped,omitempty"`
}

// CertRenewRequest asks the leader to sign a new certificate for the calling node (PathCertsRenew).
type CertRenewRequest struct {
	// CSR is a DER certificate request for the node's key.
	CSR []byte `json:"csr"`
}

// CertRenewResponse carries the certificate: PEM, one year, SAN URI supavise://node/<id>.
type CertRenewResponse struct {
	Cert     []byte    `json:"cert"`
	Serial   string    `json:"serial"`
	NotAfter time.Time `json:"not_after"`
}

// JoinChallenge is the answer of GET PathJoin: a nonce the joiner mixes into its proof, so
// that a recorded request cannot be replayed.
type JoinChallenge struct {
	Nonce     []byte    `json:"nonce"`
	ExpiresAt time.Time `json:"expires_at"`
}

// JoinRequest is the body of POST PathJoin. Proof is HMAC-SHA256 keyed with the token's secret over
// Nonce, CSR and Name in that order; the secret itself never travels.
type JoinRequest struct {
	TokenID    string `json:"token_id"`
	Nonce      []byte `json:"nonce"`
	Proof      []byte `json:"proof"`
	CSR        []byte `json:"csr"`
	Name       string `json:"name"`
	Region     string `json:"region"`
	PublicHost string `json:"public_host"`
	PeerAddr   string `json:"peer_addr"`
	// Provider is what the joiner knows about where it runs (the AWS instance id and zone).
	Provider registry.NodeProvider `json:"provider"`
	Version  string                `json:"version"`
	// ArtifactPins are the service versions the joiner's release pins, for the version window.
	ArtifactPins map[string]string `json:"artifact_pins,omitempty"`
	// WithoutKey: the joiner has the master key already (--master-key-file, --key-from-escrow), so
	// the leader leaves it out of the answer. The CA pin of the token proves it is the same key.
	WithoutKey bool `json:"without_key,omitempty"`
}

// JoinResponse is the answer of POST PathJoin. The node row is joining until JoinConfirm.
type JoinResponse struct {
	NodeID string `json:"node_id"`
	// Cert and CA are PEM: the node's certificate and the cluster CA.
	Cert []byte `json:"cert"`
	CA   []byte `json:"ca"`
	// MasterKey is the cluster's master key, which the new node writes to /etc/supavise/master.key.
	MasterKey []byte `json:"master_key"`
	// ClusterConfig is the text for config.d/10-cluster.toml.
	ClusterConfig string          `json:"cluster_config"`
	System        SystemBootstrap `json:"system"`
}

// RejoinRequest is the body of POST PathRejoin.
type RejoinRequest struct {
	Version string `json:"version"`
	// ArtifactPins: see JoinRequest.
	ArtifactPins map[string]string `json:"artifact_pins,omitempty"`
}

// RejoinResponse is the answer of POST PathRejoin: how to rebuild the system standby, and the
// cluster-scoped configuration as of now.
type RejoinResponse struct {
	System        SystemBootstrap `json:"system"`
	ClusterConfig string          `json:"cluster_config"`
}

// SystemBootstrap describes how the joiner seeds its standby of the system cluster.
type SystemBootstrap struct {
	// Identifier is the replica row of the system cluster on the new node.
	Identifier string `json:"identifier"`
	// BackupID is the system project's base backup to seed from.
	BackupID string `json:"backup_id"`
	Leader   string `json:"leader"`
	Epoch    int64  `json:"epoch"`
	// ReplicationPassword is the opened password of the system cluster's replication role
	// (supabase_replication_admin): the joiner has no registry to read it from yet, and the standby's
	// primary_conninfo needs it. It is a secret: it goes into the 0600 postgresql.auto.conf of the
	// standby and the 0600 join state, and into no log.
	ReplicationPassword string `json:"replication_password,omitempty"`
}

// JoinConfirm is the body of POST PathJoinConfirm: the joiner's standby streams, so the node
// may become active.
type JoinConfirm struct {
	NodeID string `json:"node_id"`
	// ReplayLSN is the standby's replay position when it confirmed.
	ReplayLSN string `json:"replay_lsn,omitempty"`
}
