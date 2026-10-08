package failover

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// The preconditions of design 2.10.2, as data. A plan lists every check with its verdict, so
// that --dry-run prints them all and a refusal names the ones that failed; the same plan is what
// the move runs on.

const statusHealthy = "ACTIVE_HEALTHY"

// pass and fail build checks. Blocking checks refuse the move while they fail; --force
// overrides them unless they are hard.
func pass(name, detail string) Check {
	return Check{Name: name, OK: true, Detail: detail, Blocking: true}
}
func fail(name, detail string) Check   { return Check{Name: name, Detail: detail, Blocking: true} }
func hard(c Check) Check               { c.Hard = true; return c }
func advice(name, detail string) Check { return Check{Name: name, OK: true, Detail: detail} }

// replicaView is a replica as the preflight sees it: the registry row and what its node says.
type replicaView struct {
	Row registry.Replica
	// OK: the replica is a healthy standby that can be promoted.
	OK     bool
	Lag    *float64
	Detail string
}

// viewReplica asks the replica's node about the instance. The registry status alone is not
// enough: on a follower the rows are the leader's last word, and in an unplanned failover the
// leader is the one that stopped talking.
func (o *Orchestrator) viewReplica(ctx context.Context, r registry.Replica) replicaView {
	v := replicaView{Row: r}
	if r.Status != statusHealthy {
		v.Detail = fmt.Sprintf("status %s", r.Status)
		return v
	}
	if o.d.Instances == nil {
		v.Detail = "no way to ask the node"
		return v
	}
	obs, err := o.d.Instances.Observe(ctx, r.NodeID, r.Identifier)
	switch {
	case err != nil:
		v.Detail = fmt.Sprintf("node %s does not answer: %v", r.NodeID, err)
	case !obs.PostgresUp:
		v.Detail = fmt.Sprintf("Postgres of %s is down on %s", r.Identifier, r.NodeID)
	case !obs.InRecovery:
		v.Detail = fmt.Sprintf("%s on %s is not a standby", r.Identifier, r.NodeID)
	default:
		v.OK, v.Lag = true, obs.LagSeconds
	}
	return v
}

// viewParallel bounds how many nodes a plan asks at once.
const viewParallel = 8

// viewReplicas views each replica, viewParallel at a time, and returns the views by identifier. A
// plan of a cluster with dozens of projects asks as many nodes as it has replicas, and one after
// another a remote follower would make the plan outlast the timeouts of `status` and the readiness
// route.
func (o *Orchestrator) viewReplicas(ctx context.Context, reps []registry.Replica) map[string]replicaView {
	views := make([]replicaView, len(reps))
	var g errgroup.Group
	g.SetLimit(viewParallel)
	for i, r := range reps {
		g.Go(func() error {
			views[i] = o.viewReplica(ctx, r)
			return nil
		})
	}
	_ = g.Wait()
	by := make(map[string]replicaView, len(reps))
	for i, r := range reps {
		by[r.Identifier] = views[i]
	}
	return by
}

// lagCheck judges the lag of a replica view that is otherwise fine.
func (o *Orchestrator) lagCheck(name string, v replicaView) Check {
	limit := o.conf().MaxLag()
	switch {
	case v.Lag == nil:
		return fail(name, "the lag is unknown")
	case time.Duration(*v.Lag*float64(time.Second)) > limit:
		return fail(name, fmt.Sprintf("lag %s is above max_lag_seconds (%d)", formatSeconds(*v.Lag), int(limit.Seconds())))
	}
	return pass(name, fmt.Sprintf("lag %s", formatSeconds(*v.Lag)))
}

func formatSeconds(s float64) string {
	if s < 10 {
		return fmt.Sprintf("%.1fs", s)
	}
	return fmt.Sprintf("%.0fs", s)
}

