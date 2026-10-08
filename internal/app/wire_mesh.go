package app

import (
	"context"

	"github.com/supavise/supavise/internal/notimpl"
)

// wireMesh starts the peer server on [node] peer_listen, the sessions to the other nodes and the
// forwarder reconciler (design 2.5), and provides mesh.Mesh and cluster.Membership. It also
// registers the peer API endpoints of the mesh itself (ping, join, certs, config, report intake) with
// mesh.Handle. On a node with no cluster it does nothing.
func wireMesh(ctx context.Context, w *Wire) error { return notimpl.For("wire mesh") }
