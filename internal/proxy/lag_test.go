package proxy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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

// slowStatuses is a replicas.Service whose Statuses waits to be released.
type slowStatuses struct {
	replicas.Service
	calls   atomic.Int32
	release chan struct{}
}

func (s *slowStatuses) Statuses(context.Context, string) ([]replicas.Status, error) {
	s.calls.Add(1)
	<-s.release
	return []replicas.Status{{Identifier: repEU, LagSeconds: 2}}, nil
}

// Requests that miss at the same moment share one question to the controller.
func TestReplicaLagAsksOnceForSimultaneousMisses(t *testing.T) {
	svc := &slowStatuses{release: make(chan struct{})}
	c := &lagCache{svc: svc, leader: func() bool { return true }, now: time.Now, refs: map[string]lagReading{}}
	var wg sync.WaitGroup
	lags := make([]time.Duration, 20)
	for i := range lags {
		wg.Add(1)
		go func() { defer wg.Done(); lags[i], _ = c.lag(repEU) }()
	}
	for svc.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // the others have arrived and wait for the flight
	close(svc.release)
	wg.Wait()
	if n := svc.calls.Load(); n != 1 {
		t.Errorf("the controller was asked %d times by 20 requests that missed together, want 1", n)
	}
	for i, d := range lags {
		if d != 2*time.Second {
			t.Errorf("request %d got a lag of %v", i, d)
		}
	}
}
