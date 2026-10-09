package oauth

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the Postgres Store over the registry's pool (registry migration 1350_oauth.sql, schema
// supavise). store.go defines the behavior; this file adds what only the database can do:
//
//   - WithCode and RotateRefresh lock one row (select ... for update, and an update that waits on the
//     row lock), so concurrent callers for one code or one refresh token run one after the other. The
//     transactions use the default isolation level, READ COMMITTED: a caller that waited re-reads
//     the row the first one committed.
//   - Time comes from the arguments. The column defaults of now() are never relied on.
//   - A secret never reaches this file in clear. Every hash that is written (a code, a token, a
//     client secret) must be 32 bytes, SHA-256, and a stored token prefix at most StoredPrefixLen
//     characters; anything else is ErrInvalid, so a caller that passes the secret itself fails
//     loudly instead of storing it.
//   - Complete refuses a grant that is not the one the locked authorization approved (same app,
//     approver, organization and resource; scopes within those of the request), and
//     DecideAuthorization refuses an approval without an approver, an organization or a code
//     expiry. The Service builds neither, so each is ErrInvalid and a bug if it happens.
//   - Errors from the database are mapped: no rows is ErrNotFound, a unique violation ErrConflict, a
//     foreign key violation (a referenced app, grant or organization is missing) ErrNotFound, a
//     check violation ErrInvalid. The text names the constraint and never a value.
//
// A method that takes fn holds one pooled connection until fn returns, so fn must not wait for a
// second connection of the same pool: enough callers at once would hold every connection and stall.
// The Service asks Admit (a registry read) between its transactions for this reason.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore returns a PGStore. The registry migrations must have been applied.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

var _ Store = (*PGStore)(nil)

// pgq is what a pool and a transaction share.
type pgq interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ---- columns and scanning
//
// The nullable columns that the types model as a zero value are read with coalesce, so a row scans
// straight into the struct. Times and byte slices scan into pointers and nil slices.

const pgAppCols = `a.id::text, a.registration_type, coalesce(a.org_id, 0), a.name, a.website, a.icon, a.redirect_uris, a.scopes,
	a.token_endpoint_auth_method, coalesce(a.created_by::text, ''), a.created_at, a.updated_at, a.last_authorized_at, a.deleted_at`

func pgAppDests(a *App) []any {
	return []any{&a.ID, &a.RegistrationType, &a.OrgID, &a.Name, &a.Website, &a.Icon, &a.RedirectURIs, &a.Scopes,
		&a.TokenEndpointAuthMethod, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt, &a.LastAuthorizedAt, &a.DeletedAt}
}

const pgSecretCols = `s.id::text, s.app_id::text, s.alias, s.secret_hash, coalesce(s.created_by::text, ''), s.created_at, s.last_used_at`

func pgSecretDests(s *AppSecret) []any {
	return []any{&s.ID, &s.AppID, &s.Alias, &s.Hash, &s.CreatedBy, &s.CreatedAt, &s.LastUsedAt}
}

// pgAuthCols needs the table as a and the organization joined as o (left join, on o.id = a.org_id).
const pgAuthCols = `a.id::text, a.app_id::text, a.redirect_uri, a.scopes, a.state, a.code_challenge, a.resource, a.org_hint,
	a.created_at, a.expires_at, a.status, coalesce(a.decided_by::text, ''), a.decided_at, coalesce(a.org_id, 0), coalesce(o.slug, ''),
	a.code_hash, a.code_expires_at, a.code_used_at, coalesce(a.grant_id, 0)`

const pgAuthFrom = ` from supavise.oauth_authorizations a left join supavise.organizations o on o.id = a.org_id `

func pgAuthDests(a *Authorization) []any {
	return []any{&a.ID, &a.AppID, &a.RedirectURI, &a.Scopes, &a.State, &a.CodeChallenge, &a.Resource, &a.OrgHint,
		&a.CreatedAt, &a.ExpiresAt, &a.Status, &a.DecidedBy, &a.DecidedAt, &a.OrgID, &a.OrgSlug,
		&a.CodeHash, &a.CodeExpiresAt, &a.CodeUsedAt, &a.GrantID}
}

const pgGrantCols = `g.id, g.app_id::text, g.user_id::text, g.org_id, g.scopes, g.resource, g.created_at, g.last_used_at, g.revoked_at, g.revoked_reason`

func pgGrantDests(g *Grant) []any {
	return []any{&g.ID, &g.AppID, &g.UserID, &g.OrgID, &g.Scopes, &g.Resource, &g.CreatedAt, &g.LastUsedAt, &g.RevokedAt, &g.RevokedReason}
}