// resolveNode finds a node by id or by name.
func (o *Orchestrator) resolveNode(ctx context.Context, idOrName string) (registry.Node, error) {
	nodes, err := o.store().ListNodes(ctx)
	if err != nil {
		return registry.Node{}, fmt.Errorf("failover: listing nodes: %w", err)
	}
	for _, n := range nodes {
		if n.ID == idOrName || n.Name == idOrName {
			return n, nil
		}
	}
	return registry.Node{}, fmt.Errorf("failover: no node %q: %w", idOrName, registry.ErrNotFound)
}

// versionCheck compares the releases of the two nodes: a promoted replica runs the same
// Postgres artifact as its primary only if both nodes run the same release.
func versionCheck(from, to registry.Node) Check {
	switch {
	case from.Version == "" || to.Version == "":
		return pass("same release", "a node has not reported its release")
	case from.Version != to.Version:
		return fail("same release", fmt.Sprintf("%s runs %s and %s runs %s: upgrade the older node first", from.Name, from.Version, to.Name, to.Version))
	}
	return pass("same release", from.Version)
}

// projectRun is a project move the plan found possible.
type projectRun struct {
	project registry.Project
	from    registry.Node
	to      registry.Node
	replica registry.Replica
	planned bool
	epoch   int64
}

// PlanProject checks the preconditions of a project switchover without changing anything.
func (o *Orchestrator) PlanProject(ctx context.Context, opts ProjectOptions) (*Plan, error) {
	pl, _, err := o.planProject(ctx, opts)
	return pl, err
}

