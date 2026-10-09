package oauth

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is the in-memory Store: for tests, the dev mock and a registry that is not Postgres.
// Everything is lost on restart.
//
// This file is a stub: every method returns ErrNotImplemented until the real store replaces it.
type MemoryStore struct {
	mu sync.Mutex
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

var _ Store = (*MemoryStore)(nil)

func (m *MemoryStore) CreateApp(ctx context.Context, app App, secret *AppSecret) error {
	return ErrNotImplemented
}

func (m *MemoryStore) GetApp(ctx context.Context, id string) (*App, error) {
	return nil, ErrNotImplemented
}

func (m *MemoryStore) UpdateApp(ctx context.Context, app App) error { return ErrNotImplemented }

func (m *MemoryStore) DeleteApp(ctx context.Context, id string, at time.Time) ([]Grant, error) {
	return nil, ErrNotImplemented
}

func (m *MemoryStore) ListManualApps(ctx context.Context, orgID int64) ([]App, error) {
	return nil, ErrNotImplemented
}

func (m *MemoryStore) CountDynamicApps(ctx context.Context) (int, error) { return 0, ErrNotImplemented }

func (m *MemoryStore) CountManualApps(ctx context.Context, orgID int64) (int, error) {
	return 0, ErrNotImplemented
}

func (m *MemoryStore) ListSecrets(ctx context.Context, appID string) ([]AppSecret, error) {
	return nil, ErrNotImplemented
}

func (m *MemoryStore) CreateSecret(ctx context.Context, s AppSecret) error { return ErrNotImplemented }

func (m *MemoryStore) DeleteSecret(ctx context.Context, appID, secretID string) error {
	return ErrNotImplemented
}

func (m *MemoryStore) TouchSecret(ctx context.Context, secretID string, at time.Time) error {
	return ErrNotImplemented
}

func (m *MemoryStore) CreateAuthorization(ctx context.Context, a Authorization) error {
	return ErrNotImplemented
}

func (m *MemoryStore) GetAuthorization(ctx context.Context, id string) (*Authorization, error) {
	return nil, ErrNotImplemented
}

func (m *MemoryStore) DecideAuthorization(ctx context.Context, id string, d Decision) (*Authorization, error) {
	return nil, ErrNotImplemented
}

func (m *MemoryStore) CountPending(ctx context.Context, appID string, now time.Time) (int, error) {
	return 0, ErrNotImplemented
}

func (m *MemoryStore) WithCode(ctx context.Context, appID string, codeHash []byte, fn func(ctx context.Context, tx CodeTx) error) error {
	return ErrNotImplemented
}

func (m *MemoryStore) LookupAccess(ctx context.Context, tokenHash []byte, now time.Time) (*AccessInfo, error) {
	return nil, ErrNotImplemented
}

func (m *MemoryStore) TouchToken(ctx context.Context, tokenID, grantID int64, at time.Time) error {
	return ErrNotImplemented
}

func (m *MemoryStore) GetToken(ctx context.Context, tokenHash []byte) (*Token, error) {
	return nil, ErrNotImplemented
}

func (m *MemoryStore) GetGrant(ctx context.Context, id int64) (*Grant, error) {
	return nil, ErrNotImplemented
}

func (m *MemoryStore) ListGrants(ctx context.Context, f GrantFilter) ([]GrantInfo, error) {
	return nil, ErrNotImplemented
}

func (m *MemoryStore) RevokeGrants(ctx context.Context, f GrantFilter, reason string, at time.Time) ([]Grant, error) {
	return nil, ErrNotImplemented
}

func (m *MemoryStore) RotateRefresh(ctx context.Context, in RotateInput, fn func(ctx context.Context, tx RotateTx) error) error {
	return ErrNotImplemented
}

func (m *MemoryStore) Prune(ctx context.Context, p PruneParams) (PruneResult, error) {
	return PruneResult{}, ErrNotImplemented
}
