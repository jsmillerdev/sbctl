package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/backup"
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
	// Fence, when set, is asked first when a running leader learns that another node holds the
	// leadership (ObserveEpoch): the failover orchestrator's FenceOnHigherEpoch, which writes the node's
	// record, stops the primaries and raises the alert. source says where it was seen ("node n2", "the
	// leader marker in the backup store"). It is called while the membership still reports the leader,
	// which it checks, and its verdict decides: false leaves the node leading. An error with a true verdict
	// is logged; the node is fenced all the same.
	Fence func(ctx context.Context, source string, epoch int64, leader string) (fenced bool, err error)
	// OnFenced is called once, when a running leader learns that another node holds the leadership
	// (ObserveEpoch). The daemon stops the clusters, raises the alert and restarts into fenced mode.
	OnFenced func(FencedRecord)
	// Marker, when set, is the backup store's leader marker, which Run reads every MarkerEvery while
	// this node leads a cluster of more than one node: a leader that no peer can reach still learns from
	// it that another node was promoted. Zero MarkerEvery is 30 s.
	Marker      backup.EpochMarkerStore
	MarkerEvery time.Duration
	Log         *slog.Logger
	Now         func() time.Time
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

	mu        sync.Mutex
	fenced    *FencedRecord
	primary   bool   // the last probe of the system cluster succeeded and found a primary
	fencing   bool   // an observation is deciding whether to fence the node
	notified  bool   // OnFenced has been called
	recordErr string // the last error of reading fenced.json that was logged
	drift     int    // consecutive polls that saw a role other than the boot role
	changed   chan struct{}
	once      sync.Once
	reason    string
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
	var markerTick <-chan time.Time
	if l.o.Marker != nil {
		every := l.o.MarkerEvery
		if every <= 0 {
			every = 30 * time.Second
		}
		t := time.NewTicker(every)
		defer t.Stop()
		markerTick = t.C
	}
	for {
		l.Refresh(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		case <-markerTick:
			l.CheckMarker(ctx)
		}
	}
}

// CheckMarker reads the leader marker and treats what it says as a peer's word (ObserveEpoch). It does
// nothing on a node that does not lead, on a cluster of one node, and when the store does not answer.
func (l *Live) CheckMarker(ctx context.Context) {
	snap := l.get()
	if l.o.Marker == nil || snap.Role != RoleLeader || len(snap.Nodes) < 2 {
		return
	}
	mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	m, err := l.o.Marker.ReadLeaderMarker(mctx)
	if err != nil {
		l.o.Log.Debug("membership: the leader marker was not read", "error", err)
		return
	}
	if m != nil && m.Leader != "" {
		l.observe("the leader marker in the backup store", m.Epoch, m.Leader)
	}
}

// adoptRecord makes the node fenced in the membership's eyes when fenced.json says so: the failover
// procedure fences a leader cooperatively by writing it (failover/fenced), outside the daemon's own
// ObserveEpoch. A record that cannot be read leaves the role as it is (the plane refuses to start a
// primary on it all the same).
func (l *Live) adoptRecord() {
	if l.Fenced() != nil {
		return
	}
	rec, err := ReadFenced(l.o.Cfg)
	if err != nil {
		l.mu.Lock()
		again := l.recordErr == err.Error() // once per distinct error: the poll runs every two seconds
		l.recordErr = err.Error()
		l.mu.Unlock()
		if !again {
			l.o.Log.Warn("membership: fenced.json cannot be read", "error", err)
		}
		return
	}
	if rec == nil || rec.Removed {
		// A record of a removal is the retirement's own: it ends the daemon, and the node comes up down.
		// It is not a fence by another node, and the membership of a running leader does not read it as one.
		return
	}
	l.mu.Lock()
	if l.fenced == nil {
		l.fenced = rec
	}
	l.mu.Unlock()
	l.o.Log.Error("this node is fenced", "reason", rec.Reason, "epoch", rec.Epoch, "leader", rec.Leader)
}

