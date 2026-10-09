package oauth

import "time"

// App registration types (oauth_apps.registration_type).
const (
	RegistrationManual  = "manual"
	RegistrationDynamic = "dynamic"
)

// Token endpoint authentication methods (RFC 7591), the values of oauth_apps.token_endpoint_auth_method.
const (
	AuthMethodNone  = "none"
	AuthMethodBasic = "client_secret_basic"
	AuthMethodPost  = "client_secret_post"
)

// Grant types, response types and PKCE methods of the protocol.
const (
	GrantTypeAuthorizationCode = "authorization_code"
	GrantTypeRefreshToken      = "refresh_token"
	// GrantTypeJWTBearer is named by the pinned spec and refused: unsupported_grant_type.
	GrantTypeJWTBearer = "urn:ietf:params:oauth:grant-type:jwt-bearer"

	ResponseTypeCode  = "code"
	ResponseModeQuery = "query"
	// PKCEMethodS256 is the only code_challenge_method accepted.
	PKCEMethodS256 = "S256"
)

// Authorization states (oauth_authorizations.status).
const (
	StatusPending   = "pending"
	StatusApproved  = "approved"
	StatusDeclined  = "declined"
	StatusExchanged = "exchanged"
)

// Token kinds (oauth_tokens.kind).
const (
	KindAccess  = "access"
	KindRefresh = "refresh"
)

// Revocation reasons (oauth_grants.revoked_reason; the empty string is "not revoked"). The
// database refuses any other value.
const (
	ReasonUser         = "user"          // the user revoked their own grant
	ReasonAdmin        = "admin"         // an Owner or Administrator revoked the app in Studio
	ReasonOperator     = "operator"      // `supavise oauth grants revoke`
	ReasonClient       = "client"        // POST /v1/oauth/revoke
	ReasonAppDeleted   = "app_deleted"   // the app was deleted
	ReasonSuperseded   = "superseded"    // a newer grant of the same app, user and organization, or the cap on live grants
	ReasonRefreshReuse = "refresh_reuse" // a rotated refresh token was presented again
	ReasonCodeReuse    = "code_reuse"    // an authorization code was redeemed twice
	ReasonMembership   = "membership"    // the user left the organization
	ReasonUserRemoved  = "user_removed"  // the user was removed, or SSO no longer admits them
)

// ValidReason reports whether r is one of the Reason constants.
func ValidReason(r string) bool {
	switch r {
	case ReasonUser, ReasonAdmin, ReasonOperator, ReasonClient, ReasonAppDeleted, ReasonSuperseded,
		ReasonRefreshReuse, ReasonCodeReuse, ReasonMembership, ReasonUserRemoved:
		return true
	}
	return false
}

// Actors recorded in audit events when no dashboard user caused the change.
const (
	ActorOperator = "operator" // the CLI on the node
	ActorSystem   = "system"   // the server itself (reuse detection, a lost membership)
)

// Token formats. Every secret is its prefix plus lowercase hex from crypto/rand; the stored form
// is SHA-256 (secrets.HashToken). AccessTokenPrefix extends the personal access token pattern
// sbp_[a-f0-9]{40} on purpose: the Supabase CLI accepts it.
//
// AuthCodePrefix is also the prefix of the claim tokens of `supavise claim` (sbc_ plus 48 hex). The
// two never meet: they are looked up in different tables, and a code is 64 hex.
const (
	AccessTokenPrefix  = "sbp_oauth_"
	AccessTokenHexLen  = 40
	RefreshTokenPrefix = "sbr_"
	RefreshTokenHexLen = 64
	AuthCodePrefix     = "sbc_"
	AuthCodeHexLen     = 64
	ClientSecretPrefix = "sba_"
	ClientSecretHexLen = 64

	// StoredPrefixLen is how many leading characters of a token oauth_tokens.prefix keeps, as
	// access_tokens.prefix does for personal access tokens (token[:8]). The first eight characters of
	// "sbp_oauth_..." are the constant "sbp_oaut", so the column identifies nothing; it is never used
	// for lookup.
	StoredPrefixLen = 8
)

// Lifetimes. They are constants in this release.
const (
	AccessTokenTTL  = time.Hour
	RefreshTokenTTL = 90 * 24 * time.Hour
	// AuthCodeTTL is how long an approved authorization's code can be redeemed.
	AuthCodeTTL = 60 * time.Second
	// AuthorizationTTL is how long a pending authorization waits for a decision.
	AuthorizationTTL = 10 * time.Minute
	// RefreshGrace is the window in which a refresh token that was exchanged already is exchanged
	// again without revoking the grant (two processes of one client refreshing at once). Each such
	// exchange issues a fresh pair.
	RefreshGrace = 10 * time.Second
)

