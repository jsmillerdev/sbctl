package backup

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	clock    time.Time
}

func newGuardEnv(t *testing.T, refs ...string) *guardEnv {
	t.Helper()
	e := newTestEnv(t)
	dir, err := os.MkdirTemp("/tmp", "sbg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	g := &guardEnv{replicas: map[string]bool{}, epoch: 3, clock: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
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
	g.relay.now = func() time.Time {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.clock
	}
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
	// Demoted again in place: ConfigureStandby deletes promote.ok, and the push is refused again.
	if err := g.svc.ConfigureStandby(ReplicaSeedPlan{Ref: testRef, DataDir: fakeDataDir(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g.cfg.Paths().PromoteOK(testRef)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("promote.ok after ConfigureStandby: %v", err)
	}
	if err := RelayPush(ctx, g.sock(testRef), testRef, small); !errors.Is(err, ErrPushRefused) {
		t.Fatalf("push after the demotion = %v; want ErrPushRefused", err)
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

// A promote.ok that exists but cannot be read is no authorization either, and says why.
func TestRelayRefusesWhenPromoteOKCannotBeRead(t *testing.T) {
	g := newGuardEnv(t, testRef)
	g.set(func() { g.replicas[testRef] = true })
	if err := os.MkdirAll(g.cfg.Paths().PromoteOK(testRef), 0o755); err != nil { // a directory is not a file
		t.Fatal(err)
	}
	err := RelayPush(context.Background(), g.sock(testRef), testRef, writeWAL(t, walA, 1<<10, 1))
	if !errors.Is(err, ErrPushRefused) || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("push with an unreadable promote.ok = %v", err)
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
	push := func() {
		t.Helper()
		if err := RelayPush(context.Background(), g.sock(testRef), testRef, p); !errors.Is(err, ErrPushRefused) {
			t.Fatal(err)
		}
	}
	heard := func() (n int, first string) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if len(g.refused) > 0 {
			first = g.refused[0]
		}
		return len(g.refused), first
	}
	for range 5 {
		push()
	}
	if n, first := heard(); n != 1 || !strings.HasPrefix(first, testRef+": ") || !strings.Contains(first, "promote.ok") {
		t.Fatalf("Refused heard %d times: %q", n, first)
	}
	if c := strings.Count(g.log.String(), "push refused for a replica"); c != 1 {
		t.Fatalf("log has %d refusal lines:\n%s", c, g.log.String())
	}
	// Still inside the minute: nothing new. Past it: the next refusal is reported again.
	g.set(func() { g.clock = g.clock.Add(59 * time.Second) })
	push()
	if n, _ := heard(); n != 1 {
		t.Fatalf("Refused heard %d times after 59 seconds", n)
	}
	g.set(func() { g.clock = g.clock.Add(2 * time.Second) })
	push()
	push()
	if n, _ := heard(); n != 2 {
		t.Fatalf("Refused heard %d times after a minute; want 2", n)
	}
	if c := strings.Count(g.log.String(), "push refused for a replica"); c != 2 {
		t.Fatalf("log has %d refusal lines after a minute:\n%s", c, g.log.String())
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

// A relay that was given a role check and no way to find promote.ok has nothing to authorize a
// replica with: it refuses, and does not panic.
func TestRelayGuardWithoutPromoteOKPathRefuses(t *testing.T) {
	r := NewRelay(RelayOptions{Replica: func(context.Context, string) (bool, error) { return true, nil }})
	code, err := r.pushGuard(context.Background(), testRef)
	if err == nil || code != http.StatusPreconditionFailed {
		t.Fatalf("pushGuard = %d, %v; want a 412", code, err)
	}
	r = NewRelay(RelayOptions{Replica: func(context.Context, string) (bool, error) { return false, nil }})
	if code, err := r.pushGuard(context.Background(), testRef); err != nil || code != 0 {
		t.Fatalf("pushGuard of a primary = %d, %v", code, err)
	}
}

// A guard that cannot judge a push (the registry or the epoch does not answer) holds it back with a 503
// and says so in the log, once a minute per project: a registry that stays away stalls the archiving
// of every project, and Postgres's own log is the only other place that shows it.
func TestRelayLogsAPushItCouldNotJudgeOncePerMinute(t *testing.T) {
	g := newGuardEnv(t, testRef)
	ctx := context.Background()
	g.set(func() { g.roleErr = errors.New("registry unreachable") })
	wal := writeWAL(t, walA, 1<<10, 7)
	for i := 0; i < 3; i++ {
		if err := RelayPush(ctx, g.sock(testRef), testRef, wal); err == nil {
			t.Fatal("a push the guard could not judge went through")
		}
	}
	if n := strings.Count(g.log.String(), "the guard could not tell whether the cluster may archive"); n != 1 {
		t.Fatalf("logged %d times in one minute:\n%s", n, g.log.String())
	}
	g.set(func() { g.clock = g.clock.Add(2 * time.Minute) })
	if err := RelayPush(ctx, g.sock(testRef), testRef, wal); err == nil {
		t.Fatal("a push the guard could not judge went through")
	}
	if n := strings.Count(g.log.String(), "the guard could not tell whether the cluster may archive"); n != 2 {
		t.Fatalf("logged %d times after a minute:\n%s", n, g.log.String())
	}
	// The epoch that does not answer is the same.
	g.set(func() { g.roleErr, g.replicas[testRef], g.epochErr = nil, true, errors.New("no epoch") })
	g.promoteOK(t, testRef, FormatPromoteOK(3))
	g.set(func() { g.clock = g.clock.Add(2 * time.Minute) })
	if err := RelayPush(ctx, g.sock(testRef), testRef, wal); err == nil {
		t.Fatal("a push under an unknown epoch went through")
	}
	if !strings.Contains(g.log.String(), "cannot read the cluster epoch") {
		t.Fatalf("the epoch failure is not in the log:\n%s", g.log.String())
	}
}
