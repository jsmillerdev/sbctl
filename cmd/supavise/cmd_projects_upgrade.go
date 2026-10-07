package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jsmillerdev/supavise/internal/app"
	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/registry"
)

var (
	upAll, upYes, upDryRun, upNoGC bool
	upAllowOlder                   bool
	upTo                           []string
	versionsJSON                   bool
)

// upgradeRequest is what the flags ask for: --to names the releases to move to (the node's pins
// for the services it does not name), --allow-older lifts the refusal of an older one.
func upgradeRequest() (lifecycle.UpgradeRequest, error) {
	req := lifecycle.UpgradeRequest{AllowOlder: upAllowOlder}
	for _, kv := range upTo {
		svc, tag, ok := strings.Cut(kv, "=")
		svc = serviceOf(svc)
		if !ok || tag == "" || !slices.Contains(config.ProjectServices, svc) {
			return req, fmt.Errorf("--to %q: want <service>=<release tag> with the service one of %s", kv, strings.Join(config.ProjectServices, ", "))
		}
		if req.Target == nil {
			req.Target = map[string]string{}
		}
		req.Target[svc] = tag
	}
	return req, nil
}

// upgradeRow is what the CLI knows about one project's upgrade: its eligibility and why a
// project is skipped.
type upgradeRow struct {
	Project *registry.Project
	El      *lifecycle.UpgradeEligibility
}

func (r upgradeRow) state() string {
	switch {
	case r.El.Eligible:
		return "upgrade"
	case len(r.El.Ahead) > 0:
		return "ahead of the node; stays on its versions"
	case len(r.El.Blockers) > 0:
		return "skip: " + r.El.Blockers[0].Message
	}
	return "up to date"
}

// changesText is "gotrue v2.100.0-r1 -> v2.195.0-r1, postgrest ...".
func changesText(el *lifecycle.UpgradeEligibility) string {
	if len(el.Changes) == 0 || len(el.Ahead) > 0 {
		return "-"
	}
	parts := make([]string, 0, len(el.Changes))
	for _, c := range el.Changes {
		parts = append(parts, fmt.Sprintf("%s %s -> %s", c.Service, lifecycle.ShortVersion(c.Service, c.From), lifecycle.ShortVersion(c.Service, c.To)))
	}
	return strings.Join(parts, ", ")
}

// upgradeRows computes the eligibility of every user project (or of refs).
func upgradeRows(ctx context.Context, n *lifecycle.Node, refs []string, req lifecycle.UpgradeRequest) ([]upgradeRow, error) {
	ps, err := n.Registry.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, r := range refs {
		want[r] = true
	}
	var rows []upgradeRow
	for i := range ps {
		p := &ps[i]
		if p.Ref == config.SystemRef || (len(want) > 0 && !want[p.Ref]) {
			continue
		}
		delete(want, p.Ref)
		el, err := n.Engine.UpgradeEligibilityFor(ctx, p.Ref, req)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Ref, err)
		}
		rows = append(rows, upgradeRow{Project: p, El: el})
	}
	for r := range want {
		return nil, fmt.Errorf("project %s: %w", r, registry.ErrNotFound)
	}
	return rows, nil
}

func printPlan(w io.Writer, rows []upgradeRow) {
	t := newTable(w)
	fmt.Fprintln(t, "REF\tNAME\tSTATUS\tCHANGES\tRESULT")
	for _, r := range rows {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", r.Project.Ref, r.Project.Name, r.Project.Status, changesText(r.El), r.state())
	}
	t.Flush()
}

// confirm asks on a terminal, and refuses to guess when stdin is not one.
func confirm(cmd *cobra.Command, question string) error {
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return errors.New("nothing was changed; run it again with --yes to upgrade without being asked")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", question)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("nothing was changed")
	}
	return nil
}

