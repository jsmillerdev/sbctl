package replicas

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/registry"
)

// The setup is a level-triggered machine over the replica's row. Each pass, a worker looks at the
// step the row holds and does what leads to the next one:
//
//	0_requested   the pass admits the replica (a free slot, a project that runs, room on the node)
//	1_started     take or find a base backup, ask the node to create the instance
//	2..4          the node seeds the standby; the worker watches its steps
//	5_replayed    the standby streams; the leader creates the Supavisor tenant
//	6_completed   ACTIVE_HEALTHY (or ACTIVE_UNHEALTHY, by the status mapping)
//
// A call that fails is tried again with a growing pause; when it keeps failing for Timeouts.Retry,
// or the node reports an error, or a step takes longer than its allowance, the row becomes
// INIT_READ_REPLICA_FAILED with the code of the step. A failed setup is not retried: Studio tells
// the user to drop the replica and add it again.

// replicable reports whether a project can be copied now: a project that is not the system
// project or a branch, and runs, so that a base backup can be taken or found.
func replicable(p *registry.Project) bool {
	if p.Ref == config.SystemRef || p.Branch != nil {
		return false
	}
	switch p.Status {
	case registry.StatusActiveHealthy, registry.StatusActiveUnhealthy, registry.StatusRestarting, registry.StatusResizing:
		return true
	}
	return false
}

// admit moves requested replicas to 1_started while there is a free slot and room. A replica that
// does not fit stays at 0_requested and the node gets a replica_capacity alert.
func (c *Controller) admit(ctx context.Context, nodes []registry.Node, projects []registry.Project, rows []registry.Replica) {
	nodeByID := make(map[string]registry.Node, len(nodes))
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}
	projByRef := make(map[string]*registry.Project, len(projects))
	for i := range projects {
		projByRef[projects[i].Ref] = &projects[i]
	}
	slots := c.cfg.Replicas.SetupConcurrency()
	for i := range rows {
		if r := &rows[i]; settingUp(r) && !r.IsSystemStandby() && r.InitStep != StepRequested {
			slots--
		}
	}
	hosted := map[string][]registry.Project{}
	waiting := map[string]int{}      // node -> replicas at 0_requested that could start, or admitted after a refusal and not launched yet
	cleared := map[string]bool{}     // node -> one was admitted this pass
	blocked := map[string][]string{} // node -> refs that wait for room
	reasons := map[string]string{}
	seeds := map[string]int64{}
	for i := range rows {
		r := &rows[i]
		if settingUp(r) && !r.IsSystemStandby() && r.InitStep == StepStarted && c.roomRefusal(r.Identifier) != nil {
			// Admitted again after the node refused it, and the node has not taken it yet: the
			// node is still the one this replica waits for, so its alert stays open.
			if n := nodeByID[r.NodeID]; n.ID != "" {
				waiting[n.ID]++
			}
			continue
		}
		if !settingUp(r) || r.IsSystemStandby() || r.InitStep != StepRequested {
			continue
		}
		p, n := projByRef[r.Ref], nodeByID[r.NodeID]
		if p == nil || n.ID == "" || n.State != registry.NodeActive || !replicable(p) {
			continue
		}
		waiting[n.ID]++
		refused := c.roomRefusal(r.Identifier)
		if refused != nil && c.now().Before(refused.until) {
			blocked[n.ID] = append(blocked[n.ID], r.Ref)
			reasons[n.ID] = refused.reason
			continue
		}
		if slots <= 0 {
			continue
		}
		if c.o.Admit != nil {
			h, ok := hosted[n.ID]
			if !ok {
				h = hostedOn(n.ID, projects, rows, r.Identifier)
			}
			seed, ok := seeds[r.Ref]
			if !ok {
				seed = c.seedSize(ctx, r.Ref)
				seeds[r.Ref] = seed
			}
			if err := c.o.Admit.Admit(ctx, AdmitRequest{Node: n, Project: *p, Hosted: h, SeedBytes: seed}); err != nil {
				blocked[n.ID] = append(blocked[n.ID], r.Ref)
				reasons[n.ID] = err.Error()
				continue
			}
			hosted[n.ID] = append(h, standIn(*p, r.Identifier))
		}
		if c.setStatus(ctx, r.Identifier, registry.ReplicaInit, StepStarted, "") {
			slots--
			// A replica the node refused is not a room found until the node takes it.
			cleared[n.ID] = cleared[n.ID] || refused == nil
		}
	}
	c.capacityAlerts(ctx, nodeByID, blocked, reasons, func(node string) bool { return waiting[node] == 0 || cleared[node] })
}

