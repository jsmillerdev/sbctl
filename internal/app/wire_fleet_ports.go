package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
	"github.com/supavise/supavise/internal/units"
)

// The adapters in this file are the shared services' side of ports that other packages declare. Each
// is a few lines over fleet's exported calls; the services themselves are fleet's.

// replicaPooler is replicas.Pooler: the Supavisor tenant of a replica, made when the replica is healthy
// and dropped when it goes. After a change the other nodes that run Supavisor are told to forget the
// tenant (E's protocol: the tenant row first, then fleet.PeerRefresher). A node that does not answer
// is not a failure of the replica: the refresh is idempotent and the next change sends it again.
type replicaPooler struct {
	fleet fleet.Fleet
	deps  fleet.Deps
	peers fleet.PeerRefresher
	log   *slog.Logger
}

var _ replicas.Pooler = (*replicaPooler)(nil)

func (p *replicaPooler) EnsureReplicaTenant(ctx context.Context, ref, identifier string) error {
	spec, err := fleet.LoadReplicaTenantSpec(ctx, p.deps, identifier)
	if err != nil {
		return err
	}
	if spec.Ref != ref {
		return fmt.Errorf("fleet: replica %s belongs to project %s, not %s", identifier, spec.Ref, ref)
	}
	if err := p.fleet.EnsureReplicaTenant(ctx, spec); err != nil {
		return err
	}
	refreshPeers(ctx, p.peers, identifier, p.log)
	return nil
}

func (p *replicaPooler) RemoveReplicaTenant(ctx context.Context, identifier string) error {
	if err := p.fleet.RemoveReplicaTenant(ctx, identifier); err != nil {
		return err
	}
	refreshPeers(ctx, p.peers, identifier, p.log)
	return nil
}

// projectTenants is failover.Fleet: what a move asks of Supavisor and Realtime for a project. Quiesce
// makes them let go of the database before a planned stop (Realtime's logical walsender otherwise holds
// the shutdown of the cluster), and EnsureTenant registers the project again once its database answers
// at the new home, which makes Realtime create its slot again.
type projectTenants struct {
	fleet fleet.Fleet
	deps  fleet.Deps
	peers fleet.PeerRefresher
	log   *slog.Logger
}

var _ failover.Fleet = (*projectTenants)(nil)

func (p *projectTenants) QuiesceTenant(ctx context.Context, ref string) error {
	return p.fleet.QuiesceTenant(ctx, ref)
}

func (p *projectTenants) EnsureTenant(ctx context.Context, ref string) error {
	if err := p.ensureTenantHere(ctx, ref); err != nil {
		return err
	}
	p.tellPeers(ctx, ref)
	return nil
}

// ensureTenantHere registers the project with this node's shared services and tells no other node, and
// tellPeers is the second half of EnsureTenant. A caller that holds the project's lock for the write
// and not for the peers' answers (each peer may take peerRefreshTimeout) calls the two apart
// (ensureRemoteTenant).
func (p *projectTenants) ensureTenantHere(ctx context.Context, ref string) error {
	spec, err := fleet.LoadTenantSpec(ctx, p.deps, ref)
	if err != nil {
		return err
	}
	return p.fleet.EnsureTenant(ctx, spec)
}

func (p *projectTenants) tellPeers(ctx context.Context, ref string) {
	refreshPeers(ctx, p.peers, ref, p.log)
}

// refreshPeers asks the other nodes to drop their Supavisor's copy of a tenant. It never fails the caller.
func refreshPeers(ctx context.Context, peers fleet.PeerRefresher, tenant string, log *slog.Logger) {
	if peers == nil {
		return
	}
	if err := peers.RefreshPeers(ctx, tenant); err != nil {
		log.Warn("a node did not refresh its pooler's copy of a tenant; the next change to the tenant tries again", "tenant", tenant, "error", err)
	}
}

// localServices is failover.LocalServices: the system project's GoTrue and the shared services of the
// leader, stopped and started as one. The system cluster stays up, because the registry is in it.
type localServices struct {
	mgr *fleet.Manager
	sup units.Supervisor
}

var _ failover.LocalServices = (*localServices)(nil)

// systemAuthUnit is the dashboard's sign-in service.
var systemAuthUnit = config.UnitName(config.SvcGoTrue, config.SystemRef)

func (l *localServices) Stop(ctx context.Context) error {
	var errs []error
	if err := l.sup.Stop(ctx, systemAuthUnit); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", systemAuthUnit, err))
	}
	if err := l.mgr.Stop(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (l *localServices) Start(ctx context.Context) error {
	var errs []error
	if err := l.mgr.Start(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := l.sup.Start(ctx, systemAuthUnit); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", systemAuthUnit, err))
	}
	return errors.Join(errs...)
}

// artifactsCheck is the preflight check of a server move that the shared services own (design 2.10.2):
// the artifacts of every service the node would run as the leader are in place. It looks at this node
// only, which is the node that takes over (the move runs there); for another target it says nothing.
func artifactsCheck(mgr *fleet.Manager, self func() string) failover.ExtraChecks {
	return func(ctx context.Context, to registry.Node) []failover.Check {
		if to.ID != self() {
			return nil
		}
		_, err := mgr.Specs(ctx)
		if err != nil {
			return []failover.Check{{Name: "shared-service artifacts", Detail: err.Error(), Blocking: true}}
		}
		return []failover.Check{{Name: "shared-service artifacts", OK: true, Detail: "present on " + to.Name, Blocking: true}}
	}
}
