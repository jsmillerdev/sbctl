package health

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jsmillerdev/supavise/internal/backup"
	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/fleet"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
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
		d.Escrow = EscrowCheck(n.Cfg, time.Hour)
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
// key (what `supavise backups status` prints as "master key"). The answer is reused for ttl:
// listing an S3 bucket on every probe would be wasteful for something that changes once.
func EscrowCheck(cfg *config.Config, ttl time.Duration) func(context.Context) (*Escrow, error) {
	var (
		mu   sync.Mutex
		at   time.Time
		last *Escrow
	)
	return func(ctx context.Context) (*Escrow, error) {
		mu.Lock()
		defer mu.Unlock()
		if last != nil && time.Since(at) < ttl {
			return last, nil
		}
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
		switch {
		case rerr != nil:
			e.Covered = len(all) > 0 // the key cannot be read by this user, so any copy counts
			e.Detail = fmt.Sprintf("%d encrypted copy(ies) in the backup backend", len(all))
		default:
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
		}
		last, at = e, time.Now()
		return e, nil
	}
}