// hostedOn lists what node holds room for: the projects homed there and, for each replica there
// that is past admission, the replica's project under its identifier. The replica exclude is
// the one being admitted. The standby of the system cluster is not counted: the capacity
// accounting leaves the system project out, and a stand-in under the replica's identifier would
// make its 1 GiB class count as a hosted project.
func hostedOn(node string, projects []registry.Project, rows []registry.Replica, exclude string) []registry.Project {
	var out []registry.Project
	byRef := make(map[string]registry.Project, len(projects))
	for _, p := range projects {
		byRef[p.Ref] = p
		if p.NodeID == node {
			out = append(out, p)
		}
	}
	for _, r := range rows {
		if r.NodeID != node || r.Identifier == exclude || r.IsSystemStandby() || (settingUp(&r) && r.InitStep == StepRequested) {
			continue
		}
		if p, ok := byRef[r.Ref]; ok {
			out = append(out, standIn(p, r.Identifier))
		}
	}
	return out
}

// standIn is project p as a replica identified by id holds room on a node.
func standIn(p registry.Project, id string) registry.Project {
	p.Ref, p.NodeID = id, ""
	return p
}

// capacityAlerts opens a replica_capacity alert for each node that has replicas waiting for room
// and closes the ones whose wait is over: no replica waits there any more, or one was admitted.
func (c *Controller) capacityAlerts(ctx context.Context, nodes map[string]registry.Node, blocked map[string][]string, reasons map[string]string, settled func(node string) bool) {
	c.mu.Lock()
	var resolve []string
	for id := range c.capacityAlerted {
		if len(blocked[id]) == 0 && settled(id) {
			resolve = append(resolve, id)
			delete(c.capacityAlerted, id)
		}
	}
	var raise []string
	for id := range blocked {
		if !c.capacityAlerted[id] {
			raise = append(raise, id)
			c.capacityAlerted[id] = true
		}
	}
	c.mu.Unlock()
	for _, id := range resolve {
		c.alert(ctx, alerts.Event{Kind: alerts.KindReplicaCapacity, Severity: alerts.SeverityInfo, Key: "replica_capacity/" + id, Resolved: true,
			Title:  titleCapacity + nodeLabel(nodes[id]),
			Detail: "Read replicas fit on node " + nodeLabel(nodes[id]) + " again."})
	}
	for _, id := range raise {
		c.alert(ctx, alerts.Event{
			Kind: alerts.KindReplicaCapacity, Key: "replica_capacity/" + id,
			Title: titleCapacity + nodeLabel(nodes[id]),
			Detail: fmt.Sprintf("%d read replicas wait for room on node %s (%s). Free memory or disk there, raise [compute] overcommit, or remove replicas you do not need.",
				len(blocked[id]), nodeLabel(nodes[id]), reasons[id]),
		})
	}
}

// setupStep acts on a replica that is setting up.
func (c *Controller) setupStep(ctx context.Context, r *registry.Replica) {
	switch step := r.InitStep; step {
	case StepRequested:
		// admitted by the pass
	case StepStarted:
		c.launch(ctx, r)
	case StepLaunched, StepInitiated, StepDownloaded, StepReplayed:
		c.watch(ctx, r)
	case StepDone:
		c.finish(ctx, r)
	}
}

