package failover

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// The automatic modes (design 2.10.7). [failover] mode = "project" lets the leader fail over one
// project whose primary stays unhealthy; "server" adds the follower taking over a dead leader. Both
// need the AWS fencer, because only a hard fence guarantees one writer without an operator, and both
// act only when every gate below says so. A gate that cannot be evaluated is closed.

// MonitorInterval is how often the monitor looks.
const MonitorInterval = 10 * time.Second

// pingTimeout bounds the monitor's ping of the leader.
const pingTimeout = 3 * time.Second

// Decision is what one look of the monitor concluded.
type Decision struct {
	// Action is "none", "project" or "server": what the look started.
	Action string
	// Ref is the project of a "project" action.
	Ref string
	// Reason says why nothing was started (the first gate that was closed), or what was started.
	Reason string
	// Err is the error of the move that was started.
	Err error
}

func none(format string, args ...any) Decision {
	return Decision{Action: "none", Reason: fmt.Sprintf(format, args...)}
}

// Monitor watches the cluster and starts a failover when the gates allow. One Tick is one look;
// Run calls it every MonitorInterval.
type Monitor struct {
	o        *Orchestrator
	Interval time.Duration

	mu sync.Mutex
	// leaderSilentSince is when the leader first failed to answer in the current run of failures.
	leaderSilentSince time.Time
	// unhealthySince is when each project was first seen with an unhealthy primary.
	unhealthySince map[string]time.Time
	// last is the previous reason, so that the log says it once.
	last string
}

// NewMonitor returns the monitor of o.
func NewMonitor(o *Orchestrator) *Monitor {
	return &Monitor{o: o, Interval: MonitorInterval, unhealthySince: map[string]time.Time{}}
}

// Run looks every Interval until ctx ends. It first continues a server move to this node that the
// restart of the daemon cut off (ResumeInterrupted), whatever the mode. Beyond that it does nothing
// for a node whose mode is manual.
func (m *Monitor) Run(ctx context.Context) {
	if _, err := m.o.ResumeInterrupted(ctx); err != nil && ctx.Err() == nil {
		m.o.d.Log.Error("continuing the server move after the restart failed", "error", err)
	}
	if !m.o.conf().Automatic() {
		return
	}
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d := m.Tick(ctx)
			m.log(d)
		}
	}
}

func (m *Monitor) log(d Decision) {
	text := d.Action + ": " + d.Reason
	m.mu.Lock()
	changed := text != m.last
	m.last = text
	m.mu.Unlock()
	if d.Action != "none" || changed {
		m.o.d.Log.Info("failover monitor", "action", d.Action, "ref", d.Ref, "reason", d.Reason, "error", d.Err)
	}
}

// Tick looks once and starts a move when every gate is open.
func (m *Monitor) Tick(ctx context.Context) Decision {
	o := m.o
	f := o.conf()
	if !f.Automatic() {
		return none("the mode is manual")
	}
	if reason := m.arm(ctx); reason != "" {
		return none("automatic failover is off: %s", reason)
	}
	if o.Busy() {
		return none("a move is running")
	}
	snap := snapshotOf(ctx, o.d.Members)
	switch snap.Role {
	case cluster.RoleLeader:
		return m.projectTick(ctx, snap)
	case cluster.RoleFollower:
		if f.Mode == config.FailoverServer {
			return m.serverTick(ctx, snap)
		}
		return none("only the leader decides on a project in this mode")
	}
	return none("this node is %s", snap.Role)
}

func snapshotOf(ctx context.Context, m cluster.Membership) cluster.Snapshot {
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	return <-m.Watch(c)
}

