package oauth

import (
	"context"
	"errors"
	"sort"

	"github.com/supavise/supavise/internal/secrets"
)

// Every way a grant ends goes through revoke: it reads the grants it will end (their apps and
// organizations name them in the audit trail), ends them, and writes one oauth.grant_revoked event
// for each. The lookup of access tokens filters on revoked_at, so a revoked grant stops working at once.

// revokeInfos ends the live grants f selects. It returns the grants as they were before the call
// (newest first, with their apps and organizations) and as the Store updated them.
func (s *Service) revokeInfos(ctx context.Context, f GrantFilter, reason, actor string) ([]GrantInfo, []Grant, error) {
	f.Live, f.Limit = true, 0
	before, err := s.Store.ListGrants(ctx, f)
	if err != nil {
		return nil, nil, err
	}
	revoked, err := s.Store.RevokeGrants(ctx, f, reason, s.now())
	if err != nil {
		return nil, nil, err
	}
	names, slugs := map[string]string{}, map[int64]string{}
	for _, gi := range before {
		names[gi.Grant.AppID], slugs[gi.Grant.OrgID] = gi.App.Name, gi.OrgSlug
	}
	s.auditRevoked(ctx, revoked, reason, actor, names, slugs)
	return before, revoked, nil
}

// revoke is revokeInfos without the grants as they were.
func (s *Service) revoke(ctx context.Context, f GrantFilter, reason, actor string) ([]Grant, error) {
	_, revoked, err := s.revokeInfos(ctx, f, reason, actor)
	return revoked, err
}

// revokeDetached ends one grant for a reason the Service found out itself, after the request that
// found it can no longer be trusted to stay open. A failure is logged.
func (s *Service) revokeDetached(ctx context.Context, grantID int64, reason string) {
	ctx = context.WithoutCancel(ctx)
	if _, err := s.revoke(ctx, GrantFilter{ID: grantID}, reason, ActorSystem); err != nil {
		s.log().ErrorContext(ctx, "oauth: a grant was not revoked", "grant_id", grantID, "reason", reason, "error", err)
	}
}

// auditRevoked writes oauth.grant_revoked for each grant. names maps app ids to names and slugs maps
// organization ids to slugs; a grant whose app or organization is missing from them is audited by id.
func (s *Service) auditRevoked(ctx context.Context, grants []Grant, reason, actor string, names map[string]string, slugs map[int64]string) {
	for _, g := range grants {
		p := map[string]any{
			"grant_id": g.ID, "app_id": g.AppID, "user_id": g.UserID, "org_id": g.OrgID, "reason": reason, "actor": actor,
		}
		if n, ok := names[g.AppID]; ok {
			p["app_name"] = n
		}
		if slug := slugs[g.OrgID]; slug != "" {
			p["org_slug"] = slug
		}
		s.audit(ctx, EventGrantRevoked, p)
	}
}

// filterFor normalizes the app id of a filter. ok is false when the filter names an app id that is not
// a UUID: it selects nothing.
func filterFor(f GrantFilter) (GrantFilter, bool) {
	if f.AppID != "" {
		id, ok := canonUUID(f.AppID)
		if !ok {
			return f, false
		}
		f.AppID = id
	}
	return f, true
}

