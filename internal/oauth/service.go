package oauth

import (
	"context"
	"log/slog"
	"time"
)

// Service is the authorization server's rules over a Store: registration, authorization, the
// token endpoint, revocation, the lookup of access tokens and the organization's apps. It
// implements Authority.
//
// Build it with a struct literal and set what it needs; the zero value of every field except
// Store, Issuer and DashboardURL has a safe default. internal/api completes a Service it is given
// (Deps.OAuth) or builds one from the registry; the operator CLI builds its own over a Store.
//
// This file is a stub: every method returns ErrNotImplemented until the rules are written.
type Service struct {
	// Store keeps the apps, authorizations, grants and tokens. Required.
	Store Store
	// Issuer is the issuer identifier, cfg.APIURL(): the origin of the endpoints, the "iss" of every
	// authorization response, and the base of the resource identifier (ResourceURL). Required.
	Issuer string
	// DashboardURL is cfg.DashboardURL(), the origin of Studio's consent page. Required.
	DashboardURL string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Admit says whether a user may hold OAuth grants in an organization right now. The Service
	// calls it when it redeems a code and when it exchanges a refresh token. Nil: nobody is
	// admitted, so a Service without an Admit cannot mint or refresh tokens (it fails closed).
	//
	// It returns nil to admit, ErrNotAdmitted when the user was removed or SSO no longer admits them,
	// ErrNotMember when the user left the organization (a refresh then also revokes the grant,
	// ReasonMembership), or any other error for a failure to find out; that is a server_error and
	// revokes nothing.
	Admit func(ctx context.Context, userID string, orgID int64) error
	// Audit records an audit event; the API wires it to Registry.AppendEvent on config.SystemRef.
	// Nil: events are dropped. It must not fail the operation, and must not block it.
	Audit func(ctx context.Context, kind string, payload map[string]any)
	// Alert reports a replay (AlertKindTokenReuse) to the operator; internal/app wires it to the
	// alerts notifier. Nil: no alert.
	Alert func(ctx context.Context, e AlertEvent)
	// Log is the logger; nil discards. Dynamic registrations are logged at info level here.
	Log *slog.Logger
}

var _ Authority = (*Service)(nil)

func (s *Service) Register(ctx context.Context, req RegisterRequest) (*RegisteredApp, error) {
	return nil, ErrNotImplemented
}

func (s *Service) StartAuthorization(ctx context.Context, req AuthorizeRequest) (*AuthorizeResult, error) {
	return nil, ErrNotImplemented
}

func (s *Service) Describe(ctx context.Context, authID string) (*AuthorizationView, error) {
	return nil, ErrNotImplemented
}

func (s *Service) Approve(ctx context.Context, req ApproveRequest) (*ApproveResult, error) {
	return nil, ErrNotImplemented
}

func (s *Service) Decline(ctx context.Context, req DeclineRequest) error { return ErrNotImplemented }

func (s *Service) Exchange(ctx context.Context, req TokenRequest) (*TokenResponse, error) {
	return nil, ErrNotImplemented
}

func (s *Service) Revoke(ctx context.Context, req RevokeRequest) error { return ErrNotImplemented }

func (s *Service) LookupAccess(ctx context.Context, token string) (*AccessInfo, error) {
	return nil, ErrNotImplemented
}

func (s *Service) TouchAccess(ctx context.Context, tokenID, grantID int64) error {
	return ErrNotImplemented
}

func (s *Service) RevokeGrant(ctx context.Context, grantID int64, reason, actor string) error {
	return ErrNotImplemented
}

func (s *Service) RevokeGrants(ctx context.Context, f GrantFilter, reason, actor string) (int, error) {
	return 0, ErrNotImplemented
}

func (s *Service) RevokeUser(ctx context.Context, userID, reason, actor string) (int, error) {
	return 0, ErrNotImplemented
}

func (s *Service) RevokeApp(ctx context.Context, req RevokeAppRequest) (*RevokedApp, error) {
	return nil, ErrNotImplemented
}

func (s *Service) ListGrants(ctx context.Context, f GrantFilter) ([]GrantInfo, error) {
	return nil, ErrNotImplemented
}

func (s *Service) ListAuthorizedApps(ctx context.Context, orgID int64) ([]AuthorizedApp, error) {
	return nil, ErrNotImplemented
}

func (s *Service) ListPublishedApps(ctx context.Context, orgID int64) ([]App, error) {
	return nil, ErrNotImplemented
}

func (s *Service) CreateApp(ctx context.Context, req CreateAppRequest) (*CreatedApp, error) {
	return nil, ErrNotImplemented
}

func (s *Service) UpdateApp(ctx context.Context, req UpdateAppRequest) (*App, error) {
	return nil, ErrNotImplemented
}

func (s *Service) DeleteApp(ctx context.Context, req DeleteAppRequest) (*App, error) {
	return nil, ErrNotImplemented
}

func (s *Service) ListClientSecrets(ctx context.Context, orgID int64, appID string) ([]AppSecret, error) {
	return nil, ErrNotImplemented
}

func (s *Service) CreateClientSecret(ctx context.Context, req CreateSecretRequest) (*CreatedSecret, error) {
	return nil, ErrNotImplemented
}

func (s *Service) DeleteClientSecret(ctx context.Context, req DeleteSecretRequest) error {
	return ErrNotImplemented
}

func (s *Service) Prune(ctx context.Context) (PruneResult, error) {
	return PruneResult{}, ErrNotImplemented
}
