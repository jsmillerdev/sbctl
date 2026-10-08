package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	failoveraws "github.com/supavise/supavise/internal/failover/aws"
	"github.com/supavise/supavise/internal/failover/command"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/notimpl"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/replicas"
)

// wireFailover builds the failover orchestrator, starts the automatic monitor when [failover]
// mode asks for it, checks the boot epoch (design 2.10.8), registers the cooperative fence endpoint
// and the readiness route, and provides failover.Service. On a node with no cluster it does nothing.
//
// It takes from the other hooks what only they can build, by interface type (Provide/Get):
//
//	required   mesh.Mesh, placement.InstanceOps (wireMesh, wirePlacement), failover.LocalPrimaries
//	           (wirePlacement: the node's plane, with the final checkpoint of a stopped cluster)
//	optional   failover.Fleet and failover.LocalServices (wireFleet: Realtime and Supavisor quiesce
//	           and re-registration; the shared services as one unit of work), failover.Takeover
//	           (the role change to leader; without it the hook waits for the membership to say so),
//	           placement.BackupOps, replicas.Service, failover.Locker (the engine's project lock),
//	           failover.ExtraChecks (what wireProxy and wireFleet add to the preflight of a server move)
//
// A part that is missing makes the matching step of a move a no-op, except the required ones,
// without which the hook is skipped as not implemented yet.
func wireFailover(ctx context.Context, w *Wire) error {
	members, ok := Get[cluster.Membership](w)
	if !ok || len(members.Nodes()) < 2 {
		return nil
	}
	rpc, ok := Get[mesh.Mesh](w)
	if !ok {
		return notimpl.For("wire failover: no mesh")
	}
	ops, ok := Get[placement.InstanceOps](w)
	if !ok {
		return notimpl.For("wire failover: no placement.InstanceOps")
	}
	local, ok := Get[failover.LocalPrimaries](w)
	if !ok {
		return notimpl.For("wire failover: no failover.LocalPrimaries")
	}

	cfg := w.Cfg
	store := func() failover.Store { return w.Node.Registry }
	d := failover.Deps{
		Cfg: cfg, Log: w.Log, Store: store, Members: members,
		Instances:      ops,
		LocalPrimaries: local,
		Primaries:      failover.MeshPrimaries{Self: func() string { return members.Self().ID }, Local: local, RPC: rpc, Epoch: members.Epoch},
	}
	peers := failover.MeshPeers{RPC: rpc}
	d.Peers, d.Leader = peers, peers
	if f, ok := Get[failover.Fleet](w); ok {
		d.Fleet = f
	}
	if s, ok := Get[failover.LocalServices](w); ok {
		d.LocalServices = s
	}
	if b, ok := Get[placement.BackupOps](w); ok {
		d.Backups = b
	}
	if r, ok := Get[replicas.Service](w); ok {
		d.Replicas = r
	}
	if l, ok := Get[failover.Locker](w); ok {
		d.Locks = l
	}
	if x, ok := Get[failover.ExtraChecks](w); ok {
		d.Extra = x
	}
	if t, ok := Get[failover.Takeover](w); ok {
		d.Takeover = t
	} else {
		d.Takeover = membershipTakeover{members}
	}
	if m, ok := w.API.Backups.(backup.EpochMarkerStore); ok {
		d.Marker = m
	}
	d.Provider = failoverProvider(w, members, store)
	d.PublicProbe = failover.PublicProbe(cfg.APIHost(), func(ctx context.Context) string {
		if cl, err := store().GetCluster(ctx); err == nil {
			return cl.ServiceAddress.IP
		}
		return ""
	}, cfg.TLS.Mode == "off")

	o, err := failover.New(d)
	if err != nil {
		return err
	}
	Provide[failover.Service](w, o)
	api.SetFailoverSource(o)
	w.OnStop(func() { api.SetFailoverSource(nil) })
	for pattern, h := range o.PeerHandlers() {
		mesh.Handle(pattern, h)
	}

	// At boot, before any project starts: a leader that was replaced while it was away learns it.
	// Only a node that leads according to its own registry is asked (a follower's copy is behind
	// and nothing it homes moved); every node has its fence record read.
	bctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	res, err := o.BootCheck(bctx)
	cancel()
	switch {
	case err != nil:
		w.Log.Error("the boot epoch check failed", "error", err)
	case res.Fenced:
		w.Log.Error("this node is fenced: no cluster starts as a primary until `supavise node rejoin`", "reason", res.Reason, "epoch", res.Epoch, "leader", res.Leader)
	}

	mon := failover.NewMonitor(o)
	w.Go("failover monitor", func(ctx context.Context) error { mon.Run(ctx); return nil })
	ctl := &failover.ControlServer{Svc: o, Log: w.Log}
	// The socket is the CLI's way in; a path that is too long or a directory that cannot be written
	// costs the CLI, not the node, so the failure is logged and the daemon goes on.
	w.Go("failover control", func(ctx context.Context) error {
		if err := ctl.Serve(ctx, failover.ControlSocket(cfg)); err != nil {
			w.Log.Error("the failover control socket is not available; supavise failover and status cannot reach this daemon", "error", err)
		}
		return nil
	})
	w.Go("failover janitor", func(ctx context.Context) error {
		sweepDiverged(ctx, cfg, w)
		return nil
	})
	return nil
}