// arm checks that the fencer is permitted. A node whose probe fails downgrades itself to manual,
// says so once, and tries again at the next look (the probe is cached for a minute); it returns
// the reason the mode is off, or "".
func (m *Monitor) arm(ctx context.Context) string {
	o := m.o
	var reason string
	switch {
	case o.d.Provider.Name() != "aws":
		reason = "automatic failover needs [failover] fencing = \"aws\""
	default:
		err := o.probe(ctx)
		if ap, ok := o.d.Provider.(AddressProber); ok && err == nil {
			err = o.probeTakeover(ctx, ap)
		}
		if err != nil {
			if ctx.Err() != nil { // the daemon is stopping: that is no verdict on the fencer
				return o.autoOff()
			}
			reason = err.Error()
		}
	}
	prev := o.autoOff()
	if reason != prev {
		o.setAutoOff(reason)
		ev := alerts.Event{Kind: alerts.KindFailoverAutoOff, Severity: alerts.SeverityWarning, Title: "Automatic failover is off", Key: "failover/auto-off"}
		if reason != "" {
			ev.Detail = "[failover] mode is " + o.conf().Mode + ", but the node cannot fence: " + reason + ". The node runs as manual until the check passes."
			o.alert(ctx, ev)
		} else if prev != "" {
			ev.Resolved, ev.Detail = true, "The node can fence again: automatic failover is on."
			o.alert(ctx, ev)
		}
	}
	return reason
}

// gates are the conditions that hold for every automatic move. It returns the reason one is
// closed, or "".
func (m *Monitor) gates(ctx context.Context, snap cluster.Snapshot, ref string, home, target registry.Node) string {
	o := m.o
	now := o.d.Now()
	if snap.Maintenance.Active(now) {
		return fmt.Sprintf("maintenance on %s until %s (%s)", snap.Maintenance.Node, snap.Maintenance.Until.Format(time.RFC3339), snap.Maintenance.Reason)
	}
	if reason := m.cooldown(ctx, now, ref); reason != "" {
		return reason
	}
	if home.Version != "" && target.Version != "" && home.Version != target.Version {
		return fmt.Sprintf("the nodes run different releases (%s, %s)", home.Version, target.Version)
	}
	if a, b := home.Provider.AWS, target.Provider.AWS; a != nil && b != nil && a.Region != "" && b.Region != "" && a.Region != b.Region {
		return "the nodes are in different regions: the address cannot move, so the move is manual"
	}
	return ""
}

// cooldown returns a reason when an unplanned failover ran within [failover] cooldown_minutes. A failed
// or aborted one counts: a node that cannot finish a move should not try again every minute.
//
// It is kept per what moves. In project mode a project waits for its own earlier failover, and for any
// failover of the server (which moved every project): one project that failed over does not hold the
// others back for an hour. A failover of the server counts the failovers of the server only: the leader
// that died after a project of it failed over still has to be replaced. ref is "" for the server.
func (m *Monitor) cooldown(ctx context.Context, now time.Time, ref string) string {
	ms, err := m.o.store().ListMoves(ctx, "", cooldownLook)
	if err != nil {
		return "the moves log cannot be read: " + err.Error()
	}
	window := m.o.conf().Cooldown()
	for _, mv := range ms {
		if mv.Kind != registry.MoveFailover {
			continue
		}
		switch {
		case mv.Scope == registry.MoveServer:
		case ref != "" && mv.Scope == registry.MoveProject && mv.Ref == ref:
		default:
			continue
		}
		at := mv.StartedAt
		if mv.EndedAt != nil {
			at = *mv.EndedAt
		}
		if now.Sub(at) < window {
			what := "a failover of the server"
			if mv.Scope == registry.MoveProject {
				what = "a failover of this project"
			}
			return fmt.Sprintf("%s ran at %s, within the cooldown of %s", what, at.Format(time.RFC3339), window)
		}
	}
	return ""
}

// cooldownLook is how many of the newest moves the cooldown reads.
const cooldownLook = 200

