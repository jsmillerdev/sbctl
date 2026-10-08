package hostsetup

import (
	"context"
	"time"
)

// Monitor watches the converged marker for the daemon: while the node is behind this binary's
// revision it tells Raise, so that host_not_converged is raised, and when the node catches up it
// tells Raise once more so that the alert is resolved.
type Monitor struct {
	StateDir string
	// Delay is the wait before the first look (default 1 minute: an upgrade converges the host
	// before it restarts the daemon, and a converge started by hand may be running); Every is the
	// pause between looks (default 5 minutes).
	Delay, Every time.Duration
	// Quiet reports a time at which the node is not judged, such as while an upgrade runs.
	Quiet func() bool
	// Raise gets the status after every look at which the node is behind, and once after the first
	// look and after each look at which it stopped being behind.
	Raise func(ctx context.Context, s Status)
}

// Run looks until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	wait := m.Delay
	if wait <= 0 {
		wait = time.Minute
	}
	every := m.Every
	if every <= 0 {
		every = 5 * time.Minute
	}
	first, wasBehind := true, false
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = every
		if m.Quiet != nil && m.Quiet() {
			continue
		}
		s := StatusOf(m.StateDir)
		if !s.Known {
			continue // a marker that cannot be read says nothing about the node
		}
		behind := s.Behind()
		if behind || wasBehind || first {
			m.Raise(ctx, s)
		}
		first, wasBehind = false, behind
	}
}
