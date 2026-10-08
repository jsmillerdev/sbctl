package units

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	godbus "github.com/godbus/dbus/v5"
)

// busAttempts is how many times a start or stop request is sent when the bus drops it: the
// request itself, then two more.
const busAttempts = 3

// busRetryDelay is the wait before the next attempt; a variable so that tests do not sleep.
var busRetryDelay = func(attempt int) time.Duration { return time.Duration(attempt) * 250 * time.Millisecond }

// transientBusError reports whether err says the request may never have reached systemd, or that
// systemd went away while it was answering: the bus daemon's NoReply ("Message recipient
// disconnected from message bus without replying", what a caller gets when the bus daemon loses
// the other end before it answers, as while PID 1 re-executes), a closed connection, or a
// connection that was reset. The same request may be sent again: stopping a stopped unit and
// starting a running one are no-ops, and a job that was queued is replaced by the new one.
//
// A refusal (access denied, no such unit, a job that failed) is not transient.
func transientBusError(err error) bool {
	if err == nil {
		return false
	}
	var de godbus.Error
	if errors.As(err, &de) {
		switch de.Name {
		case "org.freedesktop.DBus.Error.NoReply", "org.freedesktop.DBus.Error.Disconnected":
			return true
		}
		return false
	}
	return errors.Is(err, godbus.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// retryBus runs op, and again when it fails with a transient bus error, up to busAttempts times
// in all. op dials for itself, so that a retry after a closed connection uses a new one. A
// failure that is not transient, or that outlasts the attempts, is returned as it was.
func retryBus(ctx context.Context, log *slog.Logger, what string, op func() error) error {
	var err error
	for attempt := 1; ; attempt++ {
		if err = op(); err == nil || !transientBusError(err) || attempt == busAttempts {
			return err
		}
		if log != nil {
			log.Warn("systemd did not answer the request; sending it again", "request", what, "attempt", attempt, "error", err)
		}
		select {
		case <-time.After(busRetryDelay(attempt)):
		case <-ctx.Done():
			return err
		}
	}
}