// progressText is what each step of an upgrade is doing, for the lines the command prints.
var progressText = map[string]string{
	lifecycle.ProgressRequested:      "accepted",
	lifecycle.ProgressStarted:        "fetching the target artifacts",
	lifecycle.ProgressArtifactsReady: "taking the base backup (the project keeps serving)",
	lifecycle.ProgressBackupDone:     "backup done",
	lifecycle.ProgressStopping:       "stopping services",
	lifecycle.ProgressDatabase:       "starting PostgreSQL on its new release",
	lifecycle.ProgressServices:       "starting GoTrue and PostgREST",
	lifecycle.ProgressHealth:         "checking every service",
	lifecycle.ProgressCommit:         "recording the new versions",
	lifecycle.ProgressCompleted:      "done",
}

// lineWriter serializes the lines of concurrent upgrades.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, format, args...)
}

// upgradeProject upgrades ref to the node's versions and prints the steps as the registry's
// status for the upgrade moves through them.
func upgradeProject(ctx context.Context, n *lifecycle.Node, out *lineWriter, ref string, req lifecycle.UpgradeRequest) error {
	started := time.Now()
	run, err := n.Engine.BeginUpgrade(ctx, ref, req)
	if err != nil {
		out.printf("%s: refused: %v\n", ref, err)
		return err
	}
	id := run.Upgrade().TrackingID
	out.printf("%s: upgrading (tracking id %s)\n", ref, id)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		last := lifecycle.ProgressRequested
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Second):
			}
			if u, err := registry.Upgrades(n.Registry).LatestUpgrade(ctx, ref); err == nil && u.TrackingID == id && u.Progress != last {
				last = u.Progress
				if text := progressText[last]; text != "" && last != lifecycle.ProgressCompleted {
					out.printf("%s: %s\n", ref, text)
				}
			}
		}
	}()
	err = run.Run(ctx)
	close(stop)
	wg.Wait()
	u := run.Upgrade()
	if err != nil {
		out.printf("%s: FAILED after %s: %v\n", ref, time.Since(started).Round(time.Second), err)
		return err
	}
	out.printf("%s: upgraded in %s (pre-upgrade backup %d)\n", ref, time.Since(started).Round(time.Second), u.BackupID)
	return nil
}

func runUpgrade(cmd *cobra.Command, n *lifecycle.Node, args []string) error {
	ctx := cmd.Context()
	out := &lineWriter{w: cmd.OutOrStdout()}
	req, err := upgradeRequest()
	if err != nil {
		return err
	}
	rows, err := upgradeRows(ctx, n, args, req)
	if err != nil {
		return err
	}
	var todo []upgradeRow
	for _, r := range rows {
		if r.El.Eligible {
			todo = append(todo, r)
		}
	}
	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no projects")
		return nil
	}
	printPlan(cmd.OutOrStdout(), rows)
	if len(todo) == 0 {
		if len(args) == 1 && len(rows[0].El.Blockers) > 0 {
			return fmt.Errorf("%s cannot be upgraded: %s", args[0], rows[0].El.Blockers[0].Message)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "nothing to upgrade")
		return nil
	}
	notes := map[string]bool{}
	for _, r := range todo {
		for _, note := range r.El.Notes {
			notes[note] = true
		}
	}
	for note := range notes {
		fmt.Fprintf(cmd.OutOrStdout(), "note: %s\n", note)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "A base backup is taken first; if it fails nothing is stopped. If the new versions do not start, the previous ones are started again.")
	if upDryRun {
		return nil
	}
	if !upYes {
		if err := confirm(cmd, fmt.Sprintf("Upgrade %d project(s)?", len(todo))); err != nil {
			return err
		}
	}

	// An upgrade must outlive the terminal that started it: a dropped SSH session sends SIGHUP,
	// and a write to the closed terminal can raise SIGPIPE; either would end the process between
	// its steps and leave projects UPGRADING with services stopped. Ctrl-C and SIGTERM still
	// cancel it.
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	defer signal.Reset(syscall.SIGHUP, syscall.SIGPIPE)

	// The base backup waits for its WAL to be archived through the daemon's relay; serve the
	// sockets nobody answers while the daemon is down.
	var relayRefs []string
	if len(args) > 0 {
		relayRefs = args
	}
	_, stopRelay := app.StartWALRelay(ctx, n.Cfg, newLogger(n.Cfg), true, relayRefs...)
	defer stopRelay()

	// Smallest databases first: a canary that fails costs the least to put right. Each size is
	// measured once (it walks the project's directory), not once per comparison.
	size := make(map[string]int64, len(todo))
	for _, r := range todo {
		size[r.Project.Ref] = diskBytes(ctx, n, r.Project.Ref)
	}
	sort.SliceStable(todo, func(i, j int) bool { return size[todo[i].Project.Ref] < size[todo[j].Project.Ref] })
	refs := make([]string, len(todo))
	for i, r := range todo {
		refs[i] = r.Project.Ref
	}
	opts := lifecycle.RolloutOptions{Canary: 0, Batch: 1,
		Upgrade: func(ctx context.Context, ref string) error { return upgradeProject(ctx, n, out, ref, req) }}
	if upAll && len(refs) > 1 {
		opts.Canary, opts.Batch = n.Cfg.Upgrade.Canary(), n.Cfg.Upgrade.Batch()
		out.printf("rolling out to %d projects: %d canary project(s) first, then %d at a time; it stops at the first failure\n", len(refs), min(opts.Canary, len(refs)), opts.Batch)
	}
	res := lifecycle.Rollout(ctx, refs, opts)
	out.printf("%d upgraded, %d failed, %d not attempted\n", len(res.Upgraded), len(res.Failed), len(res.NotAttempted))
	if len(res.Upgraded) > 0 && !upNoGC {
		collectArtifacts(cmd, n)
	}
	if len(res.Failed) > 0 {
		f := res.Failed[0]
		if len(res.NotAttempted) > 0 {
			return fmt.Errorf("rollout halted: %s failed (%v); not attempted: %s", f.Ref, f.Err, strings.Join(res.NotAttempted, ", "))
		}
		return fmt.Errorf("%s failed: %w", f.Ref, f.Err)
	}
	if !res.OK() {
		return fmt.Errorf("interrupted; not attempted: %s", strings.Join(res.NotAttempted, ", "))
	}
	return nil
}

