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
	// Successor, when set, says whether node is the one this node stopped its cluster for and handed the
	// leadership to, at epoch or a later one (the failover orchestrator's record of a planned switchover).
	// The system cluster of the old leader of such a switchover is stopped, so it cannot read the registry
	// that would name the new leader; it follows the word of its successor (ObserveEpoch) and answers
	// the leader's requests as those of the leader, which the demotion of its own clusters needs.
	Successor func(node string, epoch int64) bool
	// OnFenced is called once, when a running leader learns that another node holds the leadership
	// (ObserveEpoch). The daemon stops the clusters, raises the alert and restarts into fenced mode.
	OnFenced func(FencedRecord)
	// Marker, when set, is the backup store's leader marker, which Run reads every MarkerEvery while
	// this node leads a cluster of more than one node: a leader that no peer can reach still learns from
	// it that another node was promoted. Zero MarkerEvery is 30 s. A marker left by a move that failed
	// after writing it fences a healthy leader here exactly as it does at boot.
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
	notified  bool   // an observation fenced the node; OnFenced is called by it, once
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

// Wired reports whether the membership was given the fencer and the leader marker (LiveOptions.Fence and
// Marker); the daemon's wiring test asks, because a node without them fences late or not at all.
func (l *Live) Wired() (fence, marker bool) { return l.o.Fence != nil, l.o.Marker != nil }

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
// primary on it all the same). The record the failover writes has no peers' addresses, which `node
// rejoin` needs, so this adds them (by node id, from the membership) and writes the record again.
//
// It leaves the file alone while observe is asking the fencer: the fencer writes the record before it
// stops the primaries, and a node that adopted it then would tell the daemon to restart in the middle of
// that. observe adopts its own record when the fencer is done.
func (l *Live) adoptRecord(ctx context.Context) {
	l.mu.Lock()
	skip := l.fenced != nil || l.fencing
	l.mu.Unlock()
	if skip {
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
	withPeers := len(rec.Peers) == 0
	if withPeers {
		rec.Peers = l.knownPeers(ctx)
	}
	l.mu.Lock()
	if l.fenced != nil || l.fencing {
		l.mu.Unlock()
		return
	}
	l.fenced = rec
	l.mu.Unlock()
	if withPeers && len(rec.Peers) > 0 {
		if err := WriteFenced(l.o.Cfg, *rec); err != nil {
			l.o.Log.Warn("membership: the peers' addresses were not added to the fenced record", "error", err)
		}
	}
	l.o.Log.Error("this node is fenced", "reason", rec.Reason, "epoch", rec.Epoch, "leader", rec.Leader)
}

// knownPeers is the peer addresses of the other nodes as the membership has them; before the first
// snapshot it asks the registry.
func (l *Live) knownPeers(ctx context.Context) map[string]string {
	nodes := l.get().Nodes
	if len(nodes) == 0 {
		if ns, err := l.o.Reg.ListNodes(ctx); err == nil {
			nodes = ns
		}
	}
	return PeersOf(nodes, l.o.SelfID)
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
	l.adoptRecord(ctx)
	cl, err := l.o.Reg.GetCluster(ctx)
	if err != nil {
		l.o.Log.Debug("membership: cluster row not read", "error", err)
		// A fence stops the system cluster too, so a registry that cannot be read says nothing against it.
		if l.Fenced() != nil {
			l.publishFenced()
		} else {
			l.probeRole(ctx)
		}
		return
	}
	nodes, err := l.o.Reg.ListNodes(ctx)
	if err != nil {
		l.o.Log.Debug("membership: nodes not read", "error", err)
		if l.Fenced() != nil {
			l.publishFenced()
		} else {
			l.probeRole(ctx)
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
	// The epoch only rises. A copy of the registry that has not replayed a move this node was told about (the
	// boot decision asked the peers; a ping named the leader) is behind, and its leader is the old one: the
	// old leader of a planned switchover restarts as a follower with exactly such a copy, and the forwarders
	// that its standby streams through are bound from the leader the membership names.
	if prev.Epoch > snap.Epoch && prev.Leader != "" {
		snap.Epoch, snap.Leader = prev.Epoch, prev.Leader
	}
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
		if prev.Leader != snap.Leader || prev.Epoch != snap.Epoch || prev.Role != snap.Role {
			l.o.Log.Info("membership: the leader, the epoch or the role changed", "leader", snap.Leader, "epoch", snap.Epoch, "role", string(snap.Role),
				"was_leader", prev.Leader, "was_epoch", prev.Epoch, "was_role", string(prev.Role))
		}
		l.Set(snap)
	}
	l.watchRole(role)
}

// probeRole follows the recovery state of the system cluster alone, when the registry cannot be read. The
// registry handle of a follower is a connection to its standby's socket, and the promotion of that standby
// ends with a restart on the system port, which takes the socket away for good: the node has to restart in
// the leader's role then, and the registry it would read the role from is the thing that is gone. The
// demotion in place of a leader's system cluster stops the database in the same way. The snapshot is left
// as it is; only a role that differs from the boot role on two polls in a row restarts the daemon.
func (l *Live) probeRole(ctx context.Context) {
	if l.o.InRecovery == nil {
		return
	}
	rec, err := l.o.InRecovery(ctx)
	l.mu.Lock()
	l.primary = err == nil && !rec
	l.mu.Unlock()
	if err != nil {
		return
	}
	if rec {
		l.watchRole(RoleFollower)
	} else {
		l.watchRole(RoleLeader)
	}
}

// followSuccessor makes the node it stopped for the leader the membership reports: this node's system
// cluster is stopped for a planned switchover or already a standby, and a peer says that node leads at a
// higher epoch. The registry cannot say so yet (the database is stopped, or the copy has not replayed the
// move). A follower takes the epoch too, so that a copy that is behind never takes the leader back
// (Refresh keeps the epoch). A leader whose database is stopped keeps its epoch: its record of the
// switchover ends when the epoch is reached, and the record is what the daemon that starts as the
// follower believes.
func (l *Live) followSuccessor(leader string, epoch int64, takeEpoch bool) {
	if l.o.Successor == nil {
		return
	}
	if snap := l.get(); snap.Leader == leader && (!takeEpoch || snap.Epoch >= epoch) {
		return
	}
	if !l.o.Successor(leader, epoch) {
		return
	}
	snap := l.get()
	snap.Leader = leader
	if takeEpoch {
		snap.Epoch = epoch
	}
	l.Set(snap)
	l.o.Log.Info("membership: the node this one stopped for leads; its requests are the leader's", "leader", leader, "epoch", epoch)
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
	if leader == "" || leader == l.o.SelfID {
		return
	}
	if epoch > snap.Epoch || (epoch == snap.Epoch && leader != snap.Leader) {
		l.o.Log.Info("membership: a peer names another leader than this node's snapshot", "source", source, "leader", leader, "epoch", epoch,
			"mine", snap.Leader, "my_epoch", snap.Epoch, "role", string(snap.Role))
	}
	if snap.Role == RoleFollower {
		// The old leader of a planned switchover restarts as a follower with a copy of the registry that has not
		// replayed the move: its standby streams through the forwarders to the leader the membership names, so it
		// has to hear who leads from its peers, and believes only the node its record says it stopped for.
		if epoch > snap.Epoch {
			l.followSuccessor(leader, epoch, true)
		}
		return
	}
	if snap.Role != RoleLeader || epoch < snap.Epoch {
		return
	}
	l.mu.Lock()
	if l.fencing || l.fenced != nil {
		l.mu.Unlock()
		return
	}
	if l.o.InRecovery != nil && !l.primary {
		l.mu.Unlock()
		l.followSuccessor(leader, epoch, false)
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
	// The fencer wrote its own record before it stopped the primaries, and the refreshes that ran meanwhile
	// left it alone (adoptRecord). This one has the peers' addresses and replaces it. It is on disk before
	// the membership says fenced, because that is what tells the daemon to restart.
	l.mu.Lock()
	if l.notified {
		l.mu.Unlock()
		return
	}
	l.notified = true
	l.mu.Unlock()
	if err := WriteFenced(l.o.Cfg, rec); err != nil {
		l.o.Log.Error("membership: the fenced record was not written", "error", err)
	}
	l.mu.Lock()
	l.fenced = &rec
	l.mu.Unlock()
	l.o.Log.Error("this node was replaced as leader and is fenced", "reason", rec.Reason)
	// The snapshot is read again: the fencer can take minutes, and the registry may have been read since.
	next := l.get()
	next.Role = RoleFenced
	l.Set(next)
	l.watchRole(RoleFenced)
	if l.o.OnFenced != nil {
		l.o.OnFenced(rec)
	}
}
