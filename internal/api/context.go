package api

import (
	"context"

	"github.com/supavise/supavise/internal/members"
)

type ctxKey int

const (
	ctxPrincipal ctxKey = iota
	ctxRequestID
)

// Principal is the authenticated caller.
type Principal struct {
	UserID string // GoTrue user uuid
	Email  string
	// Via is "jwt" (dashboard session), "pat" (personal access token) or "oauth" (OAuth access
	// token). A token ("pat", "oauth") is not an interactive session: it carries no AAL and is not held
	// to an organization's MFA requirement.
	Via string
	// TokenID is the access token row for Via == "pat".
	TokenID int64
	// AAL is the authenticator assurance level of a dashboard session ("aal1", "aal2");
	// empty for a personal access token and an OAuth access token.
	AAL string
	// OAuth is what an OAuth access token stands for; nil unless Via == "oauth".
	OAuth *OAuthInfo

	// access is what the user may do, loaded on first use during the request. For an OAuth
	// principal authenticate has loaded it already, restricted to the grant's organization.
	access *members.Access
}

// OAuthInfo is the grant behind an OAuth access token (authOAuth, oauth_authn.go).
type OAuthInfo struct {
	// GrantID, AppID and AppName identify the grant and the client app it was given to.
	GrantID int64
	AppID   string
	AppName string
	// TokenID is the oauth_tokens row of the access token.
	TokenID int64
	// OrgID and OrgSlug are the one organization the grant is bound to.
	OrgID   int64
	OrgSlug string
	// Scopes are the scopes the token may use: the grant's intersected with the app's current ones.
	Scopes []string
	// Resource is the resource the grant is bound to (RFC 8707), "" when none; the MCP gate holds a
	// token to it.
	Resource string
}

func withPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxPrincipal, p)
}

// principalFrom returns the caller of a request that passed authentication.
func principalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxPrincipal).(*Principal)
	return p
}
