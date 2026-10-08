package replicas

import (
	"context"
	"fmt"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// Thresholds of the status mapping (design 2.7.7).
const (
	// receiverGrace is how long the receiver may not stream before the replica is unhealthy: a
	// restart or a short break of the mesh is not.
	receiverGrace = 2 * time.Minute
	// unreachableGrace is how long a node may not answer before its replicas are unhealthy.
	unreachableGrace = 2 * time.Minute
	// transientHold is how long RESTARTING and RESIZING may last before the replica counts as
	// unhealthy, if no healthy observation ended them.
	transientHold = 10 * time.Minute
	// lagWarn is the lag from which a replica that is still healthy raises replica_lag.
	lagWarn = 60 * time.Second
)

// health maps an observation to a status: ACTIVE_HEALTHY when the standby streams, lags less than
// unhealthy_lag_seconds and its PostgREST answers; ACTIVE_UNHEALTHY when the receiver has been down
// for two minutes, PostgREST is down, the lag is over the limit or the node has not answered for
// two minutes; and the current status when there is nothing new to say (a node that just missed an
// answer, a restart in progress). reason says why a replica is unhealthy.
func (c *Controller) health(ctx context.Context, r *registry.Replica, obs *peerapi.InstanceStatus) (status, reason string) {
	now := c.now()
	c.mu.Lock()
	s := c.st(r.Identifier)
	unreach, recvDown := s.unreachable, s.receiverDown
	transient := s.transient
	if (r.Status == statusRestart || r.Status == statusResizing) && transient.IsZero() {
		transient = now
		s.transient = now
	} else if r.Status != statusRestart && r.Status != statusResizing {
		s.transient, transient = time.Time{}, time.Time{}
	}
	obsAt, restarting := s.obsAt, s.restarting
	c.mu.Unlock()

	stay := r.Status
	if stay == registry.ReplicaInit {
		stay = statusHealthy
	}
	if restarting {
		return stay, ""
	}
	if !unreach.IsZero() {
		if now.Sub(unreach) >= unreachableGrace {
			return statusUnhealthy, fmt.Sprintf("node %s has not answered for %s", r.NodeID, now.Sub(unreach).Round(time.Second))
		}
		return stay, ""
	}
	if obs == nil {
		return stay, ""
	}
	bad := ""
	switch {
	case obs.Role == "primary":
		return stay, "" // promoted: the failover procedure owns the row now
	case obs.Role == "absent" || !obs.PostgresUp:
		bad = "Postgres is not running"
	case r.Origin != registry.ReplicaSystem && !obs.PostgRESTReady:
		bad = "PostgREST does not answer"
	case !recvDown.IsZero() && now.Sub(recvDown) >= receiverGrace:
		bad = fmt.Sprintf("the WAL receiver has not been streaming for %s", now.Sub(recvDown).Round(time.Second))
	case obs.LagSeconds != nil && time.Duration(*obs.LagSeconds*float64(time.Second)) > c.cfg.Replicas.UnhealthyLag():
		bad = fmt.Sprintf("replication lag is %.0f s, over the limit of %.0f s", *obs.LagSeconds, c.cfg.Replicas.UnhealthyLag().Seconds())
	}
	if bad != "" {
		if !transient.IsZero() && now.Sub(transient) < transientHold {
			return stay, "" // coming back from a restart or a resize
		}
		return statusUnhealthy, bad
	}
	if !transient.IsZero() && obsAt.Before(transient) {
		return stay, "" // the observation predates the restart
	}
	return statusHealthy, ""
}

// monitor looks at a replica that finished its setup: it polls the node unless a report is fresh,
// maps the observation to a status, writes it when it changed and raises or resolves the alerts.
func (c *Controller) monitor(ctx context.Context, r *registry.Replica, project registry.Status) {
	if paused(project) {
		return // the replica stops and starts with its project
	}
	obs, ok := c.fresh(r.Identifier, c.interval()-time.Second)
	if !ok {
		st, err := c.o.Ops.Observe(ctx, r.NodeID, r.Identifier)
		if err != nil {
			c.log.Debug("replicas: observe", "identifier", r.Identifier, "node", r.NodeID, "error", err)
			c.unreached(r.Identifier)
		} else {
			c.observed(r.Identifier, st)
			obs, ok = st, true
		}
	}
	var o *peerapi.InstanceStatus
	if ok {
		o = &obs
	}
	if ok && obs.Role == "absent" {
		c.recreateActive(ctx, r)
	}
	status, reason := c.health(ctx, r, o)
	if status != r.Status {
		if c.setStatus(ctx, r.Identifier, status, StepDone, "") {
			c.log.Info("replicas: status", "identifier", r.Identifier, "from", r.Status, "to", status, "reason", reason)
		}
	}
	c.healthAlerts(ctx, r, status, reason, o)
}

// recreateActive asks the node for an instance it has lost. The node seeds it again from the
// newest base backup, and the replica is unhealthy until it streams.
func (c *Controller) recreateActive(ctx context.Context, r *registry.Replica) {
	c.mu.Lock()
	s := c.st(r.Identifier)
	if c.now().Sub(s.reensured) < 5*time.Minute {
		c.mu.Unlock()
		return
	}
	s.reensured = c.now()
	c.mu.Unlock()
	epoch, _ := c.leader()
	st, err := c.o.Ops.Ensure(ctx, r.NodeID, peerapi.InstanceSpec{Identifier: r.Identifier, Ref: r.Ref, Epoch: epoch})
	if err != nil {
		c.log.Warn("replicas: create the lost instance again", "identifier", r.Identifier, "node", r.NodeID, "error", err)
		return
	}
	c.observed(r.Identifier, st)
}

// paused reports whether a project of that status is paused or going away: its replicas are
// stopped with it and say nothing.
func paused(s registry.Status) bool {
	switch s {
	case registry.StatusInactive, registry.StatusPausing, registry.StatusGoingDown, registry.StatusRemoved, registry.StatusRestoring:
		return true
	}
	return false
}

// healthAlerts raises replica_unhealthy and replica_lag when a replica turns unhealthy or lags,
// and resolves them when it is well again.
func (c *Controller) healthAlerts(ctx context.Context, r *registry.Replica, status, reason string, obs *peerapi.InstanceStatus) {
	if status == statusRestart || status == statusResizing {
		return // the alerts stay as they are until the replica is back
	}
	type edge struct {
		name string
		open bool
		ev   alerts.Event
	}
	var edges []edge
	edges = append(edges, edge{"replica_unhealthy", status == statusUnhealthy, alerts.Event{
		Kind: alerts.KindReplicaUnhealthy, Ref: r.Ref, Key: "replica_unhealthy/" + r.Identifier,
		Title:  "Read replica is unhealthy",
		Detail: fmt.Sprintf("The read replica %s of project %s on node %s is unhealthy: %s.", r.Identifier, r.Ref, r.NodeID, reason),
	}})
	lagging := status == statusHealthy && obs != nil && obs.LagSeconds != nil &&
		time.Duration(*obs.LagSeconds*float64(time.Second)) > min(lagWarn, c.cfg.Replicas.UnhealthyLag()/2)
	lagEv := alerts.Event{Kind: alerts.KindReplicaLag, Ref: r.Ref, Key: "replica_lag/" + r.Identifier, Title: "Read replica is behind"}
	if lagging {
		lagEv.Detail = fmt.Sprintf("The read replica %s of project %s on node %s is %.0f s behind its primary.", r.Identifier, r.Ref, r.NodeID, *obs.LagSeconds)
	}
	edges = append(edges, edge{"replica_lag", lagging, lagEv})
	for _, e := range edges {
		c.mu.Lock()
		s := c.st(r.Identifier)
		was := s.alerted[e.name]
		s.alerted[e.name] = e.open
		c.mu.Unlock()
		switch {
		case e.open && !was:
			c.alert(ctx, e.ev)
		case !e.open && was:
			e.ev.Resolved = true
			c.alert(ctx, e.ev)
		}
	}
}

// watchSystem keeps the row of a node's standby of the system cluster. The joining node seeds
// that standby itself, before its daemon runs, so the controller never creates it: it only
// reads what the node says and completes the row once the standby serves.
func (c *Controller) watchSystem(ctx context.Context, r *registry.Replica) {
	n, err := c.reg.GetNode(ctx, r.NodeID)
	if err != nil || n.State != registry.NodeActive {
		return // joining: the join procedure owns the node until it confirms
	}
	st, err := c.o.Ops.Observe(ctx, r.NodeID, r.Identifier)
	if err != nil {
		c.unreached(r.Identifier)
	} else {
		c.observed(r.Identifier, st)
	}
	obs, ok := c.fresh(r.Identifier, 2*c.interval()+30*time.Second)
	if ok && obs.Role == "replica" && obs.PostgresUp && obs.InRecovery && settingUp(r) {
		status, _ := c.health(ctx, r, &obs)
		c.setStatus(ctx, r.Identifier, status, StepDone, "")
		return
	}
	if active(r) {
		var o *peerapi.InstanceStatus
		if ok {
			o = &obs
		}
		status, reason := c.health(ctx, r, o)
		if status != r.Status {
			c.setStatus(ctx, r.Identifier, status, StepDone, "")
		}
		c.healthAlerts(ctx, r, status, reason, o)
	}
}

// ---- what the Management API reads ----------------------------------------------------------

// List implements Service.
func (c *Controller) List(ctx context.Context, ref string) ([]Replica, error) {
	rows, err := c.reg.ListReplicas(ctx, ref)
	if err != nil {
		return nil, err
	}
	nodes, err := c.reg.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]registry.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	out := make([]Replica, len(rows))
	for i, r := range rows {
		n := byID[r.NodeID]
		out[i] = Replica{Replica: r, Region: nodeRegion(n), PublicHost: n.PublicHost}
	}
	return out, nil
}

