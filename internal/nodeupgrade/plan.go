package nodeupgrade

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/selfupdate"
)

// PlanOptions are the settings the plan depends on.
type PlanOptions struct {
	// IncludePostgres moves the projects' PostgreSQL release too. Without it the projects keep
	// the PostgreSQL they run, as on hosted Supabase, where the owner of a project decides when
	// its Postgres is upgraded.
	IncludePostgres bool
	// Canary and Batch are the rollout settings of [upgrade], for the text only.
	Canary, Batch int
}

// Plan says what an upgrade does. It is computed from the node and the new release's Info, and
// printed before anything changes.
type Plan struct {
	From, To     string
	BinaryChange bool
	// Shared are the shared services whose release changes, in the order they are rolled.
	Shared []ServiceMove
	// System are the changes to the system project, which runs the node's pins: its PostgreSQL
	// (the registry) and the GoTrue behind the dashboard's sign-in.
	System []ServiceMove
	// ProjectTarget maps the project services the upgrade moves to the release they move to.
	ProjectTarget map[string]string
	// Upgrade lists the projects that will be upgraded, Skipped those that will not and why,
	// Current the number that already run the target.
	Upgrade []string
	Skipped []SkippedProject
	Current int
	// HeldBack counts projects that keep their PostgreSQL release because IncludePostgres is
	// not set, and HeldTo is the release they would move to.
	HeldBack int
	HeldTo   string

	IncludePostgres bool
	Canary, Batch   int
	// NewMigrations are the registry migrations the release adds.
	NewMigrations []string
	Restarts      []string
	Impact        []string
	Notes         []string
	// Refusal is set when the upgrade must not run; nothing has changed.
	Refusal string

	Target *Info
}

// SkippedProject is a project the upgrade leaves alone.
type SkippedProject struct {
	Ref, Why string
}

// Empty reports whether the upgrade has nothing to do: same binary, services already on the
// target, no project to move.
func (p *Plan) Empty() bool {
	return !p.BinaryChange && len(p.Shared) == 0 && len(p.System) == 0 && len(p.Upgrade) == 0
}

// projectServices are the services of a project in the order they are listed.
var projectServices = []string{config.SvcGoTrue, config.SvcPostgREST, config.SvcPostgres}

// BuildPlan computes what moving n onto the release described by to does.
func BuildPlan(n *Node, to *Info, o PlanOptions) *Plan {
	p := &Plan{From: n.Version, To: to.Version, BinaryChange: n.Version != to.Version, IncludePostgres: o.IncludePostgres,
		Canary: o.Canary, Batch: o.Batch, Target: to, ProjectTarget: map[string]string{}}
	if c, ok := selfupdate.Compare(to.Version, n.Version); ok && c < 0 {
		p.Refusal = fmt.Sprintf("%s is older than the installed %s; `supavise rollback` goes back to the previous release", to.Version, n.Version)
		return p
	}
	for _, m := range DiffPins(n.Pins, to.Pins) {
		switch m.Service {
		case config.SvcPostgres, config.SvcGoTrue:
			p.System = append(p.System, m)
		case config.SvcPostgREST:
		default:
			// A shared service the node never rendered (no dashboard on this node, no Edge
			// Functions) has no unit to move.
			if m.From != "" {
				p.Shared = append(p.Shared, m)
			}
		}
	}
	for _, m := range p.System {
		if m.Service != config.SvcPostgres {
			continue
		}
		if a, b := lifecycle.PostgresMajor(m.From), lifecycle.PostgresMajor(m.To); a != 0 && b != 0 && a != b {
			p.Refusal = fmt.Sprintf("this release moves PostgreSQL from %d to %d, and upgrades across Postgres major versions are not supported yet", a, b)
			return p
		}
	}
	for _, svc := range projectServices {
		if svc == config.SvcPostgres && !o.IncludePostgres {
			continue
		}
		if t := to.Pins[svc]; t != "" {
			p.ProjectTarget[svc] = t
		}
	}
	p.HeldTo = to.Pins[config.SvcPostgres]
	for _, pr := range n.UserProjects() {
		var changes []ServiceMove
		for _, svc := range projectServices {
			if t, ok := p.ProjectTarget[svc]; ok {
				if cur := pr.Effective(svc, n.Pins); cur != t {
					changes = append(changes, ServiceMove{Service: svc, From: cur, To: t})
				}
			}
		}
		if !o.IncludePostgres && p.HeldTo != "" && pr.Active() && pr.Effective(config.SvcPostgres, n.Pins) != p.HeldTo {
			p.HeldBack++
		}
		if len(changes) == 0 {
			p.Current++
			continue
		}
		if why := skipReason(pr, changes); why != "" {
			p.Skipped = append(p.Skipped, SkippedProject{Ref: pr.Ref, Why: why})
			continue
		}
		p.Upgrade = append(p.Upgrade, pr.Ref)
	}
	sort.Strings(p.Upgrade)
	if len(to.RegistryMigrations) > 0 {
		p.NewMigrations = missing(to.RegistryMigrations, n.AppliedMigrations)
	}
	p.describe()
	return p
}

