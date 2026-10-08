package app

import (
	"context"

	"github.com/supavise/supavise/internal/notimpl"
)

// wireFleet puts the shared services in follower mode when this node follows (design 2.6): no
// bin/prepare and no tenant writes on a standby, cold units whose ports are forwarders, the
// Supavisor tenant refresh endpoint, and the reconciliation that starts or stops the services when the
// role changes. On a leader it does nothing.
func wireFleet(ctx context.Context, w *Wire) error { return notimpl.For("wire fleet") }