func diskBytes(ctx context.Context, n *lifecycle.Node, ref string) int64 {
	u, err := n.Plane.Usage(ctx, ref)
	if err != nil {
		return 0
	}
	return u.DiskBytes
}

// collectArtifacts removes the releases nothing runs any more. A failure only warns: the
// upgrade is done.
func collectArtifacts(cmd *cobra.Command, n *lifecycle.Node) {
	gone, err := n.Engine.CollectArtifacts(cmd.Context(), n.Cfg.Upgrade.Keep(), false)
	for _, u := range gone {
		fmt.Fprintf(cmd.OutOrStdout(), "removed the unused artifact %s %s\n", u.Name, u.Tag)
	}
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: removing unused artifacts: %v\n", err)
	}
}

type versionsView struct {
	Ref      string            `json:"ref"`
	Name     string            `json:"name"`
	Status   string            `json:"status"`
	Versions map[string]string `json:"versions"`
	Node     map[string]string `json:"node_versions"`
	Upgrade  struct {
		Available bool `json:"available"`
		UpToDate  bool `json:"up_to_date"`
		// Ahead: the project runs a newer release than the node pins; it is not upgraded.
		Ahead    bool     `json:"ahead_of_node"`
		Blockers []string `json:"blockers,omitempty"`
		Changes  []string `json:"changes,omitempty"`
	} `json:"upgrade"`
	Last *lastUpgradeView `json:"last_upgrade,omitempty"`
}

type lastUpgradeView struct {
	TrackingID string `json:"tracking_id"`
	Status     string `json:"status"`
	Progress   string `json:"progress"`
	Error      string `json:"error,omitempty"`
	Detail     string `json:"detail,omitempty"`
	BackupID   int64  `json:"backup_id,omitempty"`
	At         string `json:"at"`
}

