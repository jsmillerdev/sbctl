package cluster

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
)

// ReportEvery is how often a node tells the leader what it observes.
const ReportEvery = 10 * time.Second

// Reports is the leader's memory of what the nodes last reported: replica steps, lag, receiver
// state, project health. Lag and receiver state are not persisted (invariant I5); the replica
// controller reads them here and writes only status transitions to the registry.
type Reports struct {
	mu     sync.Mutex
	byNode map[string]StoredReport
	subs   []func(peerapi.Report)
}

// StoredReport is a Report and when the leader received it.
type StoredReport struct {
	peerapi.Report
	Received time.Time
}

// NewReports returns an empty store.
func NewReports() *Reports { return &Reports{byNode: map[string]StoredReport{}} }

// Put records r as node's latest report and tells the subscribers.
func (r *Reports) Put(node string, rep peerapi.Report, at time.Time) {
	rep.Node = node
	r.mu.Lock()
	r.byNode[node] = StoredReport{Report: rep, Received: at}
	subs := append([]func(peerapi.Report){}, r.subs...)
	r.mu.Unlock()
	for _, fn := range subs {
		fn(rep)
	}
}

// Latest is node's last report.
func (r *Reports) Latest(node string) (StoredReport, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byNode[node]
	return s, ok
}

// All lists the last report of every node, by node id.
func (r *Reports) All() []StoredReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]StoredReport, 0, len(r.byNode))
	for _, s := range r.byNode {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// Instance finds the last observation of a replica instance in any node's report.
func (r *Reports) Instance(identifier string) (peerapi.InstanceStatus, time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.byNode {
		for _, in := range s.Instances {
			if in.Identifier == identifier {
				return in, s.Received, true
			}
		}
	}
	return peerapi.InstanceStatus{}, time.Time{}, false
}

// Subscribe calls fn with every report that arrives, in the goroutine that stored it. fn must not block.
func (r *Reports) Subscribe(fn func(peerapi.Report)) {
	r.mu.Lock()
	r.subs = append(r.subs, fn)
	r.mu.Unlock()
}

// Contributor adds to the report a node sends: the replica agent adds its instances, the project
// health check adds the projects. It runs every ReportEvery and should answer from memory.
type Contributor func(ctx context.Context) (instances []peerapi.InstanceStatus, projects []peerapi.ProjectHealth)

// Reporter sends the node's report to the leader every ReportEvery, or stores it when this node is
// the leader.
type Reporter struct {
	Self     func() string
	Epoch    func() int64
	Leader   func() (string, bool)
	IsLeader func() bool
	RPC      mesh.RPC
	Reports  *Reports
	Log      *slog.Logger
	Now      func() time.Time
	// Every overrides ReportEvery (tests).
	Every time.Duration

	mu   sync.Mutex
	adds []Contributor
}

// Add registers a contributor. Call it before Run.
func (p *Reporter) Add(c Contributor) {
	p.mu.Lock()
	p.adds = append(p.adds, c)
	p.mu.Unlock()
}

func (p *Reporter) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Once builds the report and delivers it.
func (p *Reporter) Once(ctx context.Context) error {
	rep := peerapi.Report{Node: p.Self(), At: p.now().UTC(), Epoch: p.Epoch()}
	p.mu.Lock()
	adds := append([]Contributor{}, p.adds...)
	p.mu.Unlock()
	for _, c := range adds {
		in, pr := c(ctx)
		rep.Instances = append(rep.Instances, in...)
		rep.Projects = append(rep.Projects, pr...)
	}
	if p.IsLeader() {
		p.Reports.Put(rep.Node, rep, p.now())
		return nil
	}
	lead, ok := p.Leader()
	if !ok {
		return mesh.ErrNoSession
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return p.RPC.Call(cctx, lead, "POST", peerapi.PathReport, rep, nil)
}

// Run reports until ctx ends. A failure is logged at debug level: the next report follows in ten seconds,
// and a node the leader cannot hear is the failure monitor's business.
func (p *Reporter) Run(ctx context.Context) error {
	every := p.Every
	if every <= 0 {
		every = ReportEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := p.Once(ctx); err != nil && p.Log != nil {
			p.Log.Debug("report to the leader failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
