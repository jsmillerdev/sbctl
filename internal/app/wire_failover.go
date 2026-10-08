package app

import (
	"context"

	"github.com/supavise/supavise/internal/notimpl"
)

// wireFailover builds the failover orchestrator, starts the automatic monitor when [failover]
// mode asks for it, checks the boot epoch (design 2.10.8), registers the cooperative fence endpoint
// and the readiness route, and provides failover.Service. On a node with no cluster it does nothing.
func wireFailover(ctx context.Context, w *Wire) error { return notimpl.For("wire failover") }
