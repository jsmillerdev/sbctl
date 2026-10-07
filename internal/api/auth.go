package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// authKind says which credentials a route accepts.
type authKind int

const (
	// authNone: no credentials (the CLI's device-login poll, health).
	authNone authKind = iota
	// authAny: a GoTrue session JWT (Studio) or a personal access token (CLI, MCP).
	// This is every /v1 route: Studio calls some of them with its session.
	authAny
	// authJWT: a GoTrue session JWT only: every /platform route.
	authJWT
)

// patRe is the access-token shape the CLI validates before use
// (apps/cli/src/auth/access-token.ts): sbp_ plus 40 hex, with optional v0_ or oauth_.
var patRe = regexp.MustCompile(`^sbp_(oauth_|v0_)?[a-f0-9]{40}$`)

// jwtSecretTTL bounds how stale a rotated dashboard JWT secret can be.
const jwtSecretTTL = 30 * time.Second

// jwtRefreshEvery is the shortest gap between forced re-reads of the dashboard JWT
// secret. Only a token whose signature fails triggers one, so garbage tokens cost
// at most one registry read per interval.
const jwtRefreshEvery = 5 * time.Second

// AdminClaim is the app_metadata key that marks a dashboard user as allowed to use
// the Management API. supavise sets it (to true) on every user it creates in
// supavise-gotrue@system, and supavise-gotrue@system creates no other users but the ones its
// before-user-created hook allows (registered SSO providers, invited addresses; the daemon
// answers it), so nobody else can obtain a session. The gate is defense in depth. A user whose
// account came from SSO has no such claim and is admitted by DashboardSSO.Admit instead.
const AdminClaim = "supavise_admin"

// touchEvery limits last_used_at writes per token.
const touchEvery = time.Minute

type authenticator struct {
	reg   registry.Registry
	keys  func(ctx context.Context, ref string) (*secrets.ProjectKeys, error)
	store Store
	now   func() time.Time
	// admins is the [api] admin_emails allowlist, lower-cased.
	admins []string
	// removed reports whether a dashboard user was removed with `supavise users remove`
	// (nil: nobody is). It is asked on every request, with no cache, so that a removal
	// ends the user's sessions and tokens at once, whichever process made it.
	removed func(ctx context.Context, userID string) (bool, error)
	// sso admits the session of a user whose account came from SAML single sign-on (see
	// DashboardSSO.Admit). Nil: no SSO session is accepted.
	sso func(ctx context.Context, userID, email, providerID string) error
	// ssoUser is the same check for a personal access token, which carries no session: it looks
	// the owner up among the SSO users and admits them (DashboardSSO.AdmitUser). Nil: no check.
	ssoUser func(ctx context.Context, userID string) error

	mu        sync.Mutex
	secret    string
	secretAt  time.Time
	refreshAt time.Time // last forced re-read of the secret
	touchedAt map[int64]time.Time
	seenAt    map[string]time.Time // dashboard users by id: last time their row was refreshed
}

func newAuthenticator(reg registry.Registry, keys func(context.Context, string) (*secrets.ProjectKeys, error), store Store, now func() time.Time, admins []string) *authenticator {
	return &authenticator{reg: reg, keys: keys, store: store, now: now, admins: admins, touchedAt: map[int64]time.Time{}, seenAt: map[string]time.Time{}}
}

// systemSecret returns the HS256 secret of supavise-gotrue@system, cached briefly.
func (a *authenticator) systemSecret(ctx context.Context, fresh bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if a.secret != "" {
		if !fresh && now.Sub(a.secretAt) < jwtSecretTTL {
			return a.secret, nil
		}
		if fresh && now.Sub(a.refreshAt) < jwtRefreshEvery {
			return a.secret, nil
		}
	}
	if fresh {
		a.refreshAt = now
	}
	k, err := a.keys(ctx, config.SystemRef)
	if err != nil {
		return "", err
	}
	a.secret, a.secretAt = k.JWTSecret, now
	return a.secret, nil
}

// authenticate resolves the caller of r according to kind.
func (a *authenticator) authenticate(r *http.Request, kind authKind) (*Principal, error) {
	if kind == authNone {
		return nil, nil
	}
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return nil, errUnauthorized
	}
	token = strings.TrimSpace(token)
	if strings.HasPrefix(token, secrets.PrefixPAT) {
		if kind != authAny {
			return nil, errUnauthorized
		}
		return a.authPAT(r.Context(), token)
	}
	return a.authJWT(r.Context(), token)
}

