package oauth

import (
	"context"
	"time"
)

// Authority is the method set of Service as its callers see it: the HTTP layer of internal/api,
// the operator CLI and the hooks that end grants. Code that only calls the Service depends on
// this interface so that tests can fake it: embed Authority in a struct and define the methods the
// test uses; the others panic on a nil interface, which is what a test wants.
//
//	type fakeOAuth struct{ oauth.Authority }
//	func (fakeOAuth) LookupAccess(context.Context, string) (*oauth.AccessInfo, error) { ... }
//
// Every method that changes state raises its audit event (Service.Audit) and, for a replay, its
// alert (Service.Alert); callers do not audit what the Service has audited already.
type Authority interface {
	// ---- the authorization server's endpoints

	// Register creates a dynamic app (POST /platform/oauth/apps/register). A request the Service
	// refuses is an *Error with code invalid_client_metadata or invalid_redirect_uri; the caps are ErrLimit.
	// The rate limit per client address is the caller's.
	Register(ctx context.Context, req RegisterRequest) (*RegisteredApp, error)

	// StartAuthorization validates an authorization request (GET /v1/oauth/authorize) and stores it
	// as pending. Errors: ErrUnknownClient and ErrInvalidRedirectURI (no redirect), *RedirectError
	// (redirect with the error), ErrLimit (too many pending requests). The rate limit per client
	// address is the caller's.
	StartAuthorization(ctx context.Context, req AuthorizeRequest) (*AuthorizeResult, error)

	// Describe returns what the consent page shows for an authorization (GET /platform/oauth/authorizations/{id}).
	// An expired or decided request is described too. ErrNotFound if the id is unknown or its app is deleted.
	Describe(ctx context.Context, authID string) (*AuthorizationView, error)

	// Approve records the approval and returns the URL that delivers the code to the client. The
	// caller has checked who may approve (Owner or Administrator of the organization) and the second
	// factor. Errors: ErrNotFound, ErrAlreadyDecided, ErrExpired, ErrOrgMismatch.
	Approve(ctx context.Context, req ApproveRequest) (*ApproveResult, error)

	// Decline records the refusal. Errors: ErrNotFound, ErrAlreadyDecided, ErrExpired.
	Decline(ctx context.Context, req DeclineRequest) error

	// Exchange serves POST /v1/oauth/token for the authorization_code and refresh_token grants. It
	// authenticates the client, then redeems the code or rotates the refresh token. Errors are
	// *Error (the table of section 2.7: invalid_request, invalid_client, invalid_grant,
	// unsupported_grant_type, invalid_scope, invalid_target). A failure to redeem a code burns it.
	// The failure limiter per client address is the caller's.
	Exchange(ctx context.Context, req TokenRequest) (*TokenResponse, error)

	// Revoke serves POST /v1/oauth/revoke (RFC 7009): it authenticates the app and revokes the whole
	// grant the token belongs to (ReasonClient). A token that is unknown or belongs to another app is
	// not an error. Bad client credentials are an *Error invalid_client.
	Revoke(ctx context.Context, req RevokeRequest) error

	// ---- the resource side

	// LookupAccess resolves an access token (the plaintext, "sbp_oauth_...") to what it stands for.
	// ErrNotFound for every kind of unusable token (unknown, expired, revoked, deleted app), so the
	// caller cannot tell them apart. It does not run Admit: the caller applies the user checks
	// (removed, SSO, membership) it applies to every principal.
	LookupAccess(ctx context.Context, token string) (*AccessInfo, error)

	// TouchAccess records last_used_at on the token and its grant. Best effort; the caller limits how
	// often it calls.
	TouchAccess(ctx context.Context, tokenID, grantID int64) error

	// ---- ending grants (Reason is one of the Reason constants; actor is a user id, ActorOperator or ActorSystem)

	// RevokeGrant revokes one live grant. ErrNotFound if no live grant has the id.
	RevokeGrant(ctx context.Context, grantID int64, reason, actor string) error
	// RevokeGrants revokes the live grants f selects and returns how many. An empty filter without
	// f.All is ErrInvalid.
	RevokeGrants(ctx context.Context, f GrantFilter, reason, actor string) (int, error)
	// RevokeUser revokes every live grant of a user (ReasonUserRemoved when the user was removed or
	// SSO no longer admits them) and returns how many. A user with none is not an error.
	RevokeUser(ctx context.Context, userID, reason, actor string) (int, error)
	// RevokeApp revokes the live grants of an app in one organization (req.OrgID) or in all (0) and
	// returns the app as it was. ErrNotFound if the app does not exist or is deleted; an app with no
	// live grant gives Revoked == 0 and no error.
	RevokeApp(ctx context.Context, req RevokeAppRequest) (*RevokedApp, error)
	// ListGrants returns the grants f selects with their apps and organization slugs, newest first.
	ListGrants(ctx context.Context, f GrantFilter) ([]GrantInfo, error)

	// ---- the organization's OAuth Apps page. Every method takes the organization and treats an app
	// of another organization, a dynamic app (where noted) or a deleted one as ErrNotFound.

	// ListAuthorizedApps returns the apps with a live grant in the organization (the "authorized" tab).
	ListAuthorizedApps(ctx context.Context, orgID int64) ([]AuthorizedApp, error)
	// ListPublishedApps returns the manual apps of the organization (the "published" tab).
	ListPublishedApps(ctx context.Context, orgID int64) ([]App, error)
	// CreateApp publishes a manual app with its first client secret. Errors: ErrInvalid, ErrLimit (20 per organization).
	CreateApp(ctx context.Context, req CreateAppRequest) (*CreatedApp, error)
	// UpdateApp changes a manual app's name, website, icon, scopes and redirect URIs. Existing
	// grants keep working, narrowed by the new scopes.
	UpdateApp(ctx context.Context, req UpdateAppRequest) (*App, error)
	// DeleteApp deletes a manual app and revokes its grants (ReasonAppDeleted). It returns the app as it was.
	DeleteApp(ctx context.Context, req DeleteAppRequest) (*App, error)
	// ListClientSecrets returns a manual app's secrets without their hashes. ErrNotFound for any other app.
	ListClientSecrets(ctx context.Context, orgID int64, appID string) ([]AppSecret, error)
	// CreateClientSecret adds a secret to a manual app. The plaintext is in the result and nowhere else.
	CreateClientSecret(ctx context.Context, req CreateSecretRequest) (*CreatedSecret, error)
	// DeleteClientSecret removes a secret of a manual app. ErrNotFound if it does not have it.
	DeleteClientSecret(ctx context.Context, req DeleteSecretRequest) error

	// ---- housekeeping

	// Prune deletes what is old enough (section 2.16) whatever the time since the last prune. The
	// Service also prunes by itself, at most once per PruneEvery, from Register and Exchange.
	Prune(ctx context.Context) (PruneResult, error)
}

