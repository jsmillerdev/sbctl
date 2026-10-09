package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/units"
)

// A replica is the same Postgres unit in standby mode plus its own PostgREST, on a node that is not
// the project's home (design 2.7). The units keep the names of a primary's, supavise-postgres@<ref>
// and supavise-postgrest@<ref>: a node is either a project's home or holds a replica of it, never
// both (invariant I2), so the names cannot clash and no unit template changes (I4). The role decides
// the ports and the settings at render time:
//
//   - Postgres listens on config.ReplicaPorts (the canonical port of the node is a forwarder to the
//     home, which the standby streams from), with hot_standby on; GoTrue is not started.
//   - PostgREST listens on the replica PostgREST port and reads a database list of the replica
//     first and the primary second, with target_session_attrs=read-only: the pool uses the
//     replica, and only the LISTEN session reaches the primary.
//
// The operations here are the lifecycle of one standby on this node. The node agent in
// internal/placement calls them for the leader's requests; the failover orchestrator reaches
// PromoteReplica and DemoteToReplica through the agent.

// ReplicaTarget is a standby Postgres of a project that this node runs.
type ReplicaTarget struct {
	// Identifier is the replica's identifier, "<ref>-rr-<region>-<id6>" (registry.ReplicaIdentifier).
	// The system cluster's standby is "system-rr-...".
	Identifier string
	Project    *registry.Project
	Keys       *secrets.ProjectKeys
}

// Errors of the replica operations.
var (
	// ErrReplicaSeq: the project's port sequence is above config.MaxReplicaSeq, so its replica ports
	// would reach into the project range.
	ErrReplicaSeq = errors.New("lifecycle: the project's sequence number is too high for a replica port")
	// ErrNotStandby: the cluster here is not a standby of the project.
	ErrNotStandby = errors.New("lifecycle: the cluster is not a standby")
	// ErrNotCleanShutdown: DemoteToReplica found a cluster that did not shut down cleanly, which
	// cannot follow the new primary without being rebuilt.
	ErrNotCleanShutdown = errors.New("lifecycle: the cluster did not shut down cleanly")
	// ErrReplayBehind: the standby did not replay up to the position PromoteReplica waited for.
	ErrReplayBehind = errors.New("lifecycle: the standby has not replayed up to the requested position")
)

func (t ReplicaTarget) check(cfg *config.Config) error {
	if t.Project == nil || t.Keys == nil {
		return errors.New("lifecycle: a replica needs its project row and keys")
	}
	ref, _, _, ok := registry.ParseReplicaIdentifier(t.Identifier)
	if !ok || ref != t.Project.Ref {
		return fmt.Errorf("lifecycle: %q is not an identifier of a replica of %s", t.Identifier, t.Project.Ref)
	}
	if t.Project.Ref != config.SystemRef && t.Project.Seq > cfg.MaxReplicaSeq() {
		return fmt.Errorf("%w: %s has sequence %d and the replica range holds %d", ErrReplicaSeq, t.Project.Ref, t.Project.Seq, cfg.MaxReplicaSeq())
	}
	return nil
}

// replicaPaths is the cluster layout of a replica of p: p's directory, listening on the replica port.
func (pl *PostgresPlane) replicaPaths(p *registry.Project) pgPaths {
	return pathsFor(pl.cfg, p.Ref, pl.cfg.ReplicaPorts(p.Ref, p.Seq).Postgres)
}

// SeedMarker is the file the backup service's seeder keeps in a data directory while it fills it
// (backup.SeedReplica writes it first and removes it last).
const SeedMarker = "supavise-seeding"

// ReplicaSeedPlan describes the standby to build. It has the fields of backup.ReplicaSeedPlan, which
// the daemon converts to (this package cannot import backup).
type ReplicaSeedPlan struct {
	Ref                 string
	Identifier          string
	DataDir             string
	BackupID            string
	PrimaryPort         int
	ReplicationPassword string
}

// ReplicaSeeder turns the empty DataDir of plan into a hot standby (backup.Service.SeedReplica).
type ReplicaSeeder func(ctx context.Context, plan ReplicaSeedPlan) error

// ReplicaStage is how far CreateReplica got.
type ReplicaStage string

