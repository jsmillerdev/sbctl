package backup

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// This file is the contract of the replica work in this package: what the replica controller and
// the failover orchestrator call, and the formats other packages write and this package reads.
// Service implements ReplicaSeeder, BaseBackupEnsurer and EpochMarkerStore.

// ReplicaSeedPlan describes a standby Postgres to build.
type ReplicaSeedPlan struct {
	// Ref is the project the standby copies; Identifier is the replica's identifier. The
	// standby's application_name is the identifier.
	Ref        string
	Identifier string
	// DataDir is the empty directory to extract into (PGDATA of the standby).
	DataDir string
	// BackupID is the base backup to seed from (a Manifest.ID); empty means the newest complete
	// one of Ref.
	BackupID string
	// PrimaryPort is the canonical Postgres port of Ref on this node: a forwarder to the home node's
	// Postgres, or the primary itself when the standby is built where it will be promoted. Zero
	// seeds an archive-only standby, with no primary_conninfo: the standby that
	// `supavise failover --restore-missing` drains and promotes.
	PrimaryPort int
	// ReplicationPassword is the opened password of the replication role supabase_replication_admin
	// (secrets.ProjectKeys.ReplicationPassword). It goes into primary_conninfo inside the 0600
	// postgresql.auto.conf of DataDir and nowhere else. Ignored when PrimaryPort is zero.
	ReplicationPassword string
}

// ReplicaSeeder turns an empty directory into a hot standby.
//
// SeedReplica extracts the base backup as a restore does, then appends to postgresql.auto.conf:
// primary_conninfo (host=127.0.0.1, port=PrimaryPort, user=supabase_replication_admin,
// application_name=Identifier, sslmode=disable: the mesh is already TLS); restore_command, the
// node's own relay fetch for Ref; and recovery_target_timeline = 'latest'. It writes
// standby.signal and never recovery.signal. It also writes postmaster.opts, which the launcher
// needs, and syncs the tree. It refuses a DataDir that is not empty. The standby sets
// archive_mode = on, not always, so it never archives; after a promotion it pushes WAL under Ref's prefix.
type ReplicaSeeder interface {
	SeedReplica(ctx context.Context, plan ReplicaSeedPlan) error
}

// BaseBackupEnsurer hands out a base backup that is recent enough to seed from.
type BaseBackupEnsurer interface {
	// EnsureBase returns the newest complete base backup of ref when it is younger than maxAge,
	// and otherwise takes one (BaseBackup) and returns that. A maxAge of zero always takes one.
	EnsureBase(ctx context.Context, ref string, maxAge time.Duration) (*registry.Backup, error)
}

// LeaderMarkerKey is where the epoch marker lives in the backend. "_node" is not a project
// ref, like the key escrows (EscrowPrefix). Prune leaves it alone.
const LeaderMarkerKey = "_node/leader.json"

// StackPrefix is where `supavise upgrade --aws` keeps the CloudFormation template it deploys,
// outside the "<ref>/" namespace of the projects. Prune and orphan sweeps leave it alone.
const StackPrefix = "_stack/"

// LeaderMarker records which node led at which epoch: written at promotion, read at boot by a node
// that cannot reach its peers to learn that it was replaced.
type LeaderMarker struct {
	Epoch  int64     `json:"epoch"`
	Leader string    `json:"leader"`
	At     time.Time `json:"at"`
}

// ErrMarkerNewer is returned by WriteLeaderMarker when the store already holds a higher epoch, or
// the same epoch under another leader: someone else was promoted first, and the writer must stop.
var ErrMarkerNewer = errors.New("backup: the leader marker holds a higher epoch")

// EpochMarkerStore reads and writes the leader marker.
type EpochMarkerStore interface {
	// ReadLeaderMarker returns the marker, or nil with no error when none was written.
	ReadLeaderMarker(ctx context.Context) (*LeaderMarker, error)
	// WriteLeaderMarker stores m. It is a conditional write where the store supports one and a
	// read followed by a plain write otherwise; either way it returns an error wrapping
	// ErrMarkerNewer, and writes nothing, when the stored epoch is higher than m.Epoch or equal
	// to it under another leader. The same epoch under the same leader is written again. A zero
	// m.At is the time of the call.
	WriteLeaderMarker(ctx context.Context, m LeaderMarker) error
}

// FormatPromoteOK is the content of projects/<ref>/promote.ok (config.Paths.PromoteOK): the
// epoch, in decimal, and a newline. Only the promotion procedure writes the file.
func FormatPromoteOK(epoch int64) []byte { return []byte(strconv.FormatInt(epoch, 10) + "\n") }

// ParsePromoteOK reads the epoch out of a promote.ok file.
func ParsePromoteOK(b []byte) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("backup: promote.ok holds %q, want an epoch", strings.TrimSpace(string(b)))
	}
	return n, nil
}
