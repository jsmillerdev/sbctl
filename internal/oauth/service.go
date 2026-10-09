package oauth

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
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
// # Rules the Service keeps (section 2 of the design)
//
//   - Registration (register.go): open RFC 7591 registration of dynamic apps, validated field by
//     field; manual apps are published by an organization (CreateApp). Client names are cleaned, URLs
//     are checked and never fetched, and a logo is stored and never returned.
//   - Authorization (authorize.go): client_id and redirect_uri are validated before anything can
//     redirect; PKCE (S256 only) is required of dynamic apps; scopes are narrowed to the app's; the
//     resource must be this server's. Approval stores the SHA-256 of a one-time code.
//   - Tokens (token.go): a code is redeemed once, in one transaction, bound to its client, its exact
//     redirect_uri, its resource, its challenge, its approver and its organization; any mismatch burns
//     it and a replay revokes the grant it created. A refresh token rotates on every use; a reuse after
//     RefreshGrace revokes the grant. Whether the user may still hold a grant (Admit) is asked outside
//     any transaction, so that no pooled connection waits for another.
//   - Revocation (revoke.go): one path ends grants, writes the audit event and returns what it ended.
//   - Housekeeping (prune.go): lazy, from Register and Exchange, at most once per PruneEvery per node.
//
// A secret is generated here, handed to its owner once and given to the Store only as its SHA-256.
// Nothing secret reaches a log line, an audit payload, an alert or an error description.
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

	// pruneMu guards lastPrune, the time of the last lazy prune on this node (prune.go).
	pruneMu   sync.Mutex
	lastPrune time.Time
}

var _ Authority = (*Service)(nil)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// now is the Service's clock. Every comparison with a stored time uses it, never time.Now.
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return discardLog
}

// issuer is Issuer without a trailing slash: the "iss" of responses.
func (s *Service) issuer() string { return strings.TrimRight(s.Issuer, "/") }

// audit records an event. The payload never carries a token, code, secret, state or challenge.
func (s *Service) audit(ctx context.Context, kind string, payload map[string]any) {
	if s.Audit != nil {
		s.Audit(ctx, kind, payload)
	}
}

func (s *Service) alert(ctx context.Context, e AlertEvent) {
	if s.Alert != nil {
		s.Alert(ctx, e)
	}
}

// admit asks whether the user may hold a grant in the organization. Without an Admit nobody does.
func (s *Service) admit(ctx context.Context, userID string, orgID int64) error {
	if s.Admit == nil {
		return ErrNotAdmitted
	}
	return s.Admit(ctx, userID, orgID)
}

// serverError logs the cause of an internal failure of an endpoint that answers in OAuth JSON and
// returns the generic error to send. The cause is a store or driver error; it holds no secret.
func (s *Service) serverError(ctx context.Context, op string, err error) *Error {
	s.log().ErrorContext(ctx, "oauth: internal error", "op", op, "error", err)
	return &Error{Code: CodeServerError, Description: "the server could not complete the request"}
}

// redirectHost is the host name of a redirect URI, for audit payloads and the consent page; "" if
// it does not parse.
func redirectHost(redirectURI string) string {
	u, err := parseRedirectURI(redirectURI)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
