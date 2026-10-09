package oauth

import (
	"context"
	"time"
)

// Store is what the Service needs from persistence: five tables (oauth_apps, oauth_app_secrets,
// oauth_authorizations, oauth_grants, oauth_tokens; registry migration 1350_oauth.sql). PGStore
// implements it over the registry's pool and MemoryStore in memory; storetest holds the contract
// tests that both run.
//
// Conventions that hold for every method:
//
//   - Times are written as given. The Store never reads a clock: the Service passes its own, so
//     a fake clock drives expiry. (A column default of now() is never relied on.)
//   - The Store never generates an id or a secret. The Service sets App.ID, AppSecret.ID and
//     Authorization.ID (UUID v4) and the hashes; the Store assigns only the identity ids of grants
//     and tokens.
//   - An identifier that is not a well-formed UUID is "not found", not a driver error.
//   - Slices of scopes and redirect URIs keep their order; nil and empty are the same.
//   - In the nullable columns the zero value is NULL and back: OrgID 0, GrantID 0, ReplacedBy 0, an
//     empty CreatedBy or DecidedBy, a nil *time.Time, a nil CodeHash. User ids (Grant.UserID,
//     CreatedBy, DecidedBy) are GoTrue user ids, UUIDs in text form.
//   - A write that would break a unique key returns ErrConflict.
//   - Deleted apps (deleted_at set) are invisible to GetApp, ListManualApps and the counts; their
//     grants are revoked, so they do not appear in live listings either.
//   - A method that takes fn runs it inside one transaction: a non-nil error from fn undoes every
//     write fn made through the transaction handle and is returned unchanged; nil commits. To
//     commit and still report a failure to the caller, record the failure in a variable that fn
//     closes over and return nil.
type Store interface {
	// ---- apps

	// CreateApp inserts the app and, if secret is not nil, its first secret, in one transaction.
	CreateApp(ctx context.Context, app App, secret *AppSecret) error
	// GetApp returns the live app with this id, or ErrNotFound.
	GetApp(ctx context.Context, id string) (*App, error)
	// UpdateApp overwrites Name, Website, Icon, RedirectURIs, Scopes and UpdatedAt of the live app
	// with app.ID. Everything else is kept. ErrNotFound if there is no such live app.
	UpdateApp(ctx context.Context, app App) error
	// DeleteApp marks the live app deleted at `at` and, in the same transaction, revokes its live
	// grants with ReasonAppDeleted. It returns the grants it revoked. ErrNotFound if there is no
	// such live app.
	DeleteApp(ctx context.Context, id string, at time.Time) ([]Grant, error)
	// ListManualApps returns the live manual apps of the organization, oldest first (CreatedAt, ID).
	ListManualApps(ctx context.Context, orgID int64) ([]App, error)
	// CountDynamicApps and CountManualApps count live apps: all dynamic ones, and the manual ones of one organization.
	CountDynamicApps(ctx context.Context) (int, error)
	CountManualApps(ctx context.Context, orgID int64) (int, error)

	// ---- client secrets

	// ListSecrets returns the secrets of an app, oldest first (CreatedAt, ID), with their hashes
	// (the Service authenticates clients with them). An app with none, or no such app, gives an empty list.
	ListSecrets(ctx context.Context, appID string) ([]AppSecret, error)
	// CreateSecret inserts a secret. ErrNotFound if the app does not exist; ErrConflict if the hash is taken.
	CreateSecret(ctx context.Context, s AppSecret) error
	// DeleteSecret removes one secret of the app. ErrNotFound if the app has no such secret.
	DeleteSecret(ctx context.Context, appID, secretID string) error
	// TouchSecret sets last_used_at. An unknown id is not an error.
	TouchSecret(ctx context.Context, secretID string, at time.Time) error

	// ---- authorizations

	// CreateAuthorization inserts a pending authorization.
	CreateAuthorization(ctx context.Context, a Authorization) error
	// GetAuthorization returns the authorization in any state, expired ones included, with OrgSlug
	// filled when OrgID is set. ErrNotFound if unknown. It does not check that the app is live.
	GetAuthorization(ctx context.Context, id string) (*Authorization, error)
	// DecideAuthorization approves or declines a pending authorization that has not expired at
	// d.At, in one atomic update, and returns the updated row. When it changes nothing it says why:
	// ErrNotFound (no such id), ErrAlreadyDecided (the state is not pending) or ErrExpired (pending,
	// expires_at <= d.At). Two concurrent calls cannot both succeed.
	DecideAuthorization(ctx context.Context, id string, d Decision) (*Authorization, error)
	// CountPending counts authorizations that are pending and unexpired at `now`: those of one app,
	// or of all apps when appID is empty.
	CountPending(ctx context.Context, appID string, now time.Time) (int, error)
	// WithCode locks the authorization of this app whose code hash is codeHash and runs fn in one
	// transaction. If there is no such row (a code of another app does not count) it returns
	// ErrNotFound without running fn, and nothing changes. Concurrent calls for one code run one after the
	// other. The row is whatever state it is in: fn decides what that means.
	WithCode(ctx context.Context, appID string, codeHash []byte, fn func(ctx context.Context, tx CodeTx) error) error

	// ---- grants and tokens

	// LookupAccess is the hot path of every OAuth request: one query that joins the token, its
	// grant, the app and the organization. It returns ErrNotFound unless the token is an access
	// token with expires_at > now, its grant is not revoked and its app is not deleted. It fills
	// GrantScopes and AppScopes and leaves Scopes nil.
	LookupAccess(ctx context.Context, tokenHash []byte, now time.Time) (*AccessInfo, error)
	// TouchToken sets last_used_at on the token and on its grant. Unknown ids are not an error.
	TouchToken(ctx context.Context, tokenID, grantID int64, at time.Time) error
	// GetToken returns the token row with this hash whatever its kind and state, or ErrNotFound.
	GetToken(ctx context.Context, tokenHash []byte) (*Token, error)
	// GetGrant returns the grant in any state, or ErrNotFound.
	GetGrant(ctx context.Context, id int64) (*Grant, error)
	// ListGrants returns the grants f selects, newest first (CreatedAt, ID descending), with their
	// apps (deleted ones included) and organization slugs.
	ListGrants(ctx context.Context, f GrantFilter) ([]GrantInfo, error)
	// RevokeGrants revokes the live grants f selects (RevokedAt = at, RevokedReason = reason) and
	// returns them as updated. A revoked grant keeps its first reason. An empty f selects every live
	// grant; the Service guards against doing that by accident.
	RevokeGrants(ctx context.Context, f GrantFilter, reason string, at time.Time) ([]Grant, error)
	// RotateRefresh exchanges a refresh token, in one transaction.
	//
	// It stamps UsedAt = coalesce(UsedAt, in.Now) on the refresh token with in.TokenHash (kind
	// refresh) when that token has not expired at in.Now, belongs to a live grant of in.AppID, and has
	// either never been used or was first used less than in.Grace before in.Now. A second caller
	// that arrives while the first is in flight waits for it and then passes the same test.
	// If it stamped a token it runs fn with a handle on that token and its grant; an error from fn
	// rolls the stamp back.
	//
	// If no token qualifies: ErrNotFound, except when a refresh token of in.AppID with this hash
	// exists, has not expired, has a live grant and was first used in.Grace or longer before
	// in.Now. That is a replay, and the result is a *ReuseError (errors.Is(err, ErrRefreshReused)).
	// Nothing is written then; the Service revokes the grant.
	RotateRefresh(ctx context.Context, in RotateInput, fn func(ctx context.Context, tx RotateTx) error) error

	// ---- housekeeping

	// Prune deletes what the cutoffs of p allow. Each delete is idempotent, so two nodes may prune at once.
	Prune(ctx context.Context, p PruneParams) (PruneResult, error)
}