const (
	// StageLaunched: the directories, pg_hba.conf and the pgsodium root key are in place.
	StageLaunched ReplicaStage = "launched"
	// StageSeeding: the seeder starts (it downloads the base backup).
	StageSeeding ReplicaStage = "seeding"
	// StageSeeded: the base backup is extracted and the recovery settings are written.
	StageSeeded ReplicaStage = "seeded"
	// StageStarted: the standby accepts connections.
	StageStarted ReplicaStage = "started"
)

// ReplicaCreateOptions tune CreateReplica.
type ReplicaCreateOptions struct {
	Seeder ReplicaSeeder
	// BackupID is the base backup to seed from; empty means the newest complete one.
	BackupID string
	// NoUpstream seeds an archive-only standby: no primary_conninfo, so it replays what the
	// archive holds and streams from nobody (`supavise failover --restore-missing`).
	NoUpstream bool
	// Progress is told when a stage is reached.
	Progress func(ReplicaStage)
}

func (o ReplicaCreateOptions) progress(s ReplicaStage) {
	if o.Progress != nil {
		o.Progress(s)
	}
}

// CreateReplica builds the standby of t on this node and starts its cluster: directories,
// pg_hba.conf and the root key, the seed from a base backup, then the cluster, waiting until it
// accepts connections. It stops before PostgREST (StartReplicaAPI) so that the caller can wait for
// the standby to stream first. A failure removes the partly seeded data directory, so that the
// call can be repeated; the units it started stay running (RemoveReplica cleans up).
// ErrClusterExists when the directory holds a cluster already.
func (pl *PostgresPlane) CreateReplica(ctx context.Context, t ReplicaTarget, o ReplicaCreateOptions) error {
	if err := t.check(pl.cfg); err != nil {
		return err
	}
	if o.Seeder == nil {
		return errors.New("lifecycle: CreateReplica needs a seeder")
	}
	p := t.Project
	pp := pl.replicaPaths(p)
	if _, err := os.Stat(filepath.Join(pp.Data, "PG_VERSION")); err == nil {
		return fmt.Errorf("%w: %s", ErrClusterExists, pp.Data)
	}
	if err := pl.prepareAt(p, t.Keys, pp, true); err != nil {
		return err
	}
	o.progress(StageLaunched)
	plan := ReplicaSeedPlan{Ref: p.Ref, Identifier: t.Identifier, DataDir: pp.Data, BackupID: o.BackupID}
	if !o.NoUpstream {
		plan.PrimaryPort = pl.cfg.PortsFor(p.Ref, p.Seq).Postgres
		plan.ReplicationPassword = t.Keys.ReplicationPassword
	}
	o.progress(StageSeeding)
	if err := o.Seeder(ctx, plan); err != nil {
		_ = os.RemoveAll(pp.Data)
		return fmt.Errorf("lifecycle: seed replica %s: %w", t.Identifier, err)
	}
	if err := ensureLauncherInvariants(pp.Data); err != nil {
		_ = os.RemoveAll(pp.Data)
		return err
	}
	o.progress(StageSeeded)
	if err := pl.StartReplicaDatabase(ctx, t); err != nil {
		return err
	}
	o.progress(StageStarted)
	return nil
}

// replicaPostgresSpec renders the standby's cluster unit. The data directory must hold a seeded
// cluster: the launcher would initialize an empty one, and a replica must never be a new cluster.
func (pl *PostgresPlane) replicaPostgresSpec(ctx context.Context, t ReplicaTarget) (units.Spec, error) {
	pp := pl.replicaPaths(t.Project)
	if bootstrapPending(pp.Data) {
		return units.Spec{}, fmt.Errorf("lifecycle: replica %s has no seeded data directory at %s", t.Identifier, pp.Data)
	}
	// A seed that was cut off (a kill, a power loss) holds a backup_label and no standby.signal, so a
	// cluster started on it would come up as a primary. The seeder leaves its marker until it has
	// finished (backup.SeedUnfinished); nothing starts a unit on a directory that carries it.
	if fileExists(filepath.Join(pp.Data, SeedMarker)) {
		return units.Spec{}, fmt.Errorf("lifecycle: the seeding of replica %s in %s did not finish; it is seeded again, not started", t.Identifier, pp.Data)
	}
	return pl.postgresSpecFor(ctx, t.Project, t.Keys, pp, true)
}

