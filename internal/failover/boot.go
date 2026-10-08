package failover

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/failover/fenced"
	"github.com/supavise/supavise/internal/registry"
)

// The old primary's return, on its own side (design 2.10.8). A node that was down, or cut off,
// while another node took over does not know it; at boot, before the daemon lets any cluster run as
// a primary, it asks its peers for the epoch and reads the leader marker in the backup store. A
// higher epoch, or a different leader, fences a node that led: the record is written (the plane
// starts no primary while it exists), the clusters that systemd already started are stopped, and
// the critical alert `fenced` goes out. `supavise node rejoin` is the way back.
//
// Only a node that led is fenced by what it learns. A follower that was down while the leader
// changed holds a registry copy that is behind, and nothing it homes moved: the leadership change
// of a server move touches the projects of the old leader only, and a project move reaches its
// home with a fence of its own. Fencing the follower would put its projects offline and set all its
// data aside at the rejoin.

// bootTimeout bounds the whole check: a node that cannot find out starts only when its record
// shows no demotion, and must not wait for ever to find out.
const bootTimeout = 10 * time.Second

// BootResult is what BootCheck found.
type BootResult struct {
	// Fenced: this node must not run a primary.
	Fenced bool
	// Reason says what was seen and where; for a node that was fenced before, what the record says.
	Reason string
	Epoch  int64
	Leader string
	// Sources lists what answered ("peer n2", "backup store"); empty when nothing did.
	Sources []string
}

