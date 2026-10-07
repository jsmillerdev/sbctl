package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/OWNER/sbctl/internal/api"
	"github.com/OWNER/sbctl/internal/backup"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/fleet"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/proxy"
	"github.com/OWNER/sbctl/internal/registry"
)

// StopBudget is how long Serve, once told to stop, waits for lifecycle operations that
// are still running (a delete with its final backup, a restart, a create) before it
// closes the registry. sbctl.service's TimeoutStopSec must be longer (it is 11 minutes);
// an operation cut off anyway is finished or reverted by Engine.Recover at the next start.
const StopBudget = 10 * time.Minute

// registryWait bounds how long Serve waits for the system cluster's registry at boot.
// systemd orders sbctl.service after the start of sb-postgres@system, not its readiness.
// A variable so that tests can shorten it.
var registryWait = 2 * time.Minute

// Serve runs the daemon until ctx ends (SIGTERM in production): it opens the node
// (registry in the system cluster, secrets, engine with the backup service), finishes
// operations a crash interrupted, serves the Management API on the loopback admin
// listener and, through the proxy, at api.<domain>, runs the edge proxy, and starts
// every project that should be running. The system project's units (sb-postgres@system,
// sb-gotrue@system) must already exist: `sbctl system init` creates them and systemd
// starts them at boot, which sbctl.service waits for.
//
// Project units are systemd's, not the daemon's: stopping Serve leaves them running.
func Serve(ctx context.Context, cfg *config.Config, o Options) error {
	log := o.log()
	// The WAL relay is the only holder of the backup credentials that clusters archive
	// through (backup.Relay). It starts before the node opens, because the system cluster
	// archives too and the daemon may wait for it to be reachable, and it stops after the
	// drain: a delete's final base backup needs WAL archived until the very end.
	if relay, stop := StartWALRelay(ctx, cfg, log, false); relay != nil {
		defer stop()
		o.ArchiveReady = func(ref string) {
			if err := relay.Ensure(ref); err != nil {
				log.Warn("wal relay: cannot serve project", "ref", ref, "error", err)
			}
		}
	}
	lo := LifecycleOptions(cfg, o)
	// Without a Fleet from the caller the Engine registers projects with Supavisor,
	// Realtime and Storage through a Lazy fleet (credentials loaded on first use, a
	// service this node never rendered skipped), so a project created through the API
	// reaches the shared services exactly as one created by `sbctl projects create`.
	var lz *fleet.Lazy
	if len(o.Fleet) == 0 {
		lz = fleet.NewLazy(fleet.Deps{Cfg: cfg, Log: log.With("component", "fleet")})
		lo.Fleet = lz.Fleet()
	}
	backups := memoizeBackups(&lo)
	node, err := openNode(ctx, cfg, lo, log)
	if err != nil {
		return err
	}
	defer node.Close() // after the drain below: Serve returns only when nothing runs any more
	if lz != nil {
		lz.Bind(node.Registry, node.Secrets)
	}

	// Create the shared postgres-meta passphrase now, so the unit that starts pg-meta
	// reads the same sealed secret the API uses (PGMetaCryptoKey).
	if _, err := PGMetaCryptoKey(ctx, cfg, node.Registry, node.Secrets); err != nil {
		return err
	}
	recovered := node.Engine.Recover(ctx)
	for _, r := range recovered {
		log.Warn("project recovered", "ref", r.Ref, "from", r.From, "to", r.To, "note", r.Note)
	}

	pg, ok := node.Registry.(*registry.Postgres)
	if !ok {
		return fmt.Errorf("serve: the registry is %T, want Postgres", node.Registry)
	}
	apiH, err := api.NewServer(api.Deps{
		Registry: node.Registry, Secrets: node.Secrets, Manager: node.Engine, Config: cfg,
		Store:  api.NewPGStore(pg.Pool()), // explicit: the API's state must survive restarts
		Logger: log.With("component", "api"),
	})
	if err != nil {
		return err
	}
	edge, err := proxy.New(proxy.Options{
		Config: cfg, Registry: node.Registry, Keys: node.Engine, APIHandler: apiH,
		Logger: log.With("component", "proxy"),
	})
	if err != nil {
		return err
	}

	adminLn, err := net.Listen("tcp", cfg.Listen.Admin)
	if err != nil {
		return fmt.Errorf("serve: admin listener %s: %w", cfg.Listen.Admin, err)
	}
	admin := &http.Server{
		Handler: apiH, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		log.Info("management API listening", "admin", adminLn.Addr().String())
		if err := admin.Serve(adminLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: admin listener: %w", err)
		}
		return nil
	})
	budget := o.StopBudget
	if budget <= 0 {
		budget = StopBudget
	}
	superviseStop(g, gctx, budget, edge.Run, apiH.Drain, admin.Shutdown, log)
	if cfg.Supervisor == config.SupervisorSystemd {
		// Next to the projects: a shared service that takes minutes to answer (Realtime and
		// Supavisor run migrations first) must not hold the projects back.
		g.Go(func() error { startFleet(gctx, node, log); return nil })
	}
	g.Go(func() error {
		startProjects(gctx, node, recovered, backups(node), log)
		return nil
	})
	log.Info("sbctl is up", "domain", cfg.BaseDomain(), "api", cfg.APIURL(), "dashboard", cfg.DashboardURL())
	err = g.Wait()
	log.Info("sbctl stopped")
	return err
}

