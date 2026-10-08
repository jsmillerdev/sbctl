package app

import (
	"context"

	"github.com/supavise/supavise/internal/notimpl"
)

// wireReplicas starts the replica controller and the default-replica reconciler on the leader and
// provides the replica service that the Management API handlers use (design 2.7). It sets the
// service on w.API. On a node with no cluster it does nothing.
func wireReplicas(ctx context.Context, w *Wire) error { return notimpl.For("wire replicas") }