// replicaRESTSpec renders the replica's PostgREST unit: the replica port, a database list of the
// replica first and the project's canonical Postgres port second (a forwarder to the home, or the
// home itself in a single-machine test) with target_session_attrs=read-only, and the replica's own
// API host in the OpenAPI document. If the replica is down the read-only pool finds no host and
// fails, rather than silently reading from the primary.
func (pl *PostgresPlane) replicaRESTSpec(ctx context.Context, t ReplicaTarget) (units.Spec, error) {
	p := t.Project
	rp := pl.cfg.ReplicaPorts(p.Ref, p.Seq)
	return pl.restSpec(ctx, p, t.Keys, restTarget{
		port:    rp.PostgREST,
		dbURI:   replicaRESTURI(t.Keys.AuthenticatorPassword, rp.Postgres, pl.cfg.PortsFor(p.Ref, p.Seq).Postgres),
		openAPI: pl.scheme() + "://" + pl.cfg.ProjectHost(t.Identifier) + "/rest/v1",
	})
}

// replicaRESTURI is PGRST_DB_URI of a replica's PostgREST.
func replicaRESTURI(password string, replicaPort, primaryPort int) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(RoleAuthn, password),
		Host:     fmt.Sprintf("127.0.0.1:%d,127.0.0.1:%d", replicaPort, primaryPort),
		Path:     "/postgres",
		RawQuery: "sslmode=disable&application_name=postgrest-replica&target_session_attrs=read-only",
	}
	return u.String()
}

// SetRestoreCommand sets PlaneOptions.RestoreCommandFor on a plane that was built without it. The
// daemon calls it while it wires the cluster features, before any request is served.
func (pl *PostgresPlane) SetRestoreCommand(f func(ref string) string) { pl.opts.RestoreCommandFor = f }

// replicaReadyTimeout bounds the start of a standby until it accepts connections: it may replay a
// backlog of archived WAL first.
const replicaReadyTimeout = 10 * time.Minute

