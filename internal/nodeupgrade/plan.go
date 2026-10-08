package nodeupgrade

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/infra"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/selfupdate"
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
	// Pending lists the projects, not in Upgrade, whose service files an earlier upgrade rendered
	// and whose restart it held back; the rollout restarts them.
	Pending []string
	// HeldBack counts projects that keep their PostgreSQL release because IncludePostgres is
	// not set, and HeldTo is the release they would move to.
	HeldBack int
	HeldTo   string

	IncludePostgres bool
	Canary, Batch   int
	// NewMigrations are the registry migrations the release adds.
	NewMigrations []string
	// HostPending is true when the release's host layer is ahead of the node's: `supavise system
	// converge` of the new binary has something to do (HostFrom to HostTo, HostChanges are its
	// steps). The node's own run restarts no project for it.
	HostPending      bool
	HostFrom, HostTo int
	HostChanges      []string
	// Stack is the AWS stack against the release's needs; nil when the node is not on a stack or
	// the release needs none.
	Stack    *StackGap
	Restarts []string
	Impact   []string
	Notes    []string
	// Refusal is set when the upgrade must not run; nothing has changed.
	Refusal string

	Target *Info
	// cluster is the node's view of its cluster, kept for the notes.
	cluster *ClusterView
}

// SkippedProject is a project the upgrade leaves alone.
type SkippedProject struct {
	Ref, Why string
}

// StackGap is the AWS stack against what the release needs.
type StackGap struct {
	// Report is what the node's own binary says about the stack (internal/infra); its Need is the
	// installed release's, which can be lower than Need below.
	Report infra.Report
	// Need is the stack revision the release needs.
	Need int
}

// Pending reports whether the stack is behind the release.
func (g *StackGap) Pending() bool {
	return g != nil && g.Report.Platform != "" && g.Report.Have < g.Need
}

// StackPending reports whether the AWS stack needs an update for the release.
func (p *Plan) StackPending() bool { return p.Stack.Pending() }

// Empty reports whether the upgrade has nothing to do: same binary, services already on the
// target, no project to move or to restart, a host layer and an AWS stack that are current.
func (p *Plan) Empty() bool {
	return !p.NodeChanges() && !p.StackPending()
}

// NodeChanges reports whether the upgrade changes the node itself: the binary, a service, a
// project or the host layer. A plan that only has an AWS stack to update changes none of these.
func (p *Plan) NodeChanges() bool {
	return p.BinaryChange || len(p.Shared) > 0 || len(p.System) > 0 || len(p.Upgrade) > 0 || len(p.Pending) > 0 || p.HostPending
}

// Rollout reports whether the plan runs the project rollout: a project to move, a project whose
// held-back restart is owed, or a new binary (which can render any project's files differently).
func (p *Plan) Rollout() bool {
	return p.BinaryChange || len(p.Upgrade) > 0 || len(p.Pending) > 0
}

// projectServices are the services of a project in the order they are listed.
var projectServices = []string{config.SvcGoTrue, config.SvcPostgREST, config.SvcPostgres}

// BuildPlan computes what moving n onto the release described by to does.
func BuildPlan(n *Node, to *Info, o PlanOptions) *Plan {
	p := &Plan{From: n.Version, To: to.Version, BinaryChange: n.Version != to.Version, IncludePostgres: o.IncludePostgres,
		Canary: o.Canary, Batch: o.Batch, Target: to, ProjectTarget: map[string]string{}, cluster: n.Cluster}
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
	if to.WALIncompatible && n.Cluster != nil && p.movesPostgres(n) {
		if behind := n.Cluster.Behind(to.Version); len(behind) > 0 {
			p.Refusal = walOrderRefusal(to.Version, behind)
			return p
		}
	}
	moving := map[string]bool{}
	for _, ref := range p.Upgrade {
		moving[ref] = true
	}
	for _, pr := range n.UserProjects() {
		if pr.HeldRestart && pr.Status == "ACTIVE_HEALTHY" && !moving[pr.Ref] {
			p.Pending = append(p.Pending, pr.Ref)
		}
	}
	sort.Strings(p.Pending)
	if len(to.RegistryMigrations) > 0 {
		p.NewMigrations = missing(to.RegistryMigrations, n.AppliedMigrations)
	}
	if to.ConvergeRevision > 0 && n.ConvergeKnown && n.ConvergeRevision < to.ConvergeRevision {
		p.HostPending, p.HostFrom, p.HostTo, p.HostChanges = true, n.ConvergeRevision, to.ConvergeRevision, to.HostChanges
	}
	if n.Infra != nil && n.Infra.Platform != "" && to.InfraRevision > 0 {
		p.Stack = &StackGap{Report: *n.Infra, Need: to.InfraRevision}
	}
	p.describe()
	return p
}

