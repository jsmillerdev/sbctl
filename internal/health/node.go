package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/lifecycle"
)

// NodeOptions say how ForNode fills Deps from an opened node.
type NodeOptions struct {
	Version string
	Log     *slog.Logger
	// InDaemon is set by the daemon: it is evidently up, and so is its edge, so those two checks
	// are not probed. The CLI leaves it false and reaches both over loopback.
	InDaemon bool
	// Tenants asks the shared services about each project's tenant (a fleet.Lazy bound to the
	// node's registry and secrets). Nil skips the tenant checks.
	Tenants fleet.Fleet
	// Escrow, when set, replaces the check of the backup backend (tests).
	Escrow func(ctx context.Context) (*Escrow, error)
}

// ForNode returns the Deps that check the node n.
func ForNode(n *lifecycle.Node, o NodeOptions) (Deps, error) {
	d := Deps{
		Cfg: n.Cfg, Version: o.Version, Log: o.Log,
		Registry: n.Registry, Plane: n.Plane, Tenants: o.Tenants,
		Escrow: o.Escrow,
	}
	if d.Escrow == nil {
		d.Escrow = EscrowCheck(n.Cfg, time.Hour, o.InDaemon)
	}
	d.System = func(ctx context.Context) []lifecycle.ServiceHealth {
		p, err := n.Registry.GetProject(ctx, config.SystemRef)
		if err != nil {
			return []lifecycle.ServiceHealth{{Name: config.SvcPostgres, Status: "UNHEALTHY", Error: "the registry has no system project: " + err.Error()}}
		}
		return n.Plane.Health(ctx, p, nil)
	}
	m, err := fleet.NewManager(fleet.Deps{Cfg: n.Cfg, Log: o.Log, Supervisor: n.Supervisor})
	if err != nil {
		return d, err
	}
	d.Services = m.Status
	if !o.InDaemon {
		d.Daemon, d.Edge = ProbeAdmin(n.Cfg), ProbeEdge(n.Cfg)
	}
	return d, nil
}

// ForDown returns the Deps for a node whose registry could not be opened (the system cluster
// is not running, or the key cannot be read): what can still be said without it. status is
// the checks that need no registry: the system project's units through system, the shared
// services, the daemon and the edge, the disk and the certificates.
func ForDown(cfg *config.Config, version string, log *slog.Logger, regErr error,
	system func(ctx context.Context) []lifecycle.ServiceHealth, services func(ctx context.Context) []fleet.Health) Deps {
	return Deps{
		Cfg: cfg, Version: version, Log: log, RegistryErr: regErr,
		System: system, Services: services,
		Daemon: ProbeAdmin(cfg), Edge: ProbeEdge(cfg),
	}
}

// EscrowCheck returns a check of the backup backend for an encrypted copy of this node's master
// key (what `supavise backups status` prints as "master key"). The answer changes once in the
// life of a node, so it is reused for ttl.
//
// With background set (the daemon) the check never waits for the backend: it answers from what
// it last learned, or says "not checked yet", and refreshes in the background. A slow or
// unreachable bucket must not slow down /healthz. Without it (`supavise status`) it asks and waits.
func EscrowCheck(cfg *config.Config, ttl time.Duration, background bool) func(context.Context) (*Escrow, error) {
	var (
		mu         sync.Mutex
		at         time.Time
		last       *Escrow
		lastErr    error
		refreshing bool
	)
	ask := func(ctx context.Context) (*Escrow, error) {
		st, err := backup.OpenStore(ctx, cfg.Backup)
		if err != nil {
			return nil, err
		}
		all, err := backup.ListKeyEscrows(ctx, st)
		if err != nil {
			return nil, err
		}
		e := &Escrow{}
		b, rerr := os.ReadFile(cfg.KeyPath)
		if rerr != nil {
			e.Covered = len(all) > 0 // the key cannot be read by this user, so any copy counts
			e.Detail = fmt.Sprintf("%d encrypted copy(ies) in the backup backend", len(all))
			return e, nil
		}
		id := backup.KeyID(string(b))
		for _, o := range all {
			if o.KeyID == id {
				e.Covered = true
				e.Detail = "an encrypted copy of this node's key is in the backup backend"
			}
		}
		if !e.Covered {
			e.Detail = "the master key is not in the backups: run `supavise system escrow-key` or `supavise system export-key` (see `supavise backups status`)"
		}
		return e, nil
	}
	refresh := func(ctx context.Context) {
		e, err := ask(ctx)
		mu.Lock()
		defer mu.Unlock()
		last, lastErr, at, refreshing = e, err, time.Now(), false
	}
	return func(ctx context.Context) (*Escrow, error) {
		mu.Lock()
		fresh := !at.IsZero() && time.Since(at) < ttl
		if fresh {
			e, err := last, lastErr
			mu.Unlock()
			return e, err
		}
		if !background {
			mu.Unlock()
			e, err := ask(ctx)
			return e, err
		}
		stale, staleErr, known := last, lastErr, !at.IsZero()
		if !refreshing {
			refreshing = true
			go func() {
				rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				refresh(rctx)
			}()
		}
		mu.Unlock()
		if !known {
			return nil, errEscrowPending
		}
		return stale, staleErr
	}
}

var errEscrowPending = errors.New("not checked yet")
