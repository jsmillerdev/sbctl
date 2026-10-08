package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// casStore is a FileStore with the conditional writes of S3. interleave runs inside PutIf, before
// the tag is compared, to let a test slip another writer in between a read and a write.
type casStore struct {
	*FileStore
	mu          sync.Mutex
	interleave  func(call int)
	unsupported bool
	calls       int
	tags        []string // the tag each PutIf was given
}

func tagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (c *casStore) GetTagged(ctx context.Context, key string) ([]byte, string, error) {
	rc, err := c.Get(ctx, key)
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return b, tagOf(b), err
}

func (c *casStore) PutIf(ctx context.Context, key string, data []byte, tag string) error {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.tags = append(c.tags, tag)
	c.mu.Unlock()
	if c.unsupported {
		return ErrConditionalUnsupported
	}
	if c.interleave != nil {
		c.interleave(call)
	}
	cur := ""
	if b, t, err := c.GetTagged(ctx, key); err == nil {
		cur = t
		_ = b
	}
	if cur != tag {
		return ErrPreconditionFailed
	}
	return c.Put(ctx, key, strings.NewReader(string(data)))
}

func markerEnv(t *testing.T) (*testEnv, *casStore) {
	t.Helper()
	e := newTestEnv(t)
	return e, &casStore{FileStore: e.store}
}

func TestLeaderMarkerRoundTripOnAPlainStore(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if m, err := e.svc.ReadLeaderMarker(ctx); m != nil || err != nil {
		t.Fatalf("marker of an empty store = %+v, %v", m, err)
	}
	if err := e.svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 2, Leader: "n1"}); err != nil {
		t.Fatal(err)
	}
	m, err := e.svc.ReadLeaderMarker(ctx)
	if err != nil || m == nil || m.Epoch != 2 || m.Leader != "n1" || !m.At.Equal(e.now) {
		t.Fatalf("marker = %+v, %v; want epoch 2 by n1 at the service's clock %s", m, err, e.now)
	}
	// The object sits where the contract says and holds JSON a person can read.
	if b := readAll(t, e.store, LeaderMarkerKey); !strings.Contains(string(b), `"epoch": 2`) || !strings.Contains(string(b), `"leader": "n1"`) {
		t.Fatalf("%s = %s", LeaderMarkerKey, b)
	}
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.FixedZone("x", 3600))
	// The same epoch under the same leader is written again (a resumed promotion); a higher one replaces it.
	for _, w := range []LeaderMarker{{Epoch: 2, Leader: "n1", At: at}, {Epoch: 3, Leader: "n2", At: at}} {
		if err := e.svc.WriteLeaderMarker(ctx, w); err != nil {
			t.Fatalf("write %+v: %v", w, err)
		}
	}
	m, _ = e.svc.ReadLeaderMarker(ctx)
	if m.Epoch != 3 || m.Leader != "n2" || !m.At.Equal(at) || m.At.Location() != time.UTC {
		t.Fatalf("marker = %+v", m)
	}
}

func TestLeaderMarkerRefusesAnOlderOrContestedEpoch(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if err := e.svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 5, Leader: "n2"}); err != nil {
		t.Fatal(err)
	}
	before := readAll(t, e.store, LeaderMarkerKey)
	for name, w := range map[string]LeaderMarker{
		"older epoch":                {Epoch: 4, Leader: "n1"},
		"older epoch, same leader":   {Epoch: 4, Leader: "n2"},
		"same epoch, another leader": {Epoch: 5, Leader: "n1"},
		"much older epoch":           {Epoch: 1, Leader: "n3"},
	} {
		err := e.svc.WriteLeaderMarker(ctx, w)
		if !errors.Is(err, ErrMarkerNewer) {
			t.Errorf("%s: %v; want ErrMarkerNewer", name, err)
		}
	}
	if after := readAll(t, e.store, LeaderMarkerKey); string(after) != string(before) {
		t.Fatalf("a refused write changed the marker:\n%s\n%s", before, after)
	}
}