// RevokeGrant revokes one live grant. ErrNotFound if no live grant has the id.
func (s *Service) RevokeGrant(ctx context.Context, grantID int64, reason, actor string) error {
	if !ValidReason(reason) {
		return invalidf("%q is not a revocation reason", clip(reason, 40))
	}
	if grantID <= 0 {
		return ErrNotFound
	}
	revoked, err := s.revoke(ctx, GrantFilter{ID: grantID}, reason, actor)
	if err != nil {
		return err
	}
	if len(revoked) == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeGrants revokes the live grants f selects and returns how many. A filter that selects
// everything needs f.All, so that a caller who forgot a field cannot end every grant by accident.
func (s *Service) RevokeGrants(ctx context.Context, f GrantFilter, reason, actor string) (int, error) {
	if !ValidReason(reason) {
		return 0, invalidf("%q is not a revocation reason", clip(reason, 40))
	}
	if f.IsEmpty() && !f.All {
		return 0, invalidf("a filter that selects every grant needs All")
	}
	f, ok := filterFor(f)
	if !ok {
		return 0, nil
	}
	revoked, err := s.revoke(ctx, f, reason, actor)
	return len(revoked), err
}

// RevokeUser revokes every live grant of a user and returns how many. A user with none is not an error.
func (s *Service) RevokeUser(ctx context.Context, userID, reason, actor string) (int, error) {
	if userID == "" {
		return 0, invalidf("a user is required")
	}
	if !ValidReason(reason) {
		return 0, invalidf("%q is not a revocation reason", clip(reason, 40))
	}
	revoked, err := s.revoke(ctx, GrantFilter{UserID: userID}, reason, actor)
	return len(revoked), err
}

// RevokeApp revokes the live grants of an app in one organization (req.OrgID) or in all (0) and
// returns the app as it was, with the time its newest grant in that scope was created.
func (s *Service) RevokeApp(ctx context.Context, req RevokeAppRequest) (*RevokedApp, error) {
	id, ok := canonUUID(req.AppID)
	if !ok {
		return nil, ErrNotFound
	}
	if !ValidReason(req.Reason) {
		return nil, invalidf("%q is not a revocation reason", clip(req.Reason, 40))
	}
	app, err := s.Store.GetApp(ctx, id)
	if err != nil {
		return nil, err
	}
	before, revoked, err := s.revokeInfos(ctx, GrantFilter{AppID: app.ID, OrgID: req.OrgID}, req.Reason, req.Actor)
	if err != nil {
		return nil, err
	}
	out := &RevokedApp{App: *app, Revoked: len(revoked)}
	if len(before) > 0 { // newest first
		out.AuthorizedAt = before[0].Grant.CreatedAt
	}
	return out, nil
}

// ListGrants returns the grants f selects with their apps and organization slugs, newest first.
func (s *Service) ListGrants(ctx context.Context, f GrantFilter) ([]GrantInfo, error) {
	f, ok := filterFor(f)
	if !ok {
		return nil, nil
	}
	return s.Store.ListGrants(ctx, f)
}

// ListAuthorizedApps returns the apps with a live grant in the organization, the newest authorization
// first. A dynamic app's website and icon are left out: the page shows what the registrant asserted
// about itself only on the consent page, and its logo never.
func (s *Service) ListAuthorizedApps(ctx context.Context, orgID int64) ([]AuthorizedApp, error) {
	if orgID <= 0 {
		return nil, invalidf("organization is required")
	}
	infos, err := s.Store.ListGrants(ctx, GrantFilter{OrgID: orgID, Live: true}) // newest first
	if err != nil {
		return nil, err
	}
	byApp := map[string]*AuthorizedApp{}
	var order []string
	for _, gi := range infos {
		if gi.App.DeletedAt != nil {
			continue
		}
		a, seen := byApp[gi.App.ID]
		if !seen {
			a = &AuthorizedApp{App: gi.App, AuthorizedAt: gi.Grant.CreatedAt}
			byApp[gi.App.ID] = a
			order = append(order, gi.App.ID)
		}
		a.Scopes = UnionScopes(a.Scopes, IntersectScopes(gi.Grant.Scopes, gi.App.Scopes))
		a.FirstApprover = gi.Grant.UserID // the last one seen is the oldest
	}
	out := make([]AuthorizedApp, 0, len(order))
	for _, id := range order {
		a := byApp[id]
		a.Scopes = NormalizeScopes(a.Scopes)
		if a.App.RegistrationType == RegistrationDynamic {
			a.App.Website, a.App.Icon = "", ""
		}
		out = append(out, *a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].AuthorizedAt.Equal(out[j].AuthorizedAt) {
			return out[i].AuthorizedAt.After(out[j].AuthorizedAt)
		}
		if out[i].App.Name != out[j].App.Name {
			return out[i].App.Name < out[j].App.Name
		}
		return out[i].App.ID < out[j].App.ID
	})
	return out, nil
}

// Revoke serves POST /v1/oauth/revoke (RFC 7009): it authenticates the app and revokes the whole grant
// the token belongs to (ReasonClient). A token that is unknown, of the wrong shape or another app's is
// not an error and changes nothing, so the endpoint tells a caller nothing about tokens it does not
// own. An app without a secret may revoke only when it is a dynamic app that registered with
// token_endpoint_auth_method none.
func (s *Service) Revoke(ctx context.Context, req RevokeRequest) error {
	ca, err := s.authenticateClient(ctx, req.ClientID, req.ClientSecret)
	if err != nil {
		return err
	}
	if !ca.bySecret && !(ca.app.RegistrationType == RegistrationDynamic && ca.app.TokenEndpointAuthMethod == AuthMethodNone) {
		return invalidClient()
	}
	if req.Token == "" {
		return NewError(CodeInvalidRequest, "token is required")
	}
	if !secrets.IsOAuthAccessToken(req.Token) && !secrets.IsOAuthRefreshToken(req.Token) {
		return nil
	}
	tok, err := s.Store.GetToken(ctx, secrets.HashToken(req.Token))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return s.serverError(ctx, "revoke: get token", err)
	}
	grant, err := s.Store.GetGrant(ctx, tok.GrantID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return s.serverError(ctx, "revoke: get grant", err)
	}
	if grant.AppID != ca.app.ID {
		return nil
	}
	if _, err := s.revoke(ctx, GrantFilter{ID: grant.ID}, ReasonClient, "app:"+ca.app.ID); err != nil {
		return s.serverError(ctx, "revoke: revoke grant", err)
	}
	return nil
}
