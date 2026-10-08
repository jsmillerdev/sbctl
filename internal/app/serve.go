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

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/diskquota"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/functions"
	"github.com/supavise/supavise/internal/health"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/notice"
	"github.com/supavise/supavise/internal/proxy"
	"github.com/supavise/supavise/internal/registry"
)

// StopBudget is how long Serve, once told to stop, waits for lifecycle operations that
// are still running (a delete with its final backup, a restart, a create) before it
// closes the registry. supavise.service's TimeoutStopSec must be longer (it is 11 minutes);
// an operation cut off anyway is finished or reverted by Engine.Recover at the next start.
const StopBudget = 10 * time.Minute

// registryWait bounds how long Serve waits for the system cluster's registry at boot.
// systemd orders supavise.service after the start of supavise-postgres@system, not its readiness.
// A variable so that tests can shorten it.
var registryWait = 2 * time.Minute

// Serve runs the daemon until ctx ends (SIGTERM in production): it opens the node
// (registry in the system cluster, secrets, engine with the backup service), finishes
// operations a crash interrupted, serves the Management API on the loopback admin
// listener and, through the proxy, at api.<domain>, runs the edge proxy, and starts
// every project that should be running. The system project's units (supavise-postgres@system,
// supavise-gotrue@system) must already exist: `supavise system init` creates them and systemd
// starts them at boot, which supavise.service waits for.
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
	// Alerts: what the rest of the node raises through alerts.Notify, and the project upgrades'
	// events, which the Engine hands to a hook because it cannot import the alerting package. Both
	// come before the node opens, so the upgrades that Recover closes are told too.
	notifier := alerts.New(cfg, alerts.Options{Log: log.With("component", "alerts")})
	alerts.SetDefault(notifier)
	upgradeAlerts := newUpgradeAlerter(notifier, log.With("component", "alerts"))
	defer upgradeAlerts.wait()
	lo.UpgradeNotify = upgradeAlerts.notify
	// Without a Fleet from the caller the Engine registers projects with Supavisor,
	// Realtime and Storage through a Lazy fleet (credentials loaded on first use, a
	// service this node never rendered skipped), so a project created through the API
	// reaches the shared services exactly as one created by `supavise projects create`.
	bindFleet := o.BindFleet
	if len(o.Fleet) == 0 {
		lz := fleet.NewLazy(fleet.Deps{Cfg: cfg, Log: log.With("component", "fleet")})
		lo.Fleet, bindFleet = lz.Fleet(), lz.Bind
	}
	backups := memoizeBackups(&lo)
	node, err := openNode(ctx, cfg, lo, log)
	if err != nil {
		return err
	}
	defer node.Close() // after the drain below: Serve returns only when nothing runs any more
	if bindFleet != nil {
		bindFleet(node.Registry, node.Secrets)
	}

	// Create the shared postgres-meta passphrase now, so the unit that starts pg-meta
	// reads the same sealed secret the API uses (PGMetaCryptoKey).
	if _, err := PGMetaCryptoKey(ctx, cfg, node.Registry, node.Secrets); err != nil {
		return err
	}
	// The artifact versions this release pins join the node's release history, which
	// `supavise artifacts gc` reads to keep the artifacts of the previous release.
	if rs, ok := node.Artifacts.(interface{ RecordPins() (bool, error) }); ok {
		if added, err := rs.RecordPins(); err != nil {
			log.Warn("could not record the release's artifact pins", "error", err)
		} else if added {
			log.Info("release's artifact pins recorded")
		}
	}
	recovered := node.Engine.Recover(ctx)
	for _, r := range recovered {
		log.Warn("project recovered", "ref", r.Ref, "from", r.From, "to", r.To, "note", r.Note)
	}

	pg, ok := node.Registry.(*registry.Postgres)
	if !ok {
		return fmt.Errorf("serve: the registry is %T, want Postgres", node.Registry)
	}
	// Branches: the Management API's branch endpoints and the expiry sweeper (internal/branching).
	store := api.NewPGStore(pg.Pool()) // explicit: the API's state must survive restarts
	bsvc, err := branching.New(branching.Deps{
		Cfg: cfg, Registry: node.Registry, Secrets: node.Secrets, Engine: node.Engine,
		Backup: backups(node), Functions: api.FunctionDigests(store), Log: log.With("component", "branching"),
	})
	if err != nil {
		return err
	}
	// Studio shows "Continue with SSO" only while the dashboard has an SSO provider, and reads
	// that when it starts: the API re-renders its unit when the first provider appears or the
	// last one goes (fleet.Manager.RefreshStudio).
	fm, err := fleet.NewManager(fleet.Deps{Cfg: cfg, Log: log.With("component", "fleet"), Registry: node.Registry,
		Secrets: node.Secrets, Supervisor: node.Supervisor, Artifacts: node.Artifacts})
	if err != nil {
		return err
	}
	// The node's health: one probe shared by /healthz, the operator's detailed view and the
	// alert checker below (internal/health, internal/alerts).
	hz := fleet.NewLazy(fleet.Deps{Cfg: cfg, Log: log.With("component", "fleet")})
	hz.Bind(node.Registry, node.Secrets)
	hdeps, err := health.ForNode(node, health.NodeOptions{Version: o.Version, Log: log.With("component", "health"), InDaemon: true, Tenants: hz.Fleet()})
	if err != nil {
		return err
	}
	monitor := health.NewMonitor(func(ctx context.Context) (*health.Report, error) { return health.CheckNode(ctx, hdeps) }, cfg.Health.Cache())
	checker := &alerts.Checker{Notifier: notifier, Report: monitor.Fresh, Cfg: cfg, Log: log.With("component", "alerts")}
	if us := health.ReadUpdateSettings(o.ConfigPath); !us.Off {
		checker.UpdateInterval = us.CheckInterval
		checker.CheckUpdate = func(ctx context.Context, now time.Time) (*health.UpdateRecord, error) {
			return health.CheckUpdate(ctx, cfg, o.Version, health.UpdateSource{}, now)
		}
	}
	apiDeps := api.Deps{
		Health:   monitor,
		Registry: node.Registry, Secrets: node.Secrets, Manager: node.Engine, Config: cfg, Branching: bsvc,
		Store:         store,
		Settings:      node.Settings, // the settings the engine renders units and tenants from
		StudioRefresh: fm.RefreshStudio,
		Logger:        log.With("component", "api"),
		// Per-project disk sizes: the supervisor starts the root unit that sets an XFS quota.
		Disk: diskquota.New(cfg, node.Supervisor),
	}
	if bs := backups(node); bs != nil { // a nil *backup.Service in the interface would not be nil
		apiDeps.Backups = bs
	}
	// Edge Functions: the syncer turns the stored deployments and secrets into the files the
	// runtime (supavise-edge-runtime, started with the other shared services) serves, and bundles
	// the sources that `supabase functions deploy --use-api` uploads.
	var fnSyncer *functions.Syncer
	if cfg.Functions.Enabled {
		fnSyncer, err = functions.New(functions.Deps{
			Cfg: cfg, Registry: node.Registry, Secrets: node.Secrets, Store: store, Keys: node.Engine.Keys,
			Log: log.With("component", "functions"), Supervisor: node.Supervisor, Artifacts: node.Artifacts,
		})
		if err != nil {
			return err
		}
		apiDeps.Functions = fnSyncer
	}
	apiH, err := api.NewServer(apiDeps)
	if err != nil {
		return err
	}
	// `supavise functions dev --token-file` grants a stand-in Owner seat and removes it when it
	// exits; one that was killed leaves the seat behind.
	if m, t, err := api.SweepStandIn(ctx, node.Registry, apiH.Members()); err != nil {
		log.Warn("could not sweep the stand-in seats of `functions dev`", "error", err)
	} else if m+t > 0 {
		log.Warn("removed the stand-in seats and tokens that `functions dev` left behind", "memberships", m, "tokens", t)
	}
	edge, err := proxy.New(proxy.Options{
		Config: cfg, Registry: node.Registry, Keys: node.Engine, APIHandler: apiH,
		FunctionsEnabled: cfg.Functions.Enabled,
		Logger:           log.With("component", "proxy"),
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
	drain := func(ctx context.Context) error {
		err := apiH.Drain(ctx)
		bsvc.Drain(ctx) // branch operations the API detached; cancelled when the budget ends
		return err
	}
	superviseStop(g, gctx, budget, edge.Run, drain, admin.Shutdown, log)
	fleetUp := make(chan bool, 1)
	projectsUp := make(chan struct{})
	if cfg.Supervisor == config.SupervisorSystemd {
		// Next to the projects: a shared service that takes minutes to answer (Realtime and
		// Supavisor run migrations first) must not hold the projects back.
		g.Go(func() error { fleetUp <- startFleet(gctx, node, log); return nil })
		// Once both are up, every project is registered with the shared services again. It is a
		// no-op when nothing changed, and when a release moved Storage or Realtime it is what runs
		// the new release's migrations in each project's database (they need the database, so the
		// projects come first).
		g.Go(func() error {
			select {
			case ok := <-fleetUp:
				if !ok {
					return nil
				}
			case <-gctx.Done():
				return nil
			}
			select {
			case <-projectsUp:
			case <-gctx.Done():
				return nil
			}
			ensureTenants(gctx, node, log)
			return nil
		})
	}
	g.Go(func() error {
		_ = bsvc.Run(gctx) // expiry sweeper; returns when the daemon stops
		return nil
	})
	g.Go(func() error {
		checker.Run(gctx) // alerts on what the health report finds, and the daily update check
		return nil
	})
	if fnSyncer != nil {
		g.Go(func() error {
			fnSyncer.Run(gctx) // keeps the runtime's files in step with the registry; returns when the daemon stops
			return nil
		})
	}
	g.Go(func() error {
		startProjects(gctx, node, recovered, backups(node), log, func() { close(projectsUp) })
		return nil
	})
	g.Go(func() error { settleUpgrades(gctx, node, log); return nil })
	log.Info("Supavise is up", "domain", cfg.BaseDomain(), "api", cfg.APIURL(), "dashboard", cfg.DashboardURL())
	err = g.Wait()
	log.Info("Supavise stopped")
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
// reboot brings the whole node back from one enabled unit (supavise.service). Already
// running services whose files are unchanged are left alone, and one whose files changed
// (a release moved its pin) is restarted, one at a time, each waited for. The installer renders
// and first starts them (`supavise fleet start`); a service whose artifact was never fetched
// fails here and is logged, and the rest of the node still comes up. While a `supavise upgrade`
// runs, the first failure ends the roll instead, so that the upgrade, which watches the
// services, rolls back with the later ones still on the old release. It reports whether every
// service started.
func startFleet(ctx context.Context, n *lifecycle.Node, log *slog.Logger) bool {
	// fleet.Setup generates the services' sealed secrets on first use, renders and starts
	// the units in order, and waits for each. Its tenants are not used here: the Engine's
	// own Fleet (a Lazy over the same Setup) registers projects.
	_, upgrading := notice.UpgradeRunning(n.Cfg.Paths(), time.Now())
	_, err := fleet.Setup(ctx, fleet.Deps{Cfg: n.Cfg, Log: log.With("component", "fleet"), Registry: n.Registry,
		Secrets: n.Secrets, Supervisor: n.Supervisor, Artifacts: n.Artifacts, Start: true, HaltOnFailure: upgrading})
	if err != nil {
		log.Error("shared services did not all start", "error", err)
		return false
	}
	log.Info("shared services started")
	return true
}

// ensureTenants registers the active projects with the shared services again (see
// lifecycle.Engine.EnsureTenants). A failure is logged: `supavise status` reports a project
// whose tenant is missing, and the next change to the project registers it.
func ensureTenants(ctx context.Context, n *lifecycle.Node, log *slog.Logger) {
	errs := n.Engine.EnsureTenants(ctx)
	for ref, err := range errs {
		log.Warn("project not registered with the shared services", "ref", ref, "error", err)
	}
	log.Info("projects registered with the shared services", "failed", len(errs))
}

// startProjects brings the system project's backup timer and every active project up
// after a (re)start, one project at a time so that boot does not start a hundred
// Postgres clusters at once. It runs next to the listeners: the API answers while
// projects start, and a project that fails is marked ACTIVE_UNHEALTHY. Projects whose
// restart the previous process cut off after the pause are resumed first, and restored
// clones whose recovery outlasted the restore's wait are finished in the background.
func startProjects(ctx context.Context, n *lifecycle.Node, recovered []lifecycle.Recovered, bk *backup.Service, log *slog.Logger, ready func()) {
	if n.Cfg.Supervisor == config.SupervisorSystemd {
		for _, unit := range []string{backup.BackupTimerInstance(config.SystemRef), backup.PruneTimerUnit} {
			if err := n.Supervisor.Start(ctx, unit); err != nil {
				log.Warn("backup timer did not start", "unit", unit, "error", err)
			}
		}
	}
	if n.Cfg.Supervisor == config.SupervisorSystemd {
		refreshSystem(ctx, n, log)
	}
	for ref, err := range n.Engine.ResumeRecovered(ctx, recovered) {
		log.Error("project did not resume after an interrupted restart", "ref", ref, "error", err)
	}
	// While an upgrade moves the node forward, a project whose service files the new binary
	// rendered differently keeps running on the old ones; the upgrade's rollout restarts those
	// projects, canary first, and stops at the first failure.
	startCtx := ctx
	if u, running := notice.UpgradeRunning(n.Cfg.Paths(), time.Now()); running && u.Forward() && u.ProcessAlive() {
		startCtx = lifecycle.DeferRestarts(ctx)
		log.Info("an upgrade is running; project services whose files changed are restarted by its rollout")
	}
	errs := n.Engine.StartActive(startCtx)
	for ref, err := range errs {
		log.Error("project did not start", "ref", ref, "error", err)
	}
	// Every project has been tried: the daemon may register them with the shared services now.
	// (The restore sweep below runs until the daemon stops, so it cannot be waited for.)
	ready()
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

// settleUpgrades runs until ctx ends: a project left UPGRADING by an upgrade whose process died
// (the CLI's, since the daemon's own upgrades end with the daemon and Recover settles those)
// is started again on its recorded versions, and an upgrade record that never got its final
// status is closed. An upgrade whose process is alive is left alone.
func settleUpgrades(ctx context.Context, n *lifecycle.Node, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
		for _, r := range n.Engine.SettleUpgrades(ctx) {
			log.Warn("project recovered", "ref", r.Ref, "from", r.From, "to", r.To, "note", r.Note)
		}
	}
}

// refreshSystem re-renders the system cluster with the current settings. StartActive covers
// the user projects, but supavise-postgres@system is started by systemd at boot from the files an
// earlier run rendered: after an upgrade that changes how clusters archive (the relay, whose
// unit no longer reads the config) its run script would still hold the old archive_command and
// archiving would fail silently. Nothing is restarted when the files are unchanged.
func refreshSystem(ctx context.Context, n *lifecycle.Node, log *slog.Logger) {
	p, err := n.Registry.GetProject(ctx, config.SystemRef)
	if err != nil {
		log.Warn("system cluster not refreshed", "error", err)
		return
	}
	keys, err := n.Engine.Keys(ctx, config.SystemRef)
	if err != nil {
		log.Warn("system cluster not refreshed", "error", err)
		return
	}
	// The dashboard's sign-in service gets the same treatment: a node upgraded to a version that
	// turns on dashboard SSO (SAML key, the sign-up hook) or a changed [mail] section renders
	// new files, and the unit is restarted only when they differ. When the cluster itself restarts
	// the sign-in service goes down with it and is started again.
	var re *lifecycle.SystemRefreshError
	switch err := n.Plane.RefreshSystem(ctx, p, keys); {
	case err == nil:
	case errors.As(err, &re) && re.Auth:
		log.Warn("the dashboard's sign-in service is not refreshed; run `supavise system init` to apply its settings", "error", err)
	default:
		log.Warn("system cluster not refreshed", "error", err)
	}
}

// finishRestores runs until ctx ends: restored clones tagged restore.cleanup_pending
// (recovery outlasted RecoveryTimeout, the restore reported success) get their recovery
// settings cleared once their cluster has promoted, so a later start never replays the
// source project's archive. Nothing else would do it unless an operator ran
// `supavise backups finish-restore`.
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
// supavise.service when supavise-postgres@system has started, not when it accepts connections, and
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
