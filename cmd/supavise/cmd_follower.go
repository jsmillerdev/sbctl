package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/supavise/supavise/internal/app"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// A server that follows a leader has no primary on the system port: its system cluster is a hot standby
// on the replica port, and its registry is a read-only copy of the leader's. The commands that open the
// node or the registry decide which they are talking to the way `supavise node` does (writableNodeEnv):
// the sockets of app.RegistryDSNs are tried in order, and the first that answers says whether it is in
// recovery. A standby opens read-only (no migrations, no locks); a write then fails with
// registry.ErrReadOnly, which main explains (followerHint).

// inRecovery is cluster.InRecovery; the tests replace it.
var inRecovery = cluster.InRecovery

// localRegistry returns the socket of the system cluster that answers on this server and whether it is a
// standby. It returns "" when none answers (the system cluster is down): the caller goes on with its
// default and fails with the usual "cannot reach the registry" error.
func localRegistry(ctx context.Context, cfg *config.Config) (dsn string, standby bool) {
	for _, d := range app.RegistryDSNs(cfg) {
		rec, err := inRecovery(ctx, d)
		if err == nil {
			return d, rec
		}
	}
	return "", false
}

// nodeOptions finishes the lifecycle options of a command that opens the node: the registry it names
// ($SUPAVISE_REGISTRY_DSN, which wins), or the one that answers on this server, read-only on a follower.
func nodeOptions(ctx context.Context, cfg *config.Config, lo lifecycle.OpenOptions) lifecycle.OpenOptions {
	if lo.RegistryDSN == "" {
		lo.RegistryDSN = os.Getenv(envRegistryDSN)
	}
	if lo.RegistryDSN == "" {
		lo.RegistryDSN, lo.ReadOnly = localRegistry(ctx, cfg)
	}
	return lo
}

// openLifecycle is lifecycle.Open for a command: on a follower the node opens read-only.
func openLifecycle(ctx context.Context, cfg *config.Config, lo lifecycle.OpenOptions) (*lifecycle.Node, error) {
	return lifecycle.Open(ctx, cfg, nodeOptions(ctx, cfg, lo))
}

// followerHint explains a refused write: the registry of a server that follows another is a read-only
// copy, so only the leader can change anything.
func followerHint(err error) error {
	if err == nil || !errors.Is(err, registry.ErrReadOnly) {
		return err
	}
	return fmt.Errorf("%w\nthis server follows another one: its registry is a read-only copy of the leader's, so this command changes nothing here; run it on the leader (`supavise node ls` shows which server leads)", err)
}
