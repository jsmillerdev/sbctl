package units

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	godbus "github.com/godbus/dbus/v5"
)

func noWait(t *testing.T) {
	t.Helper()
	old := busRetryDelay
	busRetryDelay = func(int) time.Duration { return 0 }
	t.Cleanup(func() { busRetryDelay = old })
}

var noReply = godbus.Error{Name: "org.freedesktop.DBus.Error.NoReply", Body: []any{"Message recipient disconnected from message bus without replying"}}

func TestTransientBusError(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{noReply, true},
		{fmt.Errorf("units: stop x: %w", noReply), true},
		{godbus.Error{Name: "org.freedesktop.DBus.Error.Disconnected"}, true},
		{godbus.ErrClosed, true},
		{io.EOF, true},
		{godbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}, false},
		{godbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit"}, false},
		{errors.New("units: start job finished with \"failed\""), false},
		{context.Canceled, false},
	} {
		if got := transientBusError(c.err); got != c.want {
			t.Errorf("transientBusError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// A request the bus dropped once is sent again and succeeds.
func TestRetryBusSendsTheRequestAgain(t *testing.T) {
	noWait(t)
	calls := 0
	err := retryBus(context.Background(), nil, "stop u", func() error {
		calls++
		if calls == 1 {
			return noReply
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("err = %v after %d calls, want nil after 2", err, calls)
	}
}

// A bus that never answers is given busAttempts requests and its error comes back unchanged.
func TestRetryBusGivesUp(t *testing.T) {
	noWait(t)
	calls := 0
	err := retryBus(context.Background(), nil, "stop u", func() error { calls++; return noReply })
	if err == nil || !transientBusError(err) || calls != busAttempts {
		t.Fatalf("err = %v after %d calls, want the NoReply after %d", err, calls, busAttempts)
	}
}

// A refusal is a real failure: one request, no retry.
func TestRetryBusDoesNotHideARefusal(t *testing.T) {
	noWait(t)
	denied := godbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}
	calls := 0
	err := retryBus(context.Background(), nil, "stop u", func() error { calls++; return denied })
	if calls != 1 || err == nil {
		t.Fatalf("err = %v after %d calls, want the refusal after 1", err, calls)
	}
}

// A cancelled context ends the retries.
func TestRetryBusStopsWhenCancelled(t *testing.T) {
	old := busRetryDelay
	busRetryDelay = func(int) time.Duration { return time.Hour }
	defer func() { busRetryDelay = old }()
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	done := make(chan error, 1)
	go func() {
		done <- retryBus(ctx, nil, "stop u", func() error { calls++; return noReply })
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil || calls != 1 {
			t.Fatalf("err = %v after %d calls", err, calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retryBus did not return after the context was cancelled")
	}
}
