package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/app"
	"github.com/supavise/supavise/internal/artifacts"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/health"
	"github.com/supavise/supavise/internal/infra"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/notice"
	"github.com/supavise/supavise/internal/units"
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

A failure to check at all (no readable config, a user who may not read the node's files) prints
an error and also exits 1, as every supavise command does. A script that must tell "degraded"
from "could not check" reads the "status" field of --json: when there is no report, there is
no verdict.

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
	deps, closeNode, openErr := statusDeps(ctx, cfg)
	defer closeNode()
	if errors.Is(openErr, fs.ErrPermission) {
		// Not a verdict about the node: this user cannot read its files.
		return 0, fmt.Errorf("%w\nrun it as the user that owns the state directory: sudo -u supavise supavise status", openErr)
	}
	rep, err := health.CheckNode(ctx, deps)
	if err != nil {
		return 0, err
	}
	sections := collectStatusSections(ctx, cfg)
	if asJSON {
		if err := printJSON(w, statusJSON{Report: rep, statusSections: sections}); err != nil {
			return 0, err
		}
	} else {
		health.Render(w, rep, verbose)
		sections.render(w)
	}
	return rep.Verdict.ExitCode(), nil
}

// hostBlockOf reads the host block of the report; the tests of the other blocks replace it.
var hostBlockOf = hostStatusBlock

// statusSections are the blocks added below the node report: the host layer when a step of
// `supavise system converge` has work or could not be checked, and, for a cluster, the nodes and replicas,
// the failover readiness and the infrastructure the release needs. Each is nil on a node that has
// nothing to say, which is every node that is not part of a cluster, converged and not on a stack that
// lags, so for those the output is the node report alone. They do not change the verdict.
type statusSections struct {
	Host           *hostBlock          `json:"host,omitempty"`
	Cluster        *clusterBlock       `json:"cluster,omitempty"`
	Failover       *failover.Readiness `json:"failover,omitempty"`
	Infrastructure *infra.Report       `json:"infrastructure,omitempty"`
	// Studio is set while the dashboard runs a build other than the one this binary pins.
	Studio *studioBlock `json:"studio,omitempty"`
	// Errors are the blocks that could not be read, by name.
	Errors map[string]string `json:"errors,omitempty"`
}

// statusJSON is the report with the sections beside it.
type statusJSON struct {
	*health.Report
	statusSections
}

// collectStatusSections reads each block. A block that cannot be read is named in Errors and
// does not stop the others.
func collectStatusSections(ctx context.Context, cfg *config.Config) statusSections {
	var s statusSections
	fail := func(name string, err error) {
		if s.Errors == nil {
			s.Errors = map[string]string{}
		}
		s.Errors[name] = err.Error()
	}
	var err error
	if s.Host, err = hostBlockOf(ctx, cfg); err != nil {
		fail("host", err)
	}
	if s.Cluster, err = clusterStatus(ctx, cfg); err != nil {
		fail("cluster", err)
	}
	if s.Failover, err = failoverStatus(ctx, cfg); err != nil {
		fail("failover", err)
	}
	if s.Infrastructure, err = infraStatus(ctx, cfg); err != nil {
		fail("infrastructure", err)
	}
	s.Studio = studioStatus(cfg, time.Now())
	return s
}

// studioBlock says that Studio runs a build other than the one the installed binary pins: the
// binary came with `supavise self-update` and its converge could not fetch the build, or the daemon
// has not restarted since. Nothing else says so: Studio answers on the build it runs.
type studioBlock struct {
	Runs   string `json:"runs"`
	Pinned string `json:"pinned"`
}

func (b *studioBlock) render(w io.Writer) {
	fmt.Fprintf(w, "studio  runs the build %s and this release pins %s.\n", b.Runs, b.Pinned)
	fmt.Fprintln(w, "        Run `sudo supavise upgrade`: it fetches the build and restarts Studio on it (the projects keep running).")
}

// studioStatus is the Studio block of a node whose Studio unit runs a build other than the pinned
// one, outside an upgrade (which moves it), and nil otherwise, a node without a dashboard included.
func studioStatus(cfg *config.Config, now time.Time) *studioBlock {
	if cfg.Studio.ArtifactURL == "" {
		return nil
	}
	if _, running := notice.UpgradeRunning(cfg.Paths(), now); running {
		return nil
	}
	runs, err := fleet.RenderedTag(cfg, config.SvcStudio)
	if err != nil {
		return nil
	}
	v, err := artifacts.LoadVersions(cfg)
	if err != nil {
		return nil
	}
	pinned, err := v.Tag(config.SvcStudio)
	if err != nil || pinned == runs {
		return nil
	}
	return &studioBlock{Runs: runs, Pinned: pinned}
}

// render writes the blocks, each after a blank line.
func (s statusSections) render(w io.Writer) {
	if s.Host != nil {
		fmt.Fprintln(w)
		s.Host.Render(w)
	}
	if s.Cluster != nil {
		fmt.Fprintln(w)
		s.Cluster.render(w)
	}
	if s.Failover != nil {
		fmt.Fprintln(w)
		s.Failover.Render(w)
	}
	if s.Infrastructure != nil && s.Infrastructure.Behind() {
		fmt.Fprintln(w)
		s.Infrastructure.Render(w)
	}
	if s.Studio != nil {
		fmt.Fprintln(w)
		s.Studio.render(w)
	}
	for _, name := range []string{"host", "cluster", "failover", "infrastructure"} {
		if msg, ok := s.Errors[name]; ok {
			fmt.Fprintf(w, "\n%s  could not be read: %s\n", name, msg)
		}
	}
}

// statusDeps opens the node for the checks and returns the function that closes it. A node
// that cannot be opened (the system cluster is down, the master key is missing) is still
// checked for everything that does not need the registry, and the report says why the rest was
// skipped; the error is returned too, so the caller can tell a user who may not read the node's
// files from a node that is down.
func statusDeps(ctx context.Context, cfg *config.Config) (health.Deps, func(), error) {
	log := newLogger(cfg)
	node, err := openLifecycle(ctx, cfg, openOptions(cfg))
	if err != nil {
		return downDeps(cfg, log, err), func() {}, err
	}
	lz := fleet.NewLazy(fleet.Deps{Cfg: cfg, Log: log.With("component", "fleet")})
	lz.Bind(node.Registry, node.Secrets)
	// The projects homed on other nodes have no units here; their nodes report them.
	deps, err := health.ForNode(app.NodeAtHome(node), health.NodeOptions{Version: version, Log: log, Tenants: lz.Fleet()})
	if err != nil {
		node.Close()
		return downDeps(cfg, log, err), func() {}, err
	}
	return deps, node.Close, nil
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
