package failover

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// The per-project half of a server move (design 2.10.4 steps 8 and 9). Projects are promoted
// four at a time, in the order of their sequence numbers. A project that fails does not stop the
// others: the move reports it and a resume runs only what is left.

const (
	// serverParallel is how many projects a server move promotes at once.
	serverParallel = 4
	// backupParallel is how many base backups on the new timeline run at once.
	backupParallel = 2
	// seedTimeout bounds the wait for a standby built from the archive.
	seedTimeout = 30 * time.Minute
)

// moveProjects promotes every project of the plan. It returns the failures, one line each.
func (o *Orchestrator) moveProjects(ctx context.Context, j *journal, run *serverRun, rec serverPlanRecord, timeout int) []string {
	var mu sync.Mutex
	var failed []string
	var g errgroup.Group
	g.SetLimit(serverParallel)
	for _, ch := range rec.Projects {
		g.Go(func() error {
			if err := o.serverProject(ctx, j, run, ch, timeout); err != nil {
				mu.Lock()
				failed = append(failed, fmt.Sprintf("project %s: %v", ch.Ref, err))
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	sort.Strings(failed)
	return failed
}

// serverProject moves one project to its node. The steps are recorded as p:<ref>:<step>, and
// p:<ref>:done ends the project.
func (o *Orchestrator) serverProject(ctx context.Context, j *journal, run *serverRun, ch projectRecord, timeout int) (err error) {
	ref, pfx := ch.Ref, "p:"+ch.Ref+":"
	if j.has(pfx + "done") {
		return nil
	}
	st := o.store()
	to, err := o.node(ctx, ch.Node)
	if err != nil {
		return err
	}
	if !ch.Paused {
		_ = st.SetProjectStatus(ctx, ref, registry.StatusComingUp)
	}
	defer func() {
		if err != nil {
			_ = st.SetProjectStatus(ctx, ref, registry.StatusActiveUnhealthy)
		}
	}()

	identifier := ch.Replica
	if ch.Restore {
		if err := j.step(ctx, pfx+"seed", func() (string, error) { return o.seedFromArchive(ctx, run, ch, to) }); err != nil {
			return err
		}
		identifier = j.detail(pfx + "seed")
	}
	if err := j.step(ctx, pfx+"promote", func() (string, error) {
		a := promoteArgs{Epoch: run.epoch, Timeout: timeout, Drain: !run.planned || ch.Restore}
		if run.planned && !ch.Restore {
			if a.WaitLSN = j.detail("stopped:" + ref); a.WaitLSN == "" {
				return "", errors.New("the old leader reported no final position for the project")
			}
		}
		d, err := o.promoteReplica(ctx, to, identifier, a)
		var abort *abortError
		if errors.As(err, &abort) { // the leadership has moved: there is no going back for one project
			err = abort.cause
		}
		return d, err
	}); err != nil {
		return err
	}
	if err := j.step(ctx, pfx+"homed", func() (string, error) {
		if err := st.SetProjectNode(ctx, ref, to.ID, run.epoch); err != nil {
			return "", fmt.Errorf("moving the project's home to %s: %w", to.Name, err)
		}
		if !run.planned || ch.Restore {
			// A project restored from the archive may end before the old primary did: the old
			// primary cannot follow the new timeline and is rebuilt instead (reseed).
			return "-", nil
		}
		return o.addReplicaRow(ctx, ref, run.from, ch.Origin)
	}); err != nil {
		return err
	}
	if ch.Paused {
		if err := j.step(ctx, pfx+"started", func() (string, error) {
			if _, err := o.d.Primaries.Stop(ctx, to.ID, ref); err != nil {
				return "", fmt.Errorf("pausing the project on %s: %w", to.Name, err)
			}
			return "paused", nil
		}); err != nil {
			return err
		}
	} else {
		if err := j.step(ctx, pfx+"started", func() (string, error) {
			if err := o.whileTheNodeLearnsWhoLeads(ctx, func() error { return o.d.Primaries.Start(ctx, to.ID, ref) }); err != nil {
				return "", fmt.Errorf("starting the project on %s: %w", to.Name, err)
			}
			return "", nil
		}); err != nil {
			return err
		}
		if err := j.step(ctx, pfx+"tenant", func() (string, error) {
			// The engine registers a project with the shared services only while it is active.
			if err := st.SetProjectStatus(ctx, ref, registry.StatusActiveHealthy); err != nil {
				o.d.Log.Warn("could not set the project's status", "ref", ref, "error", err)
			}
			return "", o.ensureTenant(ctx, ref)
		}); err != nil {
			return err
		}
	}
	status := registry.StatusActiveHealthy
	if ch.Paused {
		status = registry.StatusInactive
	}
	if err := st.SetProjectStatus(ctx, ref, status); err != nil {
		o.d.Log.Warn("could not set the project's status", "ref", ref, "error", err)
	}
	return j.record(ctx, pfx+"done", to.Name)
}

// seedFromArchive builds a standby of a project that had no replica from the WAL archive alone,
// with no upstream, and waits until it replays. It returns the new replica's identifier.
func (o *Orchestrator) seedFromArchive(ctx context.Context, run *serverRun, ch projectRecord, to registry.Node) (string, error) {
	id, err := o.addReplicaRow(ctx, ch.Ref, to, registry.ReplicaManual)
	if err != nil {
		return "", err
	}
	st, err := o.d.Instances.Ensure(ctx, to.ID, peerapi.InstanceSpec{Identifier: id, Ref: ch.Ref, NoUpstream: true, Epoch: run.epoch})
	deadline := o.d.Now().Add(seedTimeout)
	for {
		switch {
		case err != nil:
			return "", fmt.Errorf("seeding a standby of %s on %s from the archive: %w", ch.Ref, to.Name, err)
		case st.Error != "":
			return "", fmt.Errorf("seeding a standby of %s on %s failed at %s: %s", ch.Ref, to.Name, st.Error, st.Detail)
		case st.PostgresUp && st.InRecovery:
			return id, nil
		case !o.d.Now().Before(deadline):
			return "", fmt.Errorf("the standby of %s on %s was not ready in %s (step %s)", ch.Ref, to.Name, seedTimeout, st.Step)
		}
		if err := o.wait(ctx, 5*time.Second); err != nil {
			return "", err
		}
		st, err = o.d.Instances.Observe(ctx, to.ID, id)
	}
}

// nodeWait bounds how long a move waits for the old leader to answer again, and nodePoll how often it
// asks. A variable so that tests can shorten it.
var (
	nodeWait = 3 * time.Minute
	nodePoll = 3 * time.Second
)

// waitForNode waits until node answers over the mesh and, when epoch is set, reports that epoch and
// leader: its copy of the registry has caught up with the move. The old leader of a switchover is
// down for the daemon's restart after its system cluster became a standby, and its copy of the
// registry trails the new leader's by a moment; the demotion of its other clusters needs both.
func (o *Orchestrator) waitForNode(ctx context.Context, n registry.Node, epoch int64, leader string) error {
	if o.d.Peers == nil {
		return nil
	}
	deadline := o.d.Now().Add(nodeWait)
	var last string
	for {
		p, err := o.d.Peers.Ping(ctx, n.ID)
		switch {
		case err != nil:
			last = err.Error()
		case epoch > 0 && (p.Epoch < epoch || leader != "" && p.Leader != leader):
			last = fmt.Sprintf("it answers and still reports epoch %d under %q; its daemon restarts by itself once its system cluster is a standby, and if it does not, restart supavise on it and run supavise failover --resume", p.Epoch, p.Leader)
		default:
			return nil
		}
		if !o.d.Now().Before(deadline) {
			return fmt.Errorf("%s does not answer as a follower of the new leader after %s: %s", n.Name, nodeWait, last)
		}
		if err := o.wait(ctx, nodePoll); err != nil {
			return err
		}
	}
}

// demoteOldLeader turns the clusters the old leader stopped into replicas of their new homes, in
// place: the system cluster first, because the node needs its registry back to render the
// others. The old leader's daemon restarts once its system cluster is a standby, so the others wait
// until it answers again with the new leader's epoch. It returns the failures.
func (o *Orchestrator) demoteOldLeader(ctx context.Context, j *journal, run *serverRun, rec serverPlanRecord, timeout int) []string {
	var failed []string
	demote := func(ref, identifier string) {
		step := "demote:" + ref
		if j.has(step) {
			return
		}
		if err := j.step(ctx, step, func() (string, error) {
			d, err := o.demoteOld(ctx, run.from, ref, identifier, run.epoch, timeout)
			if err != nil || ref == config.SystemRef {
				return d, err // the system cluster is not held: it is no project's primary
			}
			// The old leader's clusters were held when its quiesce stopped them; this one follows the new primary.
			if err := o.releaseOn(ctx, run.from.ID, ref, run.epoch); err != nil {
				return "", fmt.Errorf("%s follows the new primary, but the hold on its old primary could not be released: %w; run supavise failover --resume", run.from.Name, err)
			}
			return d, nil
		}); err != nil {
			failed = append(failed, fmt.Sprintf("demote %s: %v", ref, err))
		}
	}
	if !j.has("demote:" + config.SystemRef) {
		if err := o.waitForNode(ctx, run.from, 0, ""); err != nil {
			return []string{fmt.Sprintf("demote %s: %v", config.SystemRef, err)}
		}
	}
	demote(config.SystemRef, j.detail("system-homed"))
	if len(failed) > 0 { // the node's registry is not back: the rest would only wait for it
		return failed
	}
	var todo []projectRecord
	for _, ch := range rec.Projects {
		if j.has("p:"+ch.Ref+":done") && !j.has("demote:"+ch.Ref) && !j.has("reseed:"+ch.Ref) {
			todo = append(todo, ch)
		}
	}
	if len(todo) > 0 {
		if err := o.waitForNode(ctx, run.from, run.epoch, run.to.ID); err != nil {
			return []string{fmt.Sprintf("demote the projects of %s: %v", run.from.Name, err)}
		}
	}
	for _, ch := range todo {
		if ch.Restore {
			_ = j.record(ctx, "reseed:"+ch.Ref, o.reseedOld(ctx, run.from, ch.Ref, run.epoch))
			continue
		}
		demote(ch.Ref, j.detail("p:"+ch.Ref+":homed"))
	}
	return failed
}

// baseBackups takes a base backup of the system project and of each project that was promoted, on
// its new timeline, once per project: a project that finishes on a later --resume gets its own
// then, as base-backup:<ref>. A failure is a warning in the log: the projects run, and their
// backup timers take the next one. "base-backups" ends the step when every project has been
// promoted and had its turn.
func (o *Orchestrator) baseBackups(ctx context.Context, j *journal, run *serverRun, rec serverPlanRecord) {
	if j.has("base-backups") || o.d.Backups == nil {
		return
	}
	type target struct{ ref, node string }
	var targets []target
	if !j.has("base-backup:" + config.SystemRef) {
		targets = append(targets, target{config.SystemRef, run.to.ID})
	}
	complete := true
	for _, ch := range rec.Projects {
		switch {
		case ch.Paused:
		case !j.has("p:" + ch.Ref + ":done"):
			complete = false
		case !j.has("base-backup:" + ch.Ref):
			targets = append(targets, target{ch.Ref, ch.Node})
		}
	}
	var g errgroup.Group
	g.SetLimit(backupParallel)
	for _, t := range targets {
		g.Go(func() error {
			detail := "taken"
			if _, err := o.d.Backups.BaseBackup(ctx, t.node, t.ref, peerapi.BackupRequest{Epoch: run.epoch, Reason: "failover"}); err != nil {
				detail = "warning: " + err.Error()
			}
			_ = j.record(ctx, "base-backup:"+t.ref, detail)
			return nil
		})
	}
	_ = g.Wait()
	if !complete {
		return
	}
	var warned []string
	taken := 0
	for ref, d := range j.withPrefix("base-backup:") {
		if rest, ok := strings.CutPrefix(d, "warning: "); ok {
			warned = append(warned, fmt.Sprintf("%s (%s)", ref, rest))
		} else {
			taken++
		}
	}
	sort.Strings(warned)
	detail := fmt.Sprintf("%d taken", taken)
	if len(warned) > 0 {
		detail = "warning: no base backup on the new timeline for " + strings.Join(warned, ", ") + "; their backup timers take the next one"
	}
	_ = j.record(ctx, "base-backups", detail)
}