func TestLeaderMarkerRejectsNonsense(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	for _, w := range []LeaderMarker{{Epoch: 0, Leader: "n1"}, {Epoch: -1, Leader: "n1"}, {Epoch: 1}, {Epoch: 1, Leader: "N 1"}, {Epoch: 1, Leader: "../x"}} {
		if err := e.svc.WriteLeaderMarker(ctx, w); err == nil || errors.Is(err, ErrMarkerNewer) {
			t.Errorf("write %+v = %v; want a plain error", w, err)
		}
	}
	if _, err := e.store.Stat(ctx, LeaderMarkerKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused marker was stored: %v", err)
	}
	// A marker nobody can read may hold a higher epoch: it is an error, not "none".
	for _, junk := range []string{"not json", `{"epoch":0,"leader":"n1"}`, `{"epoch":3}`} {
		if err := e.store.Put(ctx, LeaderMarkerKey, strings.NewReader(junk)); err != nil {
			t.Fatal(err)
		}
		if m, err := e.svc.ReadLeaderMarker(ctx); err == nil || m != nil {
			t.Errorf("read of %q = %+v, %v", junk, m, err)
		}
		if err := e.svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 9, Leader: "n1"}); err == nil || errors.Is(err, ErrMarkerNewer) {
			t.Errorf("write over %q = %v; want an error that is not ErrMarkerNewer", junk, err)
		}
	}
}

func TestLeaderMarkerConditionalWrites(t *testing.T) {
	e, cs := markerEnv(t)
	e.svc.opt.Store = cs
	ctx := context.Background()
	if err := e.svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 2, Leader: "n1"}); err != nil {
		t.Fatal(err)
	}
	first := readAll(t, e.store, LeaderMarkerKey)
	if err := e.svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 3, Leader: "n2"}); err != nil {
		t.Fatal(err)
	}
	// The first write asked for "no object", the second for the object the first wrote.
	if len(cs.tags) != 2 || cs.tags[0] != "" || cs.tags[1] != tagOf(first) {
		t.Fatalf("tags given to PutIf = %q; want [\"\" %q]", cs.tags, tagOf(first))
	}
	if m, err := e.svc.ReadLeaderMarker(ctx); err != nil || m.Epoch != 3 {
		t.Fatalf("marker = %+v, %v", m, err)
	}
}

// Two nodes promote at once: both read the same marker, one writes first. The other's conditional
// write fails, it reads again, sees the winner and stops.
func TestLeaderMarkerLosesTheRaceToAnotherWriter(t *testing.T) {
	for name, winner := range map[string]LeaderMarker{
		"same epoch":   {Epoch: 5, Leader: "n3"},
		"higher epoch": {Epoch: 6, Leader: "n3"},
	} {
		t.Run(name, func(t *testing.T) {
			e, cs := markerEnv(t)
			e.svc.opt.Store = cs
			ctx := context.Background()
			if err := e.svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 4, Leader: "n1"}); err != nil {
				t.Fatal(err)
			}
			cs.calls, cs.tags = 0, nil
			cs.interleave = func(call int) {
				if call != 1 {
					return
				}
				b := []byte(`{"epoch": ` + string(rune('0'+winner.Epoch)) + `, "leader": "` + winner.Leader + `", "at": "2026-10-08T00:00:00Z"}`)
				if err := cs.FileStore.Put(ctx, LeaderMarkerKey, strings.NewReader(string(b))); err != nil {
					t.Error(err)
				}
			}
			err := e.svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 5, Leader: "n2"})
			if !errors.Is(err, ErrMarkerNewer) {
				t.Fatalf("write that lost the race = %v; want ErrMarkerNewer", err)
			}
			if m, _ := e.svc.ReadLeaderMarker(ctx); m.Leader != "n3" {
				t.Fatalf("marker = %+v; the winner's write must stand", m)
			}
			if cs.calls != 1 {
				t.Errorf("PutIf calls = %d; the loser must stop after re-reading", cs.calls)
			}
		})
	}
}

