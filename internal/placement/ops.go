package placement

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// Ops runs replica and backup operations on any node: its own agent when the node is this one, the
// peer API otherwise. It implements InstanceOps and BackupOps.
type Ops struct {
	// Self is the id of this node.
	Self func() string
	// Agent serves the operations for this node.
	Agent Agent
	// Backups serves the backup operations for this node; nil answers lifecycle.ErrNoSnapshot.
	Backups LocalBackups
	// RPC reaches the other nodes.
	RPC mesh.RPC
	// Epoch is the cluster epoch requests are made under unless the request names one.
	Epoch func() int64
	// Recorder records, in the registry this node writes, a base backup that another node took for it
	// (backup.Service.RecordBase on the leader). With it every BaseBackup of a project homed elsewhere
	// leaves its row and its event, whoever asked: the failover orchestrator's backup on the new
	// timeline, RoutedBackups, the final backup of a delete. Without it the caller records, or the
	// backup is in the store and in no list.
	Recorder BaseRecorder
}

// BaseRecorder is the part of the leader's backup service that records a base backup taken elsewhere.
type BaseRecorder interface {
	RecordBase(ctx context.Context, ref string, b backup.RemoteBase) (*registry.Backup, error)
}

var (
	_ InstanceOps = (*Ops)(nil)
	_ BackupOps   = (*Ops)(nil)
)

func (o *Ops) epoch(e int64) int64 {
	if e == 0 && o.Epoch != nil {
		return o.Epoch()
	}
	return e
}

func (o *Ops) local(node string) bool { return node == o.Self() }

// Ensure implements InstanceOps.
func (o *Ops) Ensure(ctx context.Context, node string, spec peerapi.InstanceSpec) (peerapi.InstanceStatus, error) {
	spec.Epoch = o.epoch(spec.Epoch)
	if o.local(node) {
		return o.Agent.Ensure(ctx, spec)
	}
	var st peerapi.InstanceStatus
	if err := o.RPC.Call(ctx, node, http.MethodPut, peerapi.InstancePath(spec.Identifier), spec, &st); err != nil {
		return peerapi.InstanceStatus{}, wrapRemote(node, err)
	}
	return st, nil
}

// Observe implements InstanceOps.
func (o *Ops) Observe(ctx context.Context, node, identifier string) (peerapi.InstanceStatus, error) {
	if o.local(node) {
		return o.Agent.Observe(ctx, identifier)
	}
	var st peerapi.InstanceStatus
	if err := o.RPC.Call(ctx, node, http.MethodGet, peerapi.InstancePath(identifier), nil, &st); err != nil {
		return peerapi.InstanceStatus{}, wrapRemote(node, err)
	}
	return st, nil
}

// Remove implements InstanceOps.
func (o *Ops) Remove(ctx context.Context, node, identifier string) error {
	if o.local(node) {
		return o.Agent.Remove(ctx, identifier)
	}
	path := peerapi.InstancePath(identifier)
	if epoch := o.epoch(0); epoch > 0 {
		path += "?" + queryEpoch + "=" + strconv.FormatInt(epoch, 10)
	}
	return wrapRemote(node, o.RPC.Call(ctx, node, http.MethodDelete, path, nil, nil))
}

// Do implements InstanceOps.
func (o *Ops) Do(ctx context.Context, node, identifier string, a peerapi.Action, req peerapi.InstanceAction) (peerapi.InstanceStatus, error) {
	req.Epoch = o.epoch(req.Epoch)
	if o.local(node) {
		return o.Agent.Do(ctx, identifier, a, req)
	}
	var st peerapi.InstanceStatus
	if err := o.RPC.Call(ctx, node, http.MethodPost, peerapi.InstanceActionPath(identifier, a), req, &st); err != nil {
		return peerapi.InstanceStatus{}, wrapRemote(node, err)
	}
	return st, nil
}

