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
		return fmt.Errorf("the read replicas %s of project %s are still being removed (the leader's daemon finishes it): run this command again when `supavise replicas ls` no longer lists them",
			strings.Join(pe.Identifiers, ", "), ref)
	}
	return err
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

// removeReplicasBeforeRestore is removeReplicasFirst for `backups restore`, which opens the registry
// of its own for it: the backup service it restores with keeps its registry to itself.
func removeReplicasBeforeRestore(ctx context.Context, ref string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	reg, err := openRegistry(ctx, cfg)
	if err != nil {
		return err
	}
	defer reg.Close()
	return removeReplicasFirst(ctx, reg, cfg, ref)
}
