package api

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Claim token kinds.
const (
	KindClaim  = "claim"  // creates the first dashboard administrator
	KindInvite = "invite" // creates one user for a fixed email address
)

// ClaimToken is a stored token (never the token itself: only its hash is kept).
type ClaimToken struct {
	ID    int64
	Kind  string
	Email string // invites only
	// InvitationID is the organization invitation an invite token was issued for (0: none).
	// Redeeming the token accepts that invitation and no other.
	InvitationID int64
	CreatedAt    time.Time
	ExpiresAt    time.Time
	UsedAt       *time.Time
	UsedBy       string
}

// ClaimStore keeps claim and invite tokens (registry migration 0600_claim_tokens.sql).
type ClaimStore interface {
	// CreateClaimToken stores a token and revokes the unused tokens of the same kind (and,
	// for invites, the same address and invitation), so at most one is live at a time. An
	// invite for one organization never revokes the invite for another. invitationID is
	// the invitation the token is bound to (invites only; 0: none).
	CreateClaimToken(ctx context.Context, kind string, hash []byte, email string, invitationID int64, expiresAt time.Time) (*ClaimToken, error)
	// LookupClaimToken returns the live token (unused, not expired at now) or ErrNotFound.
	LookupClaimToken(ctx context.Context, hash []byte, now time.Time) (*ClaimToken, error)
	// ConsumeClaimToken marks a live token used by usedBy in one atomic step and returns
	// it; ErrNotFound when it is unknown, used or expired. Two concurrent calls with the
	// same token cannot both succeed.
	ConsumeClaimToken(ctx context.Context, hash []byte, now time.Time, usedBy string) (*ClaimToken, error)
	// ReleaseClaimToken undoes a consume whose user could not be created.
	ReleaseClaimToken(ctx context.Context, id int64) error
	// Claimed reports whether a claim token has been used.
	Claimed(ctx context.Context) (bool, error)
}

// PGClaimStore is the Postgres ClaimStore over the registry's pool.
type PGClaimStore struct{ pool *pgxpool.Pool }

// NewPGClaimStore returns a PGClaimStore. The registry migrations must have been applied.
func NewPGClaimStore(pool *pgxpool.Pool) *PGClaimStore { return &PGClaimStore{pool: pool} }

const claimCols = `id, kind, coalesce(email, ''), coalesce(invitation_id, 0), created_at, expires_at, used_at, coalesce(used_by, '')`

func scanClaim(row pgx.Row) (*ClaimToken, error) {
	var t ClaimToken
	if err := row.Scan(&t.ID, &t.Kind, &t.Email, &t.InvitationID, &t.CreatedAt, &t.ExpiresAt, &t.UsedAt, &t.UsedBy); err != nil {
		return nil, notFound(err)
	}
	return &t, nil
}

func (s *PGClaimStore) CreateClaimToken(ctx context.Context, kind string, hash []byte, email string, invitationID int64, expiresAt time.Time) (*ClaimToken, error) {
	var out *ClaimToken
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var mail, inv any
		if kind == KindInvite {
			mail = email
			if invitationID != 0 {
				inv = invitationID
			}
		}
		if _, err := tx.Exec(ctx, `delete from sbctl.claim_tokens where kind = $1 and used_at is null
			and email is not distinct from $2 and invitation_id is not distinct from $3::bigint`, kind, mail, inv); err != nil {
			return err
		}
		var err error
		out, err = scanClaim(tx.QueryRow(ctx,
			`insert into sbctl.claim_tokens (kind, token_hash, email, invitation_id, expires_at) values ($1, $2, $3, $4, $5) returning `+claimCols,
			kind, hash, mail, inv, expiresAt))
		return err
	})
	return out, err
}

func (s *PGClaimStore) LookupClaimToken(ctx context.Context, hash []byte, now time.Time) (*ClaimToken, error) {
	return scanClaim(s.pool.QueryRow(ctx,
		`select `+claimCols+` from sbctl.claim_tokens where token_hash = $1 and used_at is null and expires_at > $2`, hash, now))
}

func (s *PGClaimStore) ConsumeClaimToken(ctx context.Context, hash []byte, now time.Time, usedBy string) (*ClaimToken, error) {
	return scanClaim(s.pool.QueryRow(ctx,
		`update sbctl.claim_tokens set used_at = $2, used_by = $3
		  where token_hash = $1 and used_at is null and expires_at > $2 returning `+claimCols, hash, now, usedBy))
}

func (s *PGClaimStore) ReleaseClaimToken(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `update sbctl.claim_tokens set used_at = null, used_by = null where id = $1`, id)
	return err
}

func (s *PGClaimStore) Claimed(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `select exists (select 1 from sbctl.claim_tokens where kind = 'claim' and used_at is not null)`).Scan(&ok)
	return ok, err
}

// MemoryClaimStore is the in-memory ClaimStore for tests.
type MemoryClaimStore struct {
	mu     sync.Mutex
	next   int64
	tokens map[int64]*memClaim
}

type memClaim struct {
	ClaimToken
	hash string
}

// NewMemoryClaimStore returns an empty MemoryClaimStore.
func NewMemoryClaimStore() *MemoryClaimStore { return &MemoryClaimStore{tokens: map[int64]*memClaim{}} }

func (m *MemoryClaimStore) CreateClaimToken(_ context.Context, kind string, hash []byte, email string, invitationID int64, expiresAt time.Time) (*ClaimToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if kind != KindInvite {
		email, invitationID = "", 0
	}
	for id, t := range m.tokens {
		if t.Kind == kind && t.UsedAt == nil && t.Email == email && t.InvitationID == invitationID {
			delete(m.tokens, id)
		}
	}
	m.next++
	t := &memClaim{ClaimToken: ClaimToken{ID: m.next, Kind: kind, Email: email, InvitationID: invitationID, CreatedAt: time.Now(), ExpiresAt: expiresAt}, hash: string(hash)}
	m.tokens[t.ID] = t
	c := t.ClaimToken
	return &c, nil
}

func (m *MemoryClaimStore) find(hash []byte, now time.Time) *memClaim {
	for _, t := range m.tokens {
		if t.hash == string(hash) && t.UsedAt == nil && t.ExpiresAt.After(now) {
			return t
		}
	}
	return nil
}

func (m *MemoryClaimStore) LookupClaimToken(_ context.Context, hash []byte, now time.Time) (*ClaimToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.find(hash, now)
	if t == nil {
		return nil, ErrNotFound
	}
	c := t.ClaimToken
	return &c, nil
}

func (m *MemoryClaimStore) ConsumeClaimToken(_ context.Context, hash []byte, now time.Time, usedBy string) (*ClaimToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.find(hash, now)
	if t == nil {
		return nil, ErrNotFound
	}
	t.UsedAt, t.UsedBy = &now, usedBy
	c := t.ClaimToken
	return &c, nil
}

func (m *MemoryClaimStore) ReleaseClaimToken(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.tokens[id]; t != nil {
		t.UsedAt, t.UsedBy = nil, ""
	}
	return nil
}

func (m *MemoryClaimStore) Claimed(context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tokens {
		if t.Kind == KindClaim && t.UsedAt != nil {
			return true, nil
		}
	}
	return false, nil
}