// movesPostgres reports whether the upgrade changes a PostgreSQL release that runs on this node:
// the system cluster's, or a project's when the projects' PostgreSQL moves too.
func (p *Plan) movesPostgres(n *Node) bool {
	for _, m := range p.System {
		if m.Service == config.SvcPostgres {
			return true
		}
	}
	if t := p.ProjectTarget[config.SvcPostgres]; t != "" {
		for _, ref := range p.Upgrade {
			for _, pr := range n.UserProjects() {
				if pr.Ref == ref && pr.Effective(config.SvcPostgres, n.Pins) != t {
					return true
				}
			}
		}
	}
	return false
}

// walOrderRefusal is why a release whose PostgreSQL keeps no WAL compatibility waits for the nodes
// that hold standbys: a standby reads the WAL of a primary on the same or an older release, so the
// standbys are upgraded first.
func walOrderRefusal(version string, behind []Standby) string {
	var parts []string
	for _, b := range behind {
		who := b.Node
		if b.Name != "" && b.Name != b.Node {
			who += " (" + b.Name + ")"
		}
		running := b.Version
		if running == "" {
			running = "an unknown release"
		}
		parts = append(parts, fmt.Sprintf("%s runs %s and holds standbys of %s", who, running, standbyRefs(b.Refs)))
	}
	return fmt.Sprintf("%s changes PostgreSQL in a way a standby on an older release cannot follow (the release manifest says wal_compat: false), so the servers that hold standbys of this server's databases are upgraded first: %s. Run `sudo supavise upgrade` on each of them, then here. A project's primary moves to another server with `supavise projects failover`, if two servers hold standbys of each other's databases", version, strings.Join(parts, "; "))
}