// BootCheck runs the boot-time epoch check. Only a node that leads according to its own registry
// is asked about the cluster (see above); any other node only has its record read. It never
// returns an error for a source that is unreachable; that is the point of having two.
func (o *Orchestrator) BootCheck(ctx context.Context) (BootResult, error) {
	paths := o.d.Cfg.Paths()
	if rec, err := fenced.Node(paths); err != nil {
		return BootResult{Fenced: true, Reason: err.Error()}, nil // a record that cannot be read fences
	} else if rec != nil {
		return BootResult{Fenced: true, Reason: rec.Reason, Epoch: rec.Epoch, Leader: rec.Leader}, nil
	}
	if !o.d.Members.IsLeader() {
		return BootResult{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()

	self := o.self()
	localEpoch := o.d.Members.Epoch()
	localLeader := self.ID
	if l, ok := o.d.Members.Leader(); ok {
		localLeader = l.ID
	}

	type seen struct {
		source string
		epoch  int64
		leader string
	}
	var (
		mu      sync.Mutex
		answers []seen
		wg      sync.WaitGroup
	)
	if o.d.Peers != nil {
		for _, n := range o.d.Members.Nodes() {
			if n.ID == self.ID || n.State == registry.NodeLeft {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				p, err := o.d.Peers.Ping(ctx, n.ID)
				if err != nil {
					return
				}
				mu.Lock()
				answers = append(answers, seen{source: "peer " + n.Name, epoch: p.Epoch, leader: p.Leader})
				mu.Unlock()
			}()
		}
	}
	if o.d.Marker != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m, err := o.d.Marker.ReadLeaderMarker(ctx); err == nil && m != nil {
				mu.Lock()
				answers = append(answers, seen{source: "the backup store's leader marker", epoch: m.Epoch, leader: m.Leader})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	res := BootResult{Epoch: localEpoch, Leader: localLeader}
	for _, a := range answers {
		res.Sources = append(res.Sources, a.source)
	}
	for _, a := range answers {
		if reason := replacedBy(a.source, a.epoch, a.leader, localEpoch, localLeader); reason != "" {
			res.Fenced, res.Reason, res.Epoch, res.Leader = true, reason, a.epoch, a.leader
			break
		}
	}
	if !res.Fenced {
		if len(answers) == 0 && len(o.d.Members.Nodes()) > 1 {
			o.d.Log.Warn("the boot epoch check reached neither a peer nor the backup store; starting on the local record, which shows no demotion")
		}
		return res, nil
	}
	return res, o.fenceSelf(ctx, res, "At boot")
}

// replacedBy says why what a source reported means that the node, which leads at localEpoch under
// localLeader, has been replaced: a higher epoch, or another leader at its own. It returns "" when
// the source shows no replacement.
func replacedBy(source string, epoch int64, leader string, localEpoch int64, localLeader string) string {
	switch {
	case epoch > localEpoch:
		return fmt.Sprintf("%s says epoch %d (leader %s); this node's registry is at epoch %d", source, epoch, leader, localEpoch)
	case epoch == localEpoch && leader != "" && leader != localLeader:
		return fmt.Sprintf("%s says %s leads at epoch %d; this node's registry says %s", source, leader, epoch, localLeader)
	}
	return ""
}

// FenceOnHigherEpoch is the boot check's verdict for a leader that is already running (invariant
// I1): the membership layer calls it when a ping, a peer call or the leader marker shows that the
// cluster is at a higher epoch, or that another node leads at this node's epoch. source names where
// it was seen ("peer standby"). A node that does not lead, or that saw nothing newer than its own
// registry, is left alone and false returned, and so is a leader that stopped for the switchover whose
// node and epoch it was told of; otherwise the node fences itself exactly as at boot:
// the record is written, the primaries stop, and the alert goes out.
func (o *Orchestrator) FenceOnHigherEpoch(ctx context.Context, source string, epoch int64, leader string) (bool, error) {
	if !o.d.Members.IsLeader() {
		return false, nil
	}
	// The leader that stopped for a switchover is told by the survivor's first ping and by the marker
	// that the survivor leads at the next epoch. That is the switchover it stopped for, and it is
	// demoted in place, not fenced.
	if rec, err := o.currentQuiesce(); err == nil && rec != nil && rec.To == leader && rec.Epoch == epoch {
		return false, nil
	}
	localLeader := o.self().ID
	if l, ok := o.d.Members.Leader(); ok {
		localLeader = l.ID
	}
	reason := replacedBy(source, epoch, leader, o.d.Members.Epoch(), localLeader)
	if reason == "" {
		return false, nil
	}
	return true, o.fenceSelf(ctx, BootResult{Fenced: true, Reason: reason, Epoch: epoch, Leader: leader}, "While running")
}

// fenceSelf makes the verdict stick: the record first, so that nothing starts a primary behind
// the daemon's back, then the clusters that run now are stopped and the alert goes out. A record
// that cannot be written (a full disk is likely on a node that is failing) does not leave the
// primaries running: they are stopped and their launchers removed all the same.
func (o *Orchestrator) fenceSelf(ctx context.Context, res BootResult, when string) error {
	var errs []error
	if err := fenced.WriteNode(o.d.Cfg.Paths(), o.nodeRecord(res.Epoch, res.Leader, res.Reason)); err != nil {
		errs = append(errs, fmt.Errorf("failover: recording that this node is fenced: %w", err))
	}
	if o.d.LocalPrimaries != nil {
		if _, err := o.fencePrimaries(context.WithoutCancel(ctx), o.primaryRefs(ctx), func() error { return nil }, true); err != nil {
			errs = append(errs, fmt.Errorf("failover: this node is fenced, but a primary could not be stopped: %w", err))
		}
	}
	o.alert(ctx, alerts.Event{
		Kind: alerts.KindFenced, Severity: alerts.SeverityCritical, Title: "This node was fenced",
		Detail: when + ": " + res.Reason + ". No cluster starts as a primary here until `supavise node rejoin` rebuilds this node as a follower of the current leader.",
		Key:    "fenced",
	})
	return errors.Join(errs...)
}

// Fenced reports whether the node has a fence record: the status line and the proxy ask.
func (o *Orchestrator) Fenced() (*fenced.Record, error) { return fenced.Node(o.d.Cfg.Paths()) }
