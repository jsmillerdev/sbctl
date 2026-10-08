package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/replicas"
)

// statusService is a replicas.Service that answers Statuses only.
type statusService struct {
	replicas.Service
	calls    int
	statuses map[string][]replicas.Status
	err      error
}

func (s *statusService) Statuses(_ context.Context, ref string) ([]replicas.Status, error) {
	s.calls++
	return s.statuses[ref], s.err
}

func TestReplicaLag(t *testing.T) {
	svc := &statusService{statuses: map[string][]replicas.Status{testRef: {
		{Identifier: repEU, LagSeconds: 1.5},
		{Identifier: repUS, LagSeconds: -1},
	}}}
	leader := true
	c := &lagCache{svc: svc, leader: func() bool { return leader }, now: time.Now, refs: map[string]lagReading{}}
	now := time.Now()
	c.now = func() time.Time { return now }

	if d, ok := c.lag(repEU); !ok || d != 1500*time.Millisecond {
		t.Errorf("known lag: %v %v", d, ok)
	}
	if _, ok := c.lag(repUS); ok {
		t.Error("a lag of -1 is unknown")
	}
	if _, ok := c.lag(testRef + "-rr-eu-zzz999"); ok {
		t.Error("a replica the controller does not list has a lag")
	}
	if svc.calls != 1 {
		t.Errorf("the controller was asked %d times for one project within the TTL", svc.calls)
	}
	now = now.Add(lagTTL)
	svc.statuses[testRef][0].LagSeconds = 4
	if d, ok := c.lag(repEU); !ok || d != 4*time.Second || svc.calls != 2 {
		t.Errorf("after the TTL: %v %v, %d calls", d, ok, svc.calls)
	}

	// A controller that cannot answer leaves the lag unknown, and is not asked again at once.
	svc.err = errors.New("down")
	now = now.Add(lagTTL)
	if _, ok := c.lag(repEU); ok {
		t.Error("a lag from a failed question")
	}
	if _, ok := c.lag(repEU); ok || svc.calls != 3 {
		t.Errorf("%d calls", svc.calls)
	}

	// Only the leader has readings.
	leader = false
	calls := svc.calls
	if _, ok := c.lag(repEU); ok || svc.calls != calls {
		t.Error("a node that is not the leader knows a lag")
	}
	leader = true
	if _, ok := c.lag("not-an-identifier"); ok {
		t.Error("a lag for something that is not a replica")
	}
	if got := ReplicaLag(svc, func() bool { return true }); got == nil {
		t.Error("ReplicaLag returned no function")
	}
}
