package main

import (
	"context"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/fleet"
	"github.com/jsmillerdev/supavise/internal/health"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/units"
)

func init() {
	var asJSON, verbose bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Check the whole node and print a one-line verdict",
		Long: `Checks the daemon, the edge, the system cluster, each shared service and every project
(PostgreSQL reachable, Auth and REST answering, the Realtime and Storage tenants present), the
age of each project's newest base backup, the free space on the state volume, the
certificates, the master key's copy in the backups and whether a release is available.

The first line is the verdict: healthy, degraded (the node serves, but something needs
attention) or down (the daemon, the edge or the system cluster is not running). Exit status 0
is healthy, 1 degraded, 2 down. --json prints the whole report.

Run it as the user that owns the state directory: sudo -u supavise supavise status.
The public /healthz of the API host carries the same verdict for uptime monitors.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			code, err := runStatus(cmd.Context(), cmd.OutOrStdout(), asJSON, verbose)
			if err != nil {
				return err
			}
			if code != 0 {
				// The verdict is the answer, not an error to print; deferred cleanup has run.
				os.Exit(code)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the full report as JSON")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "list every project, not only the ones that need attention")
	rootCmd.AddCommand(cmd)
}

// runStatus checks the node and writes the report; it returns the exit status for the verdict.
func runStatus(ctx context.Context, w io.Writer, asJSON, verbose bool) (int, error) {
	cfg, err := loadConfig()
	if err != nil {
		return 0, err
	}
	deps, closeNode := statusDeps(ctx, cfg)
	defer closeNode()
	rep, err := health.CheckNode(ctx, deps)
	if err != nil {
		return 0, err
	}
	if asJSON {
		if err := printJSON(w, rep); err != nil {
			return 0, err
		}
	} else {
		health.Render(w, rep, verbose)
	}
	return rep.Verdict.ExitCode(), nil
}

// statusDeps opens the node for the checks and returns the function that closes it. A node
// that cannot be opened (the system cluster is down, the master key is unreadable by this
// user) is still checked for everything that does not need the registry, and the report says
// why the rest was skipped.
func statusDeps(ctx context.Context, cfg *config.Config) (health.Deps, func()) {
	log := newLogger(cfg)
	node, err := lifecycle.Open(ctx, cfg, openOptions(cfg))
	if err != nil {
		return downDeps(cfg, log, err), func() {}
	}
	lz := fleet.NewLazy(fleet.Deps{Cfg: cfg, Log: log.With("component", "fleet")})
	lz.Bind(node.Registry, node.Secrets)
	deps, err := health.ForNode(node, health.NodeOptions{Version: version, Log: log, Tenants: lz.Fleet()})
	if err != nil {
		node.Close()
		return downDeps(cfg, log, err), func() {}
	}
	return deps, node.Close
}

func downDeps(cfg *config.Config, log *slog.Logger, cause error) health.Deps {
	system := func(ctx context.Context) []lifecycle.ServiceHealth {
		hs, err := lifecycle.SystemStatus(ctx, cfg, openOptions(cfg))
		if err != nil {
			return []lifecycle.ServiceHealth{{Name: config.SvcPostgres, Status: "UNHEALTHY", Error: err.Error()}}
		}
		return hs
	}
	var services func(context.Context) []fleet.Health
	if sup, err := units.New(cfg, log); err == nil {
		if m, err := fleet.NewManager(fleet.Deps{Cfg: cfg, Log: log, Supervisor: sup}); err == nil {
			services = m.Status
		}
	}
	return health.ForDown(cfg, version, log, cause, system, services)
}