// skipReason says why a project is not upgraded now, or "" when it is.
func skipReason(pr Project, changes []ServiceMove) string {
	switch pr.Status {
	case "ACTIVE_HEALTHY":
	case "INACTIVE":
		return "paused; resume it, then run `supavise projects upgrade " + pr.Ref + "`"
	default:
		return "is " + pr.Status + "; an upgrade needs it ACTIVE_HEALTHY"
	}
	for _, c := range changes {
		if cmp, err := lifecycle.CompareTags(c.Service, c.From, c.To); err == nil && cmp > 0 {
			return fmt.Sprintf("runs %s %s, which is newer than the release's %s", c.Service, lifecycle.ShortVersion(c.Service, c.From), lifecycle.ShortVersion(c.Service, c.To))
		}
	}
	return ""
}

func short(svc, tag string) string {
	if tag == "" {
		return "?"
	}
	return lifecycle.ShortVersion(svc, tag)
}

// describe fills Restarts, Impact and Notes.
func (p *Plan) describe() {
	if p.BinaryChange {
		p.Restarts = append(p.Restarts, "supavise.service, the daemon with the HTTPS proxy and the Management API")
		p.Impact = append(p.Impact, "HTTPS requests fail for a few seconds while the daemon restarts; the projects' databases keep running")
	}
	for _, m := range p.System {
		switch m.Service {
		case config.SvcPostgres:
			p.Restarts = append(p.Restarts, "the system PostgreSQL cluster, which holds the registry")
			p.Impact = append(p.Impact, "the registry, the dashboard's sign-in and the Management API are unavailable for some seconds while the system cluster restarts")
		case config.SvcGoTrue:
			p.Restarts = append(p.Restarts, "the dashboard's sign-in service (system GoTrue)")
		}
	}
	for _, m := range p.Shared {
		switch m.Service {
		case config.SvcSupavisor:
			p.Restarts = append(p.Restarts, "Supavisor, the connection pooler")
			p.Impact = append(p.Impact, "every pooled connection (ports 5432 and 6543) drops when Supavisor restarts; clients reconnect")
		case config.SvcRealtime:
			p.Restarts = append(p.Restarts, "Realtime")
			p.Impact = append(p.Impact, "every Realtime websocket drops when Realtime restarts; clients reconnect")
		case config.SvcStorage:
			p.Restarts = append(p.Restarts, "Storage")
			p.Impact = append(p.Impact, "uploads in flight fail when Storage restarts; clients retry")
		case config.SvcPGMeta:
			p.Restarts = append(p.Restarts, "postgres-meta, behind the dashboard's table editor and SQL editor")
		case config.SvcStudio:
			p.Restarts = append(p.Restarts, "Studio, the dashboard")
		case config.SvcImgproxy:
			p.Restarts = append(p.Restarts, "imgproxy, Storage's image transformations")
		case config.SvcEdgeRuntime:
			p.Restarts = append(p.Restarts, "the Edge Functions runtime")
		}
	}
	if len(p.Shared) > 0 {
		p.Notes = append(p.Notes, "Storage and Realtime run the new release's migrations in every project's database when the projects are registered with them again, right after the services are up.")
	}
	if k := len(p.Upgrade); k > 0 {
		what := "GoTrue and PostgREST"
		if p.IncludePostgres && p.ProjectTarget[config.SvcPostgres] != "" {
			what = "GoTrue, PostgREST and, where its release changes, PostgreSQL"
		}
		p.Restarts = append(p.Restarts, fmt.Sprintf("%s of %d project(s), %d canary first, then %d at a time", what, k, p.Canary, p.Batch))
		p.Impact = append(p.Impact, fmt.Sprintf("each project is offline for a minute or two while its %s restart; the rollout stops at the first project that fails", what))
		if p.IncludePostgres {
			p.Impact = append(p.Impact, "a project whose PostgreSQL release changes also drops every database connection")
		}
	}
	if p.HeldBack > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("%d project(s) keep their PostgreSQL release (the release pins %s); pass --include-postgres to move them, each restarts PostgreSQL", p.HeldBack, short(config.SvcPostgres, p.HeldTo)))
	}
	if len(p.NewMigrations) > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("the release adds %d registry migration(s) (%s). The new daemon applies them when it starts, and migrations only go forward: from then on the node cannot go back to the previous release by itself, and `supavise rollback` is refused until the system cluster is restored by hand from its pre-upgrade backup (internal/backup/README.md, \"Disaster recovery of the system cluster\")", len(p.NewMigrations), refList(p.NewMigrations)))
	}
	for _, s := range p.Skipped {
		p.Notes = append(p.Notes, fmt.Sprintf("project %s is skipped: %s", s.Ref, s.Why))
	}
	back := "If the new release does not come up healthy, the node goes back to the previous release by itself."
	switch {
	case !p.BinaryChange:
		back = "The binary does not change, so there is no earlier binary to go back to: if a project fails, the projects this run moved are put back, and the services stay on the releases the binary pins."
	case len(p.NewMigrations) > 0:
		back = "If the new daemon fails before it applies the migrations, the node goes back to the previous release by itself; after that it stays on the new binary, the projects this run moved are put back, and the node needs you."
	}
	p.Notes = append(p.Notes, "A base backup of the system project and of every running project is taken first; if one fails nothing is changed. "+back)
	if p.BinaryChange {
		p.Notes = append(p.Notes, "A project whose GoTrue or PostgREST files the new release renders differently restarts too, in the same canary and batch order; the files are rendered by the new binary, so this list cannot name those projects before the upgrade.")
	}
}