const pgTokenCols = `t.id, t.grant_id, t.kind, t.token_hash, t.prefix, t.created_at, t.expires_at, t.last_used_at, t.used_at, coalesce(t.replaced_by, 0)`

func pgTokenDests(t *Token) []any {
	return []any{&t.ID, &t.GrantID, &t.Kind, &t.Hash, &t.Prefix, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &t.UsedAt, &t.ReplacedBy}
}

// pgCollect runs scan on every row of rows (closing it) and returns the results; err is the error of
// the Query call that made rows.
func pgCollect[T any](rows pgx.Rows, err error, scan func(pgx.CollectableRow) (T, error)) ([]T, error) {
	if err != nil {
		return nil, pgMapErr(err)
	}
	out, err := pgx.CollectRows(rows, scan)
	if err != nil {
		return nil, pgMapErr(err)
	}
	return out, nil
}

func pgScanGrant(row pgx.CollectableRow) (Grant, error) {
	var g Grant
	err := row.Scan(pgGrantDests(&g)...)
	return g, err
}

// pgCollectGrants collects the grants of an update ... returning pgGrantCols, ordered by id (a
// returning clause has no order of its own).
func pgCollectGrants(rows pgx.Rows, err error) ([]Grant, error) {
	out, err := pgCollect(rows, err, pgScanGrant)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Grant) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// ---- helpers

// pgMapErr turns a database error into the Store's sentinel errors (see PGStore).
func pgMapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w (%s)", ErrConflict, pe.ConstraintName)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%w (%s)", ErrNotFound, pe.ConstraintName)
		case "23514": // check_violation
			return fmt.Errorf("%w: %s", ErrInvalid, pe.ConstraintName)
		}
	}
	return err
}

// pgIsUUID reports whether s is a UUID in the canonical 36-character form. Anything else cannot be
// the id of a row, so a lookup by it is ErrNotFound and never a driver error.
func pgIsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// pgUserArg is a user id for a nullable uuid column: "" is NULL, anything else must be a UUID.
func pgUserArg(what, s string) (any, error) {
	switch {
	case s == "":
		return nil, nil
	case !pgIsUUID(s):
		return nil, fmt.Errorf("%w: %s is not a UUID", ErrInvalid, what)
	}
	return s, nil
}

func pgCheckHash(what string, h []byte) error {
	if len(h) != sha256.Size {
		return fmt.Errorf("%w: the %s hash must be SHA-256 (%d bytes)", ErrInvalid, what, sha256.Size)
	}
	return nil
}

func pgCheckToken(what string, t Token) error {
	if err := pgCheckHash(what, t.Hash); err != nil {
		return err
	}
	if len(t.Prefix) > StoredPrefixLen {
		return fmt.Errorf("%w: the %s prefix is longer than %d characters", ErrInvalid, what, StoredPrefixLen)
	}
	return nil
}

// pgSubset reports whether every element of sub is in set.
func pgSubset(sub, set []string) bool {
	for _, s := range sub {
		if !slices.Contains(set, s) {
			return false
		}
	}
	return true
}

// pgStrings is s, or an empty slice for nil: a nil slice would be written as NULL.
func pgStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func pgNullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func pgNullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func pgNullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func pgCopyGrant(g Grant) Grant {
	g.Scopes = slices.Clone(g.Scopes)
	return g
}

func pgCopyToken(t Token) Token {
	t.Hash = slices.Clone(t.Hash)
	return t
}

func pgCopyAuth(a Authorization) Authorization {
	a.Scopes = slices.Clone(a.Scopes)
	a.CodeHash = slices.Clone(a.CodeHash)
	return a
}

// ---- apps

