package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// LiveOptions configure a Live.
type LiveOptions struct {
	Cfg *config.Config
	// Reg is the registry the node reads: its replicated copy on a follower.
	Reg registry.Registry
	// SelfID is this node's id.
	SelfID string
	// Boot is the decision made before the node opened; the role the daemon runs in until it restarts.
	Boot BootDecision
	// InRecovery asks the system cluster whether it is a standby. An error leaves the role as it was.
	InRecovery func(ctx context.Context) (bool, error)
	// Poll is how often the registry and the probe are read; zero is 2 s.
	Poll time.Duration
	// OnFenced is called once, when a running leader learns that another node holds the leadership
	// (ObserveEpoch). The daemon stops the clusters, raises the alert and restarts into fenced mode.
	OnFenced func(FencedRecord)
	Log      *slog.Logger
	Now      func() time.Time
}

// Live is the Membership of a node in a cluster: it follows the registry and the system cluster's
// recovery state, and tells the daemon when the role it started in no longer holds (a promotion or a
// demotion happened, or the node was fenced).
//
// The leader is the node whose system cluster is not in recovery (invariant I1): while this node's
// is a primary, Leader reports this node, even in the seconds before the registry says so.
type Live struct {
	*Static
	o LiveOptions

	mu      sync.Mutex
	fenced  *FencedRecord
	primary bool // the last probe of the system cluster succeeded and found a primary
	drift   int  // consecutive polls that saw a role other than the boot role
	changed chan struct{}
	once    sync.Once
	reason  string
}

var _ Membership = (*Live)(nil)

// NewLive builds a Live. Run keeps it current. A node that boots fenced starts fenced.
func NewLive(o LiveOptions) *Live {
	if o.Poll <= 0 {
		o.Poll = 2 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	l := &Live{o: o, changed: make(chan struct{})}
	l.Static = NewStatic(Snapshot{Role: o.Boot.Role, Epoch: o.Boot.Epoch, Leader: o.Boot.Leader})
	if o.Boot.Role == RoleFenced {
		l.fenced = &FencedRecord{Epoch: o.Boot.Epoch, Leader: o.Boot.Leader, Reason: o.Boot.Reason, At: o.Now()}
	}
	return l
}

// Changed is closed when the role the node runs in is no longer the role it was started in: the
// system cluster was promoted or demoted, or the node was fenced. The daemon restarts then, and the
// boot decision starts it in the new role. Why says what happened.
func (l *Live) Changed() <-chan struct{} { return l.changed }

// Why is the reason Changed was closed; empty before.
func (l *Live) Why() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reason
}

// Fenced returns the record of being fenced, or nil.
func (l *Live) Fenced() *FencedRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fenced == nil {
		return nil
	}
	r := *l.fenced
	return &r
}

// Run follows the cluster until ctx ends.
func (l *Live) Run(ctx context.Context) error {
	tick := time.NewTicker(l.o.Poll)
	defer tick.Stop()
	for {
		l.Refresh(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// Refresh reads the registry and the recovery state once and publishes the snapshot if it changed.
func (l *Live) Refresh(ctx context.Context) {
	cl, err := l.o.Reg.GetCluster(ctx)
	if err != nil {
		l.o.Log.Debug("membership: cluster row not read", "error", err)
		return
	}
	nodes, err := l.o.Reg.ListNodes(ctx)
	if err != nil {
		l.o.Log.Debug("membership: nodes not read", "error", err)
		return
	}
	prev := l.get()
	role := prev.Role
	if l.Fenced() != nil {
		role = RoleFenced
	} else if l.o.InRecovery != nil {
		rec, err := l.o.InRecovery(ctx)
		l.mu.Lock()
		l.primary = err == nil && !rec
		l.mu.Unlock()
		switch {
		case err != nil:
			l.o.Log.Debug("membership: recovery state not read", "error", err)
		case rec:
			role = RoleFollower
		default:
			role = RoleLeader
		}
	}
	snap := Snapshot{Nodes: nodes, Leader: cl.Leader, Epoch: cl.Epoch, Role: role, Maintenance: cl.Maintenance}
	for _, n := range nodes {
		if n.ID == l.o.SelfID {
			snap.Self = n
		}
	}
	if role == RoleLeader {
		snap.Leader = l.o.SelfID
	}
	if !reflect.DeepEqual(prev, snap) {
		l.Set(snap)
	}
	l.watchRole(role)
}

// watchRole closes Changed when the role differs from the boot role on two polls in a row (one
// failed probe or one lagging read must not restart the daemon).
func (l *Live) watchRole(role Role) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if role == l.o.Boot.Role {
		l.drift = 0
		return
	}
	l.drift++
	if l.drift < 2 && role != RoleFenced {
		return
	}
	l.once.Do(func() {
		l.reason = fmt.Sprintf("the node started as %s and is now %s", l.o.Boot.Role, role)
		close(l.changed)
	})
}

// ObserveEpoch is told what a peer believes (every answer to a ping). A leader that sees another
// node named as leader at its epoch or a higher one is fenced: it records that in fenced.json, which the
// next start reads, and calls OnFenced. Only a leader whose system cluster answers as a primary is
// fenced: one whose database is stopped writes nothing, and that is how the old leader of a planned
// switchover looks to the new one until it is demoted in place.
func (l *Live) ObserveEpoch(node string, epoch int64, leader string) {
	snap := l.get()
	if snap.Role != RoleLeader || leader == "" || leader == l.o.SelfID || epoch < snap.Epoch {
		return
	}
	l.mu.Lock()
	primaryUp := l.primary
	l.mu.Unlock()
	if l.o.InRecovery != nil && !primaryUp {
		return
	}
	rec := FencedRecord{Epoch: epoch, Leader: leader, At: l.o.Now(),
		Reason: fmt.Sprintf("node %s says node %s leads at epoch %d; this node's epoch is %d", node, leader, epoch, snap.Epoch)}
	l.mu.Lock()
	if l.fenced != nil {
		l.mu.Unlock()
		return
	}
	l.fenced = &rec
	l.mu.Unlock()
	if err := WriteFenced(l.o.Cfg, rec); err != nil {
		l.o.Log.Error("membership: the fenced record was not written", "error", err)
	}
	l.o.Log.Error("this node was replaced as leader and is fenced", "reason", rec.Reason)
	next := snap
	next.Role = RoleFenced
	l.Set(next)
	l.watchRole(RoleFenced)
	if l.o.OnFenced != nil {
		l.o.OnFenced(rec)
	}
}