// Render prints the plan.
func (p *Plan) Render(w io.Writer) {
	fmt.Fprintf(w, "Supavise %s -> %s", p.From, p.To)
	if !p.BinaryChange {
		fmt.Fprint(w, " (same binary; the services are brought onto its release)")
	}
	fmt.Fprintln(w)
	if p.Refusal != "" {
		fmt.Fprintf(w, "refused: %s\n", p.Refusal)
		return
	}
	if p.Empty() {
		fmt.Fprintln(w, "nothing to change")
	}
	if len(p.Shared) > 0 || len(p.System) > 0 {
		fmt.Fprintln(w, "Services of the node:")
		for _, m := range append(append([]ServiceMove{}, p.System...), p.Shared...) {
			fmt.Fprintf(w, "  %-13s %s -> %s\n", m.Service, short(m.Service, m.From), short(m.Service, m.To))
		}
	}
	if len(p.Upgrade) > 0 || len(p.Skipped) > 0 || p.Current > 0 {
		var targets []string
		for _, svc := range projectServices {
			if t, ok := p.ProjectTarget[svc]; ok {
				targets = append(targets, svc+" "+short(svc, t))
			}
		}
		fmt.Fprintf(w, "Projects (%s): %d to upgrade, %d skipped, %d already there\n", strings.Join(targets, ", "), len(p.Upgrade), len(p.Skipped), p.Current)
	}
	if len(p.Restarts) > 0 {
		fmt.Fprintln(w, "What restarts:")
		for _, r := range p.Restarts {
			fmt.Fprintf(w, "  - %s\n", r)
		}
	}
	if len(p.Impact) > 0 {
		fmt.Fprintln(w, "Expected impact:")
		for _, r := range p.Impact {
			fmt.Fprintf(w, "  - %s\n", r)
		}
	}
	for _, r := range p.Notes {
		fmt.Fprintf(w, "note: %s\n", r)
	}
}

// BackupRefs lists the projects the upgrade backs up first: the system project (the registry) and
// every project whose database runs. A paused project's data does not change.
func BackupRefs(n *Node) []string {
	var rest []string
	for _, pr := range n.UserProjects() {
		if pr.Active() {
			rest = append(rest, pr.Ref)
		}
	}
	sort.Strings(rest)
	return append([]string{config.SystemRef}, rest...)
}

// Gates are the conditions under which the upgrade does not start. Refusals stop it (exit status
// 2, nothing changed); Warnings are printed and the upgrade goes on.
type Gates struct {
	Refusals, Warnings []string
}