func (o *Orchestrator) planProject(ctx context.Context, opts ProjectOptions) (*Plan, *projectRun, error) {
	st := o.store()
	p, err := st.GetProject(ctx, opts.Ref)
	if err != nil {
		return nil, nil, fmt.Errorf("failover: project %s: %w", opts.Ref, err)
	}
	cl, err := st.GetCluster(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failover: reading the cluster: %w", err)
	}
	pl := &Plan{Ref: p.Ref, From: p.NodeID, Epoch: cl.Epoch}
	run := &projectRun{project: *p, epoch: cl.Epoch}
	prior, err := unfinishedProjectMove(ctx, st, p.Ref)
	if err != nil {
		return nil, nil, err
	}
	resuming := prior != nil && opts.Resume
	pl.Checks = append(pl.Checks, o.projectChecks(p, resuming)...)

	switch {
	case prior != nil && !opts.Resume:
		pl.Checks = append(pl.Checks, hard(fail("unfinished move", fmt.Sprintf("move %d of this project stopped at %s: run it again with --resume", prior.ID, lastStep(*prior)))))
	case prior != nil:
		pl.Kind, pl.To = string(prior.Kind), prior.ToNode
		pl.Checks = append(pl.Checks, pass("unfinished move", fmt.Sprintf("move %d (%s %s to %s) continues after %s", prior.ID, prior.Kind, prior.FromNode, prior.ToNode, lastStep(*prior))))
	case opts.Resume:
		pl.Checks = append(pl.Checks, hard(fail("unfinished move", "there is none to resume")))
	}
	if fs, err := readStateFile(o.d.Cfg.Paths().FailoverState()); err != nil {
		return nil, nil, err
	} else if fs != nil {
		pl.Checks = append(pl.Checks, hard(fail("server move", "a server failover is unfinished on this node; finish it first with supavise failover --resume")))
	}
	if !o.d.Members.IsLeader() {
		pl.Checks = append(pl.Checks, hard(fail("leader", "only the leader moves a project: run this on "+o.leaderName())))
	}
	if resuming {
		return pl, run, nil
	}

	from, err := o.node(ctx, p.NodeID)
	if err != nil {
		return nil, nil, err
	}
	run.from = from
	pl.FromName = from.Name
	if from.State != registry.NodeActive {
		pl.Checks = append(pl.Checks, hard(fail("home node", fmt.Sprintf("%s is %s", from.Name, from.State))))
	} else {
		pl.Checks = append(pl.Checks, pass("home node", fmt.Sprintf("%s (%s) is active", from.Name, from.ID)))
	}

	rep, ok := o.pickReplica(ctx, p.Ref, opts.To, pl)
	if !ok {
		return pl, run, nil
	}
	run.replica = rep.Row
	to, err := o.node(ctx, rep.Row.NodeID)
	if err != nil {
		return nil, nil, err
	}
	run.to = to
	pl.To, pl.ToName = to.ID, to.Name
	pl.Projects = []ProjectPlan{{Ref: p.Ref, Replica: rep.Row.Identifier, Node: to.ID, LagSeconds: rep.Lag}}
	if to.State != registry.NodeActive {
		pl.Checks = append(pl.Checks, hard(fail("target node", fmt.Sprintf("%s is %s", to.Name, to.State))))
	}
	if rep.OK {
		pl.Checks = append(pl.Checks, pass("replica", fmt.Sprintf("%s on %s is a healthy standby", rep.Row.Identifier, to.Name)), o.lagCheck("replica lag", rep))
	} else {
		pl.Checks = append(pl.Checks, fail("replica", rep.Detail))
	}
	pl.Checks = append(pl.Checks, versionCheck(from, to))
	pl.Checks = append(pl.Checks, advice("capacity", fmt.Sprintf("%s already holds the replica, which counts against its capacity", to.Name)))

	// A project whose primary answers is switched over (the old primary stops cleanly and nothing
	// is lost); one that does not is failed over and its old primary is fenced first. A paused
	// project has no answering primary on purpose: its cluster is stopped cleanly already, so it is
	// switched over too, and its control file gives the position the replica must reach.
	healthy, detail, herr := o.projectHealthy(ctx, from, p.Ref)
	switch {
	case herr != nil:
		pl.Kind = string(registry.MoveFailover)
		pl.Checks = append(pl.Checks, hard(fail("home answers", fmt.Sprintf("%s does not answer (%v): that is a node failure, %s", from.Name, herr, o.nodeFailureAdvice(from)))))
	case healthy:
		pl.Kind = string(registry.MoveSwitchover)
		pl.Checks = append(pl.Checks, advice("primary", "healthy: a switchover, the old primary stops cleanly and becomes a replica"))
	case p.Status == registry.StatusInactive:
		pl.Kind = string(registry.MoveSwitchover)
		pl.Checks = append(pl.Checks, advice("primary", "paused: a switchover, the stopped primary becomes a replica of the new home and the project stays paused"))
	default:
		pl.Kind = string(registry.MoveFailover)
		pl.Checks = append(pl.Checks, advice("primary", fmt.Sprintf("not healthy (%s): a failover, the old primary is fenced first and loses what the replica has not received", detail)))
	}
	run.planned = pl.Kind == string(registry.MoveSwitchover)
	if err := o.pingCheck(ctx, pl, to); err != nil {
		return nil, nil, err
	}
	return pl, run, nil
}

// nodeFailureAdvice says what an operator does about a project whose home node is down. Only the
// server failover fences a node, and it moves only the projects homed on the old leader; a
// follower's projects have no move that covers them.
func (o *Orchestrator) nodeFailureAdvice(home registry.Node) string {
	if l, ok := o.d.Members.Leader(); ok && l.ID == home.ID {
		return "run supavise failover on a survivor"
	}
	return fmt.Sprintf("and %s does not lead: no move fences a node but the leader's, so the project stays down until %s is back, and its replica keeps the data", home.Name, home.Name)
}

func (o *Orchestrator) leaderName() string {
	if n, ok := o.d.Members.Leader(); ok {
		return n.Name
	}
	return "the leader"
}

// projectChecks are the checks on the project itself.
func (o *Orchestrator) projectChecks(p *registry.Project, resuming bool) []Check {
	switch {
	case p.Ref == config.SystemRef:
		return []Check{hard(fail("project", "the system project moves with the server: use supavise failover"))}
	case p.Branch != nil:
		return []Check{hard(fail("project", "a branch has no replica to promote"))}
	}
	switch p.Status {
	case registry.StatusActiveHealthy, registry.StatusActiveUnhealthy, registry.StatusInactive:
		return []Check{pass("project", fmt.Sprintf("%s is %s", p.Ref, p.Status))}
	case registry.StatusRestarting, registry.StatusComingUp:
		if resuming { // the move set it
			return []Check{pass("project", fmt.Sprintf("%s is %s: the move that stopped set that", p.Ref, p.Status))}
		}
	}
	return []Check{hard(fail("project", fmt.Sprintf("%s is %s: wait for it to settle", p.Ref, p.Status)))}
}

