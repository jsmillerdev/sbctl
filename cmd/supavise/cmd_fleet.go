package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/units"
)

var fleetCmd = &cobra.Command{
	Use:   "fleet",
	Short: "Run the shared services (Supavisor, Realtime, Storage, postgres-meta, Studio) and register projects with them",
	Long: `The shared services run once for every project: supavise-supavisor (the Postgres pooler),
supavise-realtime, supavise-storage, supavise-pgmeta and supavise-studio, as units of the configured supervisor.
They keep their metadata in the system cluster (supavise system init) and learn about a
project through a tenant call that the project lifecycle makes when it creates, re-keys or
deletes the project. These commands start and stop the services and repeat the tenant
calls by hand. Run them as the user that owns the state directory (supavise).`,
}

var (
	fleetNoFetch bool
	fleetSkip    []string
	fleetAll     bool
)

func init() {
	start := &cobra.Command{
		Use:   "start",
		Short: "Render and start the shared services and wait until each answers",
		Long: `Generates the services' secrets on first use (sealed in the registry, system
project), renders their units and starts them in order: pgmeta, supavisor, realtime,
storage, studio. Idempotent: a running service whose files are unchanged is left alone.
A service that cannot start does not stop the others; the command then exits non-zero.
Missing artifacts are fetched first (--no-fetch to skip). Studio is skipped with a note
when no artifact is installed and [studio] artifact_url is not set.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			n, err := openLifecycle(cmd.Context(), cfg, openOptions(cfg))
			if err != nil {
				return err
			}
			defer n.Close()
			skip := fleetSkip
			if !fleetNoFetch {
				if s, ok := n.Artifacts.(interface {
					Fetch(context.Context, string) (string, error)
				}); ok {
					for _, svc := range fleet.ServicesFor(cfg) {
						if contains(skip, svc) || (svc == config.SvcStudio && cfg.Studio.ArtifactURL == "") {
							continue
						}
						if _, err := s.Fetch(cmd.Context(), svc); err != nil {
							return fmt.Errorf("fetch %s: %w", svc, err)
						}
					}
				}
			}
			if _, err := n.Artifacts.Dir(config.SvcStudio); err != nil && !contains(skip, config.SvcStudio) && cfg.Studio.ArtifactURL == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "skipping studio: no artifact installed and [studio] artifact_url is not set")
				skip = append(skip[:len(skip):len(skip)], config.SvcStudio)
			}
			m, err := fleet.NewManager(fleet.Deps{Cfg: cfg, Log: newLogger(cfg), Registry: n.Registry, Secrets: n.Secrets,
				Supervisor: n.Supervisor, Artifacts: n.Artifacts, Skip: skip})
			if err != nil {
				return err
			}
			startErr := m.Start(cmd.Context())
			fleetTable(cmd.OutOrStdout(), m.Status(cmd.Context()))
			return startErr
		},
	}
	start.Flags().BoolVar(&fleetNoFetch, "no-fetch", false, "do not download artifacts; use what is in place")
	start.Flags().StringSliceVar(&fleetSkip, "skip", nil, "services to leave alone (pgmeta, supavisor, realtime, storage, studio)")

	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop the shared services (their data stays)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, closeFn, err := statelessManager()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := m.Stop(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "stopped")
			return nil
		},
	}
	stop.Flags().StringSliceVar(&fleetSkip, "skip", nil, "services to leave alone")

	status := &cobra.Command{
		Use:   "status",
		Short: "Check the shared services",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, closeFn, err := statelessManager()
			if err != nil {
				return err
			}
			defer closeFn()
			hs := m.Status(cmd.Context())
			fleetTable(cmd.OutOrStdout(), hs)
			if !fleet.AllHealthy(hs) {
				return errors.New("the shared services are not all healthy")
			}
			return nil
		},
	}
	status.Flags().StringSliceVar(&fleetSkip, "skip", nil, "services to leave out")

	ensure := &cobra.Command{
		Use:   "ensure-tenant <ref>...",
		Short: "Register projects with Supavisor, Realtime and Storage (idempotent)",
		Long: `Repeats the tenant calls the lifecycle makes when it creates a project: Supavisor gets
the tenant and its manager login (the pgbouncer role of the project's database gets a
password), Realtime and Storage get the tenant with the project's keys. A tenant the
services already hold with the same configuration is left alone. The project's units and
the shared services must be running. --all does every active project.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if fleetAll {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return withFleet(cmd, func(ctx context.Context, d fleet.Deps, f fleet.Fleet, reg registry.Registry) error {
				refs := args
				if fleetAll {
					ps, err := reg.ListProjects(ctx)
					if err != nil {
						return err
					}
					for _, p := range ps {
						if p.Ref != config.SystemRef && (p.Status == registry.StatusActiveHealthy || p.Status == registry.StatusActiveUnhealthy) {
							refs = append(refs, p.Ref)
						}
					}
				}
				var failed []string
				for _, ref := range refs {
					spec, err := fleet.LoadTenantSpec(ctx, d, ref)
					if err == nil {
						// What nobody saved is the project's size's pool and client limit, as in the lifecycle.
						if p, perr := reg.GetProject(ctx, ref); perr == nil {
							lifecycle.ApplyPoolDefaults(d.Cfg, &spec, p)
						}
						err = f.EnsureTenant(ctx, spec)
					}
					if err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", ref, err)
						failed = append(failed, ref)
						continue
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s: tenant ready (%s)\n", ref, serviceNames(f))
				}
				if len(failed) > 0 {
					return fmt.Errorf("%d of %d tenant(s) failed: %s", len(failed), len(refs), strings.Join(failed, ", "))
				}
				return nil
			})
		},
	}
	ensure.Flags().BoolVar(&fleetAll, "all", false, "every active project")

	remove := &cobra.Command{
		Use:   "remove-tenant <ref>...",
		Short: "Remove projects from Supavisor, Realtime and Storage",
		Long: `Deletes the tenant records (the project itself is untouched; Storage keeps the objects
under <ref>/). Tenants the services do not hold count as removed.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withFleet(cmd, func(ctx context.Context, _ fleet.Deps, f fleet.Fleet, _ registry.Registry) error {
				var failed []string
				for _, ref := range args {
					if err := f.RemoveTenant(ctx, ref); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", ref, err)
						failed = append(failed, ref)
						continue
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s: tenant removed\n", ref)
				}
				if len(failed) > 0 {
					return fmt.Errorf("%d tenant(s) failed: %s", len(failed), strings.Join(failed, ", "))
				}
				return nil
			})
		},
	}

	fleetCmd.AddCommand(start, stop, status, ensure, remove)
	rootCmd.AddCommand(fleetCmd)
}

// withFleet opens the node and builds the tenant fleet from it.
func withFleet(cmd *cobra.Command, run func(ctx context.Context, d fleet.Deps, f fleet.Fleet, reg registry.Registry) error) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	n, err := openLifecycle(cmd.Context(), cfg, openOptions(cfg))
	if err != nil {
		return err
	}
	defer n.Close()
	d := fleet.Deps{Cfg: cfg, Log: newLogger(cfg), Registry: n.Registry, Secrets: n.Secrets, Supervisor: n.Supervisor, Artifacts: n.Artifacts}
	f, err := fleet.Setup(cmd.Context(), d)
	if err != nil {
		return err
	}
	return run(cmd.Context(), d, f, n.Registry)
}

// statelessManager builds a Manager that needs neither the registry nor the artifacts:
// stopping and checking units only takes their names.
func statelessManager() (*fleet.Manager, func(), error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	sup, err := units.New(cfg, newLogger(cfg))
	if err != nil {
		return nil, nil, err
	}
	closeFn := func() {
		if c, ok := sup.(interface{ Close() }); ok {
			c.Close()
		}
	}
	m, err := fleet.NewManager(fleet.Deps{Cfg: cfg, Log: newLogger(cfg), Supervisor: sup, Skip: fleetSkip})
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	return m, closeFn, nil
}

func fleetTable(w io.Writer, hs []fleet.Health) {
	t := newTable(w)
	fmt.Fprintln(t, "SERVICE\tUNIT\tSTATUS\tDETAIL")
	for _, h := range hs {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", h.Service, h.Unit, h.Status, h.Error)
	}
	t.Flush()
}

func serviceNames(f fleet.Fleet) string {
	var s []string
	for _, t := range f {
		s = append(s, t.Service())
	}
	return strings.Join(s, ", ")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
