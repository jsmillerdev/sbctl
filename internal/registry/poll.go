package registry

import (
	"context"
	"sync"
	"time"
)

// changePoller serves the Subscribe calls of one read-only registry from a single poll of
// cluster.change_seq. A standby cannot LISTEN, so each subscriber used to run its own query every
// readOnlyPoll; the proxy table, the forwarders and the cluster membership all subscribe, and the
// poll is the same for all of them. Each subscriber keeps its own view of the counter: it is told to
// reload when the counter differs from the one it last heard, whenever it subscribed.
//
// The loop runs while there is a subscriber. A subscriber's feed closes when its context ends and,
// for every subscriber at once, when a poll fails, so that consumers subscribe again.
type changePoller struct {
	seq func(ctx context.Context) (int64, error)

	mu   sync.Mutex
	subs map[*pollSub]struct{}
	stop context.CancelFunc // ends the loop that runs; nil when none does
}

type pollSub struct {
	ch   chan Change
	done chan struct{} // closed when the subscriber is dropped
	seq  int64         // the counter this subscriber has been told about
}

// add registers a subscriber that has seen the counter at seq and returns its feed.
func (p *changePoller) add(ctx context.Context, seq int64) <-chan Change {
	s := &pollSub{ch: make(chan Change, len(reloadTables)*4), done: make(chan struct{}), seq: seq}
	p.mu.Lock()
	if p.subs == nil {
		p.subs = map[*pollSub]struct{}{}
	}
	p.subs[s] = struct{}{}
	if p.stop == nil {
		lctx, cancel := context.WithCancel(context.Background())
		p.stop = cancel
		go p.loop(lctx)
	}
	p.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			p.drop(s)
		case <-s.done:
		}
	}()
	return s.ch
}

// drop removes s and closes its feed; the loop ends with the last subscriber.
func (p *changePoller) drop(s *pollSub) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.subs[s]; !ok {
		return
	}
	p.dropLocked(s)
	if len(p.subs) == 0 && p.stop != nil {
		p.stop()
		p.stop = nil
	}
}

func (p *changePoller) dropLocked(s *pollSub) {
	delete(p.subs, s)
	close(s.ch)
	close(s.done)
}

func (p *changePoller) loop(ctx context.Context) {
	tick := time.NewTicker(readOnlyPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		cur, err := p.seq(ctx)
		p.mu.Lock()
		if ctx.Err() != nil { // the last subscriber left during the poll
			p.mu.Unlock()
			return
		}
		if err != nil {
			for s := range p.subs {
				p.dropLocked(s)
			}
			p.stop()
			p.stop = nil
			p.mu.Unlock()
			return
		}
		for s := range p.subs {
			// A feed that cannot take the whole burst (its consumer is slow) is told at a later look: its
			// counter stays where it was, so the difference is still there.
			if s.seq == cur || cap(s.ch)-len(s.ch) < len(reloadTables) {
				continue
			}
			s.seq = cur
			for _, t := range reloadTables {
				s.ch <- Change{Table: t, Op: "reload"}
			}
		}
		p.mu.Unlock()
	}
}
