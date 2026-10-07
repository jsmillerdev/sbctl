package health

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMonitorReusesAReportWithinTheTTL(t *testing.T) {
	var n atomic.Int32
	m := NewMonitor(func(context.Context) (*Report, error) { n.Add(1); return &Report{Verdict: Healthy}, nil }, time.Minute)
	for i := 0; i < 5; i++ {
		if _, err := m.Report(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n.Load() != 1 {
		t.Errorf("%d checks for five requests inside the TTL", n.Load())
	}
	if _, err := m.Fresh(context.Background()); err != nil || n.Load() != 2 {
		t.Errorf("Fresh did not run a check: %d, %v", n.Load(), err)
	}
	now := time.Now()
	m.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := m.Report(context.Background()); err != nil || n.Load() != 3 {
		t.Errorf("an old report was reused: %d, %v", n.Load(), err)
	}
}

// Anyone who can reach /healthz must not be able to make the node probe every project once
// per request.
func TestMonitorSharesOneCheckBetweenConcurrentCallers(t *testing.T) {
	var n atomic.Int32
	release := make(chan struct{})
	m := NewMonitor(func(context.Context) (*Report, error) {
		n.Add(1)
		<-release
		return &Report{Verdict: Healthy}, nil
	}, 0)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, err := m.Report(context.Background()); err != nil || r.Verdict != Healthy {
				t.Errorf("%v %v", r, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n.Load() != 1 {
		t.Errorf("%d checks for twenty simultaneous callers", n.Load())
	}
}

func TestMonitorFallsBackOnlyToAYoungReport(t *testing.T) {
	fail := false
	m := NewMonitor(func(context.Context) (*Report, error) {
		if fail {
			return nil, errors.New("registry hung")
		}
		return &Report{Verdict: Healthy}, nil
	}, time.Nanosecond)
	if _, err := m.Fresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail = true
	time.Sleep(2 * time.Millisecond)
	if r, err := m.Report(context.Background()); err != nil || r == nil {
		t.Errorf("a failing check did not fall back to the young report: %v %v", r, err)
	}
	now := time.Now()
	m.now = func() time.Time { return now.Add(3 * time.Minute) }
	if r, err := m.Report(context.Background()); err == nil {
		t.Errorf("a failing check was covered by a three-minute-old report: %+v", r)
	}
}

func TestMonitorWaiterThatGivesUpDoesNotCancelTheCheck(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	m := NewMonitor(func(ctx context.Context) (*Report, error) {
		close(started)
		<-release
		finished.Store(ctx.Err() == nil)
		return &Report{Verdict: Healthy}, nil
	}, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := m.Report(ctx); done <- err }()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Error("the caller was not told its context ended")
	}
	close(release)
	time.Sleep(20 * time.Millisecond)
	if !finished.Load() {
		t.Error("the check was canceled with its first caller")
	}
	if r, err := m.Report(context.Background()); err != nil || r.Verdict != Healthy {
		t.Errorf("the finished check was not cached: %v %v", r, err)
	}
}