// serverTick is the follower's look at the leader.
func (m *Monitor) serverTick(ctx context.Context, snap cluster.Snapshot) Decision {
	o := m.o
	leader, ok := o.d.Members.Leader()
	if !ok || leader.ID == snap.Self.ID {
		return none("no other node leads")
	}
	now := o.d.Now()

	// 1. The mesh. Silence for the grace period, and only silence, opens the first gate.
	if o.d.Peers != nil {
		pctx, cancel := context.WithTimeout(ctx, pingTimeout)
		_, err := o.d.Peers.Ping(pctx, leader.ID)
		cancel()
		if err == nil {
			m.mu.Lock()
			m.leaderSilentSince = time.Time{}
			m.mu.Unlock()
			return none("the leader %s answers", leader.Name)
		}
	}
	m.mu.Lock()
	if m.leaderSilentSince.IsZero() {
		m.leaderSilentSince = now
	}
	silent := now.Sub(m.leaderSilentSince)
	m.mu.Unlock()
	grace := o.conf().Grace()
	if silent < grace {
		return none("the leader %s has been silent for %s of %s", leader.Name, silent.Round(time.Second), grace)
	}
	// With more than one follower every one of them reaches this point. They take turns: the
	// follower first in line (by node id) goes ahead after the grace period, the next one a grace
	// period later, and so on, so that the one that acts first has taken over, and shows as the
	// leader, before the others look again. The epoch marker is what holds when two still act at once.
	if turn := o.turn(snap.Self.ID, leader.ID); turn > 0 {
		if wait := grace * time.Duration(turn+1); silent < wait {
			return none("the leader %s has been silent for %s; %d follower(s) take their turn first, this node's is at %s", leader.Name, silent.Round(time.Second), turn, wait)
		}
	}

	// 2. The public address, from outside. A leader that no peer reaches may still serve clients.
	if o.d.PublicProbe == nil {
		return none("there is no public probe of the service address")
	}
	if err := o.d.PublicProbe(ctx); err == nil {
		return none("the service address still answers")
	}

	// 3. The cloud. A peer that does not answer is not a peer that is down: EC2 must say so.
	cloud, ok := o.d.Provider.(Cloud)
	if !ok {
		return none("the fencer cannot say what the cloud reports about %s", leader.Name)
	}
	state, err := cloud.PeerState(ctx, leader)
	if err != nil {
		return none("the cloud was not asked: %v", err)
	}
	if !state.Down() {
		return none("the cloud reports %s %s (%s)", leader.Name, state.State, state.Detail)
	}

	// 4. The rest of the gates and the preconditions: maintenance, cooldown, the release window,
	// and replicas whose lag is known and within the limit.
	self, err := o.node(ctx, snap.Self.ID)
	if err != nil {
		return none("%v", err)
	}
	if reason := m.gates(ctx, snap, "", leader, self); reason != "" {
		return none("%s", reason)
	}
	// A project with no replica has an unknown lag, and no automatic move runs on one: restoring it
	// from the archive loses up to archive_timeout of its writes, which an operator chooses
	// (--restore-missing). The plan then refuses the whole server and the gate says why.
	opts := ServerOptions{}
	pl, err := o.PlanServer(ctx, opts)
	if err != nil {
		return none("the plan failed: %v", err)
	}
	if blocked := pl.Refused(false); len(blocked) > 0 {
		return none("%s: %s", blocked[0].Name, blocked[0].Detail)
	}
	// The plan pings the leader once more. A leader that answers now is not stopped, cleanly or
	// otherwise, by a monitor that saw it silent a moment ago.
	if pl.Kind != string(registry.MoveFailover) {
		return none("the leader %s answers again", leader.Name)
	}
	opts.ExpectKind, opts.ExpectEpoch = pl.Kind, pl.Epoch

	o.d.Log.Warn("automatic server failover", "leader", leader.Name, "silent", now.Sub(m.leaderSilentSince).String(), "cloud", state.State)
	_, err = o.FailoverServer(ctx, opts)
	m.mu.Lock()
	m.leaderSilentSince = time.Time{}
	m.mu.Unlock()
	return Decision{Action: "server", Reason: fmt.Sprintf("the leader %s is down (%s) and the service address does not answer", leader.Name, state.State), Err: err}
}

