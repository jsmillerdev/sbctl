package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// A server that joins a cluster follows the leader's system cluster, which holds the registry: its
// standby of that cluster is a replica like the others (origin "system"), the unit supavise-postgres@system
// rendered from the replica's spec on the replica port. The join builds it before the daemon runs,
// when this node has no registry to read a project row or a credential from.

// OpenStandbyPlane returns the plane that builds a joining server's standby of the system cluster, over the
// supervisor and the artifact store of o. The server has no registry yet, so the plane is given a private
// in-memory one that SeedSystemStandby never reads. closeUnits lets go of the supervisor.
func OpenStandbyPlane(cfg *config.Config, o OpenOptions) (pl *PostgresPlane, closeUnits func(), err error) {
	arts, err := o.artifactStore(cfg)
	if err != nil {
		return nil, nil, err
	}
	sup, err := o.supervisor(cfg)
	if err != nil {
		return nil, nil, err
	}
	closeUnits = func() {
		if c, ok := sup.(interface{ Close() }); ok {
			c.Close()
		}
	}
	return NewPostgresPlane(cfg, sup, arts, registry.NewMemory(), o.planeOptions()), closeUnits, nil
}

// StartSystemDatabase starts the system cluster from the files it was rendered with and waits until it
// answers. It reads neither the registry nor a credential: the registry is this cluster, and its
// credentials are in it. A move that stopped the system cluster starts it again this way (the old leader of
// `supavise failover --abort` and of a resume), when nothing can be read yet.
func (pl *PostgresPlane) StartSystemDatabase(ctx context.Context) error {
	return pl.startRendered(ctx, systemProject(pl.cfg, nil))
}

// SystemStandbyPlan describes the standby of the system cluster to build (peerapi.SystemBootstrap).
type SystemStandbyPlan struct {
	// Identifier is the replica row of the system cluster on this node ("system-rr-<region>-<id6>").
	Identifier string
	// BackupID is the system project's base backup to seed from; empty means the newest complete one.
	BackupID string
	// ReplicationPassword is the opened password of the system cluster's replication role, which
	// the standby's primary_conninfo carries.
	ReplicationPassword string
}

// minSystemStandbyFree is the free space a join asks for before it starts: the system cluster is small,
// and the base backup it is seeded from is not known until the leader names it, so this is a floor.
const minSystemStandbyFree = 2 << 30

// SeedSystemStandby builds this server's standby of the system cluster from the base backup plan names
// and starts it, waiting until it accepts connections. Its primary_conninfo points at the canonical system
// port of this node, where the join serves a forwarder to the leader until the daemon takes over.
//
// It can be repeated, which a resumed join, a repeated `node rejoin` and a `node join --reset` (after the
// old data is set aside) do. A directory that holds what an earlier attempt of this standby left
// (the seeder's marker, or a standby of this identifier that was built and did not start) is
// finished or started over; one that holds anything else is refused, and nothing in it is changed.
func (pl *PostgresPlane) SeedSystemStandby(ctx context.Context, plan SystemStandbyPlan, seeder ReplicaSeeder) error {
	if seeder == nil {
		return errors.New("lifecycle: SeedSystemStandby needs a seeder")
	}
	if ref, _, _, ok := registry.ParseReplicaIdentifier(plan.Identifier); !ok || ref != config.SystemRef {
		return fmt.Errorf("lifecycle: %q is not an identifier of a replica of the system cluster", plan.Identifier)
	}
	t, err := pl.systemTarget(plan)
	if err != nil {
		return err
	}
	rp := pl.replicaPaths(t.Project)
	switch state, err := standbyState(rp.Data, plan.Identifier); {
	case err != nil:
		return err
	case state == dirEmpty:
	case state == dirUnfinished:
		pl.log.Warn("the seeding of the system standby was cut off; starting it over", "dir", rp.Data)
		if err := os.RemoveAll(rp.Data); err != nil {
			return fmt.Errorf("lifecycle: clear the unfinished seed in %s: %w", rp.Data, err)
		}
	case state == dirStandby:
		// Built by an earlier attempt: it only has to run.
		return pl.StartReplicaDatabase(ctx, t)
	default:
		return fmt.Errorf("%w: %s holds a cluster that is not this server's standby of the system cluster; `supavise node join --reset` sets it aside", ErrClusterExists, rp.Data)
	}
	return pl.CreateReplica(ctx, t, ReplicaCreateOptions{Seeder: seeder, BackupID: plan.BackupID})
}