// StartReplicaDatabase starts the standby's cluster (restarting it when its settings changed) and
// waits until it accepts connections, which a hot standby does once it is consistent.
func (pl *PostgresPlane) StartReplicaDatabase(ctx context.Context, t ReplicaTarget) error {
	if err := t.check(pl.cfg); err != nil {
		return err
	}
	pp := pl.replicaPaths(t.Project)
	if err := pl.prepareAt(t.Project, t.Keys, pp, true); err != nil {
		return err
	}
	spec, err := pl.replicaPostgresSpec(ctx, t)
	if err != nil {
		return err
	}
	// A cluster that starts as a standby must not carry promote.ok: the relay trusts the file for the
	// epoch it names, and a promotion that was aborted after it wrote the file (the process died before
	// pg_promote ran) would let a stray promotion push a new timeline. A promotion writes the file when
	// it is about to run, after the standby is up.
	if fileExists(filepath.Join(pp.Data, "standby.signal")) {
		if err := os.Remove(pl.cfg.Paths().PromoteOK(t.Project.Ref)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return pl.startCluster(ctx, spec, pp, replicaReadyTimeout)
}

// StartReplicaAPI starts the replica's PostgREST (restarting it when its settings changed) and
// waits until it answers. The system cluster's standby has none.
func (pl *PostgresPlane) StartReplicaAPI(ctx context.Context, t ReplicaTarget) error {
	if err := t.check(pl.cfg); err != nil {
		return err
	}
	if !hasPostgREST(t.Project.Ref) {
		return nil
	}
	spec, err := pl.replicaRESTSpec(ctx, t)
	if err != nil {
		return err
	}
	if err := pl.renderStopIfChanged(ctx, spec); err != nil {
		return err
	}
	if err := pl.sup.Start(ctx, spec.Unit()); err != nil {
		return err
	}
	url := pl.replicaRESTURL(t.Project)
	return pl.wait(ctx, spec.Unit(), config.SvcPostgREST, pl.opts.ServiceReadyTimeout, func(ctx context.Context) error { return pl.checkURL(ctx, url) })
}

// StartReplica starts the standby and its PostgREST.
func (pl *PostgresPlane) StartReplica(ctx context.Context, t ReplicaTarget) error {
	if err := pl.StartReplicaDatabase(ctx, t); err != nil {
		return err
	}
	return pl.StartReplicaAPI(ctx, t)
}

func (pl *PostgresPlane) replicaRESTURL(p *registry.Project) string {
	return fmt.Sprintf("http://127.0.0.1:%d/", pl.cfg.ReplicaPorts(p.Ref, p.Seq).PostgREST)
}

// StopReplica stops the replica's PostgREST and then its cluster. GoTrue is not part of a replica.
func (pl *PostgresPlane) StopReplica(ctx context.Context, ref string) error {
	var first error
	if hasPostgREST(ref) {
		if err := pl.sup.Stop(ctx, config.UnitName(config.SvcPostgREST, ref)); err != nil {
			first = err
		}
	}
	if err := pl.sup.Stop(ctx, config.UnitName(config.SvcPostgres, ref)); err != nil && first == nil {
		first = err
	}
	return first
}

// RemoveReplica stops the replica's units and deletes the project's directory on this node: its data,
// its unit files and the leftovers of a half-built replica. It is Delete under the name the node
// agent reads.
func (pl *PostgresPlane) RemoveReplica(ctx context.Context, ref string) error {
	return pl.Delete(ctx, ref)
}

// ReplicaHealth checks the standby's cluster and its PostgREST with real requests (GoTrue is not
// part of a replica).
func (pl *PostgresPlane) ReplicaHealth(ctx context.Context, t ReplicaTarget) []ServiceHealth {
	p := t.Project
	pp := pl.replicaPaths(p)
	out := []ServiceHealth{pl.serviceHealth(ctx, p.Ref, config.SvcPostgres, func(ctx context.Context) error { return pl.sql().Ping(ctx, addrOf(pp)) })}
	if hasPostgREST(p.Ref) {
		url := pl.replicaRESTURL(p)
		out = append(out, pl.serviceHealth(ctx, p.Ref, config.SvcPostgREST, func(ctx context.Context) error { return pl.checkURL(ctx, url) }))
	}
	return out
}

// ReplicaObservation is what a node sees of one of its replicas.
type ReplicaObservation struct {
	// Role is "replica" (the cluster is in recovery), "primary" (it is not: a promoted replica) or
	// "absent" (no cluster, or it does not answer).
	Role           string
	PostgresUp     bool
	PostgRESTReady bool
	InRecovery     bool
	// ReceiverStatus is pg_stat_wal_receiver.status ("streaming"), empty when there is no receiver.
	ReceiverStatus string
	// ReceiveLSN and ReplayLSN are pg_last_wal_receive_lsn() and pg_last_wal_replay_lsn() ("0/3000100").
	ReceiveLSN string
	ReplayLSN  string
	// LagSeconds is computed as Studio does: 0 when everything received is replayed and the receiver is
	// streaming, else the time since the last replayed commit. Nil when unknown.
	LagSeconds *float64
}

// Observation roles.
const (
	ReplicaRoleReplica = "replica"
	ReplicaRolePrimary = "primary"
	ReplicaRoleAbsent  = "absent"
)

// ObserveReplica reads the state of the replica of t. It never fails: a cluster that does not
// answer is "absent". A cluster that was promoted and restarted on the canonical port is found
// there and reported as a primary.
func (pl *PostgresPlane) ObserveReplica(ctx context.Context, t ReplicaTarget) ReplicaObservation {
	p := t.Project
	obs := ReplicaObservation{Role: ReplicaRoleAbsent}
	st, err := pl.sql().Status(ctx, addrOf(pl.replicaPaths(p)))
	if err != nil {
		if st, err = pl.sql().Status(ctx, addrOf(pl.paths(p))); err != nil {
			return obs
		}
	}
	obs.PostgresUp, obs.InRecovery = true, st.InRecovery
	obs.ReceiverStatus, obs.ReceiveLSN, obs.ReplayLSN = st.ReceiverStatus, st.ReceiveLSN, st.ReplayLSN
	if st.InRecovery {
		obs.Role = ReplicaRoleReplica
		switch {
		case st.ReceiverStatus == "streaming" && st.ReceiveLSN == st.ReplayLSN:
			zero := 0.0
			obs.LagSeconds = &zero
		case st.ReplayAgeSeconds != nil:
			obs.LagSeconds = st.ReplayAgeSeconds
		}
	} else {
		obs.Role = ReplicaRolePrimary
	}
	if hasPostgREST(p.Ref) && obs.Role == ReplicaRoleReplica {
		obs.PostgRESTReady = pl.checkURL(ctx, pl.replicaRESTURL(p)) == nil
	}
	return obs
}

// checkURL asks url for a 200.
func (pl *PostgresPlane) checkURL(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := pl.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return nil
}
