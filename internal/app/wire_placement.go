package app

import (
	"context"

	"github.com/supavise/supavise/internal/notimpl"
)

// wirePlacement wraps the node's plane in the plane router, registers the instance, plane and
// backup endpoints of the peer API (the node agent), and provides placement.Resolver,
// placement.InstanceOps and placement.BackupOps (design 2.5, 2.7). The Engine's filter to the
// projects homed here belongs to it. On a node with no cluster it does nothing.
func wirePlacement(ctx context.Context, w *Wire) error { return notimpl.For("wire placement") }
