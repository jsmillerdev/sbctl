package oauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/supavise/supavise/internal/secrets"
)

// ---- client authentication

func invalidClient() *Error {
	return NewError(CodeInvalidClient, "client authentication failed")
}

func invalidGrant(description string) *Error { return NewError(CodeInvalidGrant, description) }

// clientAuth is the client of a token or revocation request.
type clientAuth struct {
	app *App
	// bySecret is true when the request carried a correct client secret.
	bySecret bool
}

// authenticateClient finds the app and, if the request carries a secret, checks it. An unknown or
// deleted app and a wrong secret are the same invalid_client. A request with no secret is returned
// with bySecret false: whether that is acceptable depends on the grant (publicClientOK).
func (s *Service) authenticateClient(ctx context.Context, clientID, secret string) (*clientAuth, error) {
	if clientID == "" {
		return nil, NewError(CodeInvalidRequest, "client_id is required")
	}
	id, ok := canonUUID(clientID)
	if !ok {
		return nil, invalidClient()
	}
	app, err := s.Store.GetApp(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, invalidClient()
	}
	if err != nil {
		return nil, s.serverError(ctx, "client authentication: get app", err)
	}
	if secret == "" {
		return &clientAuth{app: app}, nil
	}
	matched, err := s.checkSecret(ctx, app, secret)
	if err != nil {
		return nil, s.serverError(ctx, "client authentication: secrets", err)
	}
	if !matched {
		return nil, invalidClient()
	}
	return &clientAuth{app: app, bySecret: true}, nil
}

// checkSecret compares the SHA-256 of a presented secret with the stored hashes of the app, in
// constant time and without stopping at the first match. A match records last_used_at, at most once a
// minute.
func (s *Service) checkSecret(ctx context.Context, app *App, secret string) (bool, error) {
	if !secrets.IsClientSecret(secret) {
		return false, nil // not the shape of any secret, so it matches none
	}
	list, err := s.Store.ListSecrets(ctx, app.ID)
	if err != nil {
		return false, err
	}
	h := secrets.HashToken(secret)
	match := -1
	for i := range list {
		if subtle.ConstantTimeCompare(h, list[i].Hash) == 1 {
			match = i
		}
	}
	if match < 0 {
		return false, nil
	}
	now := s.now()
	if m := list[match]; m.LastUsedAt == nil || now.Sub(*m.LastUsedAt) >= time.Minute {
		if err := s.Store.TouchSecret(ctx, m.ID, now); err != nil {
			s.log().WarnContext(ctx, "oauth: client secret use was not recorded", "app_id", app.ID, "error", err)
		}
	}
	return true, nil
}

// publicClientOK reports whether a client that presented no secret may take this grant. A manual app
// never may. A dynamic app may redeem a code (the PKCE verifier is checked against the challenge its
// authorization request had to carry) and, when it registered with token_endpoint_auth_method none,
// refresh (the token rotates and a reuse revokes the grant). A presented secret must always be
// correct; that is checked before this.
func publicClientOK(app *App, grantType string) bool {
	if app.RegistrationType != RegistrationDynamic {
		return false
	}
	switch grantType {
	case GrantTypeAuthorizationCode:
		return true
	case GrantTypeRefreshToken:
		return app.TokenEndpointAuthMethod == AuthMethodNone
	}
	return false
}

// ---- the token endpoint