// A write that something unrelated disturbs (a lower epoch slips in) starts over and goes through;
// one that is disturbed every time gives up with an error.
func TestLeaderMarkerRetriesAfterAnUnrelatedWrite(t *testing.T) {
	e, cs := markerEnv(t)
	e.svc.opt.Store = cs
	ctx := context.Background()
	n := 0
	cs.interleave = func(call int) {
		if call == 1 {
			n++
			if err := cs.FileStore.Put(ctx, LeaderMarkerKey, strings.NewReader(`{"epoch": 1, "leader": "n9", "at": "2026-10-08T00:00:00Z"}`)); err != nil {
				t.Error(err)
			}
		}
	}
	if err := e.svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 5, Leader: "n2"}); err != nil {
		t.Fatalf("write after one disturbance: %v", err)
	}
	if m, _ := e.svc.ReadLeaderMarker(ctx); m.Epoch != 5 || m.Leader != "n2" || n != 1 {
		t.Fatalf("marker = %+v after %d disturbances", m, n)
	}

	e2, cs2 := markerEnv(t)
	e2.svc.opt.Store = cs2
	cs2.interleave = func(call int) {
		body := `{"epoch": 1, "leader": "n9", "at": "2026-10-08T00:00:0` + string(rune('0'+call)) + `Z"}`
		if err := cs2.FileStore.Put(ctx, LeaderMarkerKey, strings.NewReader(body)); err != nil {
			t.Error(err)
		}
	}
	err := e2.svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 5, Leader: "n2"})
	if err == nil || errors.Is(err, ErrMarkerNewer) || cs2.calls != markerAttempts {
		t.Fatalf("write disturbed every time = %v after %d calls; want an error after %d", err, cs2.calls, markerAttempts)
	}
}

// A service that ignores If-Match falls back to a plain put, as the design says.
func TestLeaderMarkerFallsBackWhenTheStoreIgnoresConditions(t *testing.T) {
	e, cs := markerEnv(t)
	cs.unsupported = true
	e.svc.opt.Store = cs
	ctx := context.Background()
	if err := e.svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 2, Leader: "n1"}); err != nil {
		t.Fatal(err)
	}
	if m, err := e.svc.ReadLeaderMarker(ctx); err != nil || m.Epoch != 2 {
		t.Fatalf("marker = %+v, %v", m, err)
	}
	// Without conditions the older-epoch check still holds for writers that do not race.
	if err := e.svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 1, Leader: "n2"}); !errors.Is(err, ErrMarkerNewer) {
		t.Fatalf("older epoch = %v", err)
	}
}

func TestMarkerStoreNeedsNoService(t *testing.T) {
	e := newTestEnv(t)
	ms := MarkerStore(e.store)
	ctx := context.Background()
	if m, err := ms.ReadLeaderMarker(ctx); m != nil || err != nil {
		t.Fatalf("empty = %+v, %v", m, err)
	}
	if err := ms.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 1, Leader: "n1"}); err != nil {
		t.Fatal(err)
	}
	m, err := ms.ReadLeaderMarker(ctx)
	if err != nil || m.Epoch != 1 || time.Since(m.At) > time.Minute || m.At.IsZero() {
		t.Fatalf("marker = %+v, %v", m, err)
	}
	// The Service and the bare store read the same object.
	if sm, _ := e.svc.ReadLeaderMarker(ctx); sm == nil || sm.Epoch != 1 {
		t.Fatalf("service reads %+v", sm)
	}
}

// _node/ (the leader marker and the key escrows) and _stack/ (the CloudFormation template) are not
// project refs: prune neither reads nor removes them, however old they are.
func TestPruneLeavesNodeAndStackPrefixesAlone(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.cfg.Backup.RetentionDays = 7
	e.addProject(t, testRef)
	e.fakeBackup(t, testRef, e.now.Add(-days(30)), 1, 1)
	e.fakeBackup(t, testRef, e.now.Add(-days(1)), 1, 9)
	keep := []string{LeaderMarkerKey, EscrowKeyFor("0123456789abcdef"), StackPrefix + strings.Repeat("a", 64) + ".yaml", StackPrefix + "nested/other"}
	for _, k := range keep {
		if err := e.store.Put(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.PruneAll(ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range keep {
		if _, err := e.store.Stat(ctx, k); err != nil {
			t.Errorf("%s after PruneAll: %v", k, err)
		}
	}
	for _, ref := range []string{"_node", "_stack"} {
		if _, err := e.svc.Prune(ctx, ref); err == nil {
			t.Errorf("Prune(%q) did not refuse", ref)
		}
	}
	for _, k := range keep {
		if _, err := e.store.Stat(ctx, k); err != nil {
			t.Errorf("%s after Prune: %v", k, err)
		}
	}
}
