package cluster

import (
	"context"
	"sync"

	"github.com/supavise/supavise/internal/registry"
)

// Static is a Membership whose answers are set by the caller: for a single node that has no
// cluster behind it (Solo) and for tests of code that depends on Membership.
type Static struct {
	mu       sync.Mutex
	snap     Snapshot
	watchers map[chan Snapshot]struct{}
}

var _ Membership = (*Static)(nil)

// NewStatic returns a Static that reports snap.
func NewStatic(snap Snapshot) *Static {
	return &Static{snap: snap, watchers: map[chan Snapshot]struct{}{}}
}

// Solo is the Membership of a server that never joined a cluster: the founder node, leading at epoch 1.
func Solo(self registry.Node) *Static {
	if self.ID == "" {
		self.ID = registry.FounderNodeID
	}
	self.State = registry.NodeActive
	return NewStatic(Snapshot{Self: self, Nodes: []registry.Node{self}, Leader: self.ID, Epoch: 1, Role: RoleLeader})
}

// Set replaces the snapshot and tells the watchers.
func (s *Static) Set(snap Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = snap
	for ch := range s.watchers {
		select {
		case <-ch: // drop the one it has not read: a watcher sees the latest
		default:
		}
		ch <- snap
	}
}

func (s *Static) get() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

func (s *Static) Self() registry.Node    { return s.get().Self }
func (s *Static) Nodes() []registry.Node { return append([]registry.Node(nil), s.get().Nodes...) }
func (s *Static) Epoch() int64           { return s.get().Epoch }
func (s *Static) Role() Role             { return s.get().Role }
func (s *Static) IsLeader() bool         { return s.get().Role == RoleLeader }

func (s *Static) Leader() (registry.Node, bool) {
	snap := s.get()
	for _, n := range snap.Nodes {
		if n.ID == snap.Leader {
			return n, true
		}
	}
	return registry.Node{}, false
}

// LeaderAndEpoch is Leader and Epoch from one snapshot. Two reads can straddle a change (the leader of the
// old snapshot with the epoch of the new one), and a ping that says "node X leads at epoch E" with such a pair
// makes the node that leads at E fence itself.
func (s *Static) LeaderAndEpoch() (leader registry.Node, ok bool, epoch int64) {
	snap := s.get()
	for _, n := range snap.Nodes {
		if n.ID == snap.Leader {
			return n, true, snap.Epoch
		}
	}
	return registry.Node{}, false, snap.Epoch
}

func (s *Static) Watch(ctx context.Context) <-chan Snapshot {
	out := make(chan Snapshot, 1)
	s.mu.Lock()
	out <- s.snap
	s.watchers[out] = struct{}{}
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		delete(s.watchers, out)
		close(out)
		s.mu.Unlock()
	}()
	return out
}