// Statuses implements Service.
func (c *Controller) Statuses(ctx context.Context, ref string) ([]Status, error) {
	rows, err := c.reg.ListReplicas(ctx, ref)
	if err != nil {
		return nil, err
	}
	out := make([]Status, len(rows))
	for i := range rows {
		r := &rows[i]
		out[i] = Status{Identifier: r.Identifier, Status: r.Status, Init: c.initStatus(ctx, r), LagSeconds: c.lagNow(r.Identifier)}
	}
	return out, nil
}

// initStatus is replicaInitializationStatus.
func (c *Controller) initStatus(ctx context.Context, r *registry.Replica) *InitStatus {
	switch {
	case r.Status == registry.ReplicaInitError:
		return &InitStatus{Status: "failed", Progress: r.InitStep, Error: r.InitError}
	case settingUp(r) && r.InitStep != StepDone:
		is := &InitStatus{Status: "in_progress", Progress: r.InitStep}
		est := c.estimate(ctx, r)
		is.BaseBackupDownloadEstimateSeconds = est.BaseBackupDownloadEstimateSeconds
		is.WALArchiveReplayEstimateSeconds = est.WALArchiveReplayEstimateSeconds
		return is
	}
	return &InitStatus{Status: "completed", Progress: StepDone}
}

// lagNow is the latest lag, or -1 when the leader has no recent sample.
func (c *Controller) lagNow(id string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state[id]
	if s == nil || s.obs == nil || s.obs.LagSeconds == nil || c.now().Sub(s.obsAt) > 3*c.interval()+30*time.Second {
		return -1
	}
	return *s.obs.LagSeconds
}