// ---- dynamic registration

// RegisterRequest is the body of POST /platform/oauth/apps/register (RFC 7591), as received.
// The Service validates every field (section 2.4).
type RegisterRequest struct {
	ClientName   string
	ClientURI    string // https only; stored as the app's Website; never fetched
	LogoURI      string // https only; stored as the app's Icon; never fetched, never returned
	RedirectURIs []string
	// Scope is the space-separated scope parameter; "" means all advertised scopes.
	Scope string
	// TokenEndpointAuthMethod is "" (client_secret_basic), none, client_secret_basic or client_secret_post.
	TokenEndpointAuthMethod string
	// GrantTypes must be a subset of authorization_code and refresh_token; empty means both.
	GrantTypes []string
	// ResponseTypes must be ["code"]; empty means that.
	ResponseTypes []string
}

// RegisteredApp is the result of Register.
type RegisteredApp struct {
	// App is the stored app. Its ID is the client_id.
	App App
	// ClientSecret is the plaintext secret ("sba_..."), shown once. Dynamic apps get one whatever
	// their token_endpoint_auth_method.
	ClientSecret string
	// IssuedAt is the registration time (client_id_issued_at).
	IssuedAt time.Time
	// GrantTypes and ResponseTypes are the validated values of the request, or the defaults.
	GrantTypes    []string
	ResponseTypes []string
}

// ---- authorization

// AuthorizeRequest holds the query parameters of GET /v1/oauth/authorize, as received.
type AuthorizeRequest struct {
	ClientID            string
	ResponseType        string
	RedirectURI         string
	Scope               string // space separated; "" means the app's scopes
	State               string
	ResponseMode        string
	CodeChallenge       string
	CodeChallengeMethod string
	OrganizationSlug    string
	TargetFlow          string // ignored
	Resource            string
}

// AuthorizeResult is what a stored authorization request leads to.
type AuthorizeResult struct {
	// AuthID is the id of the pending authorization.
	AuthID string
	// ConsentURL is where to send the browser (302): ConsentURL(DashboardURL, AuthID, organization_slug).
	ConsentURL string
}

// AuthorizationView is what Studio's consent page shows (GET /platform/oauth/authorizations/{id}).
type AuthorizationView struct {
	Name string
	// Website is the https client_uri, or "".
	Website string
	// Icon is "" for a dynamic app: a self-asserted logo is never shown.
	Icon string
	// Domain is the host name of the requested redirect URI.
	Domain      string
	RedirectURI string
	ExpiresAt   time.Time
	// ApprovedAt and ApprovedOrgSlug are set once the request is approved.
	ApprovedAt      *time.Time
	ApprovedOrgSlug string
	// Scopes are the effective scopes of the request.
	Scopes           []string
	RegistrationType string
}

