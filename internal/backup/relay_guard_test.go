package backup

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// guardEnv is a relay with the replica guard on: the node's role per ref and the cluster epoch
// are what the test says they are.
type guardEnv struct {
	*relayEnv
	mu       sync.Mutex
	replicas map[string]bool
	epoch    int64
	roleErr  error
	epochErr error
	refused  []string
}

func newGuardEnv(t *testing.T, refs ...string) *guardEnv {
	t.Helper()
	e := newTestEnv(t)
	dir, err := os.MkdirTemp("/tmp", "sbg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	g := &guardEnv{replicas: map[string]bool{}, epoch: 3}
	g.relayEnv = &relayEnv{testEnv: e, dir: dir, log: &bytes.Buffer{}}
	g.relay = NewRelay(RelayOptions{
		Config:  e.cfg,
		Service: func(context.Context) (*Service, error) { return e.svc, nil },
		Refs:    func() []string { return refs },
		Socket:  func(ref string) string { return filepath.Join(dir, ref[:6]+".sock") },
		Replica: func(_ context.Context, ref string) (bool, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.replicas[ref], g.roleErr
		},
		Epoch: func(context.Context) (int64, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.epoch, g.epochErr
		},
		Refused: func(ref string, reason error) {
			g.mu.Lock()
			g.refused = append(g.refused, ref+": "+reason.Error())
			g.mu.Unlock()
		},
		Log: slog.New(slog.NewTextHandler(g.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	t.Cleanup(g.relay.Close)
	g.relay.Reconcile()
	return g
}

func (g *guardEnv) set(f func()) { g.mu.Lock(); f(); g.mu.Unlock() }

func (g *guardEnv) promoteOK(t *testing.T, ref string, content []byte) {
	t.Helper()
	writeFile(t, g.cfg.Paths().PromoteOK(ref), content)
}

func TestRelayRefusesPushOfAReplicaUntilPromoteOK(t *testing.T) {
	g := newGuardEnv(t, testRef)
	g.set(func() { g.replicas[testRef] = true })
	ctx := context.Background()
	big := writeWAL(t, walA, 3<<20, 7) // a body the relay has to read past to answer
	small := writeWAL(t, walB, 1<<10, 9)

	if err := RelayPush(ctx, g.sock(testRef), testRef, big); !errors.Is(err, ErrPushRefused) {
		t.Fatalf("push by a replica = %v; want ErrPushRefused", err)
	}
	if _, err := g.store.Stat(ctx, walKey(testRef, walA)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused push reached the archive: %v", err)
	}
	// Reading is not guarded: the replica's restore_command works.
	if err := g.svc.PushWAL(ctx, testRef, small); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "RECOVERYXLOG")
	if err := RelayFetch(ctx, g.sock(testRef), testRef, walB, dest); err != nil {
		t.Fatalf("fetch by a replica: %v", err)
	}
	if err := RelayFetch(ctx, g.sock(testRef), testRef, walA, dest); !errors.Is(err, ErrNoWAL) {
		t.Fatalf("fetch of a missing file by a replica = %v; want ErrNoWAL", err)
	}

	// The promotion procedure wrote promote.ok for the current epoch: the push goes through.
	g.promoteOK(t, testRef, FormatPromoteOK(3))
	if err := RelayPush(ctx, g.sock(testRef), testRef, big); err != nil {
		t.Fatalf("push after promote.ok: %v", err)
	}
	// ErrWALConflict is the second tripwire and still holds for a promoted replica.
	other := writeWAL(t, walA, 3<<20, 99)
	if err := RelayPush(ctx, g.sock(testRef), testRef, other); !errors.Is(err, ErrWALConflict) {
		t.Fatalf("conflicting push by a promoted replica = %v; want ErrWALConflict", err)
	}
	// Demoted again (the registry says replica and promote.ok is gone): refused again.
	if err := os.Remove(g.cfg.Paths().PromoteOK(testRef)); err != nil {
		t.Fatal(err)
	}
	if err := RelayPush(ctx, g.sock(testRef), testRef, small); !errors.Is(err, ErrPushRefused) {
		t.Fatalf("push after promote.ok was removed = %v; want ErrPushRefused", err)
	}
}

func TestRelayPromoteOKMustHoldTheCurrentEpoch(t *testing.T) {
	g := newGuardEnv(t, testRef)
	g.set(func() { g.replicas[testRef] = true })
	ctx := context.Background()
	p := writeWAL(t, walA, 1<<10, 1)

	// Written at an earlier promotion; the cluster has moved on since.
	g.promoteOK(t, testRef, FormatPromoteOK(2))
	err := RelayPush(ctx, g.sock(testRef), testRef, p)
	if !errors.Is(err, ErrPushRefused) || !strings.Contains(err.Error(), "epoch 2") || !strings.Contains(err.Error(), "epoch 3") {
		t.Fatalf("push with a stale promote.ok = %v", err)
	}
	for _, bad := range []string{"", "x\n", "0\n", "-1\n"} {
		g.promoteOK(t, testRef, []byte(bad))
		if err := RelayPush(ctx, g.sock(testRef), testRef, p); !errors.Is(err, ErrPushRefused) {
			t.Fatalf("push with promote.ok %q = %v; want ErrPushRefused", bad, err)
		}
	}
	// The promotion procedure writes the epoch it is about to start (the daemon raises its own
	// right after the promoted cluster is seen), so a higher one authorizes too.
	g.promoteOK(t, testRef, FormatPromoteOK(4))
	if err := RelayPush(ctx, g.sock(testRef), testRef, p); err != nil {
		t.Fatalf("push with promote.ok for epoch 4 at epoch 3: %v", err)
	}
	g.promoteOK(t, testRef, FormatPromoteOK(3))
	if err := RelayPush(ctx, g.sock(testRef), testRef, p); err != nil {
		t.Fatalf("push with promote.ok for epoch 3 at epoch 3: %v", err)
	}
}

func TestRelayGuardLeavesPrimariesAlone(t *testing.T) {
	g := newGuardEnv(t, testRef, testRef2)
	g.set(func() { g.replicas[testRef2] = true }) // testRef is homed here, testRef2 is a replica
	ctx := context.Background()
	p := writeWAL(t, walA, 1<<10, 1)
	if err := RelayPush(ctx, g.sock(testRef), testRef, p); err != nil {
		t.Fatalf("push by a primary: %v", err)
	}
	if err := RelayPush(ctx, g.sock(testRef2), testRef2, p); !errors.Is(err, ErrPushRefused) {
		t.Fatalf("push by the replica of the other project = %v", err)
	}
}

// A role or epoch that cannot be read fails the push with a retryable error, not a refusal that
// looks like a verdict: archive_command retries, and nothing is archived on a guess.
func TestRelayGuardFailsClosedWhenItCannotDecide(t *testing.T) {
	g := newGuardEnv(t, testRef)
	ctx := context.Background()
	p := writeWAL(t, walA, 1<<10, 1)
	g.set(func() { g.roleErr = errors.New("registry unreachable") })
	err := RelayPush(ctx, g.sock(testRef), testRef, p)
	if err == nil || errors.Is(err, ErrPushRefused) || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("push with the role unknown = %v; want a 503", err)
	}
	g.set(func() { g.roleErr, g.replicas[testRef] = nil, true; g.epochErr = errors.New("no cluster") })
	g.promoteOK(t, testRef, FormatPromoteOK(3))
	err = RelayPush(ctx, g.sock(testRef), testRef, p)
	if err == nil || errors.Is(err, ErrPushRefused) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("push with the epoch unknown = %v; want a 503", err)
	}
	if _, err := g.store.Stat(ctx, walKey(testRef, walA)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a push that could not be decided reached the archive: %v", err)
	}
	g.set(func() { g.epochErr = nil })
	if err := RelayPush(ctx, g.sock(testRef), testRef, p); err != nil {
		t.Fatalf("push once the epoch is known: %v", err)
	}
}

// Postgres retries a refused archive_command every few seconds; the log and the alert hook hear
// about it once a minute per project.
func TestRelayGuardReportsRefusalsOncePerMinute(t *testing.T) {
	g := newGuardEnv(t, testRef)
	g.set(func() { g.replicas[testRef] = true })
	p := writeWAL(t, walA, 1<<10, 1)
	for range 5 {
		if err := RelayPush(context.Background(), g.sock(testRef), testRef, p); !errors.Is(err, ErrPushRefused) {
			t.Fatal(err)
		}
	}
	g.mu.Lock()
	n, first := len(g.refused), ""
	if n > 0 {
		first = g.refused[0]
	}
	g.mu.Unlock()
	if n != 1 || !strings.HasPrefix(first, testRef+": ") || !strings.Contains(first, "promote.ok") {
		t.Fatalf("Refused heard %d times: %q", n, first)
	}
	if c := strings.Count(g.log.String(), "push refused for a replica"); c != 1 {
		t.Fatalf("log has %d refusal lines:\n%s", c, g.log.String())
	}
}

// Without the Replica hook the relay is what it was: a node with no replicas needs no promote.ok.
func TestRelayWithoutReplicaHookIsUnguarded(t *testing.T) {
	re := newRelayEnv(t, testRef)
	if err := RelayPush(context.Background(), re.sock(testRef), testRef, writeWAL(t, walA, 1<<10, 1)); err != nil {
		t.Fatal(err)
	}
}

func TestRelayPromoteOKDefaultsToThePathsOfTheConfig(t *testing.T) {
	e := newTestEnv(t)
	r := NewRelay(RelayOptions{Config: e.cfg})
	if got, want := r.opt.PromoteOK(testRef), e.cfg.Paths().PromoteOK(testRef); got != want || !strings.HasSuffix(got, "/projects/"+testRef+"/promote.ok") {
		t.Fatalf("PromoteOK = %q, want %q", got, want)
	}
}