// Decision is the outcome of an authorization request, written by DecideAuthorization.
type Decision struct {
	// Status is StatusApproved or StatusDeclined.
	Status string
	// DecidedBy is the GoTrue user id of the approver.
	DecidedBy string
	// At is DecidedAt, and the instant against which the request must not have expired.
	At time.Time
	// OrgID, CodeHash and CodeExpiresAt are set on approval only. CodeHash is SHA-256 of the code.
	OrgID         int64
	CodeHash      []byte
	CodeExpiresAt time.Time
}

// CodeTx is the handle fn gets in Store.WithCode: the locked authorization and the writes a code
// redemption makes. Every method works on the same transaction.
type CodeTx interface {
	// Authorization returns the row as WithCode read it, OrgSlug filled. It is a copy.
	Authorization() Authorization
	// Burn spends the code without issuing anything: status exchanged, code_used_at = at, grant_id unchanged.
	// The Service calls it when a check of the redemption fails, then commits and answers invalid_grant.
	Burn(ctx context.Context, at time.Time) error
	// Complete issues the grant, with these writes in order: revoke the live grants of the same
	// (app, user, organization) with ReasonSuperseded; insert the grant; insert the access and
	// refresh tokens for it; trim the live grants of (user, organization) to in.MaxLive; set the
	// authorization's status to exchanged, grant_id to the new grant and code_used_at to in.At; set the
	// app's last_authorized_at to in.At. It returns ErrAlreadyDecided, and writes nothing, if the
	// locked row is not approved any more.
	Complete(ctx context.Context, in NewGrant) (*CompleteResult, error)
}