// launch takes or finds the base backup and asks the node to create the instance.
func (c *Controller) launch(ctx context.Context, r *registry.Replica) {
	if !c.due(r.Identifier, "launch") {
		return
	}
	epoch, _ := c.leader()
	bk, err := c.o.Backups.EnsureBase(ctx, r.Ref, c.cfg.Replicas.BootstrapMaxAge())
	if err != nil {
		c.stepError(ctx, r, "launch", fmt.Errorf("base backup: %w", err), false)
		return
	}
	spec := peerapi.InstanceSpec{Identifier: r.Identifier, Ref: r.Ref, BackupID: backup.BackupIDOf(bk), Epoch: epoch}
	st, err := c.o.Ops.Ensure(ctx, r.NodeID, spec)
	if noRoom(err) {
		c.waitForRoom(ctx, r, err)
		return
	}
	if err != nil {
		c.stepError(ctx, r, "launch", fmt.Errorf("create the instance on %s: %w", r.NodeID, err), terminal(err))
		return
	}
	c.worked(r.Identifier, "launch")
	c.mu.Lock()
	s := c.st(r.Identifier)
	s.backupID, s.seedBytes, s.backupLSN = spec.BackupID, bk.SizeBytes, bk.StopLSN
	s.room = nil
	c.mu.Unlock()
	c.observed(r.Identifier, st)
	if st.Error != "" {
		c.failSetup(ctx, r, st.Error, st.Detail)
		return
	}
	c.advance(ctx, r, laterStep(StepLaunched, boundStep(st.Step)))
}

// roomRetry is how long a replica that a node refused for lack of room waits before the node is
// asked again. The wait is not a failure: it is not bounded by Timeouts.Retry.
const roomRetry = time.Minute

// RoomError is what InstanceOps.Ensure returns when the node refused the replica because it has no
// room for it (memory, cores or disk), before it created anything. The replica then waits at
// 0_requested with a replica_capacity alert, as for a node the leader knows to be full, instead of
// failing its setup. Three kinds of error count, found through any wrapping: placement.ErrNoRoom,
// which is what the node's agent answers with (507) and the leader's remote calls give back; the
// *lifecycle.CapacityError a local admission produces; and any error with a NoRoom method that
// says so.
type RoomError interface {
	error
	NoRoom() bool
}

// noRoom reports whether err is a node's refusal for lack of room.
func noRoom(err error) bool {
	if err == nil {
		return false
	}
	var ce *lifecycle.CapacityError
	if errors.Is(err, placement.ErrNoRoom) || errors.As(err, &ce) {
		return true
	}
	var re RoomError
	return errors.As(err, &re) && re.NoRoom()
}

// roomRefusal returns the node's last refusal of the replica, or nil.
func (c *Controller) roomRefusal(id string) *roomRefusal {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.state[id]; s != nil {
		return s.room
	}
	return nil
}

// waitForRoom puts back a replica that the node has no room for: the row returns to 0_requested,
// where the pass holds it for roomRetry, raises the node's replica_capacity alert and asks again.
func (c *Controller) waitForRoom(ctx context.Context, r *registry.Replica, err error) {
	c.mu.Lock()
	s := c.st(r.Identifier)
	s.room = &roomRefusal{until: c.now().Add(roomRetry), reason: err.Error()}
	delete(s.calls, "launch") // a refusal is not a failed call
	c.mu.Unlock()
	if c.setStatus(ctx, r.Identifier, registry.ReplicaInit, StepRequested, "") {
		c.log.Info("replicas: the node has no room for the replica, it waits", "identifier", r.Identifier, "node", r.NodeID, "reason", err.Error())
	}
}

// boundStep limits a step an agent reports to the ones it can reach by itself: the leader writes
// 6_completed after it created the Supavisor tenant.
func boundStep(step string) string {
	if stepIndex(step) >= stepIndex(StepDone) {
		return StepReplayed
	}
	return step
}