// pickReplica chooses the replica to promote: the one on the named node, else the healthiest.
// It adds the checks that explain a choice that cannot be made.
func (o *Orchestrator) pickReplica(ctx context.Context, ref, to string, pl *Plan) (replicaView, bool) {
	reps, err := o.store().ListReplicas(ctx, ref)
	if err != nil {
		pl.Checks = append(pl.Checks, hard(fail("replica", err.Error())))
		return replicaView{}, false
	}
	if len(reps) == 0 {
		pl.Checks = append(pl.Checks, hard(fail("replica", "the project has no replica: add one with supavise replicas add")))
		return replicaView{}, false
	}
	var views []replicaView
	for _, r := range reps {
		if to != "" {
			n, err := o.resolveNode(ctx, to)
			if err != nil {
				pl.Checks = append(pl.Checks, hard(fail("target node", err.Error())))
				return replicaView{}, false
			}
			if r.NodeID != n.ID {
				continue
			}
		}
		views = append(views, o.viewReplica(ctx, r))
	}
	if len(views) == 0 {
		pl.Checks = append(pl.Checks, hard(fail("replica", fmt.Sprintf("the project has no replica on %s", to))))
		return replicaView{}, false
	}
	return bestReplica(views), true
}

// bestReplica prefers a healthy replica with the least lag; unknown lag sorts last.
func bestReplica(views []replicaView) replicaView {
	sort.SliceStable(views, func(i, j int) bool {
		a, b := views[i], views[j]
		if a.OK != b.OK {
			return a.OK
		}
		if (a.Lag == nil) != (b.Lag == nil) {
			return a.Lag != nil
		}
		if a.Lag != nil && *a.Lag != *b.Lag {
			return *a.Lag < *b.Lag
		}
		return a.Row.NodeID < b.Row.NodeID
	})
	return views[0]
}

// projectHealthy asks the home node whether the project's primary answers.
func (o *Orchestrator) projectHealthy(ctx context.Context, home registry.Node, ref string) (bool, string, error) {
	if o.d.Primaries == nil {
		return false, "", errors.New("no way to ask the node")
	}
	return o.d.Primaries.Healthy(ctx, home.ID, ref)
}

// pingCheck adds the check that the target answers over the mesh. The node that holds the lock
// of a project move must reach the node it moves the project to.
func (o *Orchestrator) pingCheck(ctx context.Context, pl *Plan, to registry.Node) error {
	if to.ID == o.self().ID || o.d.Peers == nil {
		return nil
	}
	if _, err := o.d.Peers.Ping(ctx, to.ID); err != nil {
		pl.Checks = append(pl.Checks, fail("target reachable", fmt.Sprintf("%s does not answer: %v", to.Name, err)))
		return nil
	}
	pl.Checks = append(pl.Checks, pass("target reachable", to.Name+" answers"))
	return nil
}

func lastStep(m registry.Move) string {
	if len(m.Steps) == 0 {
		return "the start"
	}
	return m.Steps[len(m.Steps)-1].Name
}

// serverRun is a server move the plan found possible.
type serverRun struct {
	from, to registry.Node
	planned  bool
	epoch    int64
	flags    serverFlags
	// systemReplica is the identifier of the standby of the system cluster on the new leader.
	systemReplica string
	projects      []projectChoice
}

// projectChoice is what a server move does with one project.
type projectChoice struct {
	Ref string
	// Replica is the replica to promote, nil when the project has none.
	Replica *registry.Replica
	// Node is the node the project is homed on afterwards.
	Node string
	// Restore: build a standby from the archive on Node (--restore-missing).
	Restore bool
	Lag     *float64
	Paused  bool
}

