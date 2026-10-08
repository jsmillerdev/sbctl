package failover

import (
	"context"
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
// higher epoch, or a different leader, fences it: the record is written (the plane starts no
// primary while it exists), the clusters that systemd already started are stopped, and the
// critical alert `fenced` goes out. `supavise node rejoin` is the way back.

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

// BootCheck runs the boot-time epoch check. claims says whether this node would run a primary: it
// leads according to its own registry, or homes a project. A node that claims nothing has no
// primary to fence, and its registry follows the leader's anyway. It never returns an error for
// a source that is unreachable; that is the point of having two.
func (o *Orchestrator) BootCheck(ctx context.Context, claims bool) (BootResult, error) {
	paths := o.d.Cfg.Paths()
	if rec, err := fenced.Node(paths); err != nil {
		return BootResult{Fenced: true, Reason: err.Error()}, nil // a record that cannot be read fences
	} else if rec != nil {
		return BootResult{Fenced: true, Reason: rec.Reason, Epoch: rec.Epoch, Leader: rec.Leader}, nil
	}
	if !claims {
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
		switch {
		case a.epoch > localEpoch:
			res.Fenced = true
			res.Reason = fmt.Sprintf("%s says epoch %d (leader %s); this node's registry is at epoch %d", a.source, a.epoch, a.leader, localEpoch)
		case a.epoch == localEpoch && a.leader != "" && a.leader != localLeader:
			res.Fenced = true
			res.Reason = fmt.Sprintf("%s says %s leads at epoch %d; this node's registry says %s", a.source, a.leader, a.epoch, localLeader)
		default:
			continue
		}
		res.Epoch, res.Leader = a.epoch, a.leader
		break
	}
	if !res.Fenced {
		if len(answers) == 0 && len(o.d.Members.Nodes()) > 1 {
			o.d.Log.Warn("the boot epoch check reached neither a peer nor the backup store; starting on the local record, which shows no demotion")
		}
		return res, nil
	}
	return res, o.fenceSelf(ctx, res)
}

// fenceSelf makes the verdict stick: the record first, so that nothing starts a primary behind
// the daemon's back, then the clusters that run now are stopped and the alert goes out.
func (o *Orchestrator) fenceSelf(ctx context.Context, res BootResult) error {
	if err := fenced.WriteNode(o.d.Cfg.Paths(), fenced.Record{Epoch: res.Epoch, Leader: res.Leader, Reason: res.Reason, At: o.d.Now().UTC()}); err != nil {
		return fmt.Errorf("failover: recording that this node is fenced: %w", err)
	}
	var first error
	if o.d.LocalPrimaries != nil {
		_, err := o.fencePrimaries(context.WithoutCancel(ctx), o.primaryRefs(ctx), func() error { return nil })
		first = err
	}
	o.alert(ctx, alerts.Event{
		Kind: alerts.KindFenced, Severity: alerts.SeverityCritical, Title: "This node was fenced",
		Detail: "At boot: " + res.Reason + ". No cluster starts as a primary here until `supavise node rejoin` rebuilds this node as a follower of the current leader.",
		Key:    "fenced",
	})
	if first != nil {
		return fmt.Errorf("failover: this node is fenced, but a primary could not be stopped: %w", first)
	}
	return nil
}

// Fenced reports whether the node has a fence record: the status line and the proxy ask.
func (o *Orchestrator) Fenced() (*fenced.Record, error) { return fenced.Node(o.d.Cfg.Paths()) }
