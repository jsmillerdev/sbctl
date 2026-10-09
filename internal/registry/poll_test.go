package registry

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCounter is a change_seq that tests move by hand and count the reads of.
type fakeCounter struct {
	seq   atomic.Int64
	reads atomic.Int64
	fail  atomic.Bool
}

func (f *fakeCounter) read(context.Context) (int64, error) {
	f.reads.Add(1)
	if f.fail.Load() {
		return 0, errors.New("test: the database is gone")
	}
	return f.seq.Load(), nil
}

func fastPoll(t *testing.T) {
	t.Helper()
	old := readOnlyPoll
	readOnlyPoll = 10 * time.Millisecond
	t.Cleanup(func() { readOnlyPoll = old })
}

// expectReload reads one burst (a reload for each table) from feed.
func expectReload(t *testing.T, what string, feed <-chan Change) {
	t.Helper()
	seen := map[string]bool{}
	for range reloadTables {
		select {
		case c, ok := <-feed:
			if !ok {
				t.Fatalf("%s: the feed closed", what)
			}
			if c.Op != "reload" || c.Key != "" {
				t.Fatalf("%s: change %+v", what, c)
			}
			seen[c.Table] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no reload", what)
		}
	}
	if len(seen) != len(reloadTables) {
		t.Fatalf("%s: tables %v", what, seen)
	}
}

func expectQuiet(t *testing.T, what string, feed <-chan Change, d time.Duration) {
	t.Helper()
	select {
	case c, ok := <-feed:
		t.Fatalf("%s: unexpected %+v, open %v", what, c, ok)
	case <-time.After(d):
	}
}

// Every subscriber hears a change, one poll serves them all, and a subscriber is told of what changed
// after it subscribed, not before.
func TestChangePollerFansOneReadOutToEverySubscriber(t *testing.T) {
	fastPoll(t)
	fc := &fakeCounter{}
	p := &changePoller{seq: fc.read}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := p.add(ctx, 0)
	b := p.add(ctx, 0)
	fc.seq.Store(1)
	expectReload(t, "first subscriber", a)
	expectReload(t, "second subscriber", b)

	// A subscriber that arrives after the change, with the counter it read then, hears nothing for it.
	c := p.add(ctx, 1)
	expectQuiet(t, "a subscriber that has seen the counter", c, 100*time.Millisecond)
	fc.seq.Store(2)
	for name, feed := range map[string]<-chan Change{"a": a, "b": b, "c": c} {
		expectReload(t, name, feed)
	}

	// Three subscribers cost one read per look: a hundred milliseconds hold about ten looks, not thirty.
	before := fc.reads.Load()
	time.Sleep(200 * time.Millisecond)
	if n := fc.reads.Load() - before; n > 40 {
		t.Fatalf("%d reads in 200 ms at a 10 ms poll for three subscribers", n)
	}
}

// A subscriber whose feed is full is told at a later look; the others are not held up.
func TestChangePollerDoesNotWaitForASlowSubscriber(t *testing.T) {
	fastPoll(t)
	fc := &fakeCounter{}
	p := &changePoller{seq: fc.read}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := p.add(ctx, 0)
	fast := p.add(ctx, 0)
	// Four bursts fill the slow feed (it holds four); the fast one is drained as it goes.
	for i := int64(1); i <= 6; i++ {
		fc.seq.Store(i)
		expectReload(t, "fast", fast)
	}
	if len(slow) != cap(slow) {
		t.Fatalf("the slow feed holds %d of %d: it should be full, not waited for", len(slow), cap(slow))
	}
	// The slow consumer catches up: it drains what it has and hears the latest counter.
	for len(slow) > 0 {
		<-slow
	}
	fc.seq.Store(100)
	expectReload(t, "slow after draining", slow)
}

// A feed closes when its context ends, the poll stops with the last subscriber, and a later
// subscriber starts it again.
func TestChangePollerStopsWithItsLastSubscriber(t *testing.T) {
	fastPoll(t)
	fc := &fakeCounter{}
	p := &changePoller{seq: fc.read}
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	a := p.add(ctxA, 0)
	b := p.add(ctxB, 0)
	cancelA()
	select {
	case _, ok := <-a:
		if ok {
			t.Fatal("a change on a feed whose context ended")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the feed did not close with its context")
	}
	fc.seq.Store(1)
	expectReload(t, "the remaining subscriber", b)

	cancelB()
	select {
	case <-b:
	case <-time.After(5 * time.Second):
		t.Fatal("the last feed did not close")
	}
	time.Sleep(50 * time.Millisecond)
	before := fc.reads.Load()
	time.Sleep(100 * time.Millisecond)
	if fc.reads.Load() != before {
		t.Fatal("the poll runs with no subscriber")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := p.add(ctx, 1)
	fc.seq.Store(2)
	expectReload(t, "a subscriber after the poll stopped", c)
}

// A failed poll closes every feed, so that each consumer subscribes again.
func TestChangePollerClosesEveryFeedWhenAPollFails(t *testing.T) {
	fastPoll(t)
	fc := &fakeCounter{}
	p := &changePoller{seq: fc.read}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feeds := []<-chan Change{p.add(ctx, 0), p.add(ctx, 0), p.add(ctx, 0)}
	fc.fail.Store(true)
	var wg sync.WaitGroup
	for i, feed := range feeds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case _, ok := <-feed:
				if ok {
					t.Errorf("feed %d: a change after a failed poll", i)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("feed %d did not close", i)
			}
		}()
	}
	wg.Wait()
	// A consumer that subscribes again gets a working feed.
	fc.fail.Store(false)
	again := p.add(ctx, 0)
	fc.seq.Store(1)
	expectReload(t, "the new subscriber", again)
}