// record is the form of the choice that the first step of a move writes down.
func (c projectChoice) record() projectRecord {
	r := projectRecord{Ref: c.Ref, Node: c.Node, Restore: c.Restore, Paused: c.Paused}
	if c.Replica != nil {
		r.Replica, r.Origin = c.Replica.Identifier, c.Replica.Origin
	}
	return r
}

// PlanServer checks the preconditions of a server switchover or failover.
func (o *Orchestrator) PlanServer(ctx context.Context, opts ServerOptions) (*Plan, error) {
	pl, _, err := o.planServer(ctx, opts)
	return pl, err
}

func (o *Orchestrator) planServer(ctx context.Context, opts ServerOptions) (*Plan, *serverRun, error) {
	st := o.store()
	cl, err := st.GetCluster(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failover: reading the cluster: %w", err)
	}
	pl := &Plan{Epoch: cl.Epoch + 1}
	run := &serverRun{epoch: cl.Epoch + 1, flags: serverFlags{Force: opts.Force, RestoreMissing: opts.RestoreMissing, OldPrimaryIsDown: opts.OldPrimaryIsDown}}

	// An unfinished move decides everything: the run continues it, with its own nodes and epoch.
	if prior, fs, err := o.unfinishedServer(ctx); err != nil {
		return nil, nil, err
	} else if prior != nil && !opts.Resume {
		pl.Checks = append(pl.Checks, hard(fail("unfinished move", fmt.Sprintf("a server move to %s stopped at %s: run it again with --resume", prior.to, prior.last))))
		pl.Epoch = prior.epoch
	} else if prior != nil {
		pl.Kind, pl.From, pl.To, pl.Epoch = string(prior.kind), prior.from, prior.to, prior.epoch
		pl.Checks = append(pl.Checks, pass("unfinished move", fmt.Sprintf("the %s of %s to %s continues after %s", prior.kind, prior.from, prior.to, prior.last)))
		run.epoch, run.planned = prior.epoch, prior.kind == registry.MoveSwitchover
		if fs != nil {
			run.flags = fs.Flags
		}
		run.flags.Force = run.flags.Force || opts.Force
		if run.from, err = o.node(ctx, prior.from); err != nil {
			return nil, nil, err
		}
		if run.to, err = o.node(ctx, prior.to); err != nil {
			return nil, nil, err
		}
		pl.FromName, pl.ToName = run.from.Name, run.to.Name
		return pl, run, nil
	} else if opts.Resume {
		pl.Checks = append(pl.Checks, hard(fail("unfinished move", "there is none to resume")))
		return pl, run, nil
	}

	// The two nodes.
	toNode := o.self()
	if opts.To != "" {
		if toNode, err = o.resolveNode(ctx, opts.To); err != nil {
			pl.Checks = append(pl.Checks, hard(fail("target node", err.Error())))
			return pl, run, nil
		}
	} else if toNode, err = o.node(ctx, toNode.ID); err != nil { // the registry's word, not the daemon's last look
		return nil, nil, err
	}
	leader, ok := o.d.Members.Leader()
	if !ok {
		pl.Checks = append(pl.Checks, hard(fail("leader", "the cluster has no known leader")))
		return pl, run, nil
	}
	run.from, run.to = leader, toNode
	pl.From, pl.To, pl.FromName, pl.ToName = leader.ID, toNode.ID, leader.Name, toNode.Name
	switch {
	case leader.ID == toNode.ID:
		pl.Checks = append(pl.Checks, hard(fail("target node", fmt.Sprintf("%s is the leader already: run this on the node that should lead, or name it with --to", toNode.Name))))
		return pl, run, nil
	case toNode.State != registry.NodeActive:
		pl.Checks = append(pl.Checks, hard(fail("target node", fmt.Sprintf("%s is %s", toNode.Name, toNode.State))))
		return pl, run, nil
	}
	pl.Checks = append(pl.Checks, pass("target node", fmt.Sprintf("%s (%s) is active and takes over from %s", toNode.Name, toNode.ID, leader.Name)))
	pl.Checks = append(pl.Checks, versionCheck(leader, toNode))

	// Planned or not: a leader that answers is stopped cleanly, one that does not is fenced.
	alive := o.leaderAlive(ctx, leader)
	run.planned = alive && !opts.OldPrimaryIsDown
	if run.planned {
		pl.Kind = string(registry.MoveSwitchover)
		pl.Checks = append(pl.Checks, advice("leader", fmt.Sprintf("%s answers: a switchover, it stops cleanly and nothing is lost", leader.Name)))
	} else {
		pl.Kind = string(registry.MoveFailover)
		pl.Checks = append(pl.Checks, o.failoverChecks(ctx, leader, toNode, alive, opts)...)
	}

	pl.Checks = append(pl.Checks, o.storageCheck())
	pl.Checks = append(pl.Checks, o.markerCheck(ctx, run.epoch)...)
	o.planProjects(ctx, pl, run, opts)
	if o.d.Extra != nil {
		pl.Checks = append(pl.Checks, o.d.Extra(ctx, toNode)...)
	}
	return pl, run, nil
}