// systemTarget is the standby of the system cluster as the plane renders it: the project row of the
// system project from this node's pins, and credentials of its own for what the standby needs before
// it can read the registry. The replication password is the leader's. The pgsodium root key is
// made here: nothing in the system cluster is encrypted with it, the unit needs a key file to start,
// and the node agent renders the registry's key over it when the registry can be read.
func (pl *PostgresPlane) systemTarget(plan SystemStandbyPlan) (ReplicaTarget, error) {
	versions := map[string]string{}
	for _, svc := range config.ProjectServices {
		tag, err := pl.arts.Tag(svc)
		if err != nil {
			return ReplicaTarget{}, err
		}
		versions[svc] = tag
	}
	keys, err := secrets.NewProjectKeys(config.SystemRef, time.Now())
	if err != nil {
		return ReplicaTarget{}, err
	}
	keys.ReplicationPassword = plan.ReplicationPassword
	return ReplicaTarget{Identifier: plan.Identifier, Project: systemProject(pl.cfg, versions), Keys: keys}, nil
}

// SystemStandbyTarget is the standby of the system cluster on this node as the plane renders it, from the
// node's pins and the leader's replication password, without a look at the registry. The old leader of a
// planned switchover demotes its system cluster with it: the registry it would read is that cluster.
func (pl *PostgresPlane) SystemStandbyTarget(identifier, replicationPassword string) (ReplicaTarget, error) {
	return pl.systemTarget(SystemStandbyPlan{Identifier: identifier, ReplicationPassword: replicationPassword})
}

// SystemStandbyPreflight checks what SeedSystemStandby needs before a rejoin moves data aside: a backup
// backend a second server can read, room on the disk, and a Postgres release to run. It does not look at
// the data directory (the seeding refuses what it must), and it does not read the backup store, which the
// seed does.
func (pl *PostgresPlane) SystemStandbyPreflight(ctx context.Context) error {
	if pl.cfg.FileBackup() {
		return fmt.Errorf("lifecycle: [backup] names %s, a file:// backend a second server cannot read; point it at S3-compatible storage on the leader first", pl.cfg.Backup.Backend)
	}
	return pl.SystemStandbyJoinPreflight(ctx)
}

// SystemStandbyJoinPreflight is SystemStandbyPreflight for a server that has not joined yet: the backend
// is not looked at. The cluster's settings, [backup] among them, come from the leader with the join and
// replace the ones this server holds, and the leader hands out no token while its own backend is a
// file:// one (`supavise node token`).
func (pl *PostgresPlane) SystemStandbyJoinPreflight(ctx context.Context) error {
	if _, err := pl.artifactDir(systemProject(pl.cfg, nil), config.SvcPostgres); err != nil {
		return fmt.Errorf("lifecycle: the Postgres release this server runs is not installed: %w", err)
	}
	dir := pl.cfg.StateDir
	free := diskFree(dir)
	for free < 0 && filepath.Dir(dir) != dir {
		dir = filepath.Dir(dir)
		free = diskFree(dir)
	}
	if free >= 0 && free < minSystemStandbyFree {
		return fmt.Errorf("lifecycle: %s has %s free, and the standby of the system cluster wants at least %s", dir, humanBytes(free), humanBytes(minSystemStandbyFree))
	}
	return nil
}

type dirState int

const (
	dirEmpty      dirState = iota // no cluster, nothing in the way
	dirUnfinished                 // the seeder's marker: a seeding of this standby that was cut off
	dirStandby                    // a standby of this identifier, built and not (known to be) running
	dirOther                      // anything else
)

// standbyState says what dataDir holds, for SeedSystemStandby.
func standbyState(dataDir, identifier string) (dirState, error) {
	ents, err := os.ReadDir(dataDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return dirEmpty, nil
	case err != nil:
		return dirOther, err
	case len(ents) == 0:
		return dirEmpty, nil
	}
	if fileExists(filepath.Join(dataDir, SeedMarker)) {
		return dirUnfinished, nil
	}
	if fileExists(filepath.Join(dataDir, "PG_VERSION")) && fileExists(filepath.Join(dataDir, "standby.signal")) {
		if b, err := os.ReadFile(filepath.Join(dataDir, "postgresql.auto.conf")); err == nil && followsAs(string(b), identifier) {
			return dirStandby, nil
		}
	}
	return dirOther, nil
}

// followsAs reports whether the primary_conninfo in conf names application_name identifier.
func followsAs(conf, identifier string) bool {
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "primary_conninfo") {
			continue
		}
		_, rest, ok := strings.Cut(line, "application_name=")
		if !ok {
			return false
		}
		rest = strings.TrimLeft(rest, `'\`)
		if i := strings.IndexAny(rest, `'\ `); i >= 0 {
			rest = rest[:i]
		}
		return rest == identifier
	}
	return false
}
