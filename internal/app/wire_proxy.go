package app

import (
	"context"

	"github.com/supavise/supavise/internal/notimpl"
)

// wireProxy gives the edge proxy the replica and load-balancer hosts and, on a follower, the
// certificate mirror and the synced or managing switch (design 2.8). It sets the fields it owns
// on w.Proxy. On a node with no cluster it does nothing.
func wireProxy(ctx context.Context, w *Wire) error { return notimpl.For("wire proxy") }