// Caps (section 2.4, 2.5, 2.8 and 2.10 of the design). They are soft: a count is read and then a
// row is written, so concurrent requests can overshoot a cap by their own number.
const (
	MaxDynamicApps          = 5000
	MaxManualAppsPerOrg     = 20
	MaxPendingPerApp        = 25
	MaxPendingTotal         = 10000
	MaxLiveGrantsPerUserOrg = 50

	MaxRedirectURIs   = 10
	MaxRedirectURILen = 2048
	MaxClientNameLen  = 100
	// MaxRedirectAndState bounds len(redirect_uri) + len(state) of an authorization request.
	MaxRedirectAndState = 4096
)

// Pruning (section 2.16). The Service computes the cutoffs from its clock and these ages.
const (
	// PruneEvery is the shortest gap between two lazy prunes on one node.
	PruneEvery = 10 * time.Minute
	// AuthorizationRetention is how long an authorization row outlives its expires_at.
	AuthorizationRetention = 24 * time.Hour
	// TokenRetention is how long an expired token row is kept.
	TokenRetention = 7 * 24 * time.Hour
	// RevokedGrantRetention is how long a revoked grant (and with it, by cascade, its tokens) is kept.
	RevokedGrantRetention = 30 * 24 * time.Hour
	// UnusedAppRetention is how long a dynamic app that nobody ever authorized is kept.
	UnusedAppRetention = 24 * time.Hour
	// IdleAppRetention is how long a dynamic app with no live grant is kept after its last authorization.
	IdleAppRetention = 90 * 24 * time.Hour
)

// App is a registered client (oauth_apps).
type App struct {
	// ID is the client_id: a UUID v4.
	ID               string
	RegistrationType string
	// OrgID is the publishing organization of a manual app; 0 for a dynamic app.
	OrgID int64
	Name  string
	// Website is the client_uri of a dynamic app (https only) or the website of a manual one; "" if none.
	Website string
	// Icon is the logo_uri of a dynamic app or the icon of a manual one; "" if none. For a dynamic app it is
	// stored and never returned or shown: Describe and the apps API leave it out.
	Icon         string
	RedirectURIs []string
	// Scopes are the scopes the app may ask for: the 13 advertised ones for a dynamic app, any of
	// the 24 for a manual one.
	Scopes                  []string
	TokenEndpointAuthMethod string
	// CreatedBy is the GoTrue user id that published a manual app; "" for a dynamic one.
	CreatedBy        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	LastAuthorizedAt *time.Time
	DeletedAt        *time.Time
}

