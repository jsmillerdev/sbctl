package oauth

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the Postgres Store over the registry's pool (registry migration 1350_oauth.sql, schema
// supavise).
//
// This file is a stub: every method returns ErrNotImplemented until the real store replaces it.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore returns a PGStore. The registry migrations must have been applied.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

var _ Store = (*PGStore)(nil)

func (st *PGStore) CreateApp(ctx context.Context, app App, secret *AppSecret) error {
	return ErrNotImplemented
}

func (st *PGStore) GetApp(ctx context.Context, id string) (*App, error) {
	return nil, ErrNotImplemented
}

func (st *PGStore) UpdateApp(ctx context.Context, app App) error { return ErrNotImplemented }

func (st *PGStore) DeleteApp(ctx context.Context, id string, at time.Time) ([]Grant, error) {
	return nil, ErrNotImplemented
}

func (st *PGStore) ListManualApps(ctx context.Context, orgID int64) ([]App, error) {
	return nil, ErrNotImplemented
}

func (st *PGStore) CountDynamicApps(ctx context.Context) (int, error) { return 0, ErrNotImplemented }

func (st *PGStore) CountManualApps(ctx context.Context, orgID int64) (int, error) {
	return 0, ErrNotImplemented
}

func (st *PGStore) ListSecrets(ctx context.Context, appID string) ([]AppSecret, error) {
	return nil, ErrNotImplemented
}

func (st *PGStore) CreateSecret(ctx context.Context, s AppSecret) error { return ErrNotImplemented }

func (st *PGStore) DeleteSecret(ctx context.Context, appID, secretID string) error {
	return ErrNotImplemented
}

func (st *PGStore) TouchSecret(ctx context.Context, secretID string, at time.Time) error {
	return ErrNotImplemented
}

func (st *PGStore) CreateAuthorization(ctx context.Context, a Authorization) error {
	return ErrNotImplemented
}

func (st *PGStore) GetAuthorization(ctx context.Context, id string) (*Authorization, error) {
	return nil, ErrNotImplemented
}

func (st *PGStore) DecideAuthorization(ctx context.Context, id string, d Decision) (*Authorization, error) {
	return nil, ErrNotImplemented
}

func (st *PGStore) CountPending(ctx context.Context, appID string, now time.Time) (int, error) {
	return 0, ErrNotImplemented
}

func (st *PGStore) WithCode(ctx context.Context, appID string, codeHash []byte, fn func(ctx context.Context, tx CodeTx) error) error {
	return ErrNotImplemented
}

func (st *PGStore) LookupAccess(ctx context.Context, tokenHash []byte, now time.Time) (*AccessInfo, error) {
	return nil, ErrNotImplemented
}

func (st *PGStore) TouchToken(ctx context.Context, tokenID, grantID int64, at time.Time) error {
	return ErrNotImplemented
}

func (st *PGStore) GetToken(ctx context.Context, tokenHash []byte) (*Token, error) {
	return nil, ErrNotImplemented
}

func (st *PGStore) GetGrant(ctx context.Context, id int64) (*Grant, error) {
	return nil, ErrNotImplemented
}

func (st *PGStore) ListGrants(ctx context.Context, f GrantFilter) ([]GrantInfo, error) {
	return nil, ErrNotImplemented
}

func (st *PGStore) RevokeGrants(ctx context.Context, f GrantFilter, reason string, at time.Time) ([]Grant, error) {
	return nil, ErrNotImplemented
}

func (st *PGStore) RotateRefresh(ctx context.Context, in RotateInput, fn func(ctx context.Context, tx RotateTx) error) error {
	return ErrNotImplemented
}

func (st *PGStore) Prune(ctx context.Context, p PruneParams) (PruneResult, error) {
	return PruneResult{}, ErrNotImplemented
}
