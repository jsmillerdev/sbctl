package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func refsN(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("p%02d", i)
	}
	return out
}

func TestRolloutRunsCanariesAloneThenBatches(t *testing.T) {
	var mu sync.Mutex
	var running, peak int32
	var order []string
	var canaryAlone atomic.Bool
	canaryAlone.Store(true)
	res := Rollout(context.Background(), refsN(9), RolloutOptions{Canary: 2, Batch: 3, Upgrade: func(_ context.Context, ref string) error {
		n := atomic.AddInt32(&running, 1)
		defer atomic.AddInt32(&running, -1)
		mu.Lock()
		order = append(order, ref)
		if n > peak {
			peak = n
		}
		mu.Unlock()
		if (ref == "p00" || ref == "p01") && n != 1 {
			canaryAlone.Store(false)
		}
		time.Sleep(5 * time.Millisecond)
		return nil
	}})
	if !res.OK() || len(res.Upgraded) != 9 {
		t.Fatalf("result = %+v", res)
	}
	if !canaryAlone.Load() || order[0] != "p00" || order[1] != "p01" {
		t.Fatalf("the canaries did not go first and alone: %v", order)
	}
	if peak != 3 {
		t.Fatalf("peak concurrency = %d, want the batch size 3", peak)
	}
}

func TestRolloutHaltsOnTheFirstFailure(t *testing.T) {
	boom := errors.New("boom")
	var started []string
	var mu sync.Mutex
	done := map[string]error{}
	res := Rollout(context.Background(), refsN(6), RolloutOptions{Canary: 1, Batch: 1,
		Upgrade: func(_ context.Context, ref string) error {
			mu.Lock()
			started = append(started, ref)
			mu.Unlock()
			if ref == "p02" {
				return boom
			}
			return nil
		},
		Done: func(ref string, err error) { done[ref] = err },
	})
	if res.OK() || len(res.Failed) != 1 || res.Failed[0].Ref != "p02" || !errors.Is(res.Failed[0].Err, boom) {
		t.Fatalf("result = %+v", res)
	}
	if !reflect.DeepEqual(res.Upgraded, []string{"p00", "p01"}) || !reflect.DeepEqual(res.NotAttempted, []string{"p03", "p04", "p05"}) {
		t.Fatalf("result = %+v", res)
	}
	if !reflect.DeepEqual(started, []string{"p00", "p01", "p02"}) || done["p02"] == nil || len(done) != 3 {
		t.Fatalf("started = %v, done = %v", started, done)
	}
}

func TestRolloutACanaryFailureLeavesTheRestUntouched(t *testing.T) {
	var calls int32
	res := Rollout(context.Background(), refsN(5), RolloutOptions{Canary: 1, Batch: 4, Upgrade: func(context.Context, string) error {
		atomic.AddInt32(&calls, 1)
		return errors.New("no")
	}})
	if calls != 1 || len(res.NotAttempted) != 4 || len(res.Failed) != 1 {
		t.Fatalf("calls = %d, result = %+v", calls, res)
	}
}

func TestRolloutStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	res := Rollout(ctx, refsN(4), RolloutOptions{Canary: 0, Batch: 1, Upgrade: func(_ context.Context, ref string) error {
		if ref == "p01" {
			cancel()
		}
		return nil
	}})
	if len(res.Upgraded) != 2 || len(res.NotAttempted) != 2 || len(res.Failed) != 0 || res.OK() {
		t.Fatalf("result = %+v", res)
	}
}