// failoverChecks are the checks of a failover: the old leader is down or is asserted to be, and
// something must make sure it cannot write.
func (o *Orchestrator) failoverChecks(ctx context.Context, leader, to registry.Node, alive bool, opts ServerOptions) []Check {
	var cs []Check
	prov := o.d.Provider
	switch {
	case prov.Name() == "manual" && alive:
		cs = append(cs, hard(fail("old leader", fmt.Sprintf("%s still answers and no fencing method is configured: run a switchover instead, without --old-primary-is-down", leader.Name))))
	case prov.Name() == "manual" && !opts.OldPrimaryIsDown:
		cs = append(cs, hard(fail("fencing", fmt.Sprintf("%s does not answer and no fencing method is configured: stop it yourself and run again with --old-primary-is-down", leader.Name))))
	case prov.Name() == "manual":
		cs = append(cs, advice("fencing", fmt.Sprintf("none configured: you assert that %s is down; the cooperative fence is tried and the DNS change is printed", leader.Name)))
	default:
		if err := prov.Probe(ctx); err != nil {
			cs = append(cs, hard(fail("fencing", fmt.Sprintf("%s fencer: %v", prov.Name(), err))))
		} else {
			cs = append(cs, pass("fencing", fmt.Sprintf("%s fencer is permitted and stops %s before anything is promoted", prov.Name(), leader.Name)))
		}
	}
	if alive {
		cs = append(cs, advice("old leader", fmt.Sprintf("%s answers but is treated as down (--old-primary-is-down): it is fenced first", leader.Name)))
	} else {
		cs = append(cs, advice("old leader", fmt.Sprintf("%s does not answer: a failover, the data written since the last replicated WAL may be lost", leader.Name)))
	}
	return cs
}

// leaderAlive asks the leader over the mesh. A leader that is this very node is alive, and a node
// cannot ask itself over the mesh; a leader with no mesh to be asked over (a test) is taken as down.
func (o *Orchestrator) leaderAlive(ctx context.Context, leader registry.Node) bool {
	if leader.ID == o.self().ID {
		return true
	}
	if o.d.Peers == nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := o.d.Peers.Ping(cctx, leader.ID)
	return err == nil
}

// storageCheck: the Storage objects of a file backend exist only on the old node, so a server
// move without S3 would lose them (design 2.11).
func (o *Orchestrator) storageCheck() Check {
	if o.d.Cfg.Fleet.StorageBackend != "s3" {
		return hard(fail("storage backend", "[fleet] storage_backend is file, so the objects exist only on the old node: run supavise storage migrate --to s3 first"))
	}
	return pass("storage backend", "s3: the objects are not on the old node")
}

// recordsLeadership reports whether the Takeover records the leadership in the promoted system
// cluster itself, which makes the leader marker something a move can do without (LeadershipRecorder).
func (o *Orchestrator) recordsLeadership() bool {
	r, ok := o.d.Takeover.(LeadershipRecorder)
	return ok && r.RecordsLeadership()
}