// superviseStop runs the edge proxy and, once gctx ends (SIGTERM), drains the lifecycle
// operations the API detached from their requests (and the create goroutines): they must
// end before the registry closes, because a delete cut off after the data plane is gone,
// or a restart cut off after the pause, would need Recover at the next start. New
// mutations answer 503 from the first moment of the drain.
//
// The edge runs on its own context, cancelled only after the drain: project traffic
// (REST, Auth, Storage), api.<domain> and studio.<domain> keep answering while a long
// operation (a delete with its final base backup) finishes, so clients of that operation
// that came through api.<domain> still get their answer and a restart does not take the
// whole node's public edge down for the length of the drain. The loopback admin listener
// shuts down last, with the same budget.
func superviseStop(g *errgroup.Group, gctx context.Context, budget time.Duration,
	edgeRun func(context.Context) error, drain, shutdownAdmin func(context.Context) error, log *slog.Logger) {
	edgeCtx, stopEdge := context.WithCancel(context.WithoutCancel(gctx))
	g.Go(func() error {
		<-gctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		log.Info("stopping: waiting for running lifecycle operations; the proxy keeps serving", "budget", budget.String())
		if err := drain(sctx); err != nil {
			log.Warn("stopping with lifecycle operations unfinished; the next start recovers them", "error", err)
		}
		stopEdge()
		return shutdownAdmin(sctx)
	})
	g.Go(func() error {
		defer stopEdge()
		return edgeRun(edgeCtx)
	})
}

// startFleet starts the shared services (postgres-meta, Supavisor, Realtime, Storage,
// Studio) at boot. Their units are not enabled for boot, like every unit of the
// control plane's own: the daemon starts them, in order, once the registry is up, so a
// reboot brings the whole node back from one enabled unit (sbctl.service). Already
// running services whose files are unchanged are left alone. The installer renders and
// first starts them (`sbctl fleet start`); a service whose artifact was never fetched
// fails here and is logged, and the rest of the node still comes up.
func startFleet(ctx context.Context, n *lifecycle.Node, log *slog.Logger) {
	m, err := fleet.NewManager(fleet.Deps{Cfg: n.Cfg, Log: log.With("component", "fleet"), Registry: n.Registry,
		Secrets: n.Secrets, Supervisor: n.Supervisor, Artifacts: n.Artifacts})
	if err != nil {
		log.Error("fleet not started", "error", err)
		return
	}
	if err := m.Start(ctx); err != nil {
		log.Error("shared services did not all start", "error", err)
		return
	}
	log.Info("shared services started")
}

