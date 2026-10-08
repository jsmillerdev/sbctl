package app

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/lifecycle"
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
// which makes a setup request answer "No Supavise server is joined" and runs nothing.
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
	if o.Ops == nil || o.Backups == nil {
		w.Log.Debug("replica controller idle: no node operations or no base backups on this node", "ops", o.Ops != nil, "backups", o.Backups != nil)
		return nil
	}
	w.Go("replicas", func(ctx context.Context) error {
		if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	})
	return nil
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
// disk. A remote node's are not known here, so its replicas are judged by the node itself when it
// is asked to create them.
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