// BaseBackup implements BackupOps. A backup that another node took is recorded with Recorder before
// the call returns; RecordBase writes a backup once, so a caller that records it too (RoutedBackups,
// which needs the row) finds the one written. When recording fails the result is returned with the
// error: the backup is complete in the store.
func (o *Ops) BaseBackup(ctx context.Context, node, ref string, req peerapi.BackupRequest) (peerapi.BackupResult, error) {
	res, err := o.backup(ctx, node, ref, peerapi.BackupBase, req)
	if err != nil || o.local(node) || o.Recorder == nil {
		return res, err
	}
	if res.ID == "" {
		return res, fmt.Errorf("node %s took a base backup of %s and named no backup", node, ref)
	}
	if _, err := o.Recorder.RecordBase(ctx, ref, backup.RemoteBase{ID: res.ID, Reason: req.Reason, Timeline: res.Timeline,
		StartLSN: res.StartLSN, StopLSN: res.StopLSN, SizeBytes: res.SizeBytes}); err != nil {
		return res, fmt.Errorf("recording the base backup %s of %s that node %s took: %w", res.ID, ref, node, err)
	}
	return res, nil
}

// Restore implements BackupOps.
func (o *Ops) Restore(ctx context.Context, node, ref string, req peerapi.BackupRequest) (peerapi.BackupResult, error) {
	return o.backup(ctx, node, ref, peerapi.BackupRestore, req)
}

func (o *Ops) backup(ctx context.Context, node, ref string, op peerapi.BackupOp, req peerapi.BackupRequest) (peerapi.BackupResult, error) {
	req.Epoch = o.epoch(req.Epoch)
	if o.local(node) {
		if o.Backups == nil {
			return peerapi.BackupResult{}, lifecycle.ErrNoSnapshot
		}
		return RunBackup(ctx, o.Backups, ref, op, req)
	}
	var res peerapi.BackupResult
	if err := o.RPC.Call(ctx, node, http.MethodPost, peerapi.BackupPath(ref, op), req, &res); err != nil {
		return peerapi.BackupResult{}, wrapRemote(node, err)
	}
	return res, nil
}

// Fleet is the lifecycle.ReplicaFleet of the leader's Engine: it lists a project's replicas from the
// registry and restarts one on its node through InstanceOps.
type Fleet struct {
	Registry registry.Registry
	Ops      InstanceOps
	Epoch    func() int64
	// OnFailure is told of a replica that did not come back after a restart (the daemon raises the
	// replica_unhealthy alert); nil does nothing.
	OnFailure func(ctx context.Context, r registry.Replica, cause error)
}

var _ lifecycle.ReplicaFleet = (*Fleet)(nil)

// Replicas implements lifecycle.ReplicaFleet.
func (f *Fleet) Replicas(ctx context.Context, ref string) ([]registry.Replica, error) {
	return f.Registry.ListReplicas(ctx, ref)
}

// Restart implements lifecycle.ReplicaFleet: the node restarts the replica on class and the call
// returns once Postgres and PostgREST answer there.
func (f *Fleet) Restart(ctx context.Context, r registry.Replica, class string) error {
	var epoch int64
	if f.Epoch != nil {
		epoch = f.Epoch()
	}
	st, err := f.Ops.Do(ctx, r.NodeID, r.Identifier, peerapi.ActionRestart, peerapi.InstanceAction{Epoch: epoch, Class: class})
	if err != nil {
		return err
	}
	if !st.PostgresUp || (r.Ref != config.SystemRef && !st.PostgRESTReady) {
		return fmt.Errorf("after the restart Postgres is up: %v, PostgREST is ready: %v", st.PostgresUp, st.PostgRESTReady)
	}
	return nil
}

// Failed implements lifecycle.ReplicaFleet.
func (f *Fleet) Failed(ctx context.Context, r registry.Replica, cause error) {
	if f.OnFailure != nil {
		f.OnFailure(ctx, r, cause)
	}
}

// Observer is what the Reporter reads the node's state from: the agent, or a ReportCache over it.
type Observer interface {
	ObserveAll(ctx context.Context) []peerapi.InstanceStatus
}

var _ Observer = (*NodeAgent)(nil)

// Contribution is what this node adds to the report it sends the leader: the state of its replicas
// and the health of the projects homed here. It answers from memory (ReportCache.Contribute), so the
// report never waits for a probe. The cluster package's reporter takes it as a cluster.Contributor.
type Contribution func(ctx context.Context) ([]peerapi.InstanceStatus, []peerapi.ProjectHealth)

