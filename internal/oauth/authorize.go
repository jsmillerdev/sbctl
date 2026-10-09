package oauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/supavise/supavise/internal/secrets"
)

// StartAuthorization validates an authorization request and stores it as pending (section 2.5). The
// order of the checks is the point: nothing can redirect to a redirect_uri until the client_id and the
// redirect_uri have both been validated, because only then is the address one the app registered.
//
//  1. client_id is a live app: otherwise ErrUnknownClient (a page, no redirect).
//  2. redirect_uri matches a registered URI (redirect.go): otherwise ErrInvalidRedirectURI (a page, no redirect).
//  3. From here every error is a *RedirectError: the size bound, response_type, response_mode, PKCE,
//     scope, resource, organization_slug.
//  4. The caps: pending requests per app and in total are ErrLimit.
func (s *Service) StartAuthorization(ctx context.Context, req AuthorizeRequest) (*AuthorizeResult, error) {
	id, ok := canonUUID(req.ClientID)
	if !ok {
		return nil, ErrUnknownClient
	}
	app, err := s.Store.GetApp(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrUnknownClient
	}
	if err != nil {
		return nil, fmt.Errorf("oauth: authorize: %w", err)
	}
	if !matchRedirectURI(app.RedirectURIs, req.RedirectURI) {
		return nil, ErrInvalidRedirectURI
	}

	fail := func(code, format string, args ...any) error {
		return &RedirectError{RedirectURI: req.RedirectURI, State: req.State, Err: Errorf(code, format, args...)}
	}
	// A state longer than the bound is not echoed: the Location header would carry it.
	if len(req.RedirectURI)+len(req.State) > MaxRedirectAndState {
		return nil, &RedirectError{RedirectURI: req.RedirectURI, Err: NewError(CodeInvalidRequest, "redirect_uri and state are too long")}
	}
	switch req.ResponseType {
	case ResponseTypeCode:
	case "":
		return nil, fail(CodeInvalidRequest, "response_type is required")
	default:
		return nil, fail(CodeUnsupportedResponseType, "response_type must be %q", ResponseTypeCode)
	}
	if req.ResponseMode != "" && req.ResponseMode != ResponseModeQuery {
		return nil, fail(CodeInvalidRequest, "response_mode must be %q", ResponseModeQuery)
	}

	// PKCE: required of dynamic apps, optional for manual ones; S256 only; a challenge needs its method.
	switch {
	case req.CodeChallenge == "" && req.CodeChallengeMethod == "":
		if app.RegistrationType == RegistrationDynamic {
			return nil, fail(CodeInvalidRequest, "code_challenge is required: use PKCE with code_challenge_method S256")
		}
	case req.CodeChallengeMethod != PKCEMethodS256:
		return nil, fail(CodeInvalidRequest, "code_challenge_method must be %s", PKCEMethodS256)
	case !validPKCEString(req.CodeChallenge):
		return nil, fail(CodeInvalidRequest, "code_challenge must be %d to %d characters of A-Z a-z 0-9 - . _ ~", pkceMinLen, pkceMaxLen)
	}

	// Scope: what was asked for (or all the app may hold), narrowed to what the app may hold.
	effective := app.Scopes
	if requested := ParseScopes(req.Scope); len(requested) > 0 {
		effective = IntersectScopes(requested, app.Scopes)
	}
	if effective = NormalizeScopes(effective); len(effective) == 0 {
		return nil, fail(CodeInvalidScope, "none of the requested scopes is available to this client")
	}

	resource := ""
	if req.Resource != "" {
		if resource, ok = s.resolveResource(req.Resource); !ok {
			return nil, fail(CodeInvalidTarget, "resource must be the MCP endpoint of this server")
		}
	}
	if req.OrganizationSlug != "" && !orgSlugRE.MatchString(req.OrganizationSlug) {
		return nil, fail(CodeInvalidRequest, "organization_slug is not valid")
	}

	now := s.now()
	if n, err := s.Store.CountPending(ctx, app.ID, now); err != nil {
		return nil, fmt.Errorf("oauth: authorize: %w", err)
	} else if n >= MaxPendingPerApp {
		return nil, limitf("too many pending authorization requests for this client")
	}
	if n, err := s.Store.CountPending(ctx, "", now); err != nil {
		return nil, fmt.Errorf("oauth: authorize: %w", err)
	} else if n >= MaxPendingTotal {
		return nil, limitf("too many pending authorization requests")
	}

	a := Authorization{
		ID: secrets.NewUUID(), AppID: app.ID, RedirectURI: req.RedirectURI, Scopes: effective, State: req.State,
		CodeChallenge: req.CodeChallenge, Resource: resource, OrgHint: req.OrganizationSlug,
		CreatedAt: now, ExpiresAt: now.Add(AuthorizationTTL), Status: StatusPending,
	}
	if err := s.Store.CreateAuthorization(ctx, a); err != nil {
		return nil, fmt.Errorf("oauth: authorize: %w", err)
	}
	return &AuthorizeResult{AuthID: a.ID, ConsentURL: ConsentURL(s.DashboardURL, a.ID, req.OrganizationSlug)}, nil
}

