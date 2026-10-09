package oauth

import (
	"context"
	"fmt"
	"strings"

	"github.com/supavise/supavise/internal/secrets"
)

// maxSecretsPerApp bounds the client secrets of a manual app. The token endpoint compares a presented
// secret with every one of them.
const maxSecretsPerApp = 10

// ---- dynamic registration (RFC 7591)

// Register creates a dynamic app. Every field is validated (section 2.4); an unknown scope is dropped,
// a request that leaves no scope is refused. client_uri and logo_uri are checked and stored, never
// fetched, and the logo is never returned. The app gets a client secret whatever its
// token_endpoint_auth_method, as on hosted Supabase. The per-address rate limit is the caller's.
func (s *Service) Register(ctx context.Context, req RegisterRequest) (*RegisteredApp, error) {
	s.maybePrune(ctx)
	bad := func(code, format string, args ...any) error { return Errorf(code, format, args...) }

	name, ok := cleanClientName(req.ClientName)
	if !ok {
		return nil, bad(CodeInvalidClientMetadata, "client_name is required and must be 1 to %d characters", MaxClientNameLen)
	}
	uris, err := validateRedirectURIs(req.RedirectURIs)
	if err != nil {
		return nil, bad(CodeInvalidRedirectURI, "%s", err)
	}
	grantTypes := []string{GrantTypeAuthorizationCode, GrantTypeRefreshToken}
	if len(req.GrantTypes) > 0 {
		grantTypes = nil
		for _, g := range req.GrantTypes {
			if g != GrantTypeAuthorizationCode && g != GrantTypeRefreshToken {
				return nil, bad(CodeInvalidClientMetadata, "grant_types may contain %s and %s only", GrantTypeAuthorizationCode, GrantTypeRefreshToken)
			}
			if !hasString(grantTypes, g) {
				grantTypes = append(grantTypes, g)
			}
		}
	}
	responseTypes := []string{ResponseTypeCode}
	if len(req.ResponseTypes) > 0 && (len(req.ResponseTypes) != 1 || req.ResponseTypes[0] != ResponseTypeCode) {
		return nil, bad(CodeInvalidClientMetadata, "response_types must be [%q]", ResponseTypeCode)
	}
	method := req.TokenEndpointAuthMethod
	switch method {
	case "":
		method = AuthMethodBasic
	case AuthMethodNone, AuthMethodBasic, AuthMethodPost:
	default:
		return nil, bad(CodeInvalidClientMetadata, "token_endpoint_auth_method must be %s, %s or %s", AuthMethodNone, AuthMethodBasic, AuthMethodPost)
	}
	scopes := AdvertisedScopes
	if requested := ParseScopes(req.Scope); len(requested) > 0 {
		scopes = IntersectScopes(requested, AdvertisedScopes)
	}
	if scopes = NormalizeScopes(scopes); len(scopes) == 0 {
		return nil, bad(CodeInvalidClientMetadata, "scope names none of the supported scopes")
	}
	if req.ClientURI != "" {
		if err := checkWebURL(req.ClientURI, true); err != nil {
			return nil, bad(CodeInvalidClientMetadata, "client_uri %s", err)
		}
	}
	if req.LogoURI != "" {
		if err := checkWebURL(req.LogoURI, true); err != nil {
			return nil, bad(CodeInvalidClientMetadata, "logo_uri %s", err)
		}
	}

	n, err := s.Store.CountDynamicApps(ctx)
	if err != nil {
		return nil, s.serverError(ctx, "register: count apps", err)
	}
	if n >= MaxDynamicApps {
		return nil, limitf("too many registered clients")
	}

	now := s.now()
	plain := secrets.NewClientSecret()
	app := App{
		ID: newUUID(), RegistrationType: RegistrationDynamic, Name: name,
		Website: req.ClientURI, Icon: req.LogoURI, RedirectURIs: uris, Scopes: scopes,
		TokenEndpointAuthMethod: method, CreatedAt: now, UpdatedAt: now,
	}
	rec := AppSecret{ID: newUUID(), AppID: app.ID, Alias: secrets.ClientSecretAlias(plain), Hash: secrets.HashToken(plain), CreatedAt: now}
	if err := s.Store.CreateApp(ctx, app, &rec); err != nil {
		return nil, s.serverError(ctx, "register: create app", err)
	}
	hosts := make([]string, 0, len(uris))
	for _, u := range uris {
		hosts = append(hosts, redirectHost(u))
	}
	s.log().InfoContext(ctx, "oauth: client registered", "app_id", app.ID, "client_name", name,
		"redirect_hosts", strings.Join(hosts, ","), "auth_method", method)

	app.Icon = "" // a self-asserted logo is stored and never returned
	return &RegisteredApp{App: app, ClientSecret: plain, IssuedAt: now, GrantTypes: grantTypes, ResponseTypes: responseTypes}, nil
}

