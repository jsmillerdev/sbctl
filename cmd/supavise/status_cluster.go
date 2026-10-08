package main

import (
	"context"
	"io"

	"github.com/supavise/supavise/internal/config"
)

// clusterBlock is the cluster section of `supavise status`: the nodes, the epoch, the leader and
// each replica's lag.
type clusterBlock struct{}

func (b *clusterBlock) render(w io.Writer) {}

// clusterStatus reads the cluster block; nil means there is none to show (a node that is not
// part of a cluster).
func clusterStatus(ctx context.Context, cfg *config.Config) (*clusterBlock, error) {
	return nil, nil
}
