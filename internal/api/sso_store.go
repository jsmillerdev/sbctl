package api

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SSOProviderRow is what sbctl records about a SAML identity provider it registered in
// sb-gotrue@system (registry migration 1000_sso.sql); GoTrue's own tables hold the metadata.
type SSOProviderRow struct {
	ID       string // GoTrue's provider id
	OrgID    int64
	EntityID string
	// Domains are the email domains the provider vouches for, lower case.
	Domains []string
	// DefaultRole is the role a first-time user of these domains gets (0: none, the user waits
	// for approval).
	DefaultRole int
	CreatedBy   string
	CreatedAt   time.Time
}

// SSO user states.
const (
	SSOPending = "pending"
	SSOActive  = "active"
)

// SSOUser is a dashboard user who signed in through SSO.
type SSOUser struct {
	UserID, ProviderID, Email, State string
	FirstSeen, LastSeen              time.Time
}

// SSOStore keeps the dashboard's identity providers and their users.
type SSOStore interface {
	// PutProvider inserts or replaces the row of the provider with this id.
	PutProvider(ctx context.Context, p SSOProviderRow) error
	GetProvider(ctx context.Context, id string) (*SSOProviderRow, error)
	ListProviders(ctx context.Context) ([]SSOProviderRow, error)
	// DeleteProvider removes the row and returns the users that signed in through it, which it
	// also forgets.
	DeleteProvider(ctx context.Context, id string) ([]SSOUser, error)

	GetSSOUser(ctx context.Context, userID string) (*SSOUser, error)
	// InsertSSOUser records a user seen for the first time; it reports false, and changes
	// nothing, when the user is known already.
	InsertSSOUser(ctx context.Context, u SSOUser) (bool, error)
	SetSSOUserState(ctx context.Context, userID, state string, at time.Time) error
	// ListSSOUsers returns the users in state ("" for every state) of the given providers (nil
	// for every provider), oldest first.
	ListSSOUsers(ctx context.Context, state string, providers []string) ([]SSOUser, error)
	DeleteSSOUser(ctx context.Context, userID string) error
}

// PGSSOStore is the Postgres SSOStore over the registry's pool.
type PGSSOStore struct{ pool *pgxpool.Pool }

// NewPGSSOStore returns a PGSSOStore. The registry migrations must have been applied.
func NewPGSSOStore(pool *pgxpool.Pool) *PGSSOStore { return &PGSSOStore{pool: pool} }

const ssoProviderCols = `id::text, org_id, entity_id, domains, coalesce(default_role, 0), coalesce(created_by::text, ''), created_at`

func scanSSOProvider(row pgx.Row) (*SSOProviderRow, error) {
	var p SSOProviderRow
	if err := row.Scan(&p.ID, &p.OrgID, &p.EntityID, &p.Domains, &p.DefaultRole, &p.CreatedBy, &p.CreatedAt); err != nil {
		return nil, notFound(err)
	}
	return &p, nil
}

func (s *PGSSOStore) PutProvider(ctx context.Context, p SSOProviderRow) error {
	var role, by any
	if p.DefaultRole != 0 {
		role = p.DefaultRole
	}
	if p.CreatedBy != "" {
		by = p.CreatedBy
	}
	domains := p.Domains
	if domains == nil {
		domains = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		insert into sbctl.sso_providers (id, org_id, entity_id, domains, default_role, created_by)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (id) do update set org_id = excluded.org_id, entity_id = excluded.entity_id,
			domains = excluded.domains, default_role = excluded.default_role`,
		p.ID, p.OrgID, p.EntityID, domains, role, by)
	return err
}

func (s *PGSSOStore) GetProvider(ctx context.Context, id string) (*SSOProviderRow, error) {
	if !validUUID(id) {
		return nil, ErrNotFound
	}
	return scanSSOProvider(s.pool.QueryRow(ctx, `select `+ssoProviderCols+` from sbctl.sso_providers where id = $1`, id))
}

func (s *PGSSOStore) ListProviders(ctx context.Context) ([]SSOProviderRow, error) {
	rows, err := s.pool.Query(ctx, `select `+ssoProviderCols+` from sbctl.sso_providers order by created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SSOProviderRow
	for rows.Next() {
		p, err := scanSSOProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *PGSSOStore) DeleteProvider(ctx context.Context, id string) ([]SSOUser, error) {
	if !validUUID(id) {
		return nil, ErrNotFound
	}
	var users []SSOUser
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `delete from sbctl.sso_providers where id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		rows, err := tx.Query(ctx, `delete from sbctl.sso_users where provider_id = $1
			returning user_id::text, provider_id::text, email, state, first_seen, last_seen`, id)
		if err != nil {
			return err
		}
		users, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (SSOUser, error) {
			var u SSOUser
			err := r.Scan(&u.UserID, &u.ProviderID, &u.Email, &u.State, &u.FirstSeen, &u.LastSeen)
			return u, err
		})
		return err
	})
	return users, err
}

const ssoUserCols = `user_id::text, provider_id::text, email, state, first_seen, last_seen`

func scanSSOUser(row pgx.Row) (*SSOUser, error) {
	var u SSOUser
	if err := row.Scan(&u.UserID, &u.ProviderID, &u.Email, &u.State, &u.FirstSeen, &u.LastSeen); err != nil {
		return nil, notFound(err)
	}
	return &u, nil
}

func (s *PGSSOStore) GetSSOUser(ctx context.Context, userID string) (*SSOUser, error) {
	if !validUUID(userID) {
		return nil, ErrNotFound
	}
	return scanSSOUser(s.pool.QueryRow(ctx, `select `+ssoUserCols+` from sbctl.sso_users where user_id = $1`, userID))
}

func (s *PGSSOStore) InsertSSOUser(ctx context.Context, u SSOUser) (bool, error) {
	tag, err := s.pool.Exec(ctx, `insert into sbctl.sso_users (user_id, provider_id, email, state, first_seen, last_seen)
		values ($1, $2, $3, $4, $5, $5) on conflict (user_id) do nothing`, u.UserID, u.ProviderID, u.Email, u.State, u.FirstSeen)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PGSSOStore) SetSSOUserState(ctx context.Context, userID, state string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `update sbctl.sso_users set state = $2, last_seen = $3 where user_id = $1`, userID, state, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PGSSOStore) ListSSOUsers(ctx context.Context, state string, providers []string) ([]SSOUser, error) {
	q := `select ` + ssoUserCols + ` from sbctl.sso_users where ($1 = '' or state = $1)`
	args := []any{state}
	if providers != nil {
		q += ` and provider_id = any($2::uuid[])`
		args = append(args, providers)
	}
	rows, err := s.pool.Query(ctx, q+` order by first_seen, user_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SSOUser
	for rows.Next() {
		u, err := scanSSOUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *PGSSOStore) DeleteSSOUser(ctx context.Context, userID string) error {
	if !validUUID(userID) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `delete from sbctl.sso_users where user_id = $1`, userID)
	return err
}

// validUUID reports whether s is a UUID, so that a malformed id from a request is a miss and
// not a Postgres error.
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'):
		default:
			return false
		}
	}
	return true
}