// ---- manual apps (the organization's OAuth Apps page)

// manualFields are the editable fields of a manual app after validation.
type manualFields struct {
	name, website, icon  string
	scopes, redirectURIs []string
}

// checkManualFields validates what an organization sends for a manual app. The errors are ErrInvalid
// with a sentence to show.
func checkManualFields(name, website, icon string, scopes, redirectURIs []string) (manualFields, error) {
	var f manualFields
	var ok bool
	if f.name, ok = cleanClientName(name); !ok {
		return f, invalidf("name is required and must be 1 to %d characters", MaxClientNameLen)
	}
	if website != "" {
		if err := checkWebURL(website, false); err != nil {
			return f, invalidf("website %s", err)
		}
	}
	if icon != "" {
		if err := checkWebURL(icon, false); err != nil {
			return f, invalidf("icon %s", err)
		}
	}
	f.website, f.icon = website, icon
	if len(scopes) == 0 {
		return f, invalidf("scopes must list at least one scope")
	}
	for _, sc := range scopes {
		if !ValidScope(sc) {
			return f, invalidf("%q is not a scope", clip(sc, 40))
		}
	}
	f.scopes = NormalizeScopes(scopes)
	var err error
	if f.redirectURIs, err = validateRedirectURIs(redirectURIs); err != nil {
		return f, invalidf("%s", err)
	}
	return f, nil
}

// truncate shortens s to n bytes for an error text, on a character boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// manualApp returns the live manual app of the organization. An app of another organization, a
// dynamic app, a deleted app and a malformed id are all ErrNotFound: the caller cannot tell them apart.
func (s *Service) manualApp(ctx context.Context, orgID int64, appID string) (*App, error) {
	id, ok := canonUUID(appID)
	if !ok || orgID <= 0 {
		return nil, ErrNotFound
	}
	app, err := s.Store.GetApp(ctx, id)
	if err != nil {
		return nil, err
	}
	if app.RegistrationType != RegistrationManual || app.OrgID != orgID {
		return nil, ErrNotFound
	}
	return app, nil
}

// ListPublishedApps returns the manual apps of the organization, oldest first.
func (s *Service) ListPublishedApps(ctx context.Context, orgID int64) ([]App, error) {
	if orgID <= 0 {
		return nil, invalidf("organization is required")
	}
	return s.Store.ListManualApps(ctx, orgID)
}

// CreateApp publishes a manual app with its first client secret. At most MaxManualAppsPerOrg per
// organization. The plaintext secret is in the result and nowhere else.
func (s *Service) CreateApp(ctx context.Context, req CreateAppRequest) (*CreatedApp, error) {
	if req.OrgID <= 0 {
		return nil, invalidf("organization is required")
	}
	if _, ok := canonUUID(req.CreatedBy); !ok && req.CreatedBy != "" {
		return nil, invalidf("created_by must be a user id")
	}
	f, err := checkManualFields(req.Name, req.Website, req.Icon, req.Scopes, req.RedirectURIs)
	if err != nil {
		return nil, err
	}
	n, err := s.Store.CountManualApps(ctx, req.OrgID)
	if err != nil {
		return nil, fmt.Errorf("oauth: create app: %w", err)
	}
	if n >= MaxManualAppsPerOrg {
		return nil, limitf("an organization may publish at most %d OAuth apps", MaxManualAppsPerOrg)
	}
	now := s.now()
	plain := secrets.NewClientSecret()
	app := App{
		ID: newUUID(), RegistrationType: RegistrationManual, OrgID: req.OrgID, Name: f.name,
		Website: f.website, Icon: f.icon, RedirectURIs: f.redirectURIs, Scopes: f.scopes,
		TokenEndpointAuthMethod: AuthMethodBasic, CreatedBy: req.CreatedBy, CreatedAt: now, UpdatedAt: now,
	}
	rec := AppSecret{ID: newUUID(), AppID: app.ID, Alias: secrets.ClientSecretAlias(plain), Hash: secrets.HashToken(plain), CreatedBy: req.CreatedBy, CreatedAt: now}
	if err := s.Store.CreateApp(ctx, app, &rec); err != nil {
		return nil, fmt.Errorf("oauth: create app: %w", err)
	}
	s.audit(ctx, EventAppCreated, s.appPayload(app, req.CreatedBy))
	s.audit(ctx, EventClientSecretCreated, map[string]any{"app_id": app.ID, "org_id": app.OrgID, "secret_id": rec.ID, "user_id": req.CreatedBy})
	rec.Hash = nil
	return &CreatedApp{App: app, Secret: rec, ClientSecret: plain}, nil
}

