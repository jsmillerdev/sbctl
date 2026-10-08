package backup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// TestLeaderMarkerS3StoreFake runs the marker scenario against the in-process fake S3 server.
func TestLeaderMarkerS3StoreFake(t *testing.T) { runLeaderMarkerS3(t, fakeS3(t), true) }

// TestLeaderMarkerS3StoreReal runs it against a real S3-compatible service (Garage in CI);
// skipped without SUPAVISE_TEST_S3_*.
func TestLeaderMarkerS3StoreReal(t *testing.T) { runLeaderMarkerS3(t, s3FromEnv(t), false) }

// runLeaderMarkerS3 runs the scenario over o. A strict run expects the service to enforce
// conditional writes atomically (the fake does); a real service is only held to what the marker
// promises either way, and the log says what it enforces.
func runLeaderMarkerS3(t *testing.T, o S3Options, strict bool) {
	ctx := context.Background()
	st, err := NewS3Store(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	cleanS3(t, st)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc, err := New(Options{Store: st, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	// Whether the service enforces If-None-Match and If-Match, or ignores them (the marker then
	// falls back to a read followed by a plain write, which the design allows).
	probe := "_node/probe-" + fmt.Sprint(time.Now().UnixNano())
	if err := st.PutIf(ctx, probe, []byte("a"), ""); err != nil {
		t.Fatalf("PutIf of a new object: %v", err)
	}
	enforced := errors.Is(st.PutIf(ctx, probe, []byte("b"), ""), ErrPreconditionFailed)
	_, tag, err := st.GetTagged(ctx, probe)
	if err != nil || tag == "" {
		t.Fatalf("GetTagged = %q, %v", tag, err)
	}
	enforcedMatch := errors.Is(st.PutIf(ctx, probe, []byte("c"), `"0123456789abcdef0123456789abcdef"`), ErrPreconditionFailed)
	if err := st.PutIf(ctx, probe, []byte("d"), tag); err != nil && !errors.Is(err, ErrConditionalUnsupported) {
		t.Fatalf("PutIf with the current tag: %v", err)
	}
	t.Logf("store enforces If-None-Match: %v, If-Match: %v", enforced, enforcedMatch)
	_ = st.Delete(ctx, probe)

	if m, err := svc.ReadLeaderMarker(ctx); m != nil || err != nil {
		t.Fatalf("marker of an empty bucket = %+v, %v", m, err)
	}
	if err := svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 2, Leader: "n1"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 3, Leader: "n2"}); err != nil {
		t.Fatal(err)
	}
	m, err := svc.ReadLeaderMarker(ctx)
	if err != nil || m == nil || m.Epoch != 3 || m.Leader != "n2" || !m.At.Equal(now) {
		t.Fatalf("marker = %+v, %v", m, err)
	}
	if _, err := st.Stat(ctx, LeaderMarkerKey); err != nil {
		t.Fatalf("%s is not in the bucket: %v", LeaderMarkerKey, err)
	}
	for _, w := range []LeaderMarker{{Epoch: 2, Leader: "n1"}, {Epoch: 3, Leader: "n1"}} {
		if err := svc.WriteLeaderMarker(ctx, w); !errors.Is(err, ErrMarkerNewer) {
			t.Fatalf("write %+v over epoch 3 by n2 = %v; want ErrMarkerNewer", w, err)
		}
	}

	// Eight nodes promote to epoch 4 at the same moment. One marker wins and the others are told
	// so; with conditional writes enforced there is never a second winner.
	var mu sync.Mutex
	var winners []string
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leader := fmt.Sprintf("n%d", i+3)
			err := svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 4, Leader: leader})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, leader)
			case !errors.Is(err, ErrMarkerNewer):
				t.Errorf("%s: %v", leader, err)
			}
		}()
	}
	wg.Wait()
	m, err = svc.ReadLeaderMarker(ctx)
	if err != nil || m == nil || m.Epoch != 4 {
		t.Fatalf("marker after the race = %+v, %v", m, err)
	}
	if len(winners) == 0 || (strict && len(winners) != 1) {
		t.Fatalf("winners = %v; want one (strict: %v)", winners, strict)
	}
	if !slices.Contains(winners, m.Leader) {
		t.Fatalf("marker names %s, but the winners were %v", m.Leader, winners)
	}
	t.Logf("winners of the race: %v; marker names %s", winners, m.Leader)
}
