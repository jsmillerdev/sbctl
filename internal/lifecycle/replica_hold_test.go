package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReplayedPast(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		replay, receive, lsn string
		want                 bool
	}{
		{"the checkpoint record is replayed", "0/3000100", "0/3000100", "0/3000060", true},
		{"replay stands at the start of the record", "0/3000060", "0/3000060", "0/3000060", false},
		{"replay is before the record", "0/3000028", "0/3000028", "0/3000060", false},
		{"received but not replayed", "0/3000100", "0/3000200", "0/3000060", false},
		{"no receiver", "0/3000100", "", "0/3000060", true},
		{"high words compare first", "2/10", "2/10", "1/FFFFFFFF", true},
		{"a low word above that of a later high word", "1/FFFFFFFF", "1/FFFFFFFF", "2/10", false},
	} {
		got, err := replayedPast(tc.replay, tc.receive, tc.lsn)
		if err != nil || got != tc.want {
			t.Errorf("%s: replayedPast(%s, %s, %s) = %v, %v; want %v", tc.name, tc.replay, tc.receive, tc.lsn, got, err, tc.want)
		}
	}
	for _, bad := range [][3]string{{"x", "", "0/1"}, {"0/1", "", "nope"}, {"0/1", "1", "0/1"}, {"0/zz", "", "0/1"}} {
		if _, err := replayedPast(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("replayedPast(%q, %q, %q) accepted a malformed LSN", bad[0], bad[1], bad[2])
		}
	}
}

// portHolder stands for the mesh's forwarders: hold is PlaneOptions.HoldPorts.
type portHolder struct {
	mu       sync.Mutex
	held     []string
	released []string
	onHold   func(ref string)
}

func (h *portHolder) hold(ref string) func() {
	h.mu.Lock()
	h.held = append(h.held, ref)
	h.mu.Unlock()
	if h.onHold != nil {
		h.onHold(ref)
	}
	return func() {
		h.mu.Lock()
		h.released = append(h.released, ref)
		h.mu.Unlock()
	}
}

func (h *portHolder) state() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fmt.Sprintf("held %v, released %v", h.held, h.released)
}

// portSup fails the start of the project's Postgres unit when something else holds the port, as the
// kernel would: the holder stands for the mesh forwarder that has a canonical port on the node of a
// replica.
type portSup struct {
	*replicaSup
	port int
}

func (p portSup) Start(ctx context.Context, u string) error {
	if u == "supavise-postgres@"+testRef+".service" {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p.port))
		if err != nil {
			return fmt.Errorf("start %s: %w", u, err)
		}
		l.Close()
	}
	return p.replicaSup.Start(ctx, u)
}

// forwarderFixture is a standby whose project has a forwarder on its canonical port.
func forwarderFixture(t *testing.T) (*replicaFixture, net.Listener) {
	t.Helper()
	fastReplicaPoll(t)
	f := newReplicaFixture(t)
	f.seeded(t, true)
	f.sql.status[f.rp.Port] = ClusterStatus{InRecovery: true, ReceiverStatus: "streaming"}
	fwd, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", f.cp.Port))
	if err != nil {
		// A machine that is busy skips the test; CI must not, or the hold would go untested unseen.
		if os.Getenv("CI") != "" {
			t.Fatalf("port %d is in use on this machine: %v", f.cp.Port, err)
		}
		t.Skipf("port %d is in use on this machine: %v", f.cp.Port, err)
	}
	t.Cleanup(func() { fwd.Close() })
	f.pl.sup = portSup{replicaSup: f.sup, port: f.cp.Port}
	return f, fwd
}

