package app

import (
	"context"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	failoveraws "github.com/supavise/supavise/internal/failover/aws"
	"github.com/supavise/supavise/internal/failover/command"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
)

// wireFailover builds the failover orchestrator, starts the automatic monitor when [failover]
// mode asks for it, checks the boot epoch (design 2.10.8), registers the cooperative fence endpoint
// and the readiness route, and provides failover.Service. On a node with no cluster it does nothing.
//
// It takes from the other hooks what only they can build, by interface type (Provide/Get):
//
//	wireMesh       mesh.Mesh, cluster.Membership
//	wirePlacement  placement.InstanceOps, placement.BackupOps, failover.LocalPrimaries (the node's plane,
//	               with the final checkpoint of a stopped cluster)
//	wireFleet      failover.Fleet (Realtime and Supavisor quiesce and re-registration), failover.LocalServices
//	               (the shared services as one unit of work), and server checks (AddServerCheck)
//	wireReplicas   replicas.Service
//
// and provides the ports that are its own: failover.Locker (the project lock), failover.ExtraChecks (the
// checks the hooks added) and failover.Takeover (below). A part that is missing is not a quiet
// no-op: the hook switches the matching feature off with the reason (Wire.Off), and the wiring test
// fails for a port that is neither provided nor off.
//
// A server move spans two processes. The survivor is a follower, so its daemon opens the registry
// read-only and runs the shared services in follower mode; the move promotes its system cluster, and
// the daemon notices and restarts (ErrRoleChanged), as it does on every role change. The first process
// runs the move up to the promotion; Takeover.BecomeLeader hands it to the next one, which boots as the
// leader and finishes it (resumeServerMove) once the shared services and the projects are up.
func wireFailover(ctx context.Context, w *Wire) error {
	members, ok := Get[cluster.Membership](w)
	if !ok || !w.clustered() {
		return nil // a single server has nothing to fail over to
	}
	rpc, _ := Get[mesh.Mesh](w)
	ops, ok := Get[placement.InstanceOps](w)
	if !ok {
		w.Off("failover.Service", "no placement.InstanceOps: the node cannot reach the replica instances")
		return nil
	}
	local, ok := Get[failover.LocalPrimaries](w)
	if !ok {
		w.Off("failover.Service", "no failover.LocalPrimaries: the node cannot stop or start its primaries")
		return nil
	}

	cfg := w.Cfg
	store := func() failover.Store { return w.Node.Registry }
	handoff := &handoffTakeover{m: members, log: w.Log.With("component", "failover")}
	locks := &projectLocker{reg: func() registry.Registry { return w.Node.Registry }}
	extra := joinServerChecks(w.serverChecks)
	Provide[failover.Locker](w, locks)
	Provide[failover.ExtraChecks](w, extra)
	Provide[failover.Takeover](w, handoff)

	d := failover.Deps{
		Cfg: cfg, Log: w.Log, Store: store, Members: members,
		Instances:      ops,
		LocalPrimaries: local,
		Primaries:      failover.MeshPrimaries{Self: func() string { return members.Self().ID }, Local: local, RPC: rpc, Epoch: members.Epoch},
		Locks:          locks,
		Extra:          extra,
		Takeover:       handoff,
		Notify:         handoff.notify,
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
	if bs, ok := Get[*backup.Service](w); ok {
		d.Marker = bs
	} else {
		w.Off("failover.Marker", "the backup service did not open: no leader marker is written or read, so a node that cannot reach its peers cannot learn that it was replaced")
	}
	// A fence of the whole node stops every cluster and shared service of the supervisor at once and
	// removes their launchers, the system cluster's too.
	if sup := w.Node.Supervisor; sup != nil {
		log := w.Log.With("component", "fence")
		d.FenceNode = func(ctx context.Context) ([]string, error) { return cluster.FenceLocal(ctx, cfg, sup, log) }
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
	if lf, ok := Get[*lateFencer](w); ok {
		lf.set(o) // the running leader's membership asks it before it fences the node (cluster.LiveOptions.Fence)
	}
	w.API.Failover = o
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

	if reason, behind := hostBehind(w); behind {
		w.Off("failover monitor", reason)
	} else {
		mon := failover.NewMonitor(o)
		w.Go("failover monitor", func(ctx context.Context) error { mon.Run(ctx); return nil })
	}
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
	w.Go("failover resume", func(ctx context.Context) error {
		resumeServerMove(ctx, w, o, members)
		return nil
	})
	return nil
}

// joinServerChecks makes the one failover.ExtraChecks of the checks the hooks added. A node whose hooks
// added none still gets a function: the preflight asks it, and it answers with nothing.
func joinServerChecks(parts []failover.ExtraChecks) failover.ExtraChecks {
	return func(ctx context.Context, to registry.Node) []failover.Check {
		var out []failover.Check
		for _, p := range parts {
			out = append(out, p(ctx, to)...)
		}
		return out
	}
}

// resumeServerMove finishes a server move that the restart of the daemon cut short. A move that
// promoted this node's system cluster belongs to the leader this node now is: the process that ran it
// handed it over (handoffTakeover), and nothing else would continue it. It waits for the shared services
// and the projects to be up, because the move registers the projects with them again. A move that stopped
// earlier (before the promotion) is not resumed: this node is not the leader then, and `supavise
// failover --resume` is the operator's call.
func resumeServerMove(ctx context.Context, w *Wire, o failover.Service, m cluster.Membership) {
	if !m.IsLeader() {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-w.Ready():
	}
	pl, err := o.PlanServer(ctx, failover.ServerOptions{Resume: true})
	if err != nil || pl == nil || pl.To != m.Self().ID || !hasPassed(pl, "unfinished move") {
		return
	}
	w.Log.Info("finishing the server move this node was promoted by", "from", pl.From, "to", pl.To, "epoch", pl.Epoch)
	if _, err := o.FailoverServer(ctx, failover.ServerOptions{Resume: true}); err != nil && ctx.Err() == nil {
		w.Log.Error("the server move that promoted this node did not finish; run `supavise failover --resume`", "error", err)
	}
}

// hasPassed reports whether the plan holds a passed check called name.
func hasPassed(pl *failover.Plan, name string) bool {
	for _, c := range pl.Checks {
		if c.Name == name && c.OK {
			return true
		}
	}
	return false
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
