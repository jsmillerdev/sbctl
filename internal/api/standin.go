package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jsmillerdev/supavise/internal/members"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// SweepStandIn removes what `supavise functions dev --token-file` leaves behind when it dies
// before its cleanup runs: the memberships of the stand-in user (members.StandInOwnerID) and
// its personal access tokens. The stand-in never counts as an Owner, so a leftover seat cannot
// make `users remove` drop the last real Owner, but it still shows in member lists and carries
// Owner rights until something removes it. The daemon calls this at start, and the command
// itself before it grants new seats. It returns the number of memberships and tokens removed.
func SweepStandIn(ctx context.Context, reg registry.Registry, m *members.Service) (memberships, tokens int, err error) {
	ms, err := m.Store.MembershipsOf(ctx, members.StandInOwnerID)
	if err != nil {
		return 0, 0, err
	}
	if err := m.RemoveUser(ctx, members.StandInOwnerID, true); err != nil {
		return 0, 0, err
	}
	memberships = len(ms)
	ts, err := reg.ListAccessTokens(ctx, members.StandInOwnerID)
	if err != nil {
		return memberships, 0, err
	}
	var errs []error
	for _, t := range ts {
		if err := reg.DeleteAccessToken(ctx, members.StandInOwnerID, t.ID); err != nil && !errors.Is(err, registry.ErrNotFound) {
			errs = append(errs, fmt.Errorf("deleting token %d: %w", t.ID, err))
			continue
		}
		tokens++
	}
	return memberships, tokens, errors.Join(errs...)
}
