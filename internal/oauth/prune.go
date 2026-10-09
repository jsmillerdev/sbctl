package oauth

import (
	"context"
	"time"
)

// pruneTimeout bounds one lazy prune. The work is a handful of indexed deletes.
const pruneTimeout = 30 * time.Second

// Prune deletes what is old enough (section 2.16) whatever the time since the last prune: expired
// authorizations, expired tokens, long-revoked grants, and dynamic apps nobody used. The cutoffs come
// from the Service's clock and the retention constants. Each statement is idempotent, so two nodes
// may prune at once.
func (s *Service) Prune(ctx context.Context) (PruneResult, error) {
	now := s.now()
	return s.Store.Prune(ctx, PruneParams{
		AuthorizationsBefore: now.Add(-AuthorizationRetention),
		TokensBefore:         now.Add(-TokenRetention),
		RevokedGrantsBefore:  now.Add(-RevokedGrantRetention),
		UnusedAppsBefore:     now.Add(-UnusedAppRetention),
		IdleAppsBefore:       now.Add(-IdleAppRetention),
	})
}

// maybePrune is the lazy prune that Register and Exchange start: at most once per PruneEvery per node,
// run by the request that finds it due. It never fails the request; a failure is logged and the next
// request after PruneEvery tries again. The prune outlives a client that hangs up.
func (s *Service) maybePrune(ctx context.Context) {
	now := s.now()
	s.pruneMu.Lock()
	due := s.lastPrune.IsZero() || now.Sub(s.lastPrune) >= PruneEvery
	if due {
		s.lastPrune = now
	}
	s.pruneMu.Unlock()
	if !due {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pruneTimeout)
	defer cancel()
	res, err := s.Prune(ctx)
	if err != nil {
		s.log().WarnContext(ctx, "oauth: pruning failed", "error", err)
		return
	}
	if res != (PruneResult{}) {
		s.log().InfoContext(ctx, "oauth: pruned", "authorizations", res.Authorizations, "tokens", res.Tokens,
			"grants", res.Grants, "apps", res.Apps)
	}
}