// GateOptions tune CheckGates.
type GateOptions struct {
	Unattended bool
	Now        time.Time
	// BackupMaxAge is how old the newest backup of a project may be for an unattended upgrade
	// (24 hours; the upgrade takes fresh ones anyway, so this only keeps a node whose backups
	// have been failing from being upgraded by a timer).
	BackupMaxAge time.Duration
}

// DiskNeeded estimates the space the upgrade needs on the state volume: the new artifacts
// (unpacked next to the archives they come from), and, when the backups are local, a compressed
// copy of every running project's data.
func DiskNeeded(n *Node, p *Plan) uint64 {
	const (
		headroom = 2 << 30
		artifact = 400 << 20
	)
	moved := len(p.Shared) + len(p.System)
	if len(p.Upgrade) > 0 {
		moved += len(p.ProjectTarget)
	}
	need := uint64(headroom) + uint64(moved)*artifact
	if n.LocalBackups {
		var data int64
		for _, pr := range n.Projects {
			if pr.Active() || pr.Ref == config.SystemRef {
				data += pr.DiskBytes
			}
		}
		need += uint64(data) / 2
	}
	return need
}

// CheckGates applies the node's rules to a plan that would otherwise run.
func CheckGates(n *Node, p *Plan, o GateOptions) Gates {
	var g Gates
	refuse := func(format string, a ...any) { g.Refusals = append(g.Refusals, fmt.Sprintf(format, a...)) }
	warn := func(format string, a ...any) { g.Warnings = append(g.Warnings, fmt.Sprintf(format, a...)) }

	switch n.Verdict {
	case VerdictHealthy:
	case VerdictDown:
		refuse("the node is down (%s); fix that first", n.Summary)
	case VerdictDegraded:
		if o.Unattended {
			refuse("the node is degraded (%s); an unattended upgrade needs a healthy node", n.Summary)
		} else {
			warn("the node is degraded: %s. A project that is not healthy is skipped by the upgrade.", n.Summary)
		}
	default:
		if o.Unattended {
			refuse("`supavise status` could not be read; an unattended upgrade needs a healthy node")
		} else {
			warn("`supavise status` could not be read")
		}
	}
	if need := DiskNeeded(n, p); n.DiskFree != 0 && n.DiskFree < need {
		refuse("%s has %s free and the upgrade needs about %s (new artifacts and, with local backups, a copy of every project's data)", n.DiskPath, human(n.DiskFree), human(need))
	}
	if !n.Escrow.Covered {
		msg := "the master key has no encrypted copy in the backup backend: if this node is lost, its backups cannot be read (`supavise system escrow-key`)"
		if !n.Escrow.Known {
			msg = "could not check whether the master key has a copy in the backup backend (`supavise status` says why)"
		}
		// Only a copy that is known to be there lets a timer upgrade the node.
		if o.Unattended {
			refuse("%s", msg)
		} else {
			warn("%s", msg)
		}
	}
	if o.Unattended {
		limit := o.BackupMaxAge
		if limit <= 0 {
			limit = 24 * time.Hour
		}
		var stale []string
		for _, pr := range n.Projects {
			if !pr.Active() && pr.Ref != config.SystemRef {
				continue
			}
			if pr.LastBackup.IsZero() || o.Now.Sub(pr.LastBackup) > limit {
				stale = append(stale, pr.Ref)
			}
		}
		if len(stale) > 0 {
			sort.Strings(stale)
			refuse("%d project(s) have no backup newer than %s: %s. An unattended upgrade needs them; fix the backups or run `supavise upgrade --yes`", len(stale), limit, refList(stale))
		}
	}
	return g
}

func refList(refs []string) string {
	if len(refs) > 5 {
		return strings.Join(refs[:5], ", ") + ", ..."
	}
	return strings.Join(refs, ", ")
}

func human(b uint64) string {
	const g = 1 << 30
	if b >= g {
		return fmt.Sprintf("%.1f GiB", float64(b)/g)
	}
	return fmt.Sprintf("%d MiB", b>>20)
}

// missing returns the names in want that have lacks, in want's order.
func missing(want, have []string) []string {
	set := make(map[string]bool, len(have))
	for _, n := range have {
		set[n] = true
	}
	var out []string
	for _, n := range want {
		if !set[n] {
			out = append(out, n)
		}
	}
	return out
}
