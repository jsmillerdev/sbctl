package failover

import (
	"context"
	"errors"
)

// Manual is the Provider of a cluster with no [failover] fencing method: it fences nothing and
// moves no address. An unplanned failover on such a cluster needs the operator's assertion that
// the old primary is down (--old-primary-is-down); the orchestrator tries the cooperative
// fence on its own, and prints the DNS change at the end.
type Manual struct{}

var _ Provider = Manual{}

func (Manual) Name() string { return "manual" }

// Fence refuses: nothing here can make a running node stop writing.
func (Manual) Fence(context.Context, Request) error {
	return errors.New("no fencing method is configured ([failover] fencing is empty)")
}

func (Manual) Probe(context.Context) error {
	return errors.New("no fencing method is configured ([failover] fencing is empty)")
}

// TakeOver does nothing and says so, so that the caller prints the DNS guidance.
func (Manual) TakeOver(context.Context, Request) error { return ErrNoTakeover }