// MemorySSOStore is the in-memory SSOStore for tests.
type MemorySSOStore struct {
	mu        sync.Mutex
	providers map[string]SSOProviderRow
	users     map[string]SSOUser
}

// NewMemorySSOStore returns an empty MemorySSOStore.
func NewMemorySSOStore() *MemorySSOStore {
	return &MemorySSOStore{providers: map[string]SSOProviderRow{}, users: map[string]SSOUser{}}
}

func (m *MemorySSOStore) PutProvider(_ context.Context, p SSOProviderRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.providers[p.ID]; ok {
		p.CreatedAt, p.CreatedBy = old.CreatedAt, old.CreatedBy
	} else if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	p.Domains = slices.Clone(p.Domains)
	m.providers[p.ID] = p
	return nil
}

func (m *MemorySSOStore) GetProvider(_ context.Context, id string) (*SSOProviderRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.providers[strings.ToLower(id)]
	if !ok {
		return nil, ErrNotFound
	}
	p.Domains = slices.Clone(p.Domains)
	return &p, nil
}

func (m *MemorySSOStore) ListProviders(context.Context) ([]SSOProviderRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SSOProviderRow
	for _, p := range m.providers {
		p.Domains = slices.Clone(p.Domains)
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *MemorySSOStore) DeleteProvider(_ context.Context, id string) ([]SSOUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[id]; !ok {
		return nil, ErrNotFound
	}
	delete(m.providers, id)
	var gone []SSOUser
	for k, u := range m.users {
		if u.ProviderID == id {
			gone = append(gone, u)
			delete(m.users, k)
		}
	}
	sort.Slice(gone, func(i, j int) bool { return gone[i].FirstSeen.Before(gone[j].FirstSeen) })
	return gone, nil
}

func (m *MemorySSOStore) GetSSOUser(_ context.Context, userID string) (*SSOUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[userID]
	if !ok {
		return nil, ErrNotFound
	}
	return &u, nil
}

func (m *MemorySSOStore) InsertSSOUser(_ context.Context, u SSOUser) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[u.UserID]; ok {
		return false, nil
	}
	u.LastSeen = u.FirstSeen
	m.users[u.UserID] = u
	return true, nil
}

func (m *MemorySSOStore) SetSSOUserState(_ context.Context, userID, state string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[userID]
	if !ok {
		return ErrNotFound
	}
	u.State, u.LastSeen = state, at
	m.users[userID] = u
	return nil
}

func (m *MemorySSOStore) ListSSOUsers(_ context.Context, state string, providers []string) ([]SSOUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SSOUser
	for _, u := range m.users {
		if (state == "" || u.State == state) && (providers == nil || slices.Contains(providers, u.ProviderID)) {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].FirstSeen.Before(out[j].FirstSeen)
		}
		return out[i].UserID < out[j].UserID
	})
	return out, nil
}

func (m *MemorySSOStore) DeleteSSOUser(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.users, userID)
	return nil
}