// sweepDiverged removes the data directories that `node rejoin` and a failover set aside once
// they are older than [failover] keep_diverged_days, hourly.
func sweepDiverged(ctx context.Context, cfg *config.Config, w *Wire) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if removed, err := failover.SweepDiverged(cfg, cfg.Failover.KeepDiverged(), time.Now()); err != nil {
			w.Log.Warn("could not remove an old diverged data directory", "error", err)
		} else if len(removed) > 0 {
			w.Log.Info("removed diverged data directories past keep_diverged_days", "paths", removed)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// failoverProvider chooses the fencer by [failover] fencing.
func failoverProvider(w *Wire, members cluster.Membership, store func() failover.Store) failover.Provider {
	f := w.Cfg.Failover
	switch f.Fencing {
	case config.FencingAWS:
		var mu sync.Mutex
		clients := map[string]*awsapi.Client{}
		return &failoveraws.Provider{
			EC2: func(region string) (failoveraws.EC2, error) {
				mu.Lock()
				defer mu.Unlock()
				c := clients[region]
				if c == nil {
					var err error
					if c, err = awsapi.New(awsapi.Config{Region: region}); err != nil {
						return nil, err
					}
					clients[region] = c
				}
				return c.EC2, nil
			},
			Cluster: func(ctx context.Context) (failoveraws.Cluster, error) {
				cl, err := store().GetCluster(ctx)
				if err != nil {
					return failoveraws.Cluster{}, err
				}
				return failoveraws.Cluster{Self: members.Self(), Peers: members.Nodes(), Service: cl.ServiceAddress}, nil
			},
			StopTimeout: f.StopTimeout(),
		}
	case config.FencingCommand:
		return &command.Provider{FenceCommand: f.FenceCommand, TakeoverCommand: f.TakeoverCommand, Timeout: f.StopTimeout()}
	}
	return failover.Manual{}
}

// membershipTakeover waits for the membership to report this node as the leader at the epoch of
// the move: the daemon notices that its system cluster is a primary and says so. A node whose
// wiring has a richer Takeover (one that waits for the shared services too) provides it instead.
type membershipTakeover struct{ m cluster.Membership }

func (t membershipTakeover) BecomeLeader(ctx context.Context, epoch int64) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for s := range t.m.Watch(ctx) {
		if s.Role == cluster.RoleLeader && s.Leader == s.Self.ID && s.Epoch >= epoch {
			return nil
		}
	}
	return fmt.Errorf("the node did not become the leader before the wait ended")
}