func TestPromoteReplicaTakesTheCanonicalPortFromTheForwarder(t *testing.T) {
	ctx := context.Background()

	// Without the hold the primary cannot bind the port the forwarder has.
	f, fwd := forwarderFixture(t)
	if err := f.pl.PromoteReplica(ctx, f.t, PromoteOptions{Epoch: 2}); err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("a promotion beside a forwarder: %v", err)
	}
	fwd.Close()

	// With it the forwarder lets go before the primary starts, and the hold stays until Start.
	f, fwd = forwarderFixture(t)
	h := &portHolder{onHold: func(string) { fwd.Close() }}
	f.pl.SetPortHolder(h.hold)
	if err := f.pl.PromoteReplica(ctx, f.t, PromoteOptions{Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	if got := h.state(); got != "held ["+testRef+"], released []" {
		t.Fatalf("after the promotion: %s", got)
	}
	if err := f.pl.Start(ctx, f.t.Project, f.t.Keys); err != nil {
		t.Fatal(err)
	}
	if got := h.state(); got != "held ["+testRef+"], released ["+testRef+"]" {
		t.Fatalf("after Start: %s", got)
	}
	// A second Start has nothing to release.
	if err := f.pl.Start(ctx, f.t.Project, f.t.Keys); err != nil {
		t.Fatal(err)
	}
	if got := h.state(); got != "held ["+testRef+"], released ["+testRef+"]" {
		t.Fatalf("after a second Start: %s", got)
	}
}

func TestAFailedPromotionGivesThePortsBack(t *testing.T) {
	// The forwarder stays, so the primary cannot start on the port the promotion took: the hold is
	// given back when the call fails.
	f, fwd := forwarderFixture(t)
	h := &portHolder{}
	f.pl.SetPortHolder(h.hold)
	if err := f.pl.PromoteReplica(context.Background(), f.t, PromoteOptions{Epoch: 2}); err == nil {
		t.Fatal("the promotion succeeded beside the forwarder")
	}
	if got := h.state(); got != "held ["+testRef+"], released ["+testRef+"]" {
		t.Fatalf("after a failed promotion: %s", got)
	}
	fwd.Close()

	// A promotion that ends before the standby is promoted leaves the forwarders and the stream
	// through them alone.
	g, _ := forwarderFixture(t)
	h2 := &portHolder{}
	g.pl.SetPortHolder(h2.hold)
	g.sql.promoteErr = errors.New("pg_promote: the promotion did not finish")
	if err := g.pl.PromoteReplica(context.Background(), g.t, PromoteOptions{Epoch: 2}); err == nil {
		t.Fatal("the promotion succeeded")
	}
	if len(h2.held) != 0 {
		t.Fatalf("a promotion that did not start took the ports: %s", h2.state())
	}

	// A promotion that is not needed holds nothing: the cluster is a primary on the canonical port.
	k := newReplicaFixture(t)
	k.seeded(t, false)
	k.sql.status[k.cp.Port] = ClusterStatus{}
	if err := k.pl.writePromoteOK(testRef, 4); err != nil {
		t.Fatal(err)
	}
	h3 := &portHolder{}
	k.pl.SetPortHolder(h3.hold)
	if err := k.pl.PromoteReplica(context.Background(), k.t, PromoteOptions{Epoch: 4}); err != nil || len(h3.held) != 0 {
		t.Fatalf("a repeated promotion: %v, %s", err, h3.state())
	}
}

func TestDemoteToReplicaHoldsThePortsWhileItStarts(t *testing.T) {
	f := newReplicaFixture(t)
	f.seeded(t, false)
	writeControl(t, f.cp.Data, 1, 0x3000060, false)
	h := &portHolder{}
	f.pl.SetPortHolder(h.hold)
	if err := f.pl.DemoteToReplica(context.Background(), f.t); err != nil {
		t.Fatal(err)
	}
	if got := h.state(); got != "held ["+testRef+"], released ["+testRef+"]" {
		t.Fatalf("after the demotion: %s", got)
	}
}

func TestDemoteToReplicaAfterTheStandbyItStartedCrashed(t *testing.T) {
	ctx := context.Background()
	f := newReplicaFixture(t)
	f.seeded(t, true) // standby.signal and the recovery settings are in place
	// pg_control of a standby that crashed says it was in archive recovery, which is no unclean
	// primary: nothing has to be rebuilt.
	writeControl(t, f.cp.Data, 5, 0x3000060, false)
	if err := f.pl.DemoteToReplica(ctx, f.t); err != nil {
		t.Fatalf("a demotion retried after a standby crash: %v", err)
	}
	if ops := f.sup.ops(); !strings.Contains(ops, "start supavise-postgres@"+testRef) || !strings.Contains(ops, "start supavise-postgrest@"+testRef) {
		t.Fatalf("the standby was not started:\n%s", ops)
	}
	// A cluster that is a crashed primary is still refused: no standby.signal.
	g := newReplicaFixture(t)
	g.seeded(t, false)
	writeControl(t, g.cp.Data, 4, 0x3000060, false) // in crash recovery
	if err := g.pl.DemoteToReplica(ctx, g.t); !errors.Is(err, ErrNotCleanShutdown) {
		t.Fatalf("a crashed primary: %v", err)
	}
}

func TestStandbyConfigured(t *testing.T) {
	dir := t.TempDir()
	write := func(conf string, signal bool) {
		if err := os.WriteFile(filepath.Join(dir, "postgresql.auto.conf"), []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(dir, "standby.signal"))
		if signal {
			if err := os.WriteFile(filepath.Join(dir, "standby.signal"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	both := "primary_conninfo = 'x'\nrestore_command = 'y'\n"
	write(both, true)
	if !standbyConfigured(dir) {
		t.Fatal("a configured standby")
	}
	write(both, false)
	if standbyConfigured(dir) {
		t.Fatal("no standby.signal")
	}
	write("primary_conninfo = 'x'\n# restore_command = 'y'\n", true)
	if standbyConfigured(dir) {
		t.Fatal("a commented restore_command")
	}
	if standbyConfigured(t.TempDir()) {
		t.Fatal("an empty directory")
	}
}

func TestConfStringEscapes(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:         `'plain'`,
		`it's`:          `'it''s'`,
		`back\slash`:    `'back\\slash'`,
		`both \ and '`:  `'both \\ and '''`,
		`a=\'b' x=1`:    `'a=\\''b'' x=1'`,
		`C:\state\dir`:  `'C:\\state\\dir'`,
		`no escapes ''`: `'no escapes '''''`,
	} {
		if got := confString(in); got != want {
			t.Errorf("confString(%q) = %s, want %s", in, got, want)
		}
	}
	// A conninfo whose password has a quote and a backslash survives both layers: libpq quoting inside,
	// postgresql.conf quoting outside.
	got := confString(primaryConninfo(5432, `p'a\ss`, "id"))
	if !strings.Contains(got, `password=''p\\''a\\\\ss''`) {
		t.Errorf("conf line: %s", got)
	}
}

func TestReloadSchemaLeavesAFreshPostgREST(t *testing.T) {
	if !signalsAvailable() {
		t.Skip("no sh")
	}
	f := newReplicaFixture(t)
	pid, got, stop := startSignalProbe(t)
	defer stop()
	f.sup.running["supavise-postgrest@"+testRef+".service"] = true
	f.pl.sup = pidSup{replicaSup: f.sup, pid: pid, since: time.Now()}
	if err := f.pl.ReloadSchema(context.Background(), testRef); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
		t.Fatal("a PostgREST that had just started got SIGUSR1")
	case <-time.After(300 * time.Millisecond):
	}
	// Once it has run for the grace period the signal goes.
	f.pl.sup = pidSup{replicaSup: f.sup, pid: pid, since: time.Now().Add(-reloadGrace - time.Second)}
	if err := f.pl.ReloadSchema(context.Background(), testRef); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the process did not get SIGUSR1")
	}
}
