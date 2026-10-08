package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
)

// wireReplicas builds the replica controller (design 2.7) and provides it as replicas.Service,
// replicas.Remover and replicas.ReportSink, so the Management API handlers, project delete and
// restore, `supavise node rm` and the peer API's report intake can reach it. The controller runs
// on the leader once the node can reach other nodes (placement.InstanceOps) and take base
// backups (backup.BaseBackupEnsurer): it creates and watches the standbys, keeps the default
// replicas and removes the ones that go. On a node with no cluster it only serves the registry,
// which makes a setup request answer "No Supavise server is joined" and runs nothing; the Management
// API is given nothing there, so a single server answers as it always did.
//
// A node that belongs to a cluster always gives the Management API its controller, because replica rows
// can exist on it (they were made while the controller ran, and whether it runs is decided at each boot)
// and a project delete or an in-place restore removes them through replicas.Remover first. When the
// controller does not run (the host is behind, or the node cannot reach the others or take base
// backups) the API is given replicasOff: it lists the replicas and removes them, which need no running
// controller, and refuses a setup, an add or a restart with the reason.
//
// The reports that nodes send the leader (cluster.Reports) reach the controller through a subscription
// made here: the mesh hook runs first and provides the store, this hook subscribes the sink the
// controller is.
func wireReplicas(ctx context.Context, w *Wire) error {
	o := replicas.Options{
		Registry: w.Node.Registry, Config: w.Cfg, Log: w.Log.With("component", "replicas"),
		SnapshotPath: filepath.Join(w.Cfg.StateDir, replicas.SnapshotName),
	}
	if m, ok := Get[cluster.Membership](w); ok {
		o.Members = m
	}
	if ops, ok := Get[placement.InstanceOps](w); ok {
		o.Ops = ops
	}
	o.Backups = baseBackups(w)
	if p, ok := Get[replicas.Pooler](w); ok {
		o.Pooler = p
	}
	o.Admit = replicas.RoomAdmitter{Cfg: w.Cfg, Room: localRoom(w)}
	c := replicas.New(o)
	Provide[replicas.Service](w, c)
	Provide[replicas.Remover](w, c)
	Provide[replicas.ReportSink](w, c)
	clustered := w.clustered()
	// Whether the controller runs here, and if not, why. A controller that is not running still takes a
	// setup request and writes its row, which nothing then picks up.
	why, _ := hostBehind(w)
	if why == "" && (o.Ops == nil || o.Backups == nil) {
		why = fmt.Sprintf("this node cannot reach the replica nodes or take base backups (node operations: %v, base backups: %v)", o.Ops != nil, o.Backups != nil)
	}
	if clustered {
		if why == "" {
			w.API.Replicas = c
		} else {
			w.API.Replicas = replicasOff{Controller: c, why: why}
		}
		if r, ok := Get[placement.Resolver](w); ok {
			w.API.Placement = r
		}
		// What the leader hears from the other nodes: the intake keeps the latest report of each node
		// and hands it over off the intake goroutine, which must not block on a registry read. Node is
		// the authenticated peer (cluster.Reports.Put sets it from the mTLS identity, never from the body).
		if reports, ok := Get[*cluster.Reports](w); ok {
			in := newReportIntake(c)
			reports.Subscribe(in.Put)
			w.Go("replica report intake", func(ctx context.Context) error { in.Run(ctx); return nil })
		} else {
			w.Off("replicas.ReportSink", "the mesh provided no report store; the controller learns what the replicas do by polling only")
		}
	}
	if why != "" {
		// A node that has joined others and cannot create replicas is a gap in the wiring the operator
		// should hear about; a single server has nothing to run.
		if clustered {
			w.Off("replica controller", why+"; the Management API lists and removes replicas but refuses to set one up")
		}
		return nil
	}
	if o.Pooler == nil && clustered {
		w.Off("replicas.Pooler", "no pooler adapter: replicas have no Supavisor tenant")
	}
	w.Go("replicas", func(ctx context.Context) error {
		if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	})
	return nil
}