// ReportCache keeps the last observation of the node's replicas and of the projects homed here, and
// refreshes it in the background: observing a replica runs SQL and HTTP probes that can take several
// seconds each, which a report that is due every few seconds must not wait for.
type ReportCache struct {
	// Agent observes the node's replicas.
	Agent Observer
	// Projects reports the health of the projects homed on this node; nil reports none.
	Projects func(ctx context.Context) []peerapi.ProjectHealth

	mu        sync.Mutex
	instances []peerapi.InstanceStatus
	projects  []peerapi.ProjectHealth
}

// Refresh observes the node now and keeps the result.
func (c *ReportCache) Refresh(ctx context.Context) {
	// The replicas and the projects are probed side by side: one does not wait for the other.
	var in []peerapi.InstanceStatus
	var pr []peerapi.ProjectHealth
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		in = c.Agent.ObserveAll(ctx)
	}()
	if c.Projects != nil {
		pr = c.Projects(ctx)
	}
	wg.Wait()
	c.mu.Lock()
	c.instances, c.projects = in, pr
	c.mu.Unlock()
}

// Run refreshes the cache now and every interval (default 10 seconds) until ctx ends.
func (c *ReportCache) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 10 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		c.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ObserveAll implements Observer from the last refresh.
func (c *ReportCache) ObserveAll(context.Context) []peerapi.InstanceStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.instances)
}

// ProjectHealth is the Reporter's Projects function over the last refresh.
func (c *ReportCache) ProjectHealth(context.Context) []peerapi.ProjectHealth {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.projects)
}

// Contribute implements Contribution from the last refresh.
func (c *ReportCache) Contribute(ctx context.Context) ([]peerapi.InstanceStatus, []peerapi.ProjectHealth) {
	return c.ObserveAll(ctx), c.ProjectHealth(ctx)
}

// ErrNoReportEndpoint: the leader answers that it serves no report endpoint. The next report is tried
// at the next tick.
var ErrNoReportEndpoint = errors.New("placement: the leader serves no report endpoint")

// Reporter tells the leader what this node observes, every few seconds (peerapi.PathReport): the
// state of its replicas, and the health of the projects homed here when it is not the leader (the
// leader checks its own). The leader keeps lag and receiver state in memory and writes only
// status changes to the registry (invariant I5).
type Reporter struct {
	Members cluster.Membership
	RPC     mesh.RPC
	Agent   Observer
	// Projects reports the health of the projects homed on this node; nil reports none.
	Projects func(ctx context.Context) []peerapi.ProjectHealth
}

// Report builds the report of the moment.
func (r *Reporter) Report(ctx context.Context, now time.Time) peerapi.Report {
	rep := peerapi.Report{Node: r.Members.Self().ID, At: now.UTC(), Epoch: r.Members.Epoch(), Instances: r.Agent.ObserveAll(ctx)}
	if r.Projects != nil {
		rep.Projects = r.Projects(ctx)
	}
	return rep
}

// Once sends one report to the leader. A leader reports to nobody; a node that does not know a
// leader has no one to tell, which is not an error. ErrNoReportEndpoint when the leader answers that
// it has no such endpoint.
func (r *Reporter) Once(ctx context.Context, now func() time.Time) error {
	if r.Members.IsLeader() {
		return nil
	}
	leader, ok := r.Members.Leader()
	if !ok {
		return nil
	}
	rep := r.Report(ctx, now())
	err := wrapRemote(leader.ID, r.RPC.Call(ctx, leader.ID, http.MethodPost, peerapi.PathReport, rep, nil))
	if errors.Is(err, registry.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrNoReportEndpoint, err)
	}
	return err
}

// Run reports every interval (default 10 seconds) until ctx ends. A failed report is logged by the
// caller's log function and tried again at the next tick; a leader with no report endpoint is logged
// once until it answers.
func (r *Reporter) Run(ctx context.Context, every time.Duration, logf func(err error)) {
	if every <= 0 {
		every = 10 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	missing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := r.Once(ctx, time.Now)
		switch {
		case err == nil:
			missing = false
		case errors.Is(err, ErrNoReportEndpoint):
			if !missing && logf != nil {
				logf(err)
			}
			missing = true
		default:
			if logf != nil {
				logf(err)
			}
		}
	}
}