// Lag implements Service.
func (c *Controller) Lag(ctx context.Context, identifier string, since time.Time) ([]LagPoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state[identifier]
	if s == nil {
		return []LagPoint{}, nil
	}
	out := s.lag.since(since, c.now())
	if out == nil {
		out = []LagPoint{}
	}
	return out, nil
}

// Estimates are the spec's estimations (design 2.7.2).
const (
	// defaultDownloadRate is the assumed rate of the base backup download until one was measured.
	defaultDownloadRate = 100e6 // bytes per second
	// replayRate is the assumed rate at which a standby replays archived WAL.
	replayRate = 50e6 // bytes per second
)

// estimate is the time the base backup download and the replay of the WAL archive are expected to
// take from where the replica is. Zero means unknown, or done.
func (c *Controller) estimate(ctx context.Context, r *registry.Replica) InitStatus {
	var out InitStatus
	idx := stepIndex(r.InitStep)
	c.mu.Lock()
	s := c.st(r.Identifier)
	seed, backupLSN, rate := s.seedBytes, s.backupLSN, c.rate
	var replay string
	if s.obs != nil {
		replay = s.obs.ReplayLSN
	}
	head := c.headLSN(r.Ref)
	c.mu.Unlock()
	if rate <= 0 {
		rate = defaultDownloadRate
	}
	if idx < stepIndex(StepDownloaded) {
		if seed == 0 {
			seed = c.seedSize(ctx, r.Ref)
		}
		out.BaseBackupDownloadEstimateSeconds = int(float64(seed)/rate + 0.5)
	}
	if idx < stepIndex(StepReplayed) {
		from, ok := parseLSN(replay)
		if !ok {
			from, ok = parseLSN(backupLSN)
		}
		if ok && head > from {
			out.WALArchiveReplayEstimateSeconds = int(float64(head-from)/replayRate + 0.5)
		}
	}
	return out
}

// headLSN is the furthest WAL position any instance of ref has reported; zero when none has.
// The caller holds c.mu.
func (c *Controller) headLSN(ref string) uint64 {
	var head uint64
	for _, s := range c.state {
		if s.obs == nil || s.obs.Ref != ref {
			continue
		}
		for _, l := range []string{s.obs.ReceiveLSN, s.obs.ReplayLSN} {
			if v, ok := parseLSN(l); ok {
				head = max(head, v)
			}
		}
	}
	return head
}
