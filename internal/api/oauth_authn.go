package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/oauth"
)

// Authentication of OAuth access tokens ("sbp_oauth_" plus 40 hex, issued by the token endpoint).
//
// An OAuth token is a new way to obtain an API credential, so it is held to more than a personal
// access token is:
//
//   - It lives in the OAuth tables and never in access_tokens. authenticate hands it to authOAuth
//     before the personal-access-token branch can see it, so a token that is unknown to the OAuth
//     tables is a 401 and is never looked up as a PAT.
//   - It works on authAny routes (/v1, /v2 and the routes no spec lists) only. A /platform route, which is
//     the dashboard's, answers 401 before the token is even looked up.
//   - It is bound to one organization: the principal's Access is the user's live Access restricted to
//     the grant's organization (members.Access.Restrict), so every list and permission check sees that
//     organization alone, and a role change applies on the next request.
//   - Each request re-checks the user (not removed, still admitted by single sign-on, still a member of
//     the organization). A user who left the organization loses the grant.
//   - What it may call is a second, independent gate: the scopes of the grant against the scope the
//     spec annotates the operation with (oauth_scopes.go). The permission rules of authz.go run after
//     that gate, so a token is never more than its user.

// oauthTokenRe is the shape of an access token the token endpoint issues. A token of another shape
// cannot be one of ours and is refused before any lookup.
var oauthTokenRe = regexp.MustCompile(`^` + regexp.QuoteMeta(oauth.AccessTokenPrefix) + `[a-f0-9]{40}$`)

// oauthTouchMax bounds the memory of the last_used_at throttle: past it, entries older than the
// throttle are dropped.
const oauthTouchMax = 4096

// oauthAuthn is what the authenticator needs to resolve an OAuth access token. Server builds it
// (newOAuthAuthn) and hands it to authenticator.oauth; an authenticator without one refuses every
// OAuth token.
type oauthAuthn struct {
	// svc is the OAuth service as it is now: Server.oauth is read on every request so that a test can
	// replace it.
	svc func() oauth.Authority
	// disabled reports [api] disable_oauth. While it is set an OAuth token is as unknown as it is to a
	// release without OAuth: 401.
	disabled func() bool
	// access loads the memberships of a user (members.Service.Access).
	access func(ctx context.Context, userID string) (*members.Access, error)
	log    *slog.Logger

	mu        sync.Mutex
	touchedAt map[int64]time.Time // token id -> last last_used_at write
}

// newOAuthAuthn wires the OAuth side of the authenticator to the server.
func newOAuthAuthn(s *Server) *oauthAuthn {
	return &oauthAuthn{
		svc:      func() oauth.Authority { return s.oauth },
		disabled: s.oauthDisabled,
		access: func(ctx context.Context, userID string) (*members.Access, error) {
			return s.members.Access(ctx, userID)
		},
		log:       s.log.With("component", "oauth"),
		touchedAt: map[int64]time.Time{},
	}
}

// authOAuth resolves an OAuth access token to its principal. Every refusal is errUnauthorized, so a
// caller learns nothing about why a token does not work; a failure to find out (the registry is
// unreachable) is returned as it is and becomes a 500.
func (a *authenticator) authOAuth(ctx context.Context, token string) (*Principal, error) {
	o := a.oauth
	if o == nil || o.disabled() || !oauthTokenRe.MatchString(token) {
		return nil, errUnauthorized
	}
	svc := o.svc()
	if svc == nil {
		return nil, errUnauthorized
	}
	info, err := svc.LookupAccess(ctx, token)
	if errors.Is(err, oauth.ErrNotFound) {
		return nil, errUnauthorized // unknown, expired, revoked, or its app was deleted
	}
	if err != nil {
		return nil, err
	}
	if info == nil || info.UserID == "" || info.OrgID == 0 {
		return nil, errUnauthorized
	}
	// The lookup filters on expiry already; this is the same check on the server's clock, in case a
	// store or a cache answers late.
	if !info.ExpiresAt.IsZero() && !info.ExpiresAt.After(a.now()) {
		return nil, errUnauthorized
	}
	// A grant is not tied to its user's account: one that outlived the removal of the user must not
	// keep working.
	if gone, err := a.isRemoved(ctx, info.UserID); err != nil {
		return nil, err
	} else if gone {
		return nil, errUnauthorized
	}
	// A user who signed in through SSO is held to the same rule as their session. The refusal is a 401
	// here, not the 403 of a session: for a token it means "this credential no longer works", which is
	// what makes a client ask for a new one.
	if a.ssoUser != nil {
		if err := a.ssoUser(ctx, info.UserID); err != nil {
			if asError(err).Status < http.StatusInternalServerError {
				return nil, errUnauthorized
			}
			return nil, err
		}
	}
	// Rights are the user's rights now, in the grant's organization and no other.
	full, err := o.access(ctx, info.UserID)
	if err != nil {
		return nil, err
	}
	access := full.Restrict(info.OrgID)
	if !access.IsMember(info.OrgID) {
		// The user left the organization. The grant is dead: end it, so that it also leaves the
		// organization's Authorized list and a refresh fails the same way.
		if err := svc.RevokeGrant(context.WithoutCancel(ctx), info.GrantID, oauth.ReasonMembership, oauth.ActorSystem); err != nil && !errors.Is(err, oauth.ErrNotFound) {
			o.log.Warn("a grant of a user who left the organization was not revoked", "grant", info.GrantID, "error", err)
		}
		return nil, errUnauthorized
	}
	o.touch(ctx, svc, a.now(), info)
	p := &Principal{
		UserID: info.UserID,
		Via:    "oauth",
		access: access,
		OAuth: &OAuthInfo{
			GrantID: info.GrantID, AppID: info.AppID, AppName: info.AppName, TokenID: info.TokenID,
			OrgID: info.OrgID, OrgSlug: info.OrgSlug, Scopes: slices.Clone(info.Scopes), Resource: info.Resource,
		},
	}
	// The address is loaded when a handler asks for it (Principal.UserEmail): of the routes an OAuth token
	// can reach (not /platform), only GET /v1/profile reads it, and only for a user the store lacks.
	p.loadEmail = func() string {
		if u, err := a.store.GetUser(ctx, info.UserID); err == nil {
			return u.Email
		}
		return ""
	}
	return p, nil
}

// touch records last_used_at on the token and its grant, at most once a minute per token. A
// failure is not the request's.
func (o *oauthAuthn) touch(ctx context.Context, svc oauth.Authority, now time.Time, info *oauth.AccessInfo) {
	o.mu.Lock()
	if last, seen := o.touchedAt[info.TokenID]; seen && now.Sub(last) < touchEvery {
		o.mu.Unlock()
		return
	}
	if len(o.touchedAt) >= oauthTouchMax {
		for id, at := range o.touchedAt {
			if now.Sub(at) >= touchEvery {
				delete(o.touchedAt, id)
			}
		}
	}
	o.touchedAt[info.TokenID] = now
	o.mu.Unlock()
	_ = svc.TouchAccess(context.WithoutCancel(ctx), info.TokenID, info.GrantID)
}