// startProjects brings the system project's backup timer and every active project up
// after a (re)start, one project at a time so that boot does not start a hundred
// Postgres clusters at once. It runs next to the listeners: the API answers while
// projects start, and a project that fails is marked ACTIVE_UNHEALTHY. Projects whose
// restart the previous process cut off after the pause are resumed first, and restored
// clones whose recovery outlasted the restore's wait are finished in the background.
func startProjects(ctx context.Context, n *lifecycle.Node, recovered []lifecycle.Recovered, bk *backup.Service, log *slog.Logger) {
	if n.Cfg.Supervisor == config.SupervisorSystemd {
		for _, unit := range []string{backup.BackupTimerInstance(config.SystemRef), backup.PruneTimerUnit} {
			if err := n.Supervisor.Start(ctx, unit); err != nil {
				log.Warn("backup timer did not start", "unit", unit, "error", err)
			}
		}
	}
	for ref, err := range n.Engine.ResumeRecovered(ctx, recovered) {
		log.Error("project did not resume after an interrupted restart", "ref", ref, "error", err)
	}
	errs := n.Engine.StartActive(ctx)
	for ref, err := range errs {
		log.Error("project did not start", "ref", ref, "error", err)
	}
	ps, err := n.Registry.ListProjects(ctx)
	if err != nil {
		log.Error("listing projects after start", "error", err)
		return
	}
	started := 0
	for _, p := range ps {
		if p.Ref != config.SystemRef && (p.Status == registry.StatusActiveHealthy || p.Status == registry.StatusActiveUnhealthy) {
			started++
		}
	}
	log.Info("projects started", "active", started, "failed", len(errs))
	if bk != nil {
		finishRestores(ctx, bk, log)
	}
}

// finishRestores runs until ctx ends: restored clones tagged restore.cleanup_pending
// (recovery outlasted RecoveryTimeout, the restore reported success) get their recovery
// settings cleared once their cluster has promoted, so a later start never replays the
// source project's archive. Nothing else would do it unless an operator ran
// `sbctl backups finish-restore`.
func finishRestores(ctx context.Context, bk *backup.Service, log *slog.Logger) {
	for {
		wait := 5 * time.Minute
		if len(bk.PendingRestores(ctx)) > 0 {
			for _, ref := range bk.FinishPendingRestores(ctx) {
				log.Info("restored project finished recovery", "ref", ref)
			}
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// openNode is lifecycle.Open with a bounded wait for the registry: systemd starts
// sbctl.service when sb-postgres@system has started, not when it accepts connections, and
// a daemon that gave up at once would depend on Restart= staying under the start limit.
func openNode(ctx context.Context, cfg *config.Config, lo lifecycle.OpenOptions, log *slog.Logger) (*lifecycle.Node, error) {
	deadline := time.Now().Add(registryWait)
	delay := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		node, err := lifecycle.Open(ctx, cfg, lo)
		if err == nil {
			return node, nil
		}
		remaining := time.Until(deadline)
		if !errors.Is(err, lifecycle.ErrRegistryUnreachable) || ctx.Err() != nil || remaining <= 0 {
			return nil, err
		}
		wait := min(delay, remaining)
		log.Warn("waiting for the registry", "attempt", attempt, "retry_in", wait.String(), "error", err)
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(wait):
		}
		if delay *= 2; delay > 10*time.Second {
			delay = 10 * time.Second
		}
	}
}

// memoizeBackups makes lo build its backup service once, and returns a getter for that
// service (nil when the backend is not configured or does not open) so that the daemon
// can run the restore sweep on the instance the Engine uses.
func memoizeBackups(lo *lifecycle.OpenOptions) func(*lifecycle.Node) *backup.Service {
	inner := lo.BackupFactory
	var mu sync.Mutex
	var svc *backup.Service
	build := func(n *lifecycle.Node) (lifecycle.BaseBackuper, error) {
		mu.Lock()
		defer mu.Unlock()
		if svc != nil {
			return svc, nil
		}
		b, err := inner(n)
		if err != nil {
			return nil, err
		}
		if s, ok := b.(*backup.Service); ok {
			svc = s
		}
		return b, nil
	}
	lo.BackupFactory = build
	return func(n *lifecycle.Node) *backup.Service {
		if _, err := build(n); err != nil {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if svc != nil && n.Engine != nil {
			svc.SetManager(n.Engine)
		}
		return svc
	}
}