func (st *PGStore) CreateApp(ctx context.Context, app App, secret *AppSecret) error {
	if !pgIsUUID(app.ID) {
		return fmt.Errorf("%w: the app id is not a UUID", ErrInvalid)
	}
	createdBy, err := pgUserArg("created_by", app.CreatedBy)
	if err != nil {
		return err
	}
	var secretBy any
	if secret != nil {
		if err := pgCheckHash("client secret", secret.Hash); err != nil {
			return err
		}
		if !pgIsUUID(secret.ID) {
			return fmt.Errorf("%w: the client secret id is not a UUID", ErrInvalid)
		}
		if secretBy, err = pgUserArg("created_by", secret.CreatedBy); err != nil {
			return err
		}
	}
	return pgx.BeginFunc(ctx, st.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into supavise.oauth_apps
			(id, registration_type, org_id, name, website, icon, redirect_uris, scopes, token_endpoint_auth_method,
			 created_by, created_at, updated_at, last_authorized_at, deleted_at)
			values ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10::uuid, $11, $12, $13, $14)`,
			app.ID, app.RegistrationType, pgNullInt(app.OrgID), app.Name, app.Website, app.Icon,
			pgStrings(app.RedirectURIs), pgStrings(app.Scopes), app.TokenEndpointAuthMethod,
			createdBy, app.CreatedAt, app.UpdatedAt, app.LastAuthorizedAt, app.DeletedAt); err != nil {
			return pgMapErr(err)
		}
		if secret == nil {
			return nil
		}
		return pgInsertSecret(ctx, tx, app.ID, *secret, secretBy)
	})
}

func pgInsertSecret(ctx context.Context, q pgq, appID string, s AppSecret, createdBy any) error {
	_, err := q.Exec(ctx, `insert into supavise.oauth_app_secrets (id, app_id, alias, secret_hash, created_by, created_at, last_used_at)
		values ($1::uuid, $2::uuid, $3, $4, $5::uuid, $6, $7)`,
		s.ID, appID, s.Alias, s.Hash, createdBy, s.CreatedAt, s.LastUsedAt)
	return pgMapErr(err)
}

func (st *PGStore) GetApp(ctx context.Context, id string) (*App, error) {
	if !pgIsUUID(id) {
		return nil, ErrNotFound
	}
	var a App
	err := st.pool.QueryRow(ctx, `select `+pgAppCols+` from supavise.oauth_apps a where a.id = $1::uuid and a.deleted_at is null`, id).
		Scan(pgAppDests(&a)...)
	if err != nil {
		return nil, pgMapErr(err)
	}
	return &a, nil
}

func (st *PGStore) UpdateApp(ctx context.Context, app App) error {
	if !pgIsUUID(app.ID) {
		return ErrNotFound
	}
	tag, err := st.pool.Exec(ctx, `update supavise.oauth_apps
		set name = $2, website = $3, icon = $4, redirect_uris = $5, scopes = $6, updated_at = $7
		where id = $1::uuid and deleted_at is null`,
		app.ID, app.Name, app.Website, app.Icon, pgStrings(app.RedirectURIs), pgStrings(app.Scopes), app.UpdatedAt)
	if err != nil {
		return pgMapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (st *PGStore) DeleteApp(ctx context.Context, id string, at time.Time) ([]Grant, error) {
	if !pgIsUUID(id) {
		return nil, ErrNotFound
	}
	var revoked []Grant
	err := pgx.BeginFunc(ctx, st.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update supavise.oauth_apps set deleted_at = $2 where id = $1::uuid and deleted_at is null`, id, at)
		if err != nil {
			return pgMapErr(err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		rows, err := tx.Query(ctx, `update supavise.oauth_grants g set revoked_at = $2, revoked_reason = $3
			where g.app_id = $1::uuid and g.revoked_at is null returning `+pgGrantCols, id, at, ReasonAppDeleted)
		revoked, err = pgCollectGrants(rows, err)
		return err
	})
	if err != nil {
		return nil, err
	}
	return revoked, nil
}

func (st *PGStore) ListManualApps(ctx context.Context, orgID int64) ([]App, error) {
	rows, err := st.pool.Query(ctx, `select `+pgAppCols+` from supavise.oauth_apps a
		where a.org_id = $1 and a.registration_type = 'manual' and a.deleted_at is null order by a.created_at, a.id`, orgID)
	return pgCollect(rows, err, func(r pgx.CollectableRow) (App, error) {
		var a App
		err := r.Scan(pgAppDests(&a)...)
		return a, err
	})
}

func (st *PGStore) CountDynamicApps(ctx context.Context) (int, error) {
	var n int
	err := st.pool.QueryRow(ctx, `select count(*) from supavise.oauth_apps where registration_type = 'dynamic' and deleted_at is null`).Scan(&n)
	return n, pgMapErr(err)
}

func (st *PGStore) CountManualApps(ctx context.Context, orgID int64) (int, error) {
	var n int
	err := st.pool.QueryRow(ctx, `select count(*) from supavise.oauth_apps where org_id = $1 and registration_type = 'manual' and deleted_at is null`, orgID).Scan(&n)
	return n, pgMapErr(err)
}

// ---- client secrets

func (st *PGStore) ListSecrets(ctx context.Context, appID string) ([]AppSecret, error) {
	if !pgIsUUID(appID) {
		return []AppSecret{}, nil
	}
	rows, err := st.pool.Query(ctx, `select `+pgSecretCols+` from supavise.oauth_app_secrets s where s.app_id = $1::uuid order by s.created_at, s.id`, appID)
	return pgCollect(rows, err, func(r pgx.CollectableRow) (AppSecret, error) {
		var s AppSecret
		err := r.Scan(pgSecretDests(&s)...)
		return s, err
	})
}

