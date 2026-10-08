package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/registry"
)

// wirePlacement wraps the node's plane in the plane router, registers the instance, plane and
// backup endpoints of the peer API (the node agent), and provides placement.Resolver,
// placement.InstanceOps and placement.BackupOps (design 2.5, 2.7). The Engine's filter to the
// projects homed here belongs to it. On a node with no cluster it does nothing.
func wirePlacement(ctx context.Context, w *Wire) error { return placementWiring(ctx, w, mesh.Handle) }

// placementWiring is wirePlacement with the registration point of the peer API's handlers as an
// argument, so that a test can use a mux of its own.
func placementWiring(ctx context.Context, w *Wire, handle func(pattern string, fn mesh.HandlerFunc)) error {
	m, ok := Get[mesh.Mesh](w)
	if !ok {
		return nil // no cluster: one server, one plane, as it was
	}
	mem, ok := Get[cluster.Membership](w)
	if !ok {
		return errors.New("placement: the mesh is up but nothing provided the cluster membership")
	}
	node := w.Node
	self := func() string { return mem.Self().ID }
	res := placement.RegistryResolver{Reg: node.Registry}
	bk := &lazyBackups{build: func(ctx context.Context) (*backup.Service, error) {
		s, err := NewBackupService(ctx, w.Cfg, node.Registry, node.Secrets, w.Options)
		if err == nil {
			s.SetManager(node.Engine) // a restore reaches the Engine
		}
		return s, err
	}}

	// The Engine drives the router; the node's own plane serves what is homed here.
	router := placement.NewRouter(placement.RouterOptions{Local: node.Plane, Resolver: res, Self: self, RPC: m, Epoch: mem.Epoch})
	node.Engine.SetPlane(router)
	// What is not a plane call follows the home too: a project's backup timer runs where its data is,
	// and a resume or a resize is judged against the memory and cores of the node that runs it. The
	// node's own timers stay available to whoever starts or stops a primary here (failover).
	localTimers := node.Engine.Timers()
	node.Engine.SetTimers(router.Timers(localTimers))
	node.Engine.SetRemoteNodes(router)
	if localTimers != nil {
		Provide[lifecycle.Timers](w, localTimers)
	}

	agent := placement.NewNodeAgent(placement.AgentOptions{
		Cfg: w.Cfg, Plane: node.Plane, Registry: node.Registry, Keys: node.Engine.Keys, Members: mem,
		Seeder: bk.seed, Admit: node.Engine.AdmitReplica, Log: w.Log.With("component", "replica-agent"),
	})
	agent.Start(ctx)
	ops := &placement.Ops{Self: self, Agent: agent, Backups: bk, RPC: m, Epoch: mem.Epoch}
	if err := placement.Register(handle, placement.HandlerDeps{
		Agent: agent, Plane: node.Plane, Checkpoints: node.Plane, Resolver: res, Members: mem, Backups: bk,
		Timers: localTimers, Resources: func() lifecycle.NodeResources { return lifecycle.DetectNode(w.Cfg) },
	}); err != nil {
		return err
	}

	// A promotion and a demotion take the project's ports from the mesh's forwarders (the canonical
	// ports on a replica's node are forwarders to the home).
	if h, ok := providedAs[portHolder](w); ok {
		node.Plane.SetPortHolder(h.Suspend)
	}

	// A demoted cluster catches up through the node's own relay.
	node.Plane.SetRestoreCommand(func(ref string) string { return backup.RestoreCommandFor(w.Cfg, ref, ref, w.Options.ConfigPath) })
	// A resize restarts the replicas of the project in the right order; one that does not come back alerts.
	node.Engine.SetReplicaFleet(&placement.Fleet{Registry: node.Registry, Ops: ops, Epoch: mem.Epoch, OnFailure: replicaFailed})

	Provide[placement.Resolver](w, res)
	Provide[placement.PlaneRouter](w, router)
	Provide[placement.InstanceOps](w, ops)
	Provide[placement.BackupOps](w, ops)

	// The replicas of this node start with the daemon, report to the leader and get their PostgREST's
	// schema cache reloaded on a timer. The observation of the replicas and of the projects homed here
	// is kept in memory and refreshed in the background, so that a report answers from it.
	cache := &placement.ReportCache{Agent: agent, Projects: unlessLeader(mem, projectHealth(node.Registry, node.Plane, self))}
	Provide[placement.Contribution](w, cache.Contribute)
	reporter := &placement.Reporter{Members: mem, RPC: m, Agent: cache, Projects: cache.ProjectHealth}
	w.Go("replicas start", func(ctx context.Context) error { agent.StartLocal(ctx); return nil })
	w.Go("replica report cache", func(ctx context.Context) error { cache.Run(ctx, 10*time.Second); return nil })
	w.Go("replica report", func(ctx context.Context) error {
		reporter.Run(ctx, 10*time.Second, func(err error) { w.Log.Warn("replica report", "error", err) })
		return nil
	})
	w.Go("replica schema reload", func(ctx context.Context) error {
		node.Plane.RunSchemaReload(ctx, w.Cfg.Replicas.SchemaReload(), func(ctx context.Context) ([]string, error) {
			rs, err := node.Registry.ListReplicasOn(ctx, self())
			var refs []string
			for _, r := range rs {
				if r.Ref != "system" {
					refs = append(refs, r.Ref)
				}
			}
			return refs, err
		})
		return nil
	})
	return nil
}