// Exchange serves POST /v1/oauth/token for the authorization_code and refresh_token grants (section
// 2.7). It authenticates the client, then redeems the code or rotates the refresh token. Every error
// is an *Error. The failure limiter per client address is the caller's.
func (s *Service) Exchange(ctx context.Context, req TokenRequest) (*TokenResponse, error) {
	s.maybePrune(ctx)
	switch req.GrantType {
	case "":
		return nil, NewError(CodeInvalidRequest, "grant_type is required")
	case GrantTypeAuthorizationCode, GrantTypeRefreshToken:
	default: // including the jwt-bearer grant the pinned spec names
		return nil, NewError(CodeUnsupportedGrantType, "grant_type is not supported")
	}
	ca, err := s.authenticateClient(ctx, req.ClientID, req.ClientSecret)
	if err != nil {
		return nil, err
	}
	if !ca.bySecret && !publicClientOK(ca.app, req.GrantType) {
		return nil, invalidClient()
	}
	if req.GrantType == GrantTypeAuthorizationCode {
		return s.exchangeCode(ctx, ca, req)
	}
	return s.exchangeRefresh(ctx, ca, req)
}

// issuedToken is a new token: the plaintext for the response and the row for the Store.
type issuedToken struct {
	plain string
	row   Token
}

func newAccessToken(now time.Time) issuedToken {
	p := secrets.NewOAuthAccessToken()
	return issuedToken{p, Token{Kind: KindAccess, Hash: secrets.HashToken(p), Prefix: secrets.TokenPrefix(p), CreatedAt: now, ExpiresAt: now.Add(AccessTokenTTL)}}
}

func newRefreshToken(now time.Time) issuedToken {
	p := secrets.NewOAuthRefreshToken()
	return issuedToken{p, Token{Kind: KindRefresh, Hash: secrets.HashToken(p), Prefix: secrets.TokenPrefix(p), CreatedAt: now, ExpiresAt: now.Add(RefreshTokenTTL)}}
}

func tokenResponse(access, refresh issuedToken, scopes []string) *TokenResponse {
	return &TokenResponse{
		AccessToken: access.plain, TokenType: "Bearer", ExpiresIn: int(AccessTokenTTL / time.Second),
		RefreshToken: refresh.plain, Scope: JoinScopes(scopes),
	}
}