// AppSecret is a client secret of a manual app (oauth_app_secrets), or the secret issued at
// dynamic registration. The plaintext is never stored.
type AppSecret struct {
	// ID is a UUID v4 (the platform spec requires one).
	ID    string
	AppID string
	// Alias is shown instead of the secret: the first eight characters of the secret ("sba_" and four
	// hex digits) followed by eight asterisks, "sba_1a2b********".
	Alias string
	// Hash is SHA-256 of the secret. The Service clears it before it returns a secret to a caller.
	Hash       []byte
	CreatedBy  string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// Authorization is a request waiting for a decision, or a decided one until it is pruned
// (oauth_authorizations).
type Authorization struct {
	// ID is the auth_id: a UUID v4.
	ID    string
	AppID string
	// RedirectURI is as requested, a loopback port included.
	RedirectURI string
	// Scopes are the effective scopes: the requested ones (or the app's) intersected with the app's.
	Scopes []string
	State  string
	// CodeChallenge is the S256 challenge; "" when the request had none (manual apps only).
	CodeChallenge string
	// Resource is the canonical resource (RFC 8707) the grant is bound to; "" if none was requested.
	Resource string
	// OrgHint is the organization_slug of the request; "" if none.
	OrgHint   string
	CreatedAt time.Time
	ExpiresAt time.Time
	Status    string
	// DecidedBy is the GoTrue user that approved or declined.
	DecidedBy string
	DecidedAt *time.Time
	// OrgID is the organization chosen at approval; 0 until then. OrgSlug is its slug (filled by
	// the Store with a join; ignored on writes).
	OrgID   int64
	OrgSlug string
	// CodeHash is SHA-256 of the authorization code; nil until approval and after it is pruned.
	CodeHash      []byte
	CodeExpiresAt *time.Time
	CodeUsedAt    *time.Time
	// GrantID is the grant a redeemed code created; 0 until then.
	GrantID int64
}

// Grant is what a client holds: the authority of one user in one organization (oauth_grants).
type Grant struct {
	ID     int64
	AppID  string
	UserID string
	OrgID  int64
	// Scopes are the scopes approved. The effective scopes at use are these intersected with the app's.
	Scopes []string
	// Resource is the canonical resource the grant is bound to; "" if none.
	Resource      string
	CreatedAt     time.Time
	LastUsedAt    *time.Time
	RevokedAt     *time.Time
	RevokedReason string
}

// Token is an access or refresh token row (oauth_tokens). The token itself is never stored.
type Token struct {
	ID      int64
	GrantID int64
	Kind    string
	// Hash is SHA-256 of the token.
	Hash   []byte
	Prefix string
	// CreatedAt and ExpiresAt are set by the caller.
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt *time.Time
	// UsedAt is when a refresh token was first exchanged; nil while it has not been.
	UsedAt *time.Time
	// ReplacedBy is the id of the token that replaced a refresh token; 0 until then.
	ReplacedBy int64
}

// AccessInfo is what a live access token stands for. Store.LookupAccess builds it with one query
// (the hot path of every OAuth request) and Service.LookupAccess completes it.
type AccessInfo struct {
	TokenID   int64
	ExpiresAt time.Time
	GrantID   int64
	AppID     string
	AppName   string
	UserID    string
	OrgID     int64
	OrgSlug   string
	// Resource is the resource the grant is bound to; "" if none.
	Resource string
	// GrantScopes and AppScopes are the stored lists (Store.LookupAccess fills them). Scopes is their
	// intersection, sorted: what the token may do. Service.LookupAccess fills it; the Store leaves it nil.
	GrantScopes []string
	AppScopes   []string
	Scopes      []string
}

// GrantInfo is a grant with the names a listing shows.
type GrantInfo struct {
	Grant   Grant
	App     App
	OrgSlug string
}

// AuthorizedApp is an app with at least one live grant in an organization (the "Authorized" tab
// of the OAuth Apps page).
type AuthorizedApp struct {
	App App
	// AuthorizedAt is the creation time of the newest live grant of the app in the organization.
	AuthorizedAt time.Time
	// Scopes are the union of the live grants' effective scopes, sorted.
	Scopes []string
	// FirstApprover is the user id of the oldest live grant; "" if unknown.
	FirstApprover string
}

// GrantFilter selects grants. A zero field does not constrain. The Store matches on all set fields.
type GrantFilter struct {
	ID     int64
	AppID  string
	UserID string
	OrgID  int64
	// Live limits a listing to grants that are not revoked. Revocation always touches live grants only.
	Live bool
	// All allows RevokeGrants to run with no other field set, which revokes every live grant. Without
	// it an empty filter is ErrInvalid. ListGrants ignores it.
	All bool
	// Limit caps ListGrants (newest first); 0 means no cap.
	Limit int
}

// IsEmpty reports whether f has no ID, AppID, UserID or OrgID, so that it selects every grant.
func (f GrantFilter) IsEmpty() bool {
	return f.ID == 0 && f.AppID == "" && f.UserID == "" && f.OrgID == 0
}

// PruneResult counts what Prune deleted.
type PruneResult struct {
	Authorizations int
	Tokens         int
	Grants         int
	Apps           int
}

// Audit event kinds (Registry.AppendEvent on config.SystemRef). Payloads carry ids, the
// organization slug, the user id, a sanitized client name, the redirect host and the scopes; never
// a token, code, secret, state or challenge. Dynamic registrations are logged at info level and
// not audited.
const (
	EventAuthorizationApproved = "oauth.authorization_approved"
	EventAuthorizationDeclined = "oauth.authorization_declined"
	EventGrantCreated          = "oauth.grant_created"
	EventGrantRevoked          = "oauth.grant_revoked" // payload: reason, actor
	EventRefreshReuse          = "oauth.refresh_reuse"
	EventCodeReuse             = "oauth.code_reuse"
	EventAppCreated            = "oauth.app_created"
	EventAppUpdated            = "oauth.app_updated"
	EventAppDeleted            = "oauth.app_deleted"
	EventClientSecretCreated   = "oauth.client_secret_created"
	EventClientSecretDeleted   = "oauth.client_secret_deleted"
)

// AlertKindTokenReuse is the alert raised when a refresh token or a code is replayed. It equals
// alerts.KindOAuthTokenReuse; internal/oauth does not import internal/alerts.
const AlertKindTokenReuse = "oauth_token_reuse"

// AlertEvent is what Service.Alert receives. Key is AlertKindTokenReuse + "/" + the grant id, so
// the notifier raises it once per grant. Title and Detail contain no secret.
type AlertEvent struct {
	Kind   string
	Key    string
	Title  string
	Detail string
}
