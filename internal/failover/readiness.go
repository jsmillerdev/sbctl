package failover

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// probeTTL is how long a fencer probe's answer is reused: the readiness block is asked for by
// every `supavise status`, and a probe is a handful of EC2 calls.
const probeTTL = time.Minute

// probeCache keeps the last probe of the fencer.
type probeCache struct {
	mu  sync.Mutex
	at  time.Time
	err error
}

// probe runs the provider's probe, at most once per probeTTL.
func (o *Orchestrator) probe(ctx context.Context) error {
	c := &o.probes
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && o.d.Now().Sub(c.at) < probeTTL {
		return c.err
	}
	c.err = o.d.Provider.Probe(ctx)
	c.at = o.d.Now()
	return c.err
}

// Readiness reports whether a server failover would be accepted now, for the node that would
// take over: this node when it follows, else the follower with the healthiest standby of the
// system cluster. The failed preconditions are the blockers; facts that block nothing are notes.
func (o *Orchestrator) Readiness(ctx context.Context) (Readiness, error) {
	f := o.conf()
	r := Readiness{Mode: f.Mode, Fencer: o.d.Provider.Name()}
	if r.Mode == "" {
		r.Mode = config.FailoverManual
	}
	if r.Fencer == "manual" {
		r.Fencer = "none"
	}

	nodes, err := o.store().ListNodes(ctx)
	if err != nil {
		return r, fmt.Errorf("failover: listing nodes: %w", err)
	}
	active := 0
	for _, n := range nodes {
		if n.State == registry.NodeActive {
			active++
		}
	}
	if active < 2 {
		return r, ErrNoCluster
	}

	r.FencerStatus = o.fencerStatus(ctx, &r)
	r.EpochMarker = o.markerStatus(ctx)
	if reason := o.autoOff(); reason != "" {
		r.Notes = append(r.Notes, "automatic failover is off: "+reason)
	}

	to, ok := o.candidate(ctx, nodes)
	if !ok {
		r.Blockers = append(r.Blockers, "no other server holds a standby of the system cluster")
		return r, nil
	}
	pl, _, err := o.planServer(ctx, ServerOptions{To: to.ID, RestoreMissing: true})
	if err != nil {
		return r, err
	}
	for _, c := range pl.Blocked() {
		r.Blockers = append(r.Blockers, sentence(c))
	}
	var without []string
	for _, p := range pl.Projects {
		if p.RestoreFromArchive {
			without = append(without, p.Ref)
		}
	}
	if len(without) > 0 {
		r.ProjectsWithoutReplica = without
		r.Notes = append(r.Notes, fmt.Sprintf("%d project(s) have no replica (--restore-missing: RPO up to archive_timeout)", len(without)))
	}
	if r.Fencer == "none" {
		r.Notes = append(r.Notes, "no fencing method: an unplanned failover needs --old-primary-is-down and the DNS change by hand")
	}
	r.Notes = append(r.Notes, pl.Notes...)
	r.Ready = len(r.Blockers) == 0
	return r, nil
}

// sentence is a failed check as one line of the readiness block.
func sentence(c Check) string {
	if c.Detail == "" {
		return c.Name
	}
	return c.Detail
}

func (o *Orchestrator) fencerStatus(ctx context.Context, r *Readiness) string {
	if r.Fencer == "none" {
		return "not configured"
	}
	if err := o.probe(ctx); err != nil {
		return err.Error()
	}
	if r.Fencer == "aws" {
		return "DryRun OK"
	}
	return "ok"
}

func (o *Orchestrator) markerStatus(ctx context.Context) string {
	if o.d.Marker == nil {
		return "none: the backup store is not S3-compatible"
	}
	if _, err := o.d.Marker.ReadLeaderMarker(ctx); err != nil {
		return "store not reachable: " + err.Error()
	}
	return "store reachable"
}

// candidate picks the node a failover would promote: this one when it follows; else the active
// node that holds a standby of the system cluster, the one with the least lag.
func (o *Orchestrator) candidate(ctx context.Context, nodes []registry.Node) (registry.Node, bool) {
	self := o.self()
	if !o.d.Members.IsLeader() && self.State == registry.NodeActive {
		return self, true
	}
	reps, err := o.store().ListReplicas(ctx, config.SystemRef)
	if err != nil {
		return registry.Node{}, false
	}
	byID := map[string]registry.Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	var views []replicaView
	for _, rep := range reps {
		if n, ok := byID[rep.NodeID]; ok && n.State == registry.NodeActive && n.ID != self.ID {
			views = append(views, o.viewReplica(ctx, rep))
		}
	}
	if len(views) == 0 {
		return registry.Node{}, false
	}
	return byID[bestReplica(views).Row.NodeID], true
}