// portHolder is what the mesh's forwarders (*mesh.Forwarders) offer a promotion: Suspend closes the
// listeners of a project and keeps them closed until the returned function is called.
type portHolder interface {
	Suspend(ref string) (resume func())
}

// providedAs returns the value of type T that an earlier hook provided, under whatever type it
// chose. The forwarders are provided by the mesh hook under their own type.
func providedAs[T any](w *Wire) (T, bool) {
	for _, v := range w.values {
		if t, ok := v.(T); ok {
			return t, true
		}
	}
	var zero T
	return zero, false
}

// replicaFailed raises the replica_unhealthy alert for a replica that did not come back after the
// restart of a resize.
func replicaFailed(ctx context.Context, r registry.Replica, cause error) {
	_ = alerts.Notify(ctx, alerts.Event{
		Kind: alerts.KindReplicaUnhealthy, Ref: r.Ref, Key: alerts.KindReplicaUnhealthy + "/" + r.Identifier,
		Title:  fmt.Sprintf("Replica %s is unhealthy", r.Identifier),
		Detail: fmt.Sprintf("The replica on node %s did not come back after it was restarted: %v. Restart it from the dashboard or with `supavise replicas`, or remove it and add it again.", r.NodeID, cause),
	})
}

// unlessLeader is f on a node that is not the leader, and nothing on the leader: it reports to nobody
// and reads the health of its own projects itself, so the probes would be wasted.
func unlessLeader(m cluster.Membership, f func(context.Context) []peerapi.ProjectHealth) func(context.Context) []peerapi.ProjectHealth {
	return func(ctx context.Context) []peerapi.ProjectHealth {
		if m.IsLeader() {
			return nil
		}
		return f(ctx)
	}
}

// projectHealth reports the health of the projects homed on this node, for the report to the leader.
func projectHealth(reg registry.Registry, plane lifecycle.Plane, self func() string) func(ctx context.Context) []peerapi.ProjectHealth {
	return func(ctx context.Context) []peerapi.ProjectHealth {
		ps, err := reg.ListProjects(ctx)
		if err != nil {
			return nil
		}
		var out []peerapi.ProjectHealth
		for i := range ps {
			p := &ps[i]
			if p.NodeID != self() || p.Ref == "system" || (p.Status != registry.StatusActiveHealthy && p.Status != registry.StatusActiveUnhealthy) {
				continue
			}
			ph := peerapi.ProjectHealth{Ref: p.Ref, Healthy: true}
			for _, h := range plane.Health(ctx, p, nil) {
				if !h.Healthy {
					ph.Healthy, ph.Detail = false, h.Name+": "+h.Error
					break
				}
			}
			out = append(out, ph)
		}
		return out
	}
}

// lazyBackups opens the backup service when a replica or a backup operation first needs it, so
// that a node whose backend is not configured fails that operation and nothing else.
type lazyBackups struct {
	build func(ctx context.Context) (*backup.Service, error)
	mu    sync.Mutex
	svc   *backup.Service
}

func (l *lazyBackups) get(ctx context.Context) (*backup.Service, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.svc != nil {
		return l.svc, nil
	}
	s, err := l.build(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup service: %w", err)
	}
	l.svc = s
	return s, nil
}

var _ placement.LocalBackups = (*lazyBackups)(nil)

// seed is the lifecycle.ReplicaSeeder of the node: the backup service's SeedReplica.
func (l *lazyBackups) seed(ctx context.Context, plan lifecycle.ReplicaSeedPlan) error {
	s, err := l.get(ctx)
	if err != nil {
		return err
	}
	rs, ok := any(s).(backup.ReplicaSeeder)
	if !ok {
		return errors.New("this release's backup service cannot seed a replica")
	}
	return rs.SeedReplica(ctx, backup.ReplicaSeedPlan(plan))
}

func (l *lazyBackups) BaseBackupWith(ctx context.Context, ref string, bo backup.BackupOptions) (*registry.Backup, error) {
	s, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return s.BaseBackupWith(ctx, ref, bo)
}

func (l *lazyBackups) RestoreInPlace(ctx context.Context, ref string, req lifecycle.RestoreRequest) error {
	s, err := l.get(ctx)
	if err != nil {
		return err
	}
	return s.RestoreInPlace(ctx, ref, req)
}
