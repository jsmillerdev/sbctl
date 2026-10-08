package app

import (
	"context"

	"github.com/supavise/supavise/internal/notimpl"
)

// wireStorageMigrate builds the Storage migration service that `supavise storage migrate` and the
// daemon share (design 2.11). On a node that never migrates it does nothing.
func wireStorageMigrate(ctx context.Context, w *Wire) error {
	return notimpl.For("wire storagemigrate")
}
