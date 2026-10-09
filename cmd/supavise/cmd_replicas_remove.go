package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
)

// removeReplicasFirst removes the read replicas of ref before the project is deleted or restored in
// place, as the Management API does (design 2.7.8): a standby cannot follow a restored primary, and a
// delete would take the replica rows with the project and leave their instances on the other nodes.
//
// This process has no session with the other nodes, so it marks the replicas GOING_DOWN and the
// controller in the leader's daemon removes them. While one is still there the command stops
// (*replicas.PendingError), and the same command goes on when the replicas are gone.
func removeReplicasFirst(ctx context.Context, reg registry.Registry, cfg *config.Config, ref string) error {
	err := replicas.New(replicas.Options{Registry: reg, Config: cfg}).RemoveAll(ctx, ref)
	var pe *replicas.PendingError
	if errors.As(err, &pe) {
		if err := markedForRemoval(ctx, reg, pe.Identifiers); err != nil {
			return err
		}
		return fmt.Errorf("the read replicas %s of project %s are still being removed (the leader's daemon finishes it): run this command again when `supavise replicas ls` no longer lists them",
			strings.Join(pe.Identifiers, ", "), ref)
	}
	return err
}

// markedForRemoval returns the error of the write that marks the pending replicas GOING_DOWN when it did
// not happen. On a follower the registry is a read-only copy of the leader's: no daemon will finish a
// removal that this process could not start, so "run this command again" would never come true, and the
// error (registry.ErrReadOnly, which main explains) sends the operator to the leader instead.
func markedForRemoval(ctx context.Context, reg registry.Registry, ids []string) error {
	for _, id := range ids {
		r, err := reg.GetReplica(ctx, id)
		if errors.Is(err, registry.ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		if r.Status == string(registry.StatusGoingDown) {
			continue
		}
		if _, err := reg.SetReplicaStatusUnlessGoingDown(ctx, id, string(registry.StatusGoingDown), r.InitStep, r.InitError); err != nil {
			return fmt.Errorf("marking the read replica %s for removal: %w", id, err)
		}
	}
	return nil
}

// deleteProject removes the project's replicas and then the project.
func deleteProject(ctx context.Context, n *lifecycle.Node, ref string, skipFinalBackup bool) error {
	if err := removeReplicasFirst(ctx, n.Registry, n.Cfg, ref); err != nil {
		return err
	}
	return n.Engine.DeleteWith(ctx, ref, lifecycle.DeleteOptions{SkipFinalBackup: skipFinalBackup})
}

// restoresInPlace reports whether a restore of ref replaces the project itself, which takes its replicas
// away: a restore into a new project leaves the source and its replicas alone.
func restoresInPlace(ref, as string) bool { return as == "" || as == ref }

// beforeInPlaceRestore is backup.RestoreOptions.BeforeReplace for `backups restore`. The restore calls it
// once the target, the backup and the data directory are checked, so a typo in --to leaves the replicas
// alone. It opens the registry of its own: the backup service keeps its registry to itself.
func beforeInPlaceRestore(ctx context.Context, ref string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	reg, err := openRegistry(ctx, cfg)
	if err != nil {
		return err
	}
	defer reg.Close()
	return prepareInPlaceRestore(ctx, reg, cfg, ref)
}

// prepareInPlaceRestore refuses a project that is not running (or whose last restore failed), as the
// API's restore does, and then removes the project's replicas: a standby cannot follow a restored primary.
func prepareInPlaceRestore(ctx context.Context, reg registry.Registry, cfg *config.Config, ref string) error {
	p, err := reg.GetProject(ctx, ref)
	if err != nil {
		return fmt.Errorf("project %s: %w", ref, err)
	}
	switch p.Status {
	case registry.StatusActiveHealthy, registry.StatusActiveUnhealthy, registry.StatusRestoreFailed:
	default:
		return fmt.Errorf("cannot restore project %s while it is %s", ref, p.Status)
	}
	return removeReplicasFirst(ctx, reg, cfg, ref)
}
