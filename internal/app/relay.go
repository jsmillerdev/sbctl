package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// StartWALRelay starts the WAL relay over the configured backend (nil and a no-op stop
// when the node does not archive through the daemon, config.Backup.WALRelay). The relay
// outlives ctx: stop ends it, so a caller can keep archiving available while it drains.
//
// The daemon calls it with cli false. A command-line process that has to wait for WAL to
// be archived (`supavise backups create`, a delete's final base backup) calls it with cli
// true while the daemon may be down: it serves only the sockets that nobody answers (a
// relay never replaces one that answers) and looks for them more often. refs names the
// projects the command works on; without refs a CLI relay serves every project.
//
// The backend is opened on the first request that needs it and again after a failure, so
// a bucket that is unreachable at boot keeps neither the daemon from starting nor the
// relay from recovering.
//
// On a server that belongs to a cluster the relay also guards the archive (relayGuard): the cluster
// of a replica does not push WAL, whoever runs the relay.
func StartWALRelay(ctx context.Context, cfg *config.Config, log *slog.Logger, cli bool, refs ...string) (*backup.Relay, func()) {
	return StartWALRelayAt(ctx, cfg, "", log, cli, refs...)
}

// StartWALRelayAt is StartWALRelay for a process that knows the config file it loaded (the daemon),
// which is where the node's cluster identity is looked for; "" is the default path.
func StartWALRelayAt(ctx context.Context, cfg *config.Config, configPath string, log *slog.Logger, cli bool, refs ...string) (*backup.Relay, func()) {
	if !cfg.WALRelayEnabled() {
		return nil, func() {}
	}
	g := newRelayGuard(cfg, configPath, log)
	o := backup.RelayOptions{
		Config: cfg,
		Log:    log.With("component", "wal-relay"),
		Service: func(ctx context.Context) (*backup.Service, error) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			store, err := backup.OpenStore(ctx, cfg.Backup)
			if err != nil {
				return nil, err
			}
			return backup.New(backup.Options{Config: cfg, Store: store, Log: log})
		},
		Replica: g.replica, Epoch: g.epoch, Refused: g.refused,
	}
	if cli {
		o.Interval = time.Second
		if len(refs) > 0 {
			o.Only = refs
		}
	}
	relay := backup.NewRelay(o)
	rctx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() { defer close(done); _ = relay.Run(rctx) }()
	return relay, func() { stopRun(); g.close(); <-done }
}

// relayGuard answers the three questions the relay's push guard asks (backup.RelayOptions.Replica,
// Epoch and Refused). The relay starts before the registry opens and a cluster archives through it from
// the first second, so the guard cannot wait for the daemon's registry: it reads the system cluster's
// registry itself, when it can, through a read-only connection of its own (the standby's socket on a
// follower), and answers from the data directory while it cannot.
//
// A server that has no cluster identity is not guarded at all: the answer is "not a replica" without a
// look at the database, which is what the relay did before.
type relayGuard struct {
	cfg        *config.Config
	clusterDir string
	log        *slog.Logger
	// open connects to the registry; tests replace it.
	open func(ctx context.Context) (registry.Registry, error)

	mu      sync.Mutex
	reg     registry.Registry
	lastTry time.Time
	self    string
}

func newRelayGuard(cfg *config.Config, configPath string, log *slog.Logger) *relayGuard {
	g := &relayGuard{cfg: cfg, clusterDir: config.ClusterDir(configPath), log: log.With("component", "wal-relay")}
	g.open = func(ctx context.Context) (registry.Registry, error) {
		var last error
		for _, dsn := range RegistryDSNs(cfg) {
			reg, err := registry.OpenReadOnly(ctx, dsn)
			if err == nil {
				return reg, nil
			}
			last = err
		}
		return nil, last
	}
	return g
}

// registryNow returns the registry, opening it at most every ten seconds, or nil while it cannot be
// reached.
func (g *relayGuard) registryNow(ctx context.Context) registry.Registry {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reg != nil {
		return g.reg
	}
	if time.Since(g.lastTry) < 10*time.Second {
		return nil
	}
	g.lastTry = time.Now()
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	reg, err := g.open(cctx)
	if err != nil {
		g.log.Debug("the relay's guard cannot read the registry yet", "error", err)
		return nil
	}
	g.reg = reg
	return reg
}

func (g *relayGuard) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reg != nil {
		g.reg.Close()
		g.reg = nil
	}
}

// replica reports whether this node's cluster of ref is a standby that was not promoted by a move: a
// replica row of the project on this node, or, while the registry cannot be read, a data directory that
// still carries standby.signal. A promotion by hand (pg_promote()) removes standby.signal, which is why
// the registry's word comes first: the row stays until a move makes this node the home.
func (g *relayGuard) replica(ctx context.Context, ref string) (bool, error) {
	if !cluster.Joined(g.clusterDir) {
		return false, nil
	}
	reg := g.registryNow(ctx)
	if reg == nil {
		_, err := os.Stat(filepath.Join(g.cfg.Paths().PostgresData(ref), "standby.signal"))
		return err == nil, nil
	}
	self, err := g.selfID(ctx, reg)
	if err != nil {
		return false, err
	}
	rows, err := reg.ListReplicas(ctx, ref)
	if err != nil {
		return false, fmt.Errorf("replica rows of %s: %w", ref, err)
	}
	for _, r := range rows {
		if r.NodeID == self {
			return true, nil
		}
	}
	return false, nil
}

// selfID is this node's id in the registry, found by name and kept.
func (g *relayGuard) selfID(ctx context.Context, reg registry.Registry) (string, error) {
	g.mu.Lock()
	id := g.self
	g.mu.Unlock()
	if id != "" {
		return id, nil
	}
	n, err := reg.GetNodeByName(ctx, g.cfg.NodeName())
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return "", fmt.Errorf("this node (%q) is not in the cluster registry", g.cfg.NodeName())
		}
		return "", err
	}
	g.mu.Lock()
	g.self = n.ID
	g.mu.Unlock()
	return n.ID, nil
}

// epoch is the cluster's epoch; a promote.ok for a lower one does not authorize a push.
func (g *relayGuard) epoch(ctx context.Context) (int64, error) {
	reg := g.registryNow(ctx)
	if reg == nil {
		return 0, errors.New("the registry cannot be read")
	}
	cl, err := reg.GetCluster(ctx)
	if err != nil {
		return 0, err
	}
	return cl.Epoch, nil
}

// refused raises the alert for a replica that tried to archive: either someone promoted it by hand, or a
// move failed half way. The relay has already refused the push, so the archive is safe; the replica is
// not, and an operator has to look.
func (g *relayGuard) refused(ref string, reason error) {
	_ = alerts.Notify(context.Background(), alerts.Event{
		Kind: alerts.KindReplicaUnhealthy, Severity: alerts.SeverityCritical, Ref: ref, Key: "wal_push_refused/" + ref,
		Title:  "A replica tried to archive WAL",
		Detail: reason.Error() + ". The relay refused the push, so the archive is intact. If nobody promoted this replica by hand, remove it and add it again.",
	})
}