func viewVersions(ctx context.Context, n *lifecycle.Node, r upgradeRow) versionsView {
	v := versionsView{Ref: r.Project.Ref, Name: r.Project.Name, Status: string(r.Project.Status), Versions: r.El.Current, Node: r.El.Latest}
	v.Upgrade.Available, v.Upgrade.UpToDate, v.Upgrade.Ahead = r.El.Eligible, r.El.UpToDate, len(r.El.Ahead) > 0
	for _, b := range r.El.Blockers {
		v.Upgrade.Blockers = append(v.Upgrade.Blockers, b.Message)
	}
	for _, c := range r.El.Changes {
		if v.Upgrade.Ahead {
			break
		}
		v.Upgrade.Changes = append(v.Upgrade.Changes, fmt.Sprintf("%s %s -> %s", c.Service, c.From, c.To))
	}
	if store := registry.Upgrades(n.Registry); store != nil {
		if u, err := store.LatestUpgrade(ctx, r.Project.Ref); err == nil {
			status := map[registry.UpgradeStatus]string{registry.UpgradeRunning: "running", registry.UpgradeDone: "succeeded", registry.UpgradeFailed: "failed"}[u.Status]
			v.Last = &lastUpgradeView{TrackingID: u.TrackingID, Status: status, Progress: u.Progress, Error: u.Error, Detail: u.Detail, BackupID: u.BackupID, At: u.LatestStatusAt.UTC().Format(time.RFC3339)}
		}
	}
	return v
}

func runVersions(cmd *cobra.Command, n *lifecycle.Node, args []string) error {
	ctx := cmd.Context()
	rows, err := upgradeRows(ctx, n, args, lifecycle.UpgradeRequest{})
	if err != nil {
		return err
	}
	views := make([]versionsView, 0, len(rows))
	for _, r := range rows {
		views = append(views, viewVersions(ctx, n, r))
	}
	if versionsJSON {
		if len(args) == 1 && len(views) == 1 {
			return printJSON(cmd.OutOrStdout(), views[0])
		}
		return printJSON(cmd.OutOrStdout(), views)
	}
	w := cmd.OutOrStdout()
	if len(args) == 1 && len(rows) == 1 {
		r, v := rows[0], views[0]
		fmt.Fprintf(w, "project:  %s (%s), %s\n\n", r.Project.Ref, r.Project.Name, r.Project.Status)
		t := newTable(w)
		fmt.Fprintln(t, "SERVICE\tRUNS\tNODE PIN\t")
		for _, svc := range config.ProjectServices {
			mark := ""
			if r.El.Current[svc] != r.El.Latest[svc] {
				mark = "differs"
			}
			for _, c := range r.El.Ahead {
				if c.Service == svc {
					mark = "ahead"
				}
			}
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", svc, lifecycle.ShortVersion(svc, r.El.Current[svc]), lifecycle.ShortVersion(svc, r.El.Latest[svc]), mark)
		}
		t.Flush()
		fmt.Fprintf(w, "\nupgrade:  %s\n", r.state())
		if r.El.Eligible {
			fmt.Fprintf(w, "          %s; offline about %d minutes\n", changesText(r.El), int(r.El.DowntimeHours*60+0.5))
			for _, note := range r.El.Notes {
				fmt.Fprintf(w, "          %s\n", note)
			}
			fmt.Fprintf(w, "          run: supavise projects upgrade %s\n", r.Project.Ref)
		}
		if l := v.Last; l != nil {
			fmt.Fprintf(w, "last upgrade: %s at %s (tracking id %s", l.Status, l.At, l.TrackingID)
			if l.BackupID != 0 {
				fmt.Fprintf(w, ", pre-upgrade backup %d", l.BackupID)
			}
			fmt.Fprintln(w, ")")
			if l.Detail != "" {
				fmt.Fprintf(w, "              %s\n", l.Detail)
			}
		}
		return nil
	}
	t := newTable(w)
	fmt.Fprintln(t, "REF\tNAME\tSTATUS\tPOSTGRES\tGOTRUE\tPOSTGREST\tUPGRADE")
	behind := 0
	for _, r := range rows {
		c := r.El.Current
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Project.Ref, r.Project.Name, r.Project.Status,
			lifecycle.ShortVersion(config.SvcPostgres, c[config.SvcPostgres]), lifecycle.ShortVersion(config.SvcGoTrue, c[config.SvcGoTrue]),
			lifecycle.ShortVersion(config.SvcPostgREST, c[config.SvcPostgREST]), map[bool]string{true: "available", false: r.state()}[r.El.Eligible])
		if r.El.Eligible {
			behind++
		}
	}
	t.Flush()
	if node, err := n.Engine.NodeVersions(); err == nil {
		fmt.Fprintf(w, "\nnode pins: postgres %s, gotrue %s, postgrest %s\n", lifecycle.ShortVersion(config.SvcPostgres, node[config.SvcPostgres]),
			lifecycle.ShortVersion(config.SvcGoTrue, node[config.SvcGoTrue]), lifecycle.ShortVersion(config.SvcPostgREST, node[config.SvcPostgREST]))
	}
	if behind > 0 {
		fmt.Fprintf(w, "%d project(s) can be upgraded: supavise projects upgrade <ref>, or --all\n", behind)
	}
	return nil
}