func (st *PGStore) CreateSecret(ctx context.Context, s AppSecret) error {
	if err := pgCheckHash("client secret", s.Hash); err != nil {
		return err
	}
	if !pgIsUUID(s.ID) {
		return fmt.Errorf("%w: the client secret id is not a UUID", ErrInvalid)
	}
	if !pgIsUUID(s.AppID) {
		return ErrNotFound
	}
	createdBy, err := pgUserArg("created_by", s.CreatedBy)
	if err != nil {
		return err
	}
	return pgInsertSecret(ctx, st.pool, s.AppID, s, createdBy)
}

func (st *PGStore) DeleteSecret(ctx context.Context, appID, secretID string) error {
	if !pgIsUUID(appID) || !pgIsUUID(secretID) {
		return ErrNotFound
	}
	tag, err := st.pool.Exec(ctx, `delete from supavise.oauth_app_secrets where id = $2::uuid and app_id = $1::uuid`, appID, secretID)
	if err != nil {
		return pgMapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (st *PGStore) TouchSecret(ctx context.Context, secretID string, at time.Time) error {
	if !pgIsUUID(secretID) {
		return nil
	}
	_, err := st.pool.Exec(ctx, `update supavise.oauth_app_secrets set last_used_at = $2 where id = $1::uuid`, secretID, at)
	return pgMapErr(err)
}

// ---- authorizations

func (st *PGStore) CreateAuthorization(ctx context.Context, a Authorization) error {
	if !pgIsUUID(a.ID) {
		return fmt.Errorf("%w: the authorization id is not a UUID", ErrInvalid)
	}
	if !pgIsUUID(a.AppID) {
		return ErrNotFound
	}
	// The row always starts pending: a decision is made by DecideAuthorization only.
	_, err := st.pool.Exec(ctx, `insert into supavise.oauth_authorizations
		(id, app_id, redirect_uri, scopes, state, code_challenge, resource, org_hint, created_at, expires_at, status)
		values ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, 'pending')`,
		a.ID, a.AppID, a.RedirectURI, pgStrings(a.Scopes), a.State, a.CodeChallenge, a.Resource, a.OrgHint, a.CreatedAt, a.ExpiresAt)
	return pgMapErr(err)
}

func (st *PGStore) GetAuthorization(ctx context.Context, id string) (*Authorization, error) {
	if !pgIsUUID(id) {
		return nil, ErrNotFound
	}
	var a Authorization
	err := st.pool.QueryRow(ctx, `select `+pgAuthCols+pgAuthFrom+`where a.id = $1::uuid`, id).Scan(pgAuthDests(&a)...)
	if err != nil {
		return nil, pgMapErr(err)
	}
	return &a, nil
}

func (st *PGStore) DecideAuthorization(ctx context.Context, id string, d Decision) (*Authorization, error) {
	if !pgIsUUID(id) {
		return nil, ErrNotFound
	}
	var orgID, codeHash, codeExpires any
	switch d.Status {
	case StatusApproved:
		if d.DecidedBy == "" || d.OrgID == 0 || d.CodeExpiresAt.IsZero() {
			return nil, fmt.Errorf("%w: an approval needs the approver, an organization and a code expiry", ErrInvalid)
		}
		if err := pgCheckHash("authorization code", d.CodeHash); err != nil {
			return nil, err
		}
		orgID, codeHash, codeExpires = d.OrgID, d.CodeHash, d.CodeExpiresAt
	case StatusDeclined:
		// A declined request keeps no organization and no code.
	default:
		return nil, fmt.Errorf("%w: a decision is approved or declined", ErrInvalid)
	}
	decidedBy, err := pgUserArg("decided_by", d.DecidedBy)
	if err != nil {
		return nil, err
	}
	var a Authorization
	err = st.pool.QueryRow(ctx, `with u as (
			update supavise.oauth_authorizations
			   set status = $2, decided_by = $3::uuid, decided_at = $4, org_id = $5, code_hash = $6, code_expires_at = $7
			 where id = $1::uuid and status = 'pending' and expires_at > $4
			returning *)
		select `+pgAuthCols+` from u a left join supavise.organizations o on o.id = a.org_id`,
		id, d.Status, decidedBy, d.At, orgID, codeHash, codeExpires).Scan(pgAuthDests(&a)...)
	if err == nil {
		return &a, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, pgMapErr(err)
	}
	// Nothing changed: say why.
	var status string
	var expires time.Time
	err = st.pool.QueryRow(ctx, `select status, expires_at from supavise.oauth_authorizations where id = $1::uuid`, id).Scan(&status, &expires)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, pgMapErr(err)
	case status != StatusPending:
		return nil, ErrAlreadyDecided
	}
	return nil, ErrExpired
}

func (st *PGStore) CountPending(ctx context.Context, appID string, now time.Time) (int, error) {
	q := `select count(*) from supavise.oauth_authorizations where status = 'pending' and expires_at > $1`
	args := []any{now}
	if appID != "" {
		if !pgIsUUID(appID) {
			return 0, nil
		}
		q += ` and app_id = $2::uuid`
		args = append(args, appID)
	}
	var n int
	err := st.pool.QueryRow(ctx, q, args...).Scan(&n)
	return n, pgMapErr(err)
}

func (st *PGStore) WithCode(ctx context.Context, appID string, codeHash []byte, fn func(ctx context.Context, tx CodeTx) error) error {
	if !pgIsUUID(appID) || len(codeHash) == 0 {
		return ErrNotFound
	}
	return pgx.BeginFunc(ctx, st.pool, func(tx pgx.Tx) error {
		c := &pgCodeTx{tx: tx}
		// for update of a locks the authorization only; the organization is joined for its slug.
		err := tx.QueryRow(ctx, `select `+pgAuthCols+pgAuthFrom+`where a.code_hash = $1 and a.app_id = $2::uuid for update of a`,
			codeHash, appID).Scan(pgAuthDests(&c.auth)...)
		if err != nil {
			return pgMapErr(err)
		}
		return fn(ctx, c)
	})
}

// pgCodeTx is the CodeTx of WithCode.
type pgCodeTx struct {
	tx   pgx.Tx
	auth Authorization // as WithCode read it
}

func (c *pgCodeTx) Authorization() Authorization { return pgCopyAuth(c.auth) }

func (c *pgCodeTx) Burn(ctx context.Context, at time.Time) error {
	// coalesce keeps the first use if the code was spent already.
	_, err := c.tx.Exec(ctx, `update supavise.oauth_authorizations set status = 'exchanged', code_used_at = coalesce(code_used_at, $2) where id = $1::uuid`,
		c.auth.ID, at)
	return pgMapErr(err)
}

func (c *pgCodeTx) Complete(ctx context.Context, in NewGrant) (*CompleteResult, error) {
	g := in.Grant
	if err := pgCheckToken("access token", in.Access); err != nil {
		return nil, err
	}
	if err := pgCheckToken("refresh token", in.Refresh); err != nil {
		return nil, err
	}
	if !pgIsUUID(g.AppID) || !pgIsUUID(g.UserID) {
		return nil, fmt.Errorf("%w: a grant needs an app id and a user id that are UUIDs", ErrInvalid)
	}

	var status string
	if err := c.tx.QueryRow(ctx, `select status from supavise.oauth_authorizations where id = $1::uuid`, c.auth.ID).Scan(&status); err != nil {
		return nil, pgMapErr(err)
	}
	if status != StatusApproved {
		return nil, ErrAlreadyDecided
	}
	// The grant is what the approver approved: the same app, user, organization and resource, and no
	// scope beyond those of the request. The Service builds it from the locked row; this refuses a
	// grant that was built from anything else.
	if a := c.auth; !strings.EqualFold(g.AppID, a.AppID) || !strings.EqualFold(g.UserID, a.DecidedBy) || g.OrgID != a.OrgID ||
		g.Resource != a.Resource || !pgSubset(g.Scopes, a.Scopes) {
		return nil, fmt.Errorf("%w: the grant is not the one the authorization approved", ErrInvalid)
	}

	// The app row is written first. DeleteApp locks the app and then its grants, so taking the locks
	// in that order here keeps a redemption and a deletion of one app from waiting on each other.
	if _, err := c.tx.Exec(ctx, `update supavise.oauth_apps set last_authorized_at = $2 where id = $1::uuid`, g.AppID, in.At); err != nil {
		return nil, pgMapErr(err)
	}

	// An older live grant of the same app, user and organization is replaced by this one.
	rows, err := c.tx.Query(ctx, `update supavise.oauth_grants g set revoked_at = $4, revoked_reason = $5
		where g.app_id = $1::uuid and g.user_id = $2::uuid and g.org_id = $3 and g.revoked_at is null returning `+pgGrantCols,
		g.AppID, g.UserID, g.OrgID, in.At, ReasonSuperseded)
	superseded, err := pgCollectGrants(rows, err)
	if err != nil {
		return nil, err
	}

	var res CompleteResult
	err = c.tx.QueryRow(ctx, `insert into supavise.oauth_grants as g (app_id, user_id, org_id, scopes, resource, created_at)
		values ($1::uuid, $2::uuid, $3, $4, $5, $6) returning `+pgGrantCols,
		g.AppID, g.UserID, g.OrgID, pgStrings(g.Scopes), g.Resource, g.CreatedAt).Scan(pgGrantDests(&res.Grant)...)
	if err != nil {
		return nil, pgMapErr(err)
	}
	if _, err := pgInsertToken(ctx, c.tx, res.Grant.ID, KindAccess, in.Access); err != nil {
		return nil, err
	}
	if _, err := pgInsertToken(ctx, c.tx, res.Grant.ID, KindRefresh, in.Refresh); err != nil {
		return nil, err
	}

	// Keep the newest MaxLive live grants of the user in this organization. Ids rise with insertion,
	// so the order by id never trims the grant just inserted, whatever clock the caller used.
	if in.MaxLive > 0 {
		rows, err := c.tx.Query(ctx, `update supavise.oauth_grants g set revoked_at = $3, revoked_reason = $4
			where g.id in (select id from supavise.oauth_grants where user_id = $1::uuid and org_id = $2 and revoked_at is null
			               order by id desc offset $5)
			returning `+pgGrantCols, g.UserID, g.OrgID, in.At, ReasonSuperseded, int64(in.MaxLive))
		trimmed, err := pgCollectGrants(rows, err)
		if err != nil {
			return nil, err
		}
		superseded = append(superseded, trimmed...)
	}
	res.Superseded = superseded

	if _, err := c.tx.Exec(ctx, `update supavise.oauth_authorizations set status = 'exchanged', grant_id = $2, code_used_at = $3 where id = $1::uuid`,
		c.auth.ID, res.Grant.ID, in.At); err != nil {
		return nil, pgMapErr(err)
	}
	return &res, nil
}

// pgInsertToken inserts an access or refresh token of a grant and returns its id.
func pgInsertToken(ctx context.Context, q pgq, grantID int64, kind string, t Token) (int64, error) {
	if err := pgCheckToken(kind+" token", t); err != nil {
		return 0, err
	}
	var id int64
	err := q.QueryRow(ctx, `insert into supavise.oauth_tokens (grant_id, kind, token_hash, prefix, created_at, expires_at)
		values ($1, $2, $3, $4, $5, $6) returning id`, grantID, kind, t.Hash, t.Prefix, t.CreatedAt, t.ExpiresAt).Scan(&id)
	return id, pgMapErr(err)
}

// ---- grants and tokens

// pgLookupAccessSQL is the query of every OAuth request. The token is found by the unique index on
// token_hash (oauth_tokens_token_hash_key); the grant, app and organization by primary key.
// store_pg_test.go runs explain on it.
const pgLookupAccessSQL = `select t.id, t.expires_at, g.id, g.user_id::text, g.org_id, g.scopes, g.resource, a.id::text, a.name, a.scopes, o.slug
	from supavise.oauth_tokens t
	join supavise.oauth_grants g on g.id = t.grant_id and g.revoked_at is null
	join supavise.oauth_apps a on a.id = g.app_id and a.deleted_at is null
	join supavise.organizations o on o.id = g.org_id
	where t.token_hash = $1 and t.kind = 'access' and t.expires_at > $2`

func (st *PGStore) LookupAccess(ctx context.Context, tokenHash []byte, now time.Time) (*AccessInfo, error) {
	if len(tokenHash) == 0 {
		return nil, ErrNotFound
	}
	var ai AccessInfo
	err := st.pool.QueryRow(ctx, pgLookupAccessSQL, tokenHash, now).
		Scan(&ai.TokenID, &ai.ExpiresAt, &ai.GrantID, &ai.UserID, &ai.OrgID, &ai.GrantScopes, &ai.Resource, &ai.AppID, &ai.AppName, &ai.AppScopes, &ai.OrgSlug)
	if err != nil {
		return nil, pgMapErr(err)
	}
	return &ai, nil
}

func (st *PGStore) TouchToken(ctx context.Context, tokenID, grantID int64, at time.Time) error {
	// A data-modifying CTE runs whether or not the main query reads it.
	_, err := st.pool.Exec(ctx, `with t as (update supavise.oauth_tokens set last_used_at = $3 where id = $1)
		update supavise.oauth_grants set last_used_at = $3 where id = $2`, tokenID, grantID, at)
	return pgMapErr(err)
}

func (st *PGStore) GetToken(ctx context.Context, tokenHash []byte) (*Token, error) {
	if len(tokenHash) == 0 {
		return nil, ErrNotFound
	}
	var t Token
	err := st.pool.QueryRow(ctx, `select `+pgTokenCols+` from supavise.oauth_tokens t where t.token_hash = $1`, tokenHash).Scan(pgTokenDests(&t)...)
	if err != nil {
		return nil, pgMapErr(err)
	}
	return &t, nil
}

func (st *PGStore) GetGrant(ctx context.Context, id int64) (*Grant, error) {
	var g Grant
	err := st.pool.QueryRow(ctx, `select `+pgGrantCols+` from supavise.oauth_grants g where g.id = $1`, id).Scan(pgGrantDests(&g)...)
	if err != nil {
		return nil, pgMapErr(err)
	}
	return &g, nil
}

// pgGrantFilter returns the conditions on g that f sets, joined by "and" (empty if none), with their
// arguments appended to args. ok is false when f names an app or a user that cannot exist, so
// that nothing can match.
func pgGrantFilter(f GrantFilter, args []any) (cond string, out []any, ok bool) {
	var conds []string
	add := func(c string, v any) {
		args = append(args, v)
		conds = append(conds, strings.Replace(c, "?", "$"+strconv.Itoa(len(args)), 1))
	}
	if f.ID != 0 {
		add("g.id = ?", f.ID)
	}
	if f.AppID != "" {
		if !pgIsUUID(f.AppID) {
			return "", nil, false
		}
		add("g.app_id = ?::uuid", f.AppID)
	}
	if f.UserID != "" {
		if !pgIsUUID(f.UserID) {
			return "", nil, false
		}
		add("g.user_id = ?::uuid", f.UserID)
	}
	if f.OrgID != 0 {
		add("g.org_id = ?", f.OrgID)
	}
	if f.Live {
		conds = append(conds, "g.revoked_at is null")
	}
	return strings.Join(conds, " and "), args, true
}

func (st *PGStore) ListGrants(ctx context.Context, f GrantFilter) ([]GrantInfo, error) {
	cond, args, ok := pgGrantFilter(f, nil)
	if !ok {
		return []GrantInfo{}, nil
	}
	q := `select ` + pgGrantCols + `, ` + pgAppCols + `, o.slug
		from supavise.oauth_grants g
		join supavise.oauth_apps a on a.id = g.app_id
		join supavise.organizations o on o.id = g.org_id`
	if cond != "" {
		q += ` where ` + cond
	}
	q += ` order by g.created_at desc, g.id desc`
	if f.Limit > 0 {
		args = append(args, int64(f.Limit))
		q += ` limit $` + strconv.Itoa(len(args))
	}
	rows, err := st.pool.Query(ctx, q, args...)
	return pgCollect(rows, err, func(r pgx.CollectableRow) (GrantInfo, error) {
		var gi GrantInfo
		dests := append(pgGrantDests(&gi.Grant), pgAppDests(&gi.App)...)
		err := r.Scan(append(dests, &gi.OrgSlug)...)
		return gi, err
	})
}

func (st *PGStore) RevokeGrants(ctx context.Context, f GrantFilter, reason string, at time.Time) ([]Grant, error) {
	if !ValidReason(reason) {
		return nil, fmt.Errorf("%w: unknown revocation reason", ErrInvalid)
	}
	// An empty filter selects every live grant: store.go leaves the guard against doing that by
	// accident to the Service (GrantFilter.All).
	cond, args, ok := pgGrantFilter(f, []any{at, reason})
	if !ok {
		return []Grant{}, nil
	}
	q := `update supavise.oauth_grants g set revoked_at = $1, revoked_reason = $2 where g.revoked_at is null`
	if cond != "" {
		q += ` and ` + cond
	}
	rows, err := st.pool.Query(ctx, q+` returning `+pgGrantCols, args...)
	return pgCollectGrants(rows, err)
}

// pgRotateSQL stamps the refresh token that qualifies (see Store.RotateRefresh) and returns it with its
// grant. $1 hash, $2 app, $3 now, $4 now minus the grace window.
const pgRotateSQL = `update supavise.oauth_tokens t set used_at = coalesce(t.used_at, $3)
	from supavise.oauth_grants g
	where t.token_hash = $1 and t.kind = 'refresh' and t.expires_at > $3
	  and (t.used_at is null or t.used_at > $4)
	  and g.id = t.grant_id and g.revoked_at is null and g.app_id = $2::uuid
	returning ` + pgTokenCols + `, ` + pgGrantCols

// pgReuseSQL finds the refresh token that pgRotateSQL refused only because it was used before the
// grace window: a replay. Same parameters.
const pgReuseSQL = `select g.id, g.app_id::text, g.user_id::text, g.org_id
	from supavise.oauth_tokens t join supavise.oauth_grants g on g.id = t.grant_id
	where t.token_hash = $1 and t.kind = 'refresh' and t.expires_at > $3 and t.used_at <= $4
	  and g.revoked_at is null and g.app_id = $2::uuid`

func (st *PGStore) RotateRefresh(ctx context.Context, in RotateInput, fn func(ctx context.Context, tx RotateTx) error) error {
	if !pgIsUUID(in.AppID) || len(in.TokenHash) == 0 {
		return ErrNotFound
	}
	edge := in.Now.Add(-in.Grace)
	return pgx.BeginFunc(ctx, st.pool, func(tx pgx.Tx) error {
		r := &pgRotateTx{tx: tx}
		err := tx.QueryRow(ctx, pgRotateSQL, in.TokenHash, in.AppID, in.Now, edge).
			Scan(append(pgTokenDests(&r.tok), pgGrantDests(&r.grant)...)...)
		if err == nil {
			return fn(ctx, r)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return pgMapErr(err)
		}
		var re ReuseError
		err = tx.QueryRow(ctx, pgReuseSQL, in.TokenHash, in.AppID, in.Now, edge).Scan(&re.GrantID, &re.AppID, &re.UserID, &re.OrgID)
		switch {
		case err == nil:
			return &re
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotFound
		}
		return pgMapErr(err)
	})
}

// pgRotateTx is the RotateTx of RotateRefresh. Token and Grant show the writes made through the
// handle (NarrowScopes, Issue, RevokeGrant).
type pgRotateTx struct {
	tx    pgx.Tx
	tok   Token
	grant Grant
}

func (r *pgRotateTx) Token() Token { return pgCopyToken(r.tok) }
func (r *pgRotateTx) Grant() Grant { return pgCopyGrant(r.grant) }

func (r *pgRotateTx) NarrowScopes(ctx context.Context, scopes []string) error {
	for _, s := range scopes {
		if !slices.Contains(r.grant.Scopes, s) {
			return fmt.Errorf("%w: a refresh can only narrow the scopes of the grant", ErrInvalid)
		}
	}
	if _, err := r.tx.Exec(ctx, `update supavise.oauth_grants set scopes = $2 where id = $1`, r.grant.ID, pgStrings(scopes)); err != nil {
		return pgMapErr(err)
	}
	r.grant.Scopes = slices.Clone(scopes)
	return nil
}

func (r *pgRotateTx) Issue(ctx context.Context, access, refresh Token) error {
	if _, err := pgInsertToken(ctx, r.tx, r.grant.ID, KindAccess, access); err != nil {
		return err
	}
	refreshID, err := pgInsertToken(ctx, r.tx, r.grant.ID, KindRefresh, refresh)
	if err != nil {
		return err
	}
	if r.tok.ReplacedBy != 0 {
		return nil
	}
	tag, err := r.tx.Exec(ctx, `update supavise.oauth_tokens set replaced_by = $2 where id = $1 and replaced_by is null`, r.tok.ID, refreshID)
	if err != nil {
		return pgMapErr(err)
	}
	if tag.RowsAffected() == 1 {
		r.tok.ReplacedBy = refreshID
	}
	return nil
}

func (r *pgRotateTx) RevokeGrant(ctx context.Context, reason string, at time.Time) error {
	if !ValidReason(reason) {
		return fmt.Errorf("%w: unknown revocation reason", ErrInvalid)
	}
	tag, err := r.tx.Exec(ctx, `update supavise.oauth_grants set revoked_at = $2, revoked_reason = $3 where id = $1 and revoked_at is null`, r.grant.ID, at, reason)
	if err != nil {
		return pgMapErr(err)
	}
	if tag.RowsAffected() == 1 {
		r.grant.RevokedAt, r.grant.RevokedReason = &at, reason
	}
	return nil
}

// ---- housekeeping

func (st *PGStore) Prune(ctx context.Context, p PruneParams) (PruneResult, error) {
	var res PruneResult
	// A zero cutoff means "do not prune this". The statements run one after the other, each on its own,
	// so a failure leaves what the earlier ones deleted counted in res.
	for _, s := range []struct {
		before time.Time
		sql    string
		n      *int
	}{
		{p.AuthorizationsBefore, `delete from supavise.oauth_authorizations where expires_at < $1`, &res.Authorizations},
		{p.TokensBefore, `delete from supavise.oauth_tokens where expires_at < $1`, &res.Tokens},
		{p.RevokedGrantsBefore, `delete from supavise.oauth_grants where revoked_at < $1`, &res.Grants},
		{p.UnusedAppsBefore, `delete from supavise.oauth_apps where registration_type = 'dynamic' and last_authorized_at is null and created_at < $1`, &res.Apps},
		{p.IdleAppsBefore, `delete from supavise.oauth_apps a where a.registration_type = 'dynamic' and coalesce(a.last_authorized_at, a.created_at) < $1
			and not exists (select 1 from supavise.oauth_grants g where g.app_id = a.id and g.revoked_at is null)`, &res.Apps},
	} {
		if s.before.IsZero() {
			continue
		}
		tag, err := st.pool.Exec(ctx, s.sql, s.before)
		if err != nil {
			return res, pgMapErr(err)
		}
		*s.n += int(tag.RowsAffected())
	}
	return res, nil
}
