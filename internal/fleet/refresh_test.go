package fleet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeReplay struct {
	mu     sync.Mutex
	past   bool
	err    error
	asked  []string
	flipAt int // the call (1-based) from which replay has caught up; 0 means it never does
	askedN int
}

func (f *fakeReplay) ReplayedPast(_ context.Context, lsn string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.askedN++
	f.asked = append(f.asked, lsn)
	if f.err != nil {
		return false, f.err
	}
	return f.past || (f.flipAt > 0 && f.askedN >= f.flipAt), nil
}

type fakeRefresher struct {
	mu   sync.Mutex
	got  []string
	err  error
	when *fakeReplay // what the replay had been asked when the refresh came
	seen int
}

func (f *fakeRefresher) RefreshTenant(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, id)
	if f.when != nil {
		f.seen = f.when.askedN
	}
	return f.err
}

func TestPeerRefreshWaitsForReplayThenRefreshes(t *testing.T) {
	rp := &fakeReplay{flipAt: 3}
	rf := &fakeRefresher{when: rp}
	p := PeerRefresh{Replay: rp, Refresh: rf, Wait: 5 * time.Second, Poll: time.Millisecond}
	done, err := p.Do(context.Background(), testRef, "0/3000100")
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if len(rf.got) != 1 || rf.got[0] != testRef {
		t.Fatalf("refreshed %v", rf.got)
	}
	// The refresh came only after replay had caught up: on the third look.
	if rf.seen != 3 || rp.asked[0] != "0/3000100" {
		t.Errorf("replay asked %d times (%v) before the refresh", rf.seen, rp.asked)
	}
}

func TestPeerRefreshWithoutAPositionRefreshesAtOnce(t *testing.T) {
	rp := &fakeReplay{}
	rf := &fakeRefresher{}
	p := PeerRefresh{Replay: rp, Refresh: rf}
	if done, err := p.Do(context.Background(), testReplica, ""); err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if rp.askedN != 0 || len(rf.got) != 1 || rf.got[0] != testReplica {
		t.Fatalf("replay asked %d times; refreshed %v", rp.askedN, rf.got)
	}
}

func TestPeerRefreshGivesUpWhenReplayDoesNotArrive(t *testing.T) {
	rp := &fakeReplay{}
	rf := &fakeRefresher{}
	p := PeerRefresh{Replay: rp, Refresh: rf, Wait: 30 * time.Millisecond, Poll: time.Millisecond}
	done, err := p.Do(context.Background(), testRef, "0/FFFFFFF")
	if !errors.Is(err, ErrReplayBehind) || done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if len(rf.got) != 0 {
		t.Fatal("Supavisor was refreshed before the standby had the row: it would read the old one")
	}
	if !strings.Contains(err.Error(), "0/FFFFFFF") {
		t.Errorf("the error does not name the position: %v", err)
	}
}

func TestPeerRefreshPassesErrorsAndHonorsTheContext(t *testing.T) {
	boom := errors.New("standby down")
	p := PeerRefresh{Replay: &fakeReplay{err: boom}, Refresh: &fakeRefresher{}, Poll: time.Millisecond}
	if _, err := p.Do(context.Background(), testRef, "0/1"); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	p = PeerRefresh{Replay: &fakeReplay{past: true}, Refresh: &fakeRefresher{err: boom}}
	if done, err := p.Do(context.Background(), testRef, "0/1"); !errors.Is(err, boom) || done {
		t.Errorf("done=%v err=%v", done, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p = PeerRefresh{Replay: &fakeReplay{}, Refresh: &fakeRefresher{}, Poll: time.Millisecond}
	if _, err := p.Do(ctx, testRef, "0/1"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestPeerRefreshRefusesBadInputAndSkipsANodeWithoutSupavisor(t *testing.T) {
	rf := &fakeRefresher{}
	p := PeerRefresh{Replay: &fakeReplay{past: true}, Refresh: rf}
	for _, tc := range []struct{ tenant, lsn string }{
		{"system", ""}, {"../x", ""}, {"", ""}, {testRef, "banana"}, {testRef, "0/1; drop table"}, {testRef, "0/"},
	} {
		if _, err := p.Do(context.Background(), tc.tenant, tc.lsn); err == nil {
			t.Errorf("%+v accepted", tc)
		}
	}
	p.Runs = func() bool { return false }
	if done, err := p.Do(context.Background(), testRef, "0/1"); done || err != nil {
		t.Errorf("a node without Supavisor: done=%v err=%v", done, err)
	}
	if len(rf.got) != 0 {
		t.Errorf("refreshed %v", rf.got)
	}
}

func TestValidLSN(t *testing.T) {
	for s, want := range map[string]bool{
		"0/0": true, "0/3000100": true, "1A/FF00FF00": true, "ffffffff/ffffffff": true,
		"": false, "0": false, "/1": false, "0/": false, "g/1": false, "0/1/2": false, "123456789/1": false, " 0/1": false,
	} {
		if got := ValidLSN(s); got != want {
			t.Errorf("ValidLSN(%q) = %v", s, got)
		}
	}
}
