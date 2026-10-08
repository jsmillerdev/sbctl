// Package replicaid names replicas and writes their rows. It imports only the registry and the
// config, so that the cluster join, which the replica controller depends on through placement, and
// the controller can both call it: the join records the standby of the system cluster before the
// controller runs.
package replicaid

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// Region is the region a node's replicas are named for and offered in: the node's own, or the
// default region for a node that names none.
func Region(n registry.Node) string {
	if n.Region != "" {
		return n.Region
	}
	return config.DefaultRegion
}

const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// Short returns six random characters, the last segment of a replica identifier.
func Short() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the system's random source is gone
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:])
}

// Identifier is the identifier of a replica of ref on node n, ending in short().
func Identifier(ref string, n registry.Node, short func() string) string {
	if short == nil {
		short = Short
	}
	return registry.ReplicaIdentifier(ref, Region(n), short())
}

// Create writes a replica row of ref on node n with the given origin, drawing the identifier's
// last segment from short (nil draws it at random) and drawing again when it is taken. The
// registry's error comes back as it is when n already holds a replica of ref
// (registry.ErrConflict) or the row is refused for another reason.
func Create(ctx context.Context, reg registry.Registry, ref string, n registry.Node, origin string, short func() string) (*registry.Replica, error) {
	var err error
	for range 5 {
		r := &registry.Replica{Identifier: Identifier(ref, n, short), Ref: ref, NodeID: n.ID, Origin: origin}
		if err = reg.CreateReplica(ctx, r); err == nil {
			return r, nil
		}
		if _, e := reg.GetReplica(ctx, r.Identifier); e != nil {
			return nil, err // not an identifier clash
		}
	}
	return nil, err
}

// EnsureSystem records the standby of the system cluster on a node that is joining (design 2.3):
// a replica of the project "system", origin "system", hidden from the platform listings. The
// cluster join calls it when it admits the node and returns the identifier in the join response.
// The joining node seeds the standby itself, before its daemon runs, so the controller only
// watches the row. A second call for the same node returns the row the first made, which is what a
// resumed join needs.
func EnsureSystem(ctx context.Context, reg registry.Registry, n registry.Node, short func() string) (*registry.Replica, error) {
	rows, err := reg.ListReplicas(ctx, config.SystemRef)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].NodeID == n.ID {
			return &rows[i], nil
		}
	}
	r, err := Create(ctx, reg, config.SystemRef, n, registry.ReplicaSystem, short)
	if err != nil {
		return nil, fmt.Errorf("replicaid: create the system standby of node %s: %w", n.ID, err)
	}
	return r, nil
}