func init() {
	upgrade := projectCmd("upgrade [<ref>]", "Move a project (or --all) to the node's service versions", func(cmd *cobra.Command, a []string) error {
		if len(a) > 1 || upAll == (len(a) == 1) {
			return errors.New("name one project, or pass --all")
		}
		return nil
	}, func(cmd *cobra.Command, n *lifecycle.Node, a []string) error { return runUpgrade(cmd, n, a) })
	upgrade.Long = `A project runs the Postgres, GoTrue and PostgREST versions it was created or last upgraded
with. A Supavise release moves the node's pins; a project follows when it is upgraded, as on
hosted Supabase (Settings > General in Studio does the same for one project). Nothing else
changes: same data directory, same keys, same host.

Each upgrade takes a fresh base backup first (reason pre-upgrade) while the project serves, and
stops without touching anything if that fails. Then GoTrue and PostgREST restart on the new
releases (PostgreSQL too when its release changes) and every service is health checked. If the
new versions do not start, the previous ones are started again. GoTrue's database migrations run
when it starts and only go forward: the base backup is the way back for the data
(supavise backups restore).

When the PostgreSQL release changes, the extensions installed in the project's databases are
checked against the new release first; a project with an extension it cannot serve is skipped
with the extension named (run ALTER EXTENSION <name> UPDATE on it, then upgrade). The upgrade
never updates extensions itself. The command keeps running if the SSH session drops; a project
left UPGRADING by a killed upgrade is started again on its previous versions by the daemon.

--all upgrades every eligible project: the first [upgrade] canary_projects (default 1, the
smallest databases) one at a time, then [upgrade] batch_size (default 5) at a time. It stops at
the first failure and starts nobody after it. Paused and unhealthy projects are skipped and
listed. Afterwards the artifacts that nothing runs or keeps for a rollback are removed
([upgrade] keep_releases; --no-gc keeps them).

Without --yes the plan is shown and you are asked (a terminal is needed); --dry-run shows it
and stops. Run as the user that owns the state directory (supavise).`
	upgrade.Flags().BoolVar(&upAll, "all", false, "upgrade every eligible project, canary first")
	upgrade.Flags().BoolVar(&upYes, "yes", false, "do not ask for confirmation")
	upgrade.Flags().BoolVar(&upDryRun, "dry-run", false, "show what would be upgraded and stop")
	upgrade.Flags().BoolVar(&upNoGC, "no-gc", false, "keep the artifacts that nothing needs any more")
	upgrade.Flags().StringArrayVar(&upTo, "to", nil, "move this service to this release instead of the node's pin, <service>=<release tag> (repeatable; services not named keep the version they run)")
	upgrade.Flags().BoolVar(&upAllowOlder, "allow-older", false, "allow --to to name an older release than the project runs (what `supavise rollback` does for the projects an upgrade moved)")

	versions := projectCmd("versions [<ref>]", "Show the service versions projects run and whether an upgrade is available", cobra.MaximumNArgs(1), runVersions)
	versions.Flags().BoolVar(&versionsJSON, "json", false, "print JSON")

	projectsCmd.AddCommand(upgrade, versions)
}