func standbyRefs(refs []string) string {
	if len(refs) == 0 {
		return "the system cluster"
	}
	return refList(refs)
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
		p.Impact = append(p.Impact, "HTTPS requests fail for a few seconds while the daemon restarts; the projects' databases and services keep running unless the new release renders their files differently (see the notes)")
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
	if k := len(p.Pending); k > 0 {
		p.Restarts = append(p.Restarts, fmt.Sprintf("PostgreSQL, GoTrue and PostgREST of %d project(s) whose restart an earlier upgrade held back (%s), %d canary first, then %d at a time", k, refList(p.Pending), p.Canary, p.Batch))
		p.Impact = append(p.Impact, "each of those projects drops its database connections and is offline for a minute or two while its services restart; the rollout stops at the first project that fails")
	}
	if p.HeldBack > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("%d project(s) keep their PostgreSQL release (the release pins %s); pass --include-postgres to move them, each restarts PostgreSQL", p.HeldBack, short(config.SvcPostgres, p.HeldTo)))
	}
	if c := p.cluster; c != nil && len(c.Elsewhere) > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("%d project(s) are homed on other servers of the cluster (%s): each server upgrades the projects it runs, and takes their backups, so run `sudo supavise upgrade` on those servers too", len(c.Elsewhere), refList(c.Elsewhere)))
	}
	if len(p.NewMigrations) > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("the release adds %d registry migration(s) (%s). The new daemon applies them when it starts, and migrations only go forward: from then on the node cannot go back to the previous release by itself, and `supavise rollback` is refused until the system cluster is restored by hand from its pre-upgrade backup (internal/backup/README.md, \"Disaster recovery of the system cluster\")", len(p.NewMigrations), refList(p.NewMigrations)))
	}
	if p.HostPending {
		when := "right after the binary is swapped, before the daemon restarts"
		if !p.BinaryChange {
			when = "after the backups"
		}
		p.Notes = append(p.Notes, fmt.Sprintf("the host layer (`supavise system converge`, revision %d -> %d) runs %s; it restarts no project, and a failure of it fails the upgrade and rolls it back", p.HostFrom, p.HostTo, when))
	}
	if p.StackPending() {
		p.Notes = append(p.Notes, "the AWS stack is changed only by `sudo -E supavise upgrade --aws`, with your own AWS credentials: this node's instance role has no right to change it. Everything else in this plan can go ahead without it, and the new features that need the stack stay off until it is updated")
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
	if p.NodeChanges() {
		p.Notes = append(p.Notes, "A base backup of the system project and of every running project is taken first; if one fails nothing is changed. "+back)
	}
	if p.BinaryChange {
		p.Notes = append(p.Notes, "The new daemon restarts any shared service whose files it renders differently, whether or not the service's release moves, so a service this list does not name can restart too; if that is Supavisor, every pooled connection drops, and if it is Realtime, every websocket drops. The files are rendered by the new binary, so this list cannot name those services before the upgrade.")
		p.Notes = append(p.Notes, "A project whose PostgreSQL, GoTrue or PostgREST files the new release renders differently restarts too, in the same canary and batch order (a PostgreSQL restart drops the project's database connections and restarts its GoTrue and PostgREST with it); the files are rendered by the new binary, so this list cannot name those projects before the upgrade. That restart also applies PostgreSQL settings an Owner saved without restarting; a project with only such a setting waiting is not restarted for it.")
		p.Notes = append(p.Notes, "Going back to the previous release, by itself after a failed rollout or with `supavise rollback`, restarts every project whose PostgreSQL, GoTrue or PostgREST files the rollout had already restarted onto the new release's files (the projects it never reached keep running), one after another when the old daemon starts and outside the canary and batches; each such restart drops that project's database connections. A previous release built without the held-back-restart marks restarts every project whose files the two releases render differently.")
		p.Notes = append(p.Notes, "The system PostgreSQL cluster restarts when the daemon starts if the new release renders its files differently, even when its release does not move. The registry, the dashboard's sign-in and the Management API are unavailable for some seconds then; this is not part of the rollout.")
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
	if p.NodeChanges() {
		if p.HostPending {
			fmt.Fprintf(w, "Host (converge revision %d -> %d, restarts no project):\n", p.HostFrom, p.HostTo)
			for _, c := range p.HostChanges {
				fmt.Fprintf(w, "  - %s\n", c)
			}
		}
		if len(p.NewMigrations) > 0 {
			fmt.Fprintf(w, "Registry migrations (forward only): %s\n", strings.Join(p.NewMigrations, ", "))
		}
		// A new binary can render a project's files differently, and the notes below say so; the line
		// is for a run whose only work is the host and the registry.
		if len(p.Upgrade) == 0 && len(p.Pending) == 0 && !p.BinaryChange {
			fmt.Fprintln(w, "Projects restarted: none expected")
		}
	}
	if p.Stack != nil {
		p.Stack.render(w)
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

// render writes the infrastructure block of the plan: the gap between the stack and the release,
// and the command that closes it. A stack that is current writes nothing.
func (g *StackGap) render(w io.Writer) {
	if !g.Pending() {
		return
	}
	r := g.Report
	if r.Behind() && r.Need == g.Need {
		r.Render(w) // the installed release needs what this one does: its own words
		return
	}
	stack := "AWS stack"
	if r.Stack != "" {
		stack += fmt.Sprintf(" %q", r.Stack)
	}
	fmt.Fprintf(w, "Infrastructure  %s is at revision %d; this release needs %d\n", stack, r.Have, g.Need)
	for _, m := range r.Missing {
		fmt.Fprintf(w, "  missing  %s (%s)\n", m.Title, m.Why)
	}
	fix := r.Fix
	if fix == "" {
		fix = "sudo -E supavise upgrade --aws"
	}
	fmt.Fprintf(w, "  Fix: %s\n", fix)
}

// BackupRefs lists the projects the upgrade backs up first: the system project (the registry, on
// the node that leads) and every project homed on the node whose database runs. A paused project's
// data does not change.
func BackupRefs(n *Node) []string {
	var rest []string
	for _, pr := range n.UserProjects() {
		if pr.Active() {
			rest = append(rest, pr.Ref)
		}
	}
	sort.Strings(rest)
	if n.Cluster != nil && !n.Cluster.Leader {
		// The system cluster of a follower is a standby of the leader's: its base backup is the
		// leader's to take, and a standby has no data of its own to back up.
		return rest
	}
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
	if n.DiskUnknown {
		// The check is part of the brief; a timer does not go on without it.
		msg := "could not read the free space of " + n.DiskPath + ", so the upgrade cannot check that it has room for the new artifacts and the backups"
		if o.Unattended {
			refuse("%s", msg)
		} else {
			warn("%s", msg)
		}
	} else if need := DiskNeeded(n, p); n.DiskFree < need {
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
