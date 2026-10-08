package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// ReplicaPooler keeps the Supavisor tenants of read replicas (design 2.7.6). It is the
// implementation of replicas.Pooler: the replica controller calls EnsureReplicaTenant when a
// replica has finished its setup and RemoveReplicaTenant when it goes. The type is declared here
// without importing internal/replicas, which depends on this package through lifecycle; a
// compile-time check in that package's tests holds the two together.
//
// A replica's tenant has the replica's identifier as its external id and the replica's Postgres
// port as its database, and is otherwise the project's own tenant (TenantSpecForReplica). The
// port is the replica itself on its node and a forwarder everywhere else, so the one row in
// _supavisor serves every node's Supavisor, and the row replicates to the followers. A follower's
// Supavisor keeps the target of a row it has read until it is told to drop it, so each change is
// followed by PeerRefresher, once every node's standby has replayed it.
type ReplicaPooler struct {
	// Deps supplies the registry, the secrets and the configuration the specs are read with.
	Deps Deps
	// Fleet is the tenants to call (Lazy.Fleet() on a node that binds late, or the Fleet that
	// Setup returns). Only the Supavisor tenant is used: Realtime and Storage know nothing of replicas.
	Fleet Fleet
	// Peers asks the other nodes that run Supavisor to drop their cached copy of a tenant. It is nil
	// on a node that is part of no cluster. A node that does not answer is not an error here: see
	// PeerRefresher.
	Peers PeerRefresher
	// Log receives the warnings of a refresh that failed on some node; nil discards them.
	Log *slog.Logger
}

// pooler returns the Supavisor entries of the fleet.
func (p ReplicaPooler) pooler() Fleet {
	var out Fleet
	for _, t := range p.Fleet {
		if t.Service() == config.SvcSupavisor {
			out = append(out, t)
		}
	}
	return out
}

// EnsureReplicaTenant creates or updates the tenant of the replica identified by identifier, a
// replica of project ref. The project's own tenant is ensured first: it sets the pgbouncer role's
// password in the project's database, which the replica replicates, and a standby cannot be
// written. A second call with nothing changed sends nothing.
func (p ReplicaPooler) EnsureReplicaTenant(ctx context.Context, ref, identifier string) error {
	if got, _, _, ok := registry.ParseReplicaIdentifier(identifier); !ok || got != ref {
		return fmt.Errorf("fleet: %q is not a replica identifier of project %s", identifier, ref)
	}
	sv := p.pooler()
	if len(sv) == 0 {
		return errors.New("fleet: the pooler has no Supavisor tenant to ensure a replica's tenant with")
	}
	own, err := LoadTenantSpec(ctx, p.Deps, ref)
	if err != nil {
		return err
	}
	if err := sv.EnsureTenant(ctx, own); err != nil {
		return fmt.Errorf("fleet: the pooler tenant of project %s: %w", ref, err)
	}
	spec, err := LoadReplicaTenantSpec(ctx, p.Deps, identifier)
	if err != nil {
		return err
	}
	if err := sv.EnsureReplicaTenant(ctx, spec); err != nil {
		return err
	}
	p.refreshPeers(ctx, identifier)
	return nil
}

// RemoveReplicaTenant drops the tenant of the replica identified by identifier; dropping one that
// is not there succeeds.
func (p ReplicaPooler) RemoveReplicaTenant(ctx context.Context, identifier string) error {
	if _, _, _, ok := registry.ParseReplicaIdentifier(identifier); !ok {
		return fmt.Errorf("fleet: %q is not a replica identifier", identifier)
	}
	if err := p.pooler().RemoveReplicaTenant(ctx, identifier); err != nil {
		return err
	}
	p.refreshPeers(ctx, identifier)
	return nil
}

// refreshPeers asks the other nodes to drop their copy of the tenant. A refresh is idempotent and
// a node that is down costs a warning, not the call: its Supavisor starts from the replicated row
// when the node is back.
func (p ReplicaPooler) refreshPeers(ctx context.Context, identifier string) {
	if p.Peers == nil {
		return
	}
	if err := p.Peers.RefreshPeers(ctx, identifier); err != nil {
		log := p.Log
		if log == nil {
			log = slog.New(slog.DiscardHandler)
		}
		log.Warn("fleet: some nodes did not drop their copy of a replica's pooler tenant; they read the row when their Supavisor next needs it", "replica", identifier, "error", err)
	}
}