func (a *authenticator) authPAT(ctx context.Context, token string) (*Principal, error) {
	if !patRe.MatchString(token) {
		return nil, errUnauthorized
	}
	t, err := a.reg.GetAccessTokenByHash(ctx, secrets.HashToken(token))
	if errors.Is(err, registry.ErrNotFound) {
		return nil, errUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if t.ExpiresAt != nil && !t.ExpiresAt.After(a.now()) {
		return nil, errUnauthorized
	}
	// A token is not tied to its owner's account: one that outlived the removal of its
	// owner must not keep working.
	if gone, err := a.isRemoved(ctx, t.UserID); err != nil {
		return nil, err
	} else if gone {
		return nil, errUnauthorized
	}
	// The owner of a token who signed in through SSO is held to the same rule as their session: a
	// user who waits for approval (their access ended, or they never had any) is refused.
	if a.ssoUser != nil {
		if err := a.ssoUser(ctx, t.UserID); err != nil {
			return nil, err
		}
	}
	a.touch(ctx, t.ID)
	p := &Principal{UserID: t.UserID, Via: "pat", TokenID: t.ID}
	if u, err := a.store.GetUser(ctx, t.UserID); err == nil {
		p.Email = u.Email
	}
	return p, nil
}

func (a *authenticator) touch(ctx context.Context, id int64) {
	now := a.now()
	a.mu.Lock()
	last, seen := a.touchedAt[id]
	if seen && now.Sub(last) < touchEvery {
		a.mu.Unlock()
		return
	}
	a.touchedAt[id] = now
	a.mu.Unlock()
	// Best effort: a failed touch must not fail the request.
	_ = a.reg.TouchAccessToken(ctx, id, now)
}

func (a *authenticator) authJWT(ctx context.Context, token string) (*Principal, error) {
	secret, err := a.systemSecret(ctx, false)
	if err != nil {
		return nil, err
	}
	claims, err := secrets.ParseSessionHS256(token, secret)
	if errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		// Possibly signed with a rotated secret: re-read it (rate limited), once.
		fresh, ferr := a.systemSecret(ctx, true)
		if ferr != nil {
			return nil, ferr
		}
		if fresh != secret {
			claims, err = secrets.ParseSessionHS256(token, fresh)
		}
	}
	if err != nil {
		return nil, errUnauthorized
	}
	// Dashboard sessions are GoTrue access tokens of signed-in users: signed with the
	// system project's secret and issued for the audience "authenticated". The role
	// claim is not checked: users created through GoTrue's admin API have an empty
	// auth.users.role, so their tokens carry role "" (research/08 section 9). The
	// supavise-minted system keys (role anon or service_role) have no audience and fail
	// here, API keys of projects are signed with other secrets, and anonymous sign-ins
	// are not users.
	if !hasAudience(claims["aud"], "authenticated") {
		return nil, errUnauthorized
	}
	if role, _ := claims["role"].(string); role == "anon" || role == "service_role" {
		return nil, errUnauthorized // never a GoTrue user session
	}
	if anon, _ := claims["is_anonymous"].(bool); anon {
		return nil, errUnauthorized
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil, errUnauthorized
	}
	// A session issued before the user was removed stays valid until it expires; refuse it now.
	if gone, err := a.isRemoved(ctx, sub); err != nil {
		return nil, err
	} else if gone {
		return nil, errUnauthorized
	}
	email, _ := claims["email"].(string)
	sso := false
	if prov := ssoProviderOf(claims); prov != "" {
		// A user who signed in through an identity provider has no supavise_admin claim: the
		// provider being registered and the user belonging to an organization (or getting the
		// default role of their email domain on this first request) is what admits them.
		if a.sso == nil {
			return nil, errForbidden
		}
		if err := a.sso(ctx, sub, email, prov); err != nil {
			return nil, err
		}
		sso = true
	} else if !a.isAdmin(claims, email) {
		return nil, errForbidden
	}
	aal, _ := claims["aal"].(string)
	if sso {
		// A session that the company's identity provider vouched for meets an organization's
		// "require MFA": the provider is where strong authentication is enforced, and GoTrue's
		// token for an SSO sign-in is aal1 whatever the provider did, so without this the
		// switch would lock out every SSO user.
		aal = "aal2"
	}
	p := &Principal{UserID: sub, Email: email, Via: "jwt", AAL: aal}
	// Record the user on first sight, then at most once a minute: every dashboard
	// request carries a JWT and none of them should write to the database.
	now := a.now()
	a.mu.Lock()
	last, seen := a.seenAt[sub]
	a.mu.Unlock()
	if !seen || now.Sub(last) >= touchEvery {
		meta, _ := claims["user_metadata"].(map[string]any)
		if _, err := a.store.UpsertUser(ctx, userFromClaims(sub, email, meta)); err != nil {
			return nil, err
		}
		a.mu.Lock()
		a.seenAt[sub] = now
		a.mu.Unlock()
	}
	return p, nil
}

func (a *authenticator) isRemoved(ctx context.Context, userID string) (bool, error) {
	if a.removed == nil {
		return false, nil
	}
	return a.removed(ctx, userID)
}

// isAdmin reports whether a verified dashboard session may use the API: the user
// carries app_metadata.supavise_admin = true, or the email is on the [api]
// admin_emails allowlist. GoTrue lets users edit user_metadata but not app_metadata.
func (a *authenticator) isAdmin(claims map[string]any, email string) bool {
	if app, _ := claims["app_metadata"].(map[string]any); app != nil {
		if v, _ := app[AdminClaim].(bool); v {
			return true
		}
	}
	email = strings.ToLower(email)
	for _, e := range a.admins {
		if e == email && email != "" {
			return true
		}
	}
	return false
}

// userFromClaims derives the profile fields of a first-seen user from GoTrue claims.
func userFromClaims(sub, email string, meta map[string]any) User {
	u := User{UserID: sub, Email: email}
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, _ := meta[k].(string); s != "" {
				return s
			}
		}
		return ""
	}
	u.FirstName = str("first_name", "given_name")
	u.LastName = str("last_name", "family_name")
	if u.FirstName == "" && u.LastName == "" {
		if full := str("full_name", "name"); full != "" {
			u.FirstName, u.LastName, _ = strings.Cut(full, " ")
		}
	}
	u.Username = str("username", "user_name", "preferred_username")
	if u.Username == "" {
		u.Username, _, _ = strings.Cut(email, "@")
	}
	return u
}

// hasAudience reports whether the aud claim (a string, or a list of strings) names want.
func hasAudience(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, a := range v {
			if s, _ := a.(string); s == want {
				return true
			}
		}
	}
	return false
}