// publishFenced makes the snapshot say fenced, and tells the daemon to restart in that role.
func (l *Live) publishFenced() {
	if prev := l.get(); prev.Role != RoleFenced {
		next := prev
		next.Role = RoleFenced
		l.Set(next)
	}
	l.watchRole(RoleFenced)
}

// promotedEpoch is the epoch of this node's own promotion of the system cluster: the failover
// procedure writes promote.ok with the epoch before it promotes, and the registry on the node does not
// show the epoch until the procedure has moved the leadership into it. 0 when there is none.
func (l *Live) promotedEpoch() int64 {
	b, err := os.ReadFile(l.o.Cfg.Paths().PromoteOK(config.SystemRef))
	if err != nil {
		return 0
	}
	n, err := backup.ParsePromoteOK(b)
	if err != nil {
		return 0
	}
	return n
}

// Refresh reads the registry and the recovery state once and publishes the snapshot if it changed.
func (l *Live) Refresh(ctx context.Context) {
	l.adoptRecord()
	cl, err := l.o.Reg.GetCluster(ctx)
	if err != nil {
		l.o.Log.Debug("membership: cluster row not read", "error", err)
		// A fence stops the system cluster too, so a registry that cannot be read says nothing against it.
		if l.Fenced() != nil {
			l.publishFenced()
		}
		return
	}
	nodes, err := l.o.Reg.ListNodes(ctx)
	if err != nil {
		l.o.Log.Debug("membership: nodes not read", "error", err)
		if l.Fenced() != nil {
			l.publishFenced()
		}
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
		// A node that was just promoted leads at the epoch it was promoted for, before the registry
		// says so: the procedure that promoted it waits for the membership to show this.
		if e := l.promotedEpoch(); e > snap.Epoch {
			snap.Epoch = e
		}
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

// ObserveEpoch is told what a peer believes (every answer to a ping). A leader that sees another node
// named as leader at its epoch or a higher one is fenced: Fence is asked first (the orchestrator
// records the fence and stops the primaries), then the node writes fenced.json (with the peers'
// addresses, which `node rejoin` needs) and calls OnFenced, and the daemon restarts into fenced mode. Only
// a leader whose system cluster answers as a primary is fenced: one whose database is stopped writes
// nothing, and that is how the old leader of a planned switchover looks to the new one until it is
// demoted in place. The leader marker in the backup store is read the same way (CheckMarker).
func (l *Live) ObserveEpoch(node string, epoch int64, leader string) {
	l.observe("node "+node, epoch, leader)
}

func (l *Live) observe(source string, epoch int64, leader string) {
	snap := l.get()
	if snap.Role != RoleLeader || leader == "" || leader == l.o.SelfID || epoch < snap.Epoch {
		return
	}
	l.mu.Lock()
	if l.fencing || l.fenced != nil || (l.o.InRecovery != nil && !l.primary) {
		l.mu.Unlock()
		return
	}
	l.fencing = true
	l.mu.Unlock()
	defer func() { l.mu.Lock(); l.fencing = false; l.mu.Unlock() }()

	rec := FencedRecord{Epoch: epoch, Leader: leader, At: l.o.Now(), Peers: PeersOf(snap.Nodes, l.o.SelfID),
		Reason: fmt.Sprintf("%s says node %s leads at epoch %d; this node's epoch is %d", source, leader, epoch, snap.Epoch)}
	if l.o.Fence != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		fenced, err := l.o.Fence(ctx, source, epoch, leader)
		cancel()
		if err != nil {
			l.o.Log.Error("membership: fencing this node did not finish cleanly", "error", err)
		}
		if !fenced {
			return
		}
	}
	// The record may be there already: the orchestrator writes it first, and a refresh that ran meanwhile
	// adopted it. The peers' addresses are in this one, so it replaces that, and the daemon is told once.
	l.mu.Lock()
	if l.notified {
		l.mu.Unlock()
		return
	}
	l.fenced, l.notified = &rec, true
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
