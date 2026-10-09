package oauth

import (
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors. The Service and the Store return them as they are or wrapped with fmt.Errorf
// ("%w"); test with errors.Is. doc.go lists the HTTP status the API layer gives each.
var (
	// ErrNotImplemented is what the stubs of a method that is not written yet return. No finished
	// code returns it.
	ErrNotImplemented = errors.New("oauth: not implemented")

	// ErrNotFound: no such row, or the row is not visible to the caller (a deleted app, a grant of
	// another organization, an unusable token).
	ErrNotFound = errors.New("oauth: not found")

	// ErrConflict: a unique key is taken (an id or a token hash). With ids and tokens from
	// crypto/rand this is a bug or an attack on the generator; the Service treats it as internal.
	ErrConflict = errors.New("oauth: already exists")

	// ErrAlreadyDecided: the authorization is not pending any more (approved, declined or exchanged).
	ErrAlreadyDecided = errors.New("oauth: authorization was already decided")

	// ErrExpired: the authorization is pending but past its expires_at.
	ErrExpired = errors.New("oauth: authorization expired")

	// ErrOrgMismatch: the authorization asked for a particular organization (organization_slug)
	// and the approval names another.
	ErrOrgMismatch = errors.New("oauth: the authorization was requested for another organization")

	// ErrUnknownClient: the client_id of an authorization request is not a live app. The API
	// answers with a page and no redirect.
	ErrUnknownClient = errors.New("oauth: unknown client")

	// ErrInvalidRedirectURI: the redirect_uri of an authorization request does not match one of the
	// app's registered URIs. The API answers with a page and no redirect.
	ErrInvalidRedirectURI = errors.New("oauth: redirect_uri does not match a registered URI")

	// ErrLimit: a cap is reached (dynamic apps, pending authorizations, manual apps of an
	// organization). The wrapped text says which.
	ErrLimit = errors.New("oauth: limit reached")

	// ErrInvalid: the organization apps API refuses the input. The wrapped text is safe to show.
	ErrInvalid = errors.New("oauth: invalid request")

	// ErrNotAdmitted is what Service.Admit returns for a user who may no longer hold OAuth grants
	// at all: removed with `supavise users remove`, or an SSO user the provider no longer vouches for.
	ErrNotAdmitted = errors.New("oauth: user is not admitted")

	// ErrNotMember is what Service.Admit returns for a user who is not a member of the organization
	// (any role) any more. A refresh then also revokes the grant, reason ReasonMembership.
	ErrNotMember = errors.New("oauth: user is not a member of the organization")

	// ErrRefreshReused is what Store.RotateRefresh returns (as a *ReuseError) for a refresh token
	// that was exchanged before the grace window. The Service revokes the grant.
	ErrRefreshReused = errors.New("oauth: refresh token reused")
)

// ReuseError is the error RotateRefresh returns for a reused refresh token. It matches
// ErrRefreshReused with errors.Is and carries the grant to revoke.
type ReuseError struct {
	GrantID int64
	AppID   string
	UserID  string
	OrgID   int64
}

func (e *ReuseError) Error() string { return ErrRefreshReused.Error() }

// Is makes errors.Is(err, ErrRefreshReused) true.
func (e *ReuseError) Is(target error) bool { return target == ErrRefreshReused }

// Error codes of RFC 6749 section 5.2 and 4.1.2.1, RFC 7591 section 3.2.2 and RFC 8707.
const (
	CodeInvalidRequest          = "invalid_request"
	CodeInvalidClient           = "invalid_client"
	CodeInvalidGrant            = "invalid_grant"
	CodeUnsupportedGrantType    = "unsupported_grant_type"
	CodeUnsupportedResponseType = "unsupported_response_type"
	CodeInvalidScope            = "invalid_scope"
	CodeInvalidTarget           = "invalid_target"
	CodeInvalidClientMetadata   = "invalid_client_metadata"
	CodeInvalidRedirectURI      = "invalid_redirect_uri"
	CodeServerError             = "server_error"
)

// Error is an OAuth protocol error: what the token, registration and revocation endpoints answer
// as JSON {"error", "error_description", "message"}, and what an authorization error carries in a
// redirect. Description is shown to the client; it never contains a secret, a token, a code, a
// state or a challenge.
type Error struct {
	Code        string
	Description string
	// Status is the HTTP status of the token, registration and revocation endpoints. Zero means
	// StatusFor(Code).
	Status int
}

func (e *Error) Error() string {
	if e.Description == "" {
		return e.Code
	}
	return e.Code + ": " + e.Description
}

// HTTPStatus is Status, or StatusFor(Code) when Status is zero.
func (e *Error) HTTPStatus() int {
	if e.Status != 0 {
		return e.Status
	}
	return StatusFor(e.Code)
}

// NewError builds an *Error; its status follows from the code.
func NewError(code, description string) *Error { return &Error{Code: code, Description: description} }

// Errorf is NewError with a formatted description.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Description: fmt.Sprintf(format, args...)}
}

// StatusFor is the HTTP status the token endpoint gives an error code: 401 for invalid_client,
// 500 for server_error and 400 for the others.
func StatusFor(code string) int {
	switch code {
	case CodeInvalidClient:
		return http.StatusUnauthorized
	case CodeServerError:
		return http.StatusInternalServerError
	}
	return http.StatusBadRequest
}

// RedirectError is an authorization request error that goes back to the client through its
// redirect URI (RFC 6749 section 4.1.2.1), as query parameters error, error_description, state
// and iss. The Service returns it only after client_id and redirect_uri have been validated;
// before that the errors are ErrUnknownClient and ErrInvalidRedirectURI and nothing redirects.
type RedirectError struct {
	// RedirectURI is the validated redirect_uri exactly as the client sent it (a loopback port
	// included).
	RedirectURI string
	// State is the client's state, unchanged ("" when it sent none).
	State string
	Err   *Error
}

func (e *RedirectError) Error() string { return "oauth: authorization error: " + e.Err.Error() }

// Unwrap returns the protocol error.
func (e *RedirectError) Unwrap() error { return e.Err }