// markerCheck: the epoch marker lives in the backup store; the store must be reachable, and
// must not already hold the epoch of the move or a higher one (another node was promoted). The
// marker is what names the survivor as the leader when its daemon restarts after the promotion, so
// a store that cannot be read is a hard failure unless the Takeover records the leadership itself.
func (o *Orchestrator) markerCheck(ctx context.Context, epoch int64) []Check {
	if o.d.Marker == nil {
		return []Check{hard(fail("epoch marker", "no leader marker store in this build or backup mode (an S3-compatible backup store has one), so the leader marker cannot be written"))}
	}
	m, err := o.d.Marker.ReadLeaderMarker(ctx)
	switch {
	case err != nil:
		c := fail("epoch marker", fmt.Sprintf("the leader marker in the backup store cannot be read: %v. If the store is up, the marker is malformed: delete _node/leader.json after you have checked which node leads", err))
		if !o.recordsLeadership() {
			c = hard(c)
		}
		return []Check{c}
	case m != nil && m.Epoch >= epoch:
		return []Check{hard(fail("epoch marker", fmt.Sprintf("the store holds epoch %d (leader %s), which is not below this move's %d: another node was promoted", m.Epoch, m.Leader, epoch)))}
	case m == nil:
		return []Check{pass("epoch marker", "the store is reachable and holds no marker")}
	}
	return []Check{pass("epoch marker", fmt.Sprintf("the store is reachable and holds epoch %d", m.Epoch))}
}

// planProjects chooses what happens to each project homed on the old leader, and checks the
// replicas it needs, the system cluster's first.
func (o *Orchestrator) planProjects(ctx context.Context, pl *Plan, run *serverRun, opts ServerOptions) {
	st := o.store()
	// The system cluster.
	sys, err := st.ListReplicas(ctx, config.SystemRef)
	var sysView *replicaView
	if err == nil {
		for _, r := range sys {
			if r.NodeID == run.to.ID {
				v := o.viewReplica(ctx, r)
				sysView = &v
			}
		}
	}
	switch {
	case sysView == nil:
		pl.Checks = append(pl.Checks, hard(fail("system replica", fmt.Sprintf("%s holds no standby of the system cluster", run.to.Name))))
	case !sysView.OK:
		pl.Checks = append(pl.Checks, fail("system replica", sysView.Detail))
	default:
		pl.Checks = append(pl.Checks, pass("system replica", fmt.Sprintf("%s on %s is a healthy standby", sysView.Row.Identifier, run.to.Name)), o.lagCheck("system lag", *sysView))
	}
	if sysView != nil {
		run.systemReplica = sysView.Row.Identifier
	}

	projects, err := st.ListProjects(ctx)
	if err != nil {
		pl.Checks = append(pl.Checks, hard(fail("projects", err.Error())))
		return
	}
	// Every replica of every project to move is viewed in one pass, in parallel.
	repsOf := map[string][]registry.Replica{}
	var all []registry.Replica
	for _, p := range projects {
		if p.Ref != config.SystemRef && p.NodeID == run.from.ID && p.Branch == nil {
			repsOf[p.Ref], _ = st.ListReplicas(ctx, p.Ref)
			all = append(all, repsOf[p.Ref]...)
		}
	}
	seen := o.viewReplicas(ctx, all)
	var without, skipped []string
	healthy := 0
	for _, p := range projects {
		if p.Ref == config.SystemRef || p.NodeID != run.from.ID {
			continue
		}
		if p.Branch != nil {
			skipped = append(skipped, p.Ref)
			continue
		}
		ch := projectChoice{Ref: p.Ref, Paused: p.Status == registry.StatusInactive}
		var views []replicaView
		for _, r := range repsOf[p.Ref] {
			views = append(views, seen[r.Identifier])
		}
		// A replica on the new leader wins: the project then lives where the control plane does.
		var best *replicaView
		for i := range views {
			if views[i].Row.NodeID == run.to.ID {
				best = &views[i]
			}
		}
		if best == nil && len(views) > 0 {
			b := bestReplica(append([]replicaView(nil), views...))
			best = &b
		}
		switch {
		case best == nil && opts.RestoreMissing:
			ch.Restore, ch.Node = true, run.to.ID
			without = append(without, p.Ref)
		case best == nil:
			without = append(without, p.Ref)
			continue
		default:
			r := best.Row
			ch.Replica, ch.Node, ch.Lag = &r, r.NodeID, best.Lag
			if !best.OK {
				pl.Checks = append(pl.Checks, fail("replica of "+p.Ref, best.Detail))
			} else if c := o.lagCheck("replica of "+p.Ref, *best); !c.OK {
				pl.Checks = append(pl.Checks, c)
			} else {
				healthy++
			}
		}
		run.projects = append(run.projects, ch)
		pp := ProjectPlan{Ref: p.Ref, Node: ch.Node, LagSeconds: ch.Lag, RestoreFromArchive: ch.Restore}
		if ch.Replica != nil {
			pp.Replica = ch.Replica.Identifier
		}
		pl.Projects = append(pl.Projects, pp)
	}
	if healthy > 0 {
		pl.Checks = append(pl.Checks, pass("replicas", fmt.Sprintf("%d project(s) have a healthy replica within max_lag_seconds", healthy)))
	}
	switch {
	case len(without) > 0 && opts.RestoreMissing:
		pl.Checks = append(pl.Checks, advice("projects without a replica", fmt.Sprintf("%d: %s will be restored from the archive (data loss up to archive_timeout)", len(without), listRefs(without))))
	case len(without) > 0:
		// Hard: --force does not skip them. A project with no replica would stay homed on the old
		// leader, which is stopped, and the move would report success.
		pl.Checks = append(pl.Checks, hard(fail("projects without a replica", fmt.Sprintf("%d: %s (--restore-missing builds their standby from the archive, with data loss up to archive_timeout)", len(without), listRefs(without)))))
	}
	if len(skipped) > 0 {
		pl.Notes = append(pl.Notes, fmt.Sprintf("%d branch project(s) have no replica and stay on %s: %s", len(skipped), run.from.Name, listRefs(skipped)))
	}
}