// replicasOff is the replica controller as the Management API gets it while the controller does not run
// on this node. Setting up, adding and restarting a replica need the running controller and are refused
// with the reason (a *replicas.UserError, which the API shows). Everything else is the controller's:
// listing and the status and lag of the replicas that exist, and replicas.Remover (RemoveAll, RemoveOn),
// which removes through placement.InstanceOps and leaves what it cannot reach for the controller to
// retry (*replicas.PendingError). A project delete asks the Remover first, so it never drops the
// replica rows of an instance that nothing then finds.
type replicasOff struct {
	*replicas.Controller
	why string
}

func (r replicasOff) refusal() error {
	return &replicas.UserError{Msg: "Read replicas are not available on this Supavise server: " + r.why + "."}
}

func (r replicasOff) Setup(context.Context, string, string) error   { return r.refusal() }
func (r replicasOff) SetupOn(context.Context, string, string) error { return r.refusal() }
func (r replicasOff) Remove(context.Context, string, string) error  { return r.refusal() }
func (r replicasOff) Restart(context.Context, string, string) error { return r.refusal() }

// reportIntake hands the reports that arrive at the leader to the replica controller. Reports.Put calls
// its subscribers on the goroutine of the peer API request, and the controller reads the registry for
// every instance a report names, so the report is kept and a goroutine of its own passes it on. Only the
// latest report of a node is kept: a node reports every ten seconds, and the older one says less.
type reportIntake struct {
	sink replicas.ReportSink

	mu      sync.Mutex
	pending map[string]peerapi.Report
	wake    chan struct{}
}

func newReportIntake(sink replicas.ReportSink) *reportIntake {
	return &reportIntake{sink: sink, pending: map[string]peerapi.Report{}, wake: make(chan struct{}, 1)}
}

// Put is the subscriber: it never blocks.
func (in *reportIntake) Put(rep peerapi.Report) {
	in.mu.Lock()
	in.pending[rep.Node] = rep
	in.mu.Unlock()
	select {
	case in.wake <- struct{}{}:
	default:
	}
}

// Run passes the reports on until ctx ends.
func (in *reportIntake) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-in.wake:
		}
		in.mu.Lock()
		batch := in.pending
		in.pending = map[string]peerapi.Report{}
		in.mu.Unlock()
		for _, rep := range batch {
			in.sink.HandleReport(ctx, rep)
		}
	}
}

// baseBackups finds what takes the base backup a replica starts from: a hook that provides it, or
// the backup service the Management API was given.
func baseBackups(w *Wire) backup.BaseBackupEnsurer {
	if b, ok := Get[backup.BaseBackupEnsurer](w); ok {
		return b
	}
	if w.API != nil && w.API.Backups != nil {
		if b, ok := w.API.Backups.(backup.BaseBackupEnsurer); ok {
			return b
		}
	}
	return nil
}

// localRoom is what this daemon knows of a node's room: its own machine's memory, cores and free
// disk. A remote node's are not known here, so the node judges its replicas itself when it is asked
// to create them: a refusal for lack of room (replicas.RoomError) leaves the replica waiting at
// 0_requested with a replica_capacity alert.
func localRoom(w *Wire) func(ctx context.Context, n registry.Node) replicas.Room {
	return func(_ context.Context, n registry.Node) replicas.Room {
		self := registry.FounderNodeID
		if m, ok := Get[cluster.Membership](w); ok {
			self = m.Self().ID
		}
		if n.ID != self {
			return replicas.Room{}
		}
		r := replicas.Room{Resources: lifecycle.DetectNode(w.Cfg)}
		var st syscall.Statfs_t
		if syscall.Statfs(w.Cfg.StateDir, &st) == nil {
			r.FreeDiskBytes, r.DiskKnown = int64(st.Bavail)*int64(st.Bsize), true
		}
		return r
	}
}
