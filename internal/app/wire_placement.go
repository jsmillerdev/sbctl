package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
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
	} else {
		w.Off("lifecycle.Timers", "the node has no backup timers (supervisor "+w.Cfg.Supervisor+"); base backups are taken with `supavise backups create`")
	}

	agent := placement.NewNodeAgent(placement.AgentOptions{
		Cfg: w.Cfg, Plane: node.Plane, Registry: node.Registry, Keys: node.Engine.Keys, Members: mem,
		Seeder: placement.SeederFrom(bk), Admit: node.Engine.AdmitReplica, Log: w.Log.With("component", "replica-agent"),
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
	} else {
		w.Off("mesh.Forwarders", "the mesh provided no forwarders; a promotion cannot take the project's ports from them")
	}

	// A demoted cluster catches up through the node's own relay.
	node.Plane.SetRestoreCommand(func(ref string) string { return backup.RestoreCommandFor(w.Cfg, ref, ref, w.Options.ConfigPath) })
	// A resize restarts the replicas of the project in the right order; one that does not come back alerts.
	node.Engine.SetReplicaFleet(&placement.Fleet{Registry: node.Registry, Ops: ops, Epoch: mem.Epoch, OnFailure: replicaFailed})

	routeBackups(w, mem, res, ops, self)

	Provide[placement.Resolver](w, res)
	Provide[placement.PlaneRouter](w, router)
	Provide[placement.InstanceOps](w, ops)
	Provide[placement.BackupOps](w, ops)

	// The failover orchestrator's operations on the primaries of this node: the node's own plane, not the
	// router, with the backup timers that stop and start with a primary.
	Provide[failover.LocalPrimaries](w, &localPrimaries{
		cfg: w.Cfg, plane: node.Plane, reg: func() registry.Registry { return node.Registry }, keys: node.Engine.Keys,
		timers: localTimers, log: w.Log.With("component", "failover"), now: time.Now,
	})

	// The replicas of this node start with the daemon and report to the leader, and their PostgREST's
	// schema cache gets reloaded on a timer. The observation of the replicas and of the projects homed
	// here is kept in memory and refreshed in the background, so that a report answers from it. The
	// report is the mesh's (cluster.Reporter, which the mesh hook runs): this adds the node's
	// observations to it, so that one sender speaks for the node.
	cache := &placement.ReportCache{Agent: agent, Projects: unlessLeader(mem, projectHealth(node.Registry, node.Plane, self))}
	Provide[placement.Contribution](w, cache.Contribute)
	if rep, ok := Get[*cluster.Reporter](w); ok {
		rep.Add(cluster.Contributor(cache.Contribute))
	} else {
		w.Off("cluster.Reporter", "the mesh provided no reporter; this node's replicas and projects are not reported to the leader")
	}
	w.Go("replicas start", func(ctx context.Context) error { agent.StartLocal(ctx); return nil })
	w.Go("replica report cache", func(ctx context.Context) error { cache.Run(ctx, 10*time.Second); return nil })
	w.Go("replica schema reload", func(ctx context.Context) error {
		node.Plane.RunSchemaReload(ctx, w.Cfg.Replicas.SchemaReload(), func(ctx context.Context) ([]string, error) {
			rs, err := node.Registry.ListReplicasOn(ctx, self())
			var refs []string
			for _, r := range rs {
				if !r.IsSystemStandby() {
					refs = append(refs, r.Ref)
				}
			}
			return refs, err
		})
		return nil
	})
	return nil
}

// scheduledBackupsRound is how often the leader looks for projects homed on other nodes whose newest
// base backup is a day old.
const scheduledBackupsRound = 15 * time.Minute

// routeBackups lets the leader's backup service work for the projects homed on other nodes. A base backup
// reads the data directory of the project's home, and a follower's registry takes no write, so the home
// takes the backup and writes it to the store (placement.BackupOps) and the leader records it
// (backup.Service.RecordBase): the Ops record what a node took for them, the service takes the base backup
// of a replica's seed on the home (TakeBase), the Engine takes the final backup of a delete there
// (SetRemoteBackups), and the leader takes the nightly backup of those projects, whose timers do not run
// on a follower (placement.ScheduledBackups). The leader's service is the one Serve provides; without
// it, which is a backend that does not open, none of this works and the node says so.
func routeBackups(w *Wire, mem cluster.Membership, res placement.Resolver, ops *placement.Ops, self func() string) {
	bs, ok := Get[*backup.Service](w)
	if !ok {
		w.Off("placement.RoutedBackups", "the backup service did not open: the base backups of projects homed on other nodes are not taken or recorded, and a delete of one is refused")
		return
	}
	routed := &placement.RoutedBackups{Self: self, Resolver: res, Ops: ops, Local: bs}
	ops.Recorder = bs
	bs.SetTakeBase(routed.TakeBase)
	w.Node.Engine.SetRemoteBackups(routed)
	Provide(w, routed)
	sched := &placement.ScheduledBackups{Registry: w.Node.Registry, Self: self, Routed: routed, Leader: mem.IsLeader, Log: w.Log.With("component", "scheduled-backups")}
	w.Go("scheduled backups", func(ctx context.Context) error { sched.Run(ctx, scheduledBackupsRound); return nil })
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

// projectProbeTimeout bounds the health probe of one project. The report is refreshed every ten
// seconds, and a probe that hangs must not hold up the report of the other projects. (A variable so
// that a test can shorten it.)
var projectProbeTimeout = 10 * time.Second

// projectHealth reports the health of the projects homed on this node, for the report to the leader.
// It probes a few at a time (placement.DefaultConcurrency), each under projectProbeTimeout, and lists
// them in the order the registry does.
func projectHealth(reg registry.Registry, plane lifecycle.Plane, self func() string) func(ctx context.Context) []peerapi.ProjectHealth {
	return func(ctx context.Context) []peerapi.ProjectHealth {
		ps, err := reg.ListProjects(ctx)
		if err != nil {
			return nil
		}
		var homed []*registry.Project
		for i := range ps {
			p := &ps[i]
			if p.NodeID != self() || p.Ref == config.SystemRef || !p.Status.Running() {
				continue
			}
			homed = append(homed, p)
		}
		if len(homed) == 0 {
			return nil
		}
		out := make([]peerapi.ProjectHealth, len(homed))
		idx := make([]int, len(homed))
		for i := range idx {
			idx[i] = i
		}
		placement.EachLimit(ctx, placement.DefaultConcurrency, idx, func(i int) {
			p := homed[i]
			pctx, cancel := context.WithTimeout(ctx, projectProbeTimeout)
			defer cancel()
			ph := peerapi.ProjectHealth{Ref: p.Ref, Healthy: true}
			for _, h := range plane.Health(pctx, p, nil) {
				if !h.Healthy {
					ph.Healthy, ph.Detail = false, h.Name+": "+h.Error
					break
				}
			}
			out[i] = ph
		})
		// A project that was not probed because ctx ended has no entry.
		return slices.DeleteFunc(out, func(h peerapi.ProjectHealth) bool { return h.Ref == "" })
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

var _ backup.ReplicaSeeder = (*lazyBackups)(nil)

// SeedReplica is the backup service's, which opens on first use; placement.SeederFrom makes the plane's
// seeder of it.
func (l *lazyBackups) SeedReplica(ctx context.Context, plan backup.ReplicaSeedPlan) error {
	s, err := l.get(ctx)
	if err != nil {
		return err
	}
	return s.SeedReplica(ctx, plan)
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