func listRefs(refs []string) string {
	if len(refs) <= 5 {
		return strings.Join(refs, ", ")
	}
	return strings.Join(refs[:5], ", ") + fmt.Sprintf(" and %d more", len(refs)-5)
}

// unfinishedMove summarizes a server move that did not finish.
type unfinishedMove struct {
	kind     registry.MoveKind
	from, to string
	epoch    int64
	last     string
	moveID   int64
	steps    []registry.MoveStep
	// running: the move was never finished, as when the daemon was cut off; false for a move that ended failed.
	running bool
}

// unfinishedServer finds the server move that stopped: in the registry when it has one, else in failover.json.
func (o *Orchestrator) unfinishedServer(ctx context.Context) (*unfinishedMove, *stateFile, error) {
	fs, err := readStateFile(o.d.Cfg.Paths().FailoverState())
	if err != nil {
		return nil, nil, err
	}
	m, rerr := unfinishedServerMove(ctx, o.store())
	if rerr != nil {
		return nil, nil, rerr
	}
	switch {
	case m != nil:
		return &unfinishedMove{kind: m.Kind, from: m.FromNode, to: m.ToNode, epoch: m.Epoch, last: lastStep(*m), moveID: m.ID, steps: m.Steps, running: m.State == registry.MoveRunning}, fs, nil
	case fs != nil && fs.State != registry.MoveAborted:
		mv := registry.Move{Steps: fs.Steps}
		return &unfinishedMove{kind: fs.Kind, from: fs.From, to: fs.To, epoch: fs.Epoch, last: lastStep(mv), steps: fs.Steps, running: fs.State == "" || fs.State == registry.MoveRunning}, fs, nil
	}
	return nil, nil, nil
}