// NewGrant is what a redeemed code is turned into.
type NewGrant struct {
	// Grant: AppID, UserID, OrgID, Scopes, Resource and CreatedAt are used; the rest is ignored.
	Grant Grant
	// Access and Refresh: Hash, Prefix, CreatedAt and ExpiresAt are used (Kind is set by the Store);
	// ID and GrantID are ignored.
	Access  Token
	Refresh Token
	// MaxLive keeps at most this many live grants per (user, organization): after the insert, the
	// oldest live grants beyond MaxLive are revoked. 0: no trimming.
	MaxLive int
	// At is the revocation time, code_used_at and the app's last_authorized_at.
	At time.Time
}

// CompleteResult is what Complete stored.
type CompleteResult struct {
	// Grant is the new grant with its ID.
	Grant Grant
	// Superseded are the live grants Complete revoked with ReasonSuperseded, as updated: first those
	// of the same app, user and organization, then those trimmed to MaxLive. For the audit trail.
	Superseded []Grant
}

// RotateInput selects the refresh token for Store.RotateRefresh.
type RotateInput struct {
	AppID     string
	TokenHash []byte
	Now       time.Time
	// Grace is RefreshGrace.
	Grace time.Duration
}

// RotateTx is the handle fn gets in Store.RotateRefresh.
type RotateTx interface {
	// Token is the refresh token presented, with UsedAt set.
	Token() Token
	// Grant is the live grant it belongs to.
	Grant() Grant
	// NarrowScopes replaces the grant's scopes with a subset of them. The Service calls it when a
	// refresh asks for fewer scopes: scopes are stored on the grant, so narrowing is permanent for
	// the grant (least privilege; tokens carry no scopes of their own).
	NarrowScopes(ctx context.Context, scopes []string) error
	// Issue stores a new access and a new refresh token for the grant and, if the presented token has no
	// ReplacedBy yet, sets it to the new refresh token's id. Hash, Prefix, CreatedAt and ExpiresAt are used.
	Issue(ctx context.Context, access, refresh Token) error
	// RevokeGrant revokes the grant (reason, at). The Service uses it when Admit says the user left
	// the organization, then returns nil from fn so that the revocation commits.
	RevokeGrant(ctx context.Context, reason string, at time.Time) error
}

// PruneParams are the cutoffs of Store.Prune. The Service derives them from its clock and the
// retention constants of types.go. Deletes cascade by foreign key (a grant takes its tokens).
type PruneParams struct {
	// AuthorizationsBefore deletes authorizations with expires_at before it.
	AuthorizationsBefore time.Time
	// TokensBefore deletes tokens with expires_at before it.
	TokensBefore time.Time
	// RevokedGrantsBefore deletes grants with revoked_at before it.
	RevokedGrantsBefore time.Time
	// UnusedAppsBefore deletes dynamic apps that were never authorized (last_authorized_at is null)
	// and were created before it.
	UnusedAppsBefore time.Time
	// IdleAppsBefore deletes dynamic apps that have no live grant and whose last_authorized_at (or,
	// if never authorized, created_at) is before it.
	IdleAppsBefore time.Time
}