// ApproveRequest names the approval. The caller has authenticated UserID and authorized the
// approval in OrgID.
type ApproveRequest struct {
	AuthID string
	UserID string
	OrgID  int64
	// OrgSlug is the {slug} of the route. It must equal the request's organization_slug hint when
	// the request had one (ErrOrgMismatch).
	OrgSlug string
}

// ApproveResult carries the URL that delivers the code.
type ApproveResult struct {
	// RedirectURL is <redirect_uri>?code=<code>&state=<state>&iss=<issuer>; state is left out when
	// the request had none, and an existing query of the redirect URI is kept (AppendQuery). It
	// contains the only copy of the code: the caller returns it and logs nothing of it.
	RedirectURL string
}

// DeclineRequest names a refusal.
type DeclineRequest struct {
	AuthID string
	UserID string
}

// ---- tokens

// TokenRequest is the form body of POST /v1/oauth/token plus the client credentials the caller
// extracted from an Authorization: Basic header (URL-decoded per RFC 6749 section 2.3.1) or from
// the body. The caller rejects a request that sends credentials in both places with different
// values, and a body that is not application/x-www-form-urlencoded, with invalid_request.
type TokenRequest struct {
	GrantType    string
	ClientID     string
	ClientSecret string
	// authorization_code
	Code         string
	CodeVerifier string
	RedirectURI  string
	// refresh_token
	RefreshToken string
	Scope        string // space separated; must be a subset of the grant's scopes
	// Resource is optional on both grants (RFC 8707) and must equal the stored resource when sent.
	Resource string
	// Assertion belongs to the refused jwt-bearer grant and is ignored.
	Assertion string
}

// TokenResponse is the 200 body of the token endpoint. The caller sends Cache-Control: no-store.
type TokenResponse struct {
	AccessToken  string
	TokenType    string // "Bearer"
	ExpiresIn    int    // seconds: AccessTokenTTL
	RefreshToken string
	// Scope is the space-separated effective scopes of the grant.
	Scope string
}

// RevokeRequest is POST /v1/oauth/revoke, from the JSON form of the spec or the RFC 7009 form.
type RevokeRequest struct {
	ClientID     string
	ClientSecret string
	// Token is the refresh token or the access token; TokenTypeHint is only a hint.
	Token         string
	TokenTypeHint string
}

// ---- grants and apps

// RevokeAppRequest asks to revoke an app's live grants.
type RevokeAppRequest struct {
	AppID string
	// OrgID limits the revocation to one organization; 0 means every organization.
	OrgID  int64
	Reason string
	Actor  string
}

// RevokedApp is the app a RevokeApp call ended.
type RevokedApp struct {
	App App
	// AuthorizedAt is the creation time of the newest live grant before the call, in the scope of
	// the call; zero if there was none.
	AuthorizedAt time.Time
	// Revoked is the number of grants revoked.
	Revoked int
}

// CreateAppRequest publishes a manual app. The caller has authorized the request in OrgID.
type CreateAppRequest struct {
	OrgID        int64
	CreatedBy    string
	Name         string
	Website      string
	Icon         string
	Scopes       []string // any of the 24 scopes
	RedirectURIs []string
}

// CreatedApp is the result of CreateApp.
type CreatedApp struct {
	App App
	// Secret is the first client secret's record (alias, no hash); ClientSecret is its plaintext, shown once.
	Secret       AppSecret
	ClientSecret string
}

// UpdateAppRequest replaces the editable fields of a manual app.
type UpdateAppRequest struct {
	OrgID        int64
	AppID        string
	Actor        string
	Name         string
	Website      string
	Icon         string
	Scopes       []string
	RedirectURIs []string
}

// DeleteAppRequest deletes a manual app.
type DeleteAppRequest struct {
	OrgID int64
	AppID string
	Actor string
}

// CreateSecretRequest adds a client secret to a manual app.
type CreateSecretRequest struct {
	OrgID     int64
	AppID     string
	CreatedBy string
}

// CreatedSecret is the result of CreateClientSecret.
type CreatedSecret struct {
	// Secret is the record (alias, no hash); ClientSecret is the plaintext, shown once.
	Secret       AppSecret
	ClientSecret string
}

// DeleteSecretRequest removes a client secret.
type DeleteSecretRequest struct {
	OrgID    int64
	AppID    string
	SecretID string
	Actor    string
}