// watch follows the node's seeding of the standby through steps 2 to 5.
func (c *Controller) watch(ctx context.Context, r *registry.Replica) {
	st, err := c.o.Ops.Observe(ctx, r.NodeID, r.Identifier)
	if err != nil {
		c.stepError(ctx, r, "observe", fmt.Errorf("observe %s on %s: %w", r.Identifier, r.NodeID, err), terminal(err))
		return
	}
	c.worked(r.Identifier, "observe")
	c.observed(r.Identifier, st)
	switch {
	case st.Error != "":
		c.failSetup(ctx, r, st.Error, st.Detail)
		return
	case st.Role == "absent":
		// The node does not have the instance (it was wiped, or it never got it): ask again. A node
		// that keeps answering and keeps not having it runs out the step's allowance below.
		if c.ensureAgain(ctx, r, time.Minute) {
			return
		}
	default:
		step := laterStep(r.InitStep, boundStep(st.Step))
		if step != r.InitStep && c.advance(ctx, r, step) {
			r.InitStep = step
		}
		if stepIndex(st.Step) >= stepIndex(StepDone) && st.PostgresUp && st.PostgRESTReady && st.InRecovery {
			c.finish(ctx, r)
			return
		}
	}
	age := c.stepClock(r.Identifier, r.InitStep)
	if age >= nudgeAfter && c.ensureAgain(ctx, r, nudgeAfter) {
		return
	}
	if age > c.stepTimeout(ctx, r) {
		c.failSetup(ctx, r, failureFor(r.InitStep), fmt.Sprintf("no progress for %s at %s", age.Round(time.Second), r.InitStep))
	}
}

// nudgeAfter is how long a step may sit unchanged before the node is asked for the instance
// again, and the shortest time between two such requests.
const nudgeAfter = 2 * time.Minute

// ensureAgain asks the node for the instance again. The node answers a repeated request with what
// it has and resumes a setup that a restart of its daemon interrupted, so this is how an
// instance the node lost comes back and how a setup that stopped moving is nudged. gap is the
// shortest time since the last request. It reports whether the setup failed.
func (c *Controller) ensureAgain(ctx context.Context, r *registry.Replica, gap time.Duration) (failed bool) {
	if !c.due(r.Identifier, "recreate") {
		return false
	}
	c.mu.Lock()
	s := c.st(r.Identifier)
	if c.now().Sub(s.reensured) < gap {
		c.mu.Unlock()
		return false
	}
	s.reensured = c.now()
	spec := peerapi.InstanceSpec{Identifier: r.Identifier, Ref: r.Ref, BackupID: s.backupID}
	c.mu.Unlock()
	spec.Epoch, _ = c.leader()
	st, err := c.o.Ops.Ensure(ctx, r.NodeID, spec)
	if err != nil {
		return c.stepError(ctx, r, "recreate", fmt.Errorf("ask %s for the instance again: %w", r.NodeID, err), terminal(err))
	}
	c.worked(r.Identifier, "recreate")
	c.observed(r.Identifier, st)
	if st.Error != "" {
		c.failSetup(ctx, r, st.Error, st.Detail)
		return true
	}
	return false
}

// finish does the leader's part of the last step, the Supavisor tenant, and marks the replica
// complete with the status the mapping gives it.
func (c *Controller) finish(ctx context.Context, r *registry.Replica) {
	st, ok := c.fresh(r.Identifier, 2*c.interval()+30*time.Second)
	if !ok || !st.PostgresUp || !st.PostgRESTReady {
		return // the next watch observes again
	}
	if c.o.Pooler != nil {
		if !c.due(r.Identifier, "pooler") {
			return
		}
		if err := c.o.Pooler.EnsureReplicaTenant(ctx, r.Ref, r.Identifier); err != nil {
			c.stepError(ctx, r, "pooler", fmt.Errorf("create the Supavisor tenant: %w", err), false)
			return
		}
	}
	c.worked(r.Identifier, "pooler")
	status, _ := c.health(ctx, r, &st)
	c.mu.Lock()
	c.st(r.Identifier).receiverDown = time.Time{}
	c.mu.Unlock()
	if c.setStatus(ctx, r.Identifier, status, StepDone, "") {
		c.log.Info("replicas: read replica is up", "identifier", r.Identifier, "node", r.NodeID, "status", status)
		r.Status, r.InitStep = status, StepDone
	}
}

