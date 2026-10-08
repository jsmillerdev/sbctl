package lifecycle

import (
	"errors"
	"fmt"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover/fenced"
)

// ErrFenced: the primary of the project may not run on this node, because a peer holds a higher
// epoch (the node's fence record, or the project's; internal/failover/fenced). Nothing was rendered or
// started. The node's replicas are not primaries and are not held back by it: a fenced node is
// rebuilt as a follower (`supavise node rejoin`).
var ErrFenced = errors.New("lifecycle: the primary of the project is fenced on this node")

// fencedErr is nil, or ErrFenced with the reason, when the fence records of cfg's state directory
// hold ref's primary back. A record that cannot be read holds it back too (fenced.Blocks fails
// closed). It is called before anything renders or starts a primary, so that a unit file the fence
// removed (the launcher, postgres.run) is not written again.
func fencedErr(cfg *config.Config, ref string) error {
	rec, blocked := fenced.Blocks(cfg.Paths(), ref)
	if !blocked {
		return nil
	}
	if rec != nil && rec.Reason != "" {
		return fmt.Errorf("%w: %s", ErrFenced, rec.Reason)
	}
	return ErrFenced
}
