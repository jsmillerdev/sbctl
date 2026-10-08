package branching

import (
	"context"
	"errors"

	"github.com/supavise/supavise/internal/registry"
)

// seedSecret is the name, among the parent's sealed project secrets, of its branch seed.
const seedSecret = "branch_seed"

// SetSeed stores the SQL script that new schema-only branches of parentRef run after the
// parent's migrations (the equivalent of a repository's supabase/seed.sql: the Management
// API has no field for it). It is sealed like any project secret. An empty script clears it.
func (s *Service) SetSeed(ctx context.Context, parentRef, script string) error {
	p, err := s.Parent(ctx, parentRef)
	if err != nil {
		return err
	}
	sealed, err := s.sec.Seal([]byte(script))
	if err != nil {
		return err
	}
	return s.reg.PutSecret(ctx, p.Ref, seedSecret, sealed)
}

// GetSeed returns the stored seed of parentRef, or "".
func (s *Service) GetSeed(ctx context.Context, parentRef string) (string, error) {
	p, err := s.Parent(ctx, parentRef)
	if err != nil {
		return "", err
	}
	sealed, err := s.reg.GetSecret(ctx, p.Ref, seedSecret)
	if errors.Is(err, registry.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	pt, err := s.sec.Open(sealed)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