// projectTick is the leader's look at its projects.
func (m *Monitor) projectTick(ctx context.Context, snap cluster.Snapshot) Decision {
	o := m.o
	now := o.d.Now()
	ps, err := o.store().ListProjects(ctx)
	if err != nil {
		return none("the projects cannot be listed: %v", err)
	}
	grace := o.conf().ProjectGrace()
	var due []string
	m.mu.Lock()
	seen := map[string]bool{}
	for _, p := range ps {
		if p.Ref == config.SystemRef || p.Branch != nil {
			continue
		}
		seen[p.Ref] = true
		if p.Status != registry.StatusActiveUnhealthy {
			delete(m.unhealthySince, p.Ref)
			continue
		}
		if _, ok := m.unhealthySince[p.Ref]; !ok {
			m.unhealthySince[p.Ref] = now
		}
		if now.Sub(m.unhealthySince[p.Ref]) >= grace {
			due = append(due, p.Ref)
		}
	}
	for ref := range m.unhealthySince {
		if !seen[ref] {
			delete(m.unhealthySince, ref)
		}
	}
	m.mu.Unlock()
	if len(due) == 0 {
		return none("no project has been unhealthy for %s", grace)
	}

	// One move per look, but not one project per look: the moves are serialized anyway, and a
	// project that cannot move (no replica, a replica behind, a home that does not answer) must not
	// hold back the due projects after it. The first one that passes every gate is the move.
	var refused []string
	for _, ref := range due {
		reason := m.projectGate(ctx, snap, ref)
		if reason != "" {
			refused = append(refused, reason)
			continue
		}
		o.d.Log.Warn("automatic project failover", "ref", ref, "unhealthy_for", now.Sub(m.since(ref)).String())
		_, err = o.FailoverProject(ctx, ProjectOptions{Ref: ref, ExpectKind: string(registry.MoveFailover)})
		return Decision{Action: "project", Ref: ref, Reason: fmt.Sprintf("the primary of %s was unhealthy for %s", ref, grace), Err: err}
	}
	if len(refused) > maxReasons {
		refused = append(refused[:maxReasons], fmt.Sprintf("and %d more", len(refused)-maxReasons))
	}
	return Decision{Action: "none", Ref: due[0], Reason: strings.Join(refused, "; ")}
}

// maxReasons is how many refusals a look reports when it refused several projects.
const maxReasons = 3

// projectGate plans the failover of one project and checks the gates of an automatic move. It
// returns the reason the project is not failed over now, or "" when it is to be.
func (m *Monitor) projectGate(ctx context.Context, snap cluster.Snapshot, ref string) string {
	o := m.o
	pl, err := o.PlanProject(ctx, ProjectOptions{Ref: ref})
	if err != nil {
		return fmt.Sprintf("project %s: the plan failed: %v", ref, err)
	}
	if blocked := pl.Refused(false); len(blocked) > 0 {
		return fmt.Sprintf("project %s: %s: %s", ref, blocked[0].Name, blocked[0].Detail)
	}
	if pl.Kind != string(registry.MoveFailover) {
		return fmt.Sprintf("project %s: its primary answers now", ref)
	}
	home, err := o.node(ctx, pl.From)
	if err != nil {
		return err.Error()
	}
	target, err := o.node(ctx, pl.To)
	if err != nil {
		return err.Error()
	}
	if reason := m.gates(ctx, snap, ref, home, target); reason != "" {
		return fmt.Sprintf("project %s: %s", ref, reason)
	}
	return ""
}

// turn is the place of node self among the followers that could take over from the leader: 0 for
// the first by node id. A node that is not in the list is last.
func (o *Orchestrator) turn(self, leader string) int {
	var ids []string
	for _, n := range o.d.Members.Nodes() {
		if n.State == registry.NodeActive && n.ID != leader {
			ids = append(ids, n.ID)
		}
	}
	sort.Strings(ids)
	for i, id := range ids {
		if id == self {
			return i
		}
	}
	return len(ids)
}

func (m *Monitor) since(ref string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unhealthySince[ref]
}