// authorization loads an authorization and its live app. ErrNotFound for an unknown or malformed id
// and for an app that was deleted since.
func (s *Service) authorization(ctx context.Context, authID string) (*Authorization, *App, error) {
	id, ok := canonUUID(authID)
	if !ok {
		return nil, nil, ErrNotFound
	}
	a, err := s.Store.GetAuthorization(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	app, err := s.Store.GetApp(ctx, a.AppID)
	if err != nil {
		return nil, nil, err
	}
	return a, app, nil
}

// Describe returns what the consent page shows. A dynamic app's logo is never part of it, and its
// website only when it is an https client_uri. The scopes are the ones the grant would hold now: the
// request's, narrowed to the app's current scopes. An expired or decided request is described too;
// the page works out "expired" from ExpiresAt.
func (s *Service) Describe(ctx context.Context, authID string) (*AuthorizationView, error) {
	a, app, err := s.authorization(ctx, authID)
	if err != nil {
		return nil, err
	}
	v := &AuthorizationView{
		Name: app.Name, Website: app.Website, Icon: app.Icon, Domain: redirectHost(a.RedirectURI),
		RedirectURI: a.RedirectURI, ExpiresAt: a.ExpiresAt,
		Scopes: NormalizeScopes(IntersectScopes(a.Scopes, app.Scopes)), RegistrationType: app.RegistrationType,
	}
	if app.RegistrationType == RegistrationDynamic {
		v.Icon = ""
	}
	if a.Status == StatusApproved || a.Status == StatusExchanged {
		v.ApprovedAt, v.ApprovedOrgSlug = a.DecidedAt, a.OrgSlug
	}
	return v, nil
}

// Approve records the approval and returns the URL that delivers the code (section 2.6). The caller
// has checked who may approve and the second factor. The code is 64 hex digits from crypto/rand, valid
// for AuthCodeTTL; only its SHA-256 is stored. One atomic update decides the request, so of two
// concurrent approvals one gets ErrAlreadyDecided.
func (s *Service) Approve(ctx context.Context, req ApproveRequest) (*ApproveResult, error) {
	a, app, err := s.authorization(ctx, req.AuthID)
	if err != nil {
		return nil, err
	}
	if a.OrgHint != "" && a.OrgHint != req.OrgSlug {
		return nil, ErrOrgMismatch
	}
	if req.UserID == "" || req.OrgID <= 0 {
		return nil, invalidf("approval needs a user and an organization")
	}
	now := s.now()
	code := secrets.NewAuthCode()
	decided, err := s.Store.DecideAuthorization(ctx, a.ID, Decision{
		Status: StatusApproved, DecidedBy: req.UserID, At: now, OrgID: req.OrgID,
		CodeHash: secrets.HashToken(code), CodeExpiresAt: now.Add(AuthCodeTTL),
	})
	if err != nil {
		return nil, err
	}
	kv := []string{"code", code}
	if decided.State != "" {
		kv = append(kv, "state", decided.State)
	}
	kv = append(kv, "iss", s.issuer())
	redirect, err := AppendQuery(decided.RedirectURI, kv...)
	if err != nil {
		return nil, fmt.Errorf("oauth: approve: %w", err)
	}
	s.audit(ctx, EventAuthorizationApproved, map[string]any{
		"app_id": app.ID, "app_name": app.Name, "user_id": req.UserID, "org_id": req.OrgID, "org_slug": req.OrgSlug,
		"redirect_host": redirectHost(decided.RedirectURI), "scopes": decided.Scopes,
	})
	return &ApproveResult{RedirectURL: redirect}, nil
}

// Decline records the refusal. The client is not told; it waits for its own timeout.
func (s *Service) Decline(ctx context.Context, req DeclineRequest) error {
	a, app, err := s.authorization(ctx, req.AuthID)
	if err != nil {
		return err
	}
	if req.UserID == "" {
		return invalidf("a refusal needs a user")
	}
	if _, err := s.Store.DecideAuthorization(ctx, a.ID, Decision{Status: StatusDeclined, DecidedBy: req.UserID, At: s.now()}); err != nil {
		return err
	}
	s.audit(ctx, EventAuthorizationDeclined, map[string]any{
		"app_id": app.ID, "app_name": app.Name, "user_id": req.UserID, "redirect_host": redirectHost(a.RedirectURI),
	})
	return nil
}
