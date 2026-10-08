package awsapi

import (
	"testing"
	"time"
)

// A wait is a point in the upper half of its schedule: the base doubled for each failure, up to
// the cap, and never shorter than half of it.
func TestRetryDelayStaysInItsWindow(t *testing.T) {
	for _, tc := range []struct {
		base  time.Duration
		n     int
		sched time.Duration // the schedule before jitter
	}{
		{200 * time.Millisecond, 1, 200 * time.Millisecond},
		{200 * time.Millisecond, 2, 400 * time.Millisecond},
		{200 * time.Millisecond, 3, 800 * time.Millisecond},
		{200 * time.Millisecond, 4, 1600 * time.Millisecond},
		{200 * time.Millisecond, 5, 3200 * time.Millisecond},
		{200 * time.Millisecond, 6, maxRetryBackoff},
		{200 * time.Millisecond, 80, maxRetryBackoff}, // no overflow
		{time.Hour, 1, time.Hour},                     // a larger base is its own cap
		{time.Hour, 9, time.Hour},
	} {
		for _, j := range []float64{0, 0.25, 0.5, 0.999999} {
			r := newRetrier(5, tc.base)
			r.jitter = func() float64 { return j }
			got := r.delay(tc.n)
			if got < tc.sched/2 || got >= tc.sched {
				t.Errorf("base %v, failure %d, jitter %v: %v, want [%v, %v)", tc.base, tc.n, j, got, tc.sched/2, tc.sched)
			}
		}
	}

	r := newRetrier(5, 200*time.Millisecond)
	r.jitter = func() float64 { return 0 }
	if got := r.delay(1); got != 100*time.Millisecond {
		t.Errorf("the shortest first wait is %v, want half the base", got)
	}
}

// The default jitter is not constant: waits for the same failure differ.
func TestRetryDelayIsJittered(t *testing.T) {
	r := newRetrier(5, 200*time.Millisecond)
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		d := r.delay(3)
		if d < 400*time.Millisecond || d >= 800*time.Millisecond {
			t.Fatalf("delay %v outside [400ms, 800ms)", d)
		}
		seen[d] = true
	}
	if len(seen) < 10 {
		t.Errorf("50 waits took only %d different values", len(seen))
	}
}