// advance writes the step the setup reached, and reports whether the row now holds it.
func (c *Controller) advance(ctx context.Context, r *registry.Replica, step string) bool {
	if step == r.InitStep {
		return false
	}
	if c.setStatus(ctx, r.Identifier, registry.ReplicaInit, step, "") {
		c.log.Info("replicas: setup step", "identifier", r.Identifier, "step", step)
		c.stepReached(r.Identifier, step)
		return true
	}
	return false
}

// stepError handles a failed call at a step: it is tried again later unless it can never work or
// has failed for the whole retry window, in which case the setup fails and stepError reports it.
func (c *Controller) stepError(ctx context.Context, r *registry.Replica, key string, err error, never bool) (failed bool) {
	if c.failedCall(r.Identifier, key, err) || never {
		c.failSetup(ctx, r, failureFor(r.InitStep), err.Error())
		return true
	}
	c.log.Warn("replicas: setup step will be tried again", "identifier", r.Identifier, "step", r.InitStep, "error", err)
	return false
}

// failSetup ends the setup: INIT_READ_REPLICA_FAILED with the step's code. code is a failure code
// from the node, or another text, in which case the code of the current step is used.
func (c *Controller) failSetup(ctx context.Context, r *registry.Replica, code, detail string) {
	step := r.InitStep
	if !knownFailure(code) {
		if detail == "" {
			detail = code
		}
		code = failureFor(step)
	} else {
		// A node can say it failed a later step than the row shows.
		step = laterStep(step, failureStep(code))
	}
	if !c.setStatus(ctx, r.Identifier, registry.ReplicaInitError, step, code) {
		return
	}
	c.log.Error("replicas: setup failed", "identifier", r.Identifier, "node", r.NodeID, "step", step, "code", code, "detail", detail)
	c.alert(ctx, alerts.Event{
		Kind: alerts.KindReplicaUnhealthy, Ref: r.Ref, Key: "replica_unhealthy/" + r.Identifier,
		Title:  titleUnhealthy,
		Detail: fmt.Sprintf("The setup of the read replica %s of project %s on node %s failed (%s): %s. Remove it and add it again.", r.Identifier, r.Ref, r.NodeID, code, detail),
	})
	c.mu.Lock()
	c.st(r.Identifier).alerted["replica_unhealthy"] = true
	c.mu.Unlock()
}

// terminal reports whether a call's error can never go away by trying again: the node understood
// the request and refused it for what it is.
func terminal(err error) bool {
	var re *mesh.RemoteError
	if !errors.As(err, &re) {
		return false
	}
	switch re.Status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	}
	return re.Status >= 400 && re.Status < 500
}

// stepTimeout is how long the row may sit at its step before the setup fails.
func (c *Controller) stepTimeout(ctx context.Context, r *registry.Replica) time.Duration {
	est := c.estimate(ctx, r)
	switch r.InitStep {
	case StepLaunched:
		return c.to.Initiate
	case StepInitiated:
		return max(c.to.Download, 4*time.Duration(est.BaseBackupDownloadEstimateSeconds)*time.Second)
	case StepDownloaded:
		return max(c.to.Replay, 4*time.Duration(est.WALArchiveReplayEstimateSeconds)*time.Second)
	}
	return c.to.Complete
}

// stepReached keeps the clock of the download: the time between the standby starting to seed
// (3) and having the base backup (4) over its size is the download rate the estimates use.
func (c *Controller) stepReached(id, step string) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.st(id)
	switch step {
	case StepInitiated:
		s.downloadAt = now
	case StepDownloaded:
		if d := now.Sub(s.downloadAt); !s.downloadAt.IsZero() && d >= time.Second && s.seedBytes > 0 {
			rate := float64(s.seedBytes) / d.Seconds()
			if c.rate == 0 {
				c.rate = rate
			} else {
				c.rate = 0.7*c.rate + 0.3*rate
			}
		}
	}
}
