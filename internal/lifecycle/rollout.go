package lifecycle

import (
	"context"
	"sync"
)

// RolloutOptions control Rollout.
type RolloutOptions struct {
	// Canary is how many projects go first, one at a time; the rollout stops if one fails.
	Canary int
	// Batch is how many projects run at once after the canaries (at least 1).
	Batch int
	// Upgrade upgrades one project.
	Upgrade func(ctx context.Context, ref string) error
	// Done, when set, is called as each project finishes (err nil: upgraded). Calls are
	// serialized.
	Done func(ref string, err error)
}

// RolloutResult says what a rollout did.
type RolloutResult struct {
	Upgraded []string
	// Failed holds the projects whose upgrade failed, by ref, in the order they finished.
	Failed []RolloutFailure
	// NotAttempted are the projects that were never started because an earlier one failed.
	NotAttempted []string
}

// RolloutFailure is one project that did not upgrade.
type RolloutFailure struct {
	Ref string
	Err error
}

// OK reports whether every project was upgraded.
func (r RolloutResult) OK() bool { return len(r.Failed) == 0 && len(r.NotAttempted) == 0 }

// Rollout upgrades refs in order: the first Canary projects one at a time, then the rest in
// batches of Batch concurrent upgrades. The first failure halts it: no project starts after it,
// and the ones already running finish. A canary that fails therefore stops the rollout with
// every other project untouched, which is what a canary is for.
func Rollout(ctx context.Context, refs []string, o RolloutOptions) RolloutResult {
	var res RolloutResult
	var mu sync.Mutex
	halted := false
	finish := func(ref string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			halted = true
			res.Failed = append(res.Failed, RolloutFailure{Ref: ref, Err: err})
		} else {
			res.Upgraded = append(res.Upgraded, ref)
		}
		if o.Done != nil {
			o.Done(ref, err)
		}
	}
	stopped := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return halted || ctx.Err() != nil
	}
	batch := max(o.Batch, 1)
	canary := min(max(o.Canary, 0), len(refs))
	i := 0
	for ; i < canary && !stopped(); i++ {
		finish(refs[i], o.Upgrade(ctx, refs[i]))
	}
	sem := make(chan struct{}, batch)
	var wg sync.WaitGroup
	for ; i < len(refs) && !stopped(); i++ {
		ref := refs[i]
		sem <- struct{}{}
		// A failure while this one waited for a slot halts the rollout before it starts.
		if stopped() {
			<-sem
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			finish(ref, o.Upgrade(ctx, ref))
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	attempted := map[string]bool{}
	for _, r := range res.Upgraded {
		attempted[r] = true
	}
	for _, f := range res.Failed {
		attempted[f.Ref] = true
	}
	for _, r := range refs {
		if !attempted[r] {
			res.NotAttempted = append(res.NotAttempted, r)
		}
	}
	return res
}
