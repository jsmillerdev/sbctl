package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

func TestSoloIsTheLeaderOfItself(t *testing.T) {
	m := Solo(registry.Node{Name: "primary"})
	n, ok := m.Leader()
	if !m.IsLeader() || m.Role() != RoleLeader || m.Epoch() != 1 || !ok || n.ID != registry.FounderNodeID || m.Self().State != registry.NodeActive || len(m.Nodes()) != 1 {
		t.Fatalf("solo: %+v leader %+v %v", m.Self(), n, ok)
	}
}

func TestStaticWatchDeliversTheLatest(t *testing.T) {
	m := Solo(registry.Node{Name: "primary"})
	ctx, cancel := context.WithCancel(context.Background())
	ch := m.Watch(ctx)
	if first := <-ch; first.Leader != registry.FounderNodeID {
		t.Fatalf("first snapshot: %+v", first)
	}
	snap := m.get()
	snap.Role, snap.Epoch, snap.Leader = RoleFenced, 3, "n2"
	m.Set(snap)
	snap.Epoch = 4
	m.Set(snap) // the reader has not read epoch 3: it gets 4
	select {
	case got := <-ch:
		if got.Epoch != 4 || got.Role != RoleFenced {
			t.Fatalf("watch: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no snapshot")
	}
	if m.IsLeader() {
		t.Fatal("a fenced node is not the leader")
	}
	if _, ok := m.Leader(); ok {
		t.Fatal("leader n2 has no row")
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("a snapshot after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("the channel did not close")
	}
}
