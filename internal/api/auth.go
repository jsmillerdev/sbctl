package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
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

// touchEvery limits last_used_at writes per token.
const touchEvery = time.Minute

type authenticator struct {
	reg   registry.Registry
	keys  func(ctx context.Context, ref string) (*secrets.ProjectKeys, error)
	store Store
	now   func() time.Time

	mu        sync.Mutex
	secret    string
	secretAt  time.Time
	touchedAt map[int64]time.Time
	seenAt    map[string]time.Time // dashboard users by id: last time their row was refreshed
}

func newAuthenticator(reg registry.Registry, keys func(context.Context, string) (*secrets.ProjectKeys, error), store Store, now func() time.Time) *authenticator {
	return &authenticator{reg: reg, keys: keys, store: store, now: now, touchedAt: map[int64]time.Time{}, seenAt: map[string]time.Time{}}
}

// systemSecret returns the HS256 secret of sb-gotrue@system, cached briefly.
func (a *authenticator) systemSecret(ctx context.Context, fresh bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !fresh && a.secret != "" && a.now().Sub(a.secretAt) < jwtSecretTTL {
		return a.secret, nil
	}
	k, err := a.keys(ctx, config.SystemRef)
	if err != nil {
		return "", err
	}
	a.secret, a.secretAt = k.JWTSecret, a.now()
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
	var claims map[string]any
	for _, fresh := range []bool{false, true} {
		secret, err := a.systemSecret(ctx, fresh)
		if err != nil {
			return nil, err
		}
		c, err := secrets.ParseHS256(token, secret)
		if err == nil {
			claims = c
			break
		}
	}
	if claims == nil {
		return nil, errUnauthorized
	}
	// Dashboard sessions are GoTrue access tokens of signed-in users. API keys of
	// projects are signed with other secrets, and anonymous sign-ins are not users.
	if role, _ := claims["role"].(string); role != "authenticated" {
		return nil, errUnauthorized
	}
	if anon, _ := claims["is_anonymous"].(bool); anon {
		return nil, errUnauthorized
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil, errUnauthorized
	}
	email, _ := claims["email"].(string)
	p := &Principal{UserID: sub, Email: email, Via: "jwt"}
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
