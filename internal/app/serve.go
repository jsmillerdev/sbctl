package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/OWNER/sbctl/internal/api"
	"github.com/OWNER/sbctl/internal/backup"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/proxy"
	"github.com/OWNER/sbctl/internal/registry"
)

// shutdownGrace bounds how long Serve waits for in-flight admin requests at shutdown.
const shutdownGrace = 15 * time.Second

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
	node, err := lifecycle.Open(ctx, cfg, LifecycleOptions(cfg, o))
	if err != nil {
		return err
	}
	defer node.Close()

	// Create the shared postgres-meta passphrase now, so the unit that starts pg-meta
	// reads the same sealed secret the API uses (PGMetaCryptoKey).
	if _, err := PGMetaCryptoKey(ctx, cfg, node.Registry, node.Secrets); err != nil {
		return err
	}
	for _, r := range node.Engine.Recover(ctx) {
		log.Warn("project recovered", "ref", r.Ref, "from", r.From, "to", r.To)
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
	g.Go(func() error {
		<-gctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return admin.Shutdown(sctx)
	})
	g.Go(func() error { return edge.Run(gctx) })
	g.Go(func() error {
		startProjects(gctx, node, log)
		return nil
	})
	log.Info("sbctl is up", "domain", cfg.BaseDomain(), "api", cfg.APIURL(), "dashboard", cfg.DashboardURL())
	err = g.Wait()
	log.Info("sbctl stopped")
	return err
}

// startProjects brings the system project's backup timer and every active project up
// after a (re)start, one project at a time so that boot does not start a hundred
// Postgres clusters at once. It runs next to the listeners: the API answers while
// projects start, and a project that fails is marked ACTIVE_UNHEALTHY.
func startProjects(ctx context.Context, n *lifecycle.Node, log *slog.Logger) {
	if n.Cfg.Supervisor == config.SupervisorSystemd {
		for _, unit := range []string{backup.BackupTimerInstance(config.SystemRef), backup.PruneTimerUnit} {
			if err := n.Supervisor.Start(ctx, unit); err != nil {
				log.Warn("backup timer did not start", "unit", unit, "error", err)
			}
		}
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
}