// exchangeCode redeems an authorization code (section 2.6).
//
// Whether the user may still hold a grant (Admit) is asked between two transactions, not inside
// one: the call reaches into the registry's membership tables, and a transaction that waits for a second
// pooled connection while holding the first could stall the whole pool. The first WithCode only reads
// the approver and the organization (they never change after approval); the second one repeats every
// check under the lock and decides.
func (s *Service) exchangeCode(ctx context.Context, ca *clientAuth, req TokenRequest) (*TokenResponse, error) {
	app := ca.app
	if req.Code == "" {
		return nil, NewError(CodeInvalidRequest, "code is required")
	}
	if req.RedirectURI == "" {
		return nil, NewError(CodeInvalidRequest, "redirect_uri is required")
	}
	if !secrets.IsAuthCode(req.Code) {
		return nil, invalidGrant("the authorization code is invalid, expired or already used")
	}
	hash := secrets.HashToken(req.Code)

	var peek Authorization
	err := s.Store.WithCode(ctx, app.ID, hash, func(_ context.Context, tx CodeTx) error {
		peek = tx.Authorization()
		return nil
	})
	if errors.Is(err, ErrNotFound) { // no such code for this client; nothing changed
		return nil, invalidGrant("the authorization code is invalid, expired or already used")
	}
	if err != nil {
		return nil, s.serverError(ctx, "exchange code: read", err)
	}
	var admitErr error
	admitted := false
	if peek.Status == StatusApproved && peek.OrgID > 0 && peek.CodeExpiresAt != nil && peek.CodeExpiresAt.After(s.now()) {
		admitted, admitErr = true, s.admit(ctx, peek.DecidedBy, peek.OrgID)
	}

	var (
		resp     *TokenResponse
		fail     error
		replayed bool
		auth     Authorization
		created  *CompleteResult
	)
	err = s.Store.WithCode(ctx, app.ID, hash, func(ctx context.Context, tx CodeTx) error {
		a := tx.Authorization()
		auth = a
		now := s.now()
		// burn spends the code and fails the request: a redemption that does not match is never retried.
		burn := func(e error) error { fail = e; return tx.Burn(ctx, now) }
		switch {
		case a.Status == StatusExchanged:
			replayed, fail = true, invalidGrant("the authorization code is invalid, expired or already used")
			return nil
		case a.Status != StatusApproved, a.CodeExpiresAt == nil, !a.CodeExpiresAt.After(now):
			fail = invalidGrant("the authorization code is invalid, expired or already used")
			return nil
		}
		if a.RedirectURI != req.RedirectURI {
			return burn(invalidGrant("redirect_uri does not match the authorization request"))
		}
		if req.Resource != "" {
			if res, ok := s.resolveResource(req.Resource); !ok || res != a.Resource {
				return burn(NewError(CodeInvalidTarget, "resource does not match the authorization request"))
			}
		}
		if a.CodeChallenge == "" {
			if req.CodeVerifier != "" {
				return burn(invalidGrant("code_verifier was sent but the authorization request had no code_challenge"))
			}
			if !ca.bySecret { // a client without a secret must have proved itself with PKCE
				return burn(invalidClient())
			}
		} else {
			if req.CodeVerifier == "" {
				fail = NewError(CodeInvalidRequest, "code_verifier is required")
				return nil
			}
			if !verifyPKCE(a.CodeChallenge, req.CodeVerifier) {
				return burn(invalidGrant("code_verifier does not match the code_challenge"))
			}
		}
		if !admitted || a.DecidedBy != peek.DecidedBy || a.OrgID != peek.OrgID {
			fail = invalidGrant("the authorization code is invalid, expired or already used")
			return nil
		}
		switch {
		case admitErr == nil:
		case errors.Is(admitErr, ErrNotAdmitted), errors.Is(admitErr, ErrNotMember):
			return burn(invalidGrant("the user may no longer authorize this client"))
		default: // could not find out: the code stays valid for a retry
			fail = s.serverError(ctx, "exchange code: admit", admitErr)
			return nil
		}
		scopes := NormalizeScopes(IntersectScopes(a.Scopes, app.Scopes))
		if len(scopes) == 0 {
			return burn(invalidGrant("the client no longer holds the approved scopes"))
		}
		access, refresh := newAccessToken(now), newRefreshToken(now)
		res, err := tx.Complete(ctx, NewGrant{
			Grant:  Grant{AppID: app.ID, UserID: a.DecidedBy, OrgID: a.OrgID, Scopes: scopes, Resource: a.Resource, CreatedAt: now},
			Access: access.row, Refresh: refresh.row, MaxLive: MaxLiveGrantsPerUserOrg, At: now,
		})
		if errors.Is(err, ErrAlreadyDecided) {
			fail = invalidGrant("the authorization code is invalid, expired or already used")
			return nil
		}
		if err != nil {
			return err
		}
		created, resp = res, tokenResponse(access, refresh, scopes)
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return nil, invalidGrant("the authorization code is invalid, expired or already used")
	}
	if err != nil {
		return nil, s.serverError(ctx, "exchange code: redeem", err)
	}
	if replayed {
		s.codeReplayed(ctx, app, auth)
	}
	if fail != nil {
		return nil, fail
	}
	s.audit(ctx, EventGrantCreated, map[string]any{
		"grant_id": created.Grant.ID, "app_id": app.ID, "app_name": app.Name, "user_id": created.Grant.UserID,
		"org_id": created.Grant.OrgID, "org_slug": auth.OrgSlug, "scopes": created.Grant.Scopes,
		"resource": created.Grant.Resource, "redirect_host": redirectHost(auth.RedirectURI),
	})
	s.auditRevoked(ctx, created.Superseded, ReasonSuperseded, ActorSystem, map[string]string{app.ID: app.Name}, map[int64]string{auth.OrgID: auth.OrgSlug})
	return resp, nil
}

// codeReplayed handles a code presented after it was redeemed: the grant it created is revoked, an
// audit event is written and the operator is alerted. A code that was only burned (it created no grant)
// is a plain invalid_grant. It runs detached from the request, because a client that hangs up must not
// leave the grant alive.
func (s *Service) codeReplayed(ctx context.Context, app *App, a Authorization) {
	if a.GrantID == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	revoked, err := s.revoke(ctx, GrantFilter{ID: a.GrantID}, ReasonCodeReuse, ActorSystem)
	if err != nil {
		s.log().ErrorContext(ctx, "oauth: the grant of a replayed code was not revoked", "grant_id", a.GrantID, "error", err)
	} else if len(revoked) == 0 {
		return // the grant was gone already; a further replay of the code is not news
	}
	s.audit(ctx, EventCodeReuse, map[string]any{
		"grant_id": a.GrantID, "app_id": app.ID, "app_name": app.Name, "user_id": a.DecidedBy,
		"org_id": a.OrgID, "org_slug": a.OrgSlug, "revoked": err == nil,
	})
	s.alert(ctx, AlertEvent{
		Kind: AlertKindTokenReuse, Key: AlertKindTokenReuse + "/" + strconv.FormatInt(a.GrantID, 10),
		Title:  "OAuth authorization code replayed",
		Detail: fmt.Sprintf("An authorization code of the OAuth client %q was presented a second time. The grant it created (id %d) was revoked.", app.Name, a.GrantID),
	})
}

// exchangeRefresh rotates a refresh token (section 2.7). As in exchangeCode, Admit is asked before the
// transaction, from the grant the token belongs to.
func (s *Service) exchangeRefresh(ctx context.Context, ca *clientAuth, req TokenRequest) (*TokenResponse, error) {
	app := ca.app
	if req.RefreshToken == "" {
		return nil, NewError(CodeInvalidRequest, "refresh_token is required")
	}
	bad := func() error { return invalidGrant("the refresh token is invalid, expired or revoked") }
	if !secrets.IsOAuthRefreshToken(req.RefreshToken) {
		return nil, bad()
	}
	hash := secrets.HashToken(req.RefreshToken)
	now := s.now()

	tok, err := s.Store.GetToken(ctx, hash)
	if errors.Is(err, ErrNotFound) {
		return nil, bad()
	}
	if err != nil {
		return nil, s.serverError(ctx, "refresh: get token", err)
	}
	if tok.Kind != KindRefresh || !tok.ExpiresAt.After(now) {
		return nil, bad()
	}
	grant, err := s.Store.GetGrant(ctx, tok.GrantID)
	if errors.Is(err, ErrNotFound) {
		return nil, bad()
	}
	if err != nil {
		return nil, s.serverError(ctx, "refresh: get grant", err)
	}
	if grant.AppID != app.ID || grant.RevokedAt != nil {
		return nil, bad()
	}
	// A token that was exchanged before the grace window is a replay: RotateRefresh reports it and the
	// grant is revoked below, so there is no point asking whether the user is still admitted.
	if reused := tok.UsedAt != nil && !tok.UsedAt.After(now.Add(-RefreshGrace)); !reused {
		switch err := s.admit(ctx, grant.UserID, grant.OrgID); {
		case err == nil:
		case errors.Is(err, ErrNotMember):
			s.revokeDetached(ctx, grant.ID, ReasonMembership)
			return nil, bad()
		case errors.Is(err, ErrNotAdmitted):
			return nil, bad()
		default:
			return nil, s.serverError(ctx, "refresh: admit", err)
		}
	}

	var resp *TokenResponse
	err = s.Store.RotateRefresh(ctx, RotateInput{AppID: app.ID, TokenHash: hash, Now: now, Grace: RefreshGrace},
		func(ctx context.Context, tx RotateTx) error {
			g := tx.Grant()
			// An error from here rolls the stamp back: a request that asks for the wrong scope or resource
			// does not use up the token.
			if req.Resource != "" {
				if res, ok := s.resolveResource(req.Resource); !ok || res != g.Resource {
					return NewError(CodeInvalidTarget, "resource does not match the grant")
				}
			}
			effective := NormalizeScopes(IntersectScopes(g.Scopes, app.Scopes))
			scopes := effective
			if want := ParseScopes(req.Scope); len(want) > 0 {
				if !SubsetOf(want, effective) {
					return NewError(CodeInvalidScope, "scope must be a subset of the scopes already granted")
				}
				if scopes = NormalizeScopes(want); !slices.Equal(scopes, NormalizeScopes(g.Scopes)) {
					if err := tx.NarrowScopes(ctx, scopes); err != nil {
						return err
					}
				}
			}
			if len(scopes) == 0 {
				return bad()
			}
			access, refresh := newAccessToken(now), newRefreshToken(now)
			if err := tx.Issue(ctx, access.row, refresh.row); err != nil {
				return err
			}
			resp = tokenResponse(access, refresh, scopes)
			return nil
		})
	var (
		reuse *ReuseError
		oerr  *Error
	)
	switch {
	case err == nil:
		return resp, nil
	case errors.As(err, &reuse):
		s.refreshReused(ctx, app, reuse)
		return nil, bad()
	case errors.Is(err, ErrNotFound):
		return nil, bad()
	case errors.As(err, &oerr):
		return nil, oerr
	}
	return nil, s.serverError(ctx, "refresh: rotate", err)
}

// refreshReused handles a refresh token presented after it was rotated and the grace window passed:
// the grant is revoked (the token was stolen, or the client lost its state; either way the line is
// dead), an audit event is written and the operator is alerted once per grant.
func (s *Service) refreshReused(ctx context.Context, app *App, re *ReuseError) {
	ctx = context.WithoutCancel(ctx)
	revoked, err := s.revoke(ctx, GrantFilter{ID: re.GrantID}, ReasonRefreshReuse, ActorSystem)
	if err != nil {
		s.log().ErrorContext(ctx, "oauth: the grant of a reused refresh token was not revoked", "grant_id", re.GrantID, "error", err)
	} else if len(revoked) == 0 {
		return // someone else ended the grant first
	}
	s.audit(ctx, EventRefreshReuse, map[string]any{
		"grant_id": re.GrantID, "app_id": app.ID, "app_name": app.Name, "user_id": re.UserID,
		"org_id": re.OrgID, "revoked": err == nil,
	})
	s.alert(ctx, AlertEvent{
		Kind: AlertKindTokenReuse, Key: AlertKindTokenReuse + "/" + strconv.FormatInt(re.GrantID, 10),
		Title:  "OAuth refresh token reused",
		Detail: fmt.Sprintf("A refresh token of the OAuth client %q was presented again after it had been exchanged. The grant (id %d) was revoked.", app.Name, re.GrantID),
	})
}

// ---- the resource side

// LookupAccess resolves an access token to what it stands for. ErrNotFound for every kind of unusable
// token (a string of the wrong shape, unknown, expired, revoked, a deleted app), so the caller cannot
// tell them apart. Scopes is the intersection of the grant's and the app's current scopes. It does not
// run Admit: the caller applies the user checks it applies to every principal.
func (s *Service) LookupAccess(ctx context.Context, token string) (*AccessInfo, error) {
	if !secrets.IsOAuthAccessToken(token) {
		return nil, ErrNotFound
	}
	info, err := s.Store.LookupAccess(ctx, secrets.HashToken(token), s.now())
	if err != nil {
		return nil, err
	}
	info.Scopes = NormalizeScopes(IntersectScopes(info.GrantScopes, info.AppScopes))
	return info, nil
}

// TouchAccess records last_used_at on the token and its grant.
func (s *Service) TouchAccess(ctx context.Context, tokenID, grantID int64) error {
	return s.Store.TouchToken(ctx, tokenID, grantID, s.now())
}