// UpdateApp changes a manual app's name, website, icon, scopes and redirect URIs. Existing grants
// keep working, narrowed by the new scopes: effective scopes are evaluated at use.
func (s *Service) UpdateApp(ctx context.Context, req UpdateAppRequest) (*App, error) {
	app, err := s.manualApp(ctx, req.OrgID, req.AppID)
	if err != nil {
		return nil, err
	}
	f, err := checkManualFields(req.Name, req.Website, req.Icon, req.Scopes, req.RedirectURIs)
	if err != nil {
		return nil, err
	}
	app.Name, app.Website, app.Icon, app.Scopes, app.RedirectURIs = f.name, f.website, f.icon, f.scopes, f.redirectURIs
	app.UpdatedAt = s.now()
	if err := s.Store.UpdateApp(ctx, *app); err != nil {
		return nil, err
	}
	s.audit(ctx, EventAppUpdated, s.appPayload(*app, req.Actor))
	return app, nil
}

// DeleteApp deletes a manual app and revokes its live grants (ReasonAppDeleted). It returns the app
// as it was.
func (s *Service) DeleteApp(ctx context.Context, req DeleteAppRequest) (*App, error) {
	app, err := s.manualApp(ctx, req.OrgID, req.AppID)
	if err != nil {
		return nil, err
	}
	revoked, err := s.Store.DeleteApp(ctx, app.ID, s.now())
	if err != nil {
		return nil, err
	}
	s.audit(ctx, EventAppDeleted, s.appPayload(*app, req.Actor))
	s.auditRevoked(ctx, revoked, ReasonAppDeleted, req.Actor, map[string]string{app.ID: app.Name}, nil)
	return app, nil
}

// ListClientSecrets returns a manual app's secrets without their hashes.
func (s *Service) ListClientSecrets(ctx context.Context, orgID int64, appID string) ([]AppSecret, error) {
	app, err := s.manualApp(ctx, orgID, appID)
	if err != nil {
		return nil, err
	}
	list, err := s.Store.ListSecrets(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Hash = nil
	}
	return list, nil
}

// CreateClientSecret adds a secret to a manual app, up to maxSecretsPerApp. The plaintext is in the
// result and nowhere else.
func (s *Service) CreateClientSecret(ctx context.Context, req CreateSecretRequest) (*CreatedSecret, error) {
	app, err := s.manualApp(ctx, req.OrgID, req.AppID)
	if err != nil {
		return nil, err
	}
	if _, ok := canonUUID(req.CreatedBy); !ok && req.CreatedBy != "" {
		return nil, invalidf("created_by must be a user id")
	}
	have, err := s.Store.ListSecrets(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	if len(have) >= maxSecretsPerApp {
		return nil, limitf("an OAuth app may have at most %d client secrets; delete one first", maxSecretsPerApp)
	}
	plain := secrets.NewClientSecret()
	rec := AppSecret{ID: newUUID(), AppID: app.ID, Alias: secrets.ClientSecretAlias(plain), Hash: secrets.HashToken(plain), CreatedBy: req.CreatedBy, CreatedAt: s.now()}
	if err := s.Store.CreateSecret(ctx, rec); err != nil {
		return nil, err
	}
	s.audit(ctx, EventClientSecretCreated, map[string]any{"app_id": app.ID, "org_id": app.OrgID, "secret_id": rec.ID, "user_id": req.CreatedBy})
	rec.Hash = nil
	return &CreatedSecret{Secret: rec, ClientSecret: plain}, nil
}

// DeleteClientSecret removes a secret of a manual app. ErrNotFound if the app does not have it.
func (s *Service) DeleteClientSecret(ctx context.Context, req DeleteSecretRequest) error {
	app, err := s.manualApp(ctx, req.OrgID, req.AppID)
	if err != nil {
		return err
	}
	id, ok := canonUUID(req.SecretID)
	if !ok {
		return ErrNotFound
	}
	if err := s.Store.DeleteSecret(ctx, app.ID, id); err != nil {
		return err
	}
	s.audit(ctx, EventClientSecretDeleted, map[string]any{"app_id": app.ID, "org_id": app.OrgID, "secret_id": id, "user_id": req.Actor})
	return nil
}

// appPayload is the audit payload of an app event: ids, the sanitized name, the redirect hosts and the
// scopes. It never holds a secret.
func (s *Service) appPayload(app App, actor string) map[string]any {
	hosts := make([]string, 0, len(app.RedirectURIs))
	for _, u := range app.RedirectURIs {
		if h := redirectHost(u); h != "" {
			hosts = append(hosts, h)
		}
	}
	return map[string]any{
		"app_id": app.ID, "app_name": app.Name, "registration_type": app.RegistrationType, "org_id": app.OrgID,
		"redirect_hosts": hosts, "scopes": app.Scopes, "user_id": actor,
	}
}
