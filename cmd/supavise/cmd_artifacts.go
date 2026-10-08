package main

import (
	"errors"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/artifacts"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/nodeupgrade"
)

var artifactsCmd = &cobra.Command{
	Use:   "artifacts",
	Short: "Fetch and list the service artifacts pinned in versions.yaml",
}

var artifactsFetchStudio bool

var artifactsFetchCmd = &cobra.Command{
	Use:   "fetch [service...]",
	Short: "Download, verify and unpack artifacts",
	Long: `Downloads each artifact pinned in versions.yaml for this platform, verifies it
against the release SHA256SUMS and unpacks it under <state_dir>/artifacts. Without
arguments every pinned artifact is fetched. Services are named as in unit names
(postgres, gotrue, postgrest, supavisor, realtime, storage, pgmeta, imgproxy,
edge-runtime); the release names auth and pooler are accepted too. --studio also fetches
our Studio build from [studio] artifact_url and artifact_sha256.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		store, err := artifacts.New(cfg, artifacts.WithLogger(newLogger(cfg)))
		if err != nil {
			return err
		}
		svcs := make([]string, 0, len(args))
		for _, a := range args {
			svcs = append(svcs, serviceOf(a))
		}
		if len(svcs) == 0 {
			for name := range store.Versions().Artifacts {
				svcs = append(svcs, serviceOf(name))
			}
			sort.Strings(svcs)
		}
		if artifactsFetchStudio {
			svcs = append(svcs, config.SvcStudio)
		}
		for _, svc := range svcs {
			dir, err := store.Fetch(cmd.Context(), svc)
			if err != nil {
				return fmt.Errorf("%s: %w", svc, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-12s %s\n", svc, dir)
		}
		return nil
	},
}

var artifactsListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show pinned artifact versions and whether they are fetched",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		store, err := artifacts.New(cfg)
		if err != nil {
			return err
		}
		var svcs []string
		for name := range store.Versions().Artifacts {
			svcs = append(svcs, serviceOf(name))
		}
		sort.Strings(svcs)
		t := newTable(cmd.OutOrStdout())
		fmt.Fprintf(t, "SERVICE\tTAG\tPLATFORM\tFETCHED\n")
		for _, svc := range svcs {
			tag, _ := store.Tag(svc)
			state := "no"
			if dir, err := store.Dir(svc); err == nil {
				state = dir
			} else if !errors.Is(err, artifacts.ErrNotFetched) {
				state = err.Error()
			}
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", svc, tag, store.Platform(), state)
		}
		return t.Flush()
	},
}

var (
	artifactsGCDry  bool
	artifactsGCKeep int
)

var artifactsGCCmd = projectCmd("gc", "Remove artifacts that no project, pin or kept release uses", cobra.NoArgs,
	func(cmd *cobra.Command, n *lifecycle.Node, _ []string) error {
		keep := artifactsGCKeep
		if keep <= 0 {
			keep = n.Cfg.Upgrade.Keep()
		}
		gone, err := n.Engine.CollectArtifacts(cmd.Context(), keep, artifactsGCDry, keptReleasePins(n.Cfg, keep)...)
		verb := "removed"
		if artifactsGCDry {
			verb = "would remove"
		}
		for _, u := range gone {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s\n", verb, u.Name, u.Tag)
		}
		if len(gone) == 0 && err == nil {
			fmt.Fprintln(cmd.OutOrStdout(), "nothing to remove")
		}
		return err
	})

// keptReleasePins are the pins of the newest keep binaries that `supavise rollback` can go back to
// (supavise upgrade keeps them in the releases directory, which is readable by everyone and
// writable by root): their artifacts stay, whatever the daemon recorded in its history.
func keptReleasePins(cfg *config.Config, keep int) []map[string]string {
	recs, err := nodeupgrade.Releases{Dir: releasesDir(cfg.BinPath)}.List()
	if err != nil {
		return nil
	}
	var out []map[string]string
	for _, r := range recs { // newest first
		if len(out) >= keep {
			break
		}
		if !r.Withdrawn {
			out = append(out, r.Pins)
		}
	}
	return out
}

// serviceOf maps a release name (auth, pooler) to the service name used in unit names.
func serviceOf(name string) string {
	switch name {
	case "auth":
		return config.SvcGoTrue
	case "pooler":
		return config.SvcSupavisor
	}
	return name
}

func init() {
	artifactsFetchCmd.Flags().BoolVar(&artifactsFetchStudio, "studio", false, "also fetch our Studio build")
	artifactsGCCmd.Long = `Removes the unpacked artifacts that nothing needs: not the versions this binary pins,
not the versions any project runs (or is being upgraded to), and not those of the last
[upgrade] keep_releases releases this node ran (default 3: the current one and the two before, so a
rollback finds its artifacts). The archive cache is left alone. Run as the user that owns the
state directory (supavise).`
	artifactsGCCmd.Flags().BoolVar(&artifactsGCDry, "dry-run", false, "list what would be removed")
	artifactsGCCmd.Flags().IntVar(&artifactsGCKeep, "keep", 0, "releases to keep (default: [upgrade] keep_releases)")
	artifactsCmd.AddCommand(artifactsFetchCmd, artifactsListCmd, artifactsGCCmd)
	rootCmd.AddCommand(artifactsCmd)
}
