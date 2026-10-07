package backup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
)

func TestRelayPeerUnits(t *testing.T) {
	const a, b = "aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb"
	for _, tc := range []struct {
		name, cgroup, ref string
		want              bool
	}{
		{"own postgres", "0::/system.slice/supavise-postgres@" + a + ".service\n", a, true},
		{"own postgres, sub-cgroup", "0::/system.slice/supavise-postgres@" + a + ".service/postmaster\n", a, true},
		{"other project's postgres", "0::/system.slice/supavise-postgres@" + b + ".service\n", a, false},
		{"gotrue of the same project", "0::/system.slice/supavise-gotrue@" + a + ".service\n", a, false},
		{"edge runtime", "0::/system.slice/supavise-edge-runtime.service\n", a, false},
		{"studio", "0::/system.slice/supavise-studio.service\n", a, false},
		{"daemon", "0::/system.slice/supavise.service\n", a, true},
		{"base backup", "0::/system.slice/system-sb\\x2dbasebackup.slice/supavise-basebackup@" + b + ".service\n", a, true},
		{"operator session", "0::/user.slice/user-1000.slice/session-3.scope\n", a, true},
		{"no unit", "0::/\n", a, true},
		{"cgroup v1", "12:cpu:/x\n1:name=systemd:/system.slice/supavise-storage.service\n", a, false},
	} {
		unit, err := peerUnit(tc.cgroup)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := relayPeerUnitAllowed(unit, tc.ref); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v (unit %q)", tc.name, got, tc.want, unit)
		}
	}
	if _, err := peerUnit(""); err == nil {
		t.Error("an empty cgroup file must fail closed")
	}
}

// A peer that PeerCheck refuses never reaches the handler; the client sees a closed connection.
func TestRelayRefusesAPeerThatFailsTheCheck(t *testing.T) {
	re := newRelayEnv(t, testRef)
	r := NewRelay(RelayOptions{
		Config:    re.cfg,
		Service:   func(context.Context) (*Service, error) { return re.svc, nil },
		Refs:      func() []string { return []string{testRef2} },
		Socket:    func(ref string) string { return filepath.Join(re.dir, ref[:6]+".sock") },
		PeerCheck: func(net.Conn, string) error { return errors.New("not allowed") },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(r.Close)
	r.Reconcile()
	if got := r.Served(); len(got) != 1 {
		t.Fatalf("served = %v", got)
	}
	p := writeWAL(t, walA, 1<<10, 1)
	if err := RelayPush(context.Background(), re.sock(testRef2), testRef2, p); err == nil {
		t.Fatal("a push from a refused peer succeeded")
	}
	// A refused connection does not stop the listener: the next connection is refused the same way.
	if err := RelayPing(context.Background(), re.sock(testRef2)); err == nil {
		t.Fatal("a ping from a refused peer succeeded")
	}
}
