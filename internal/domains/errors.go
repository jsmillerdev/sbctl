package domains

import (
	"errors"
	"time"
)

// Kind classifies an Error for the caller that turns it into an HTTP status.
type Kind int

const (
	KindInvalid       Kind = iota + 1 // the request names something that cannot work (400)
	KindConflict                      // a name another project holds (409)
	KindNotConfigured                 // the project has no such configuration (400, Studio matches the message)
	KindState                         // the configuration is in the wrong state for the call (409)
	KindRateLimited                   // too many verification attempts (429)
	KindUnavailable                   // this node cannot do it (400)
)

// Error is a refusal with a message fit for a user.
type Error struct {
	Kind       Kind
	Msg        string
	RetryAfter time.Duration // KindRateLimited
	// Reserved marks a vanity name that is well formed but not for use (reserved, or ref-shaped).
	Reserved bool
}

func (e *Error) Error() string { return e.Msg }

func invalid(msg string) error  { return &Error{Kind: KindInvalid, Msg: msg} }
func conflict(msg string) error { return &Error{Kind: KindConflict, Msg: msg} }
func state(msg string) error    { return &Error{Kind: KindState, Msg: msg} }

// AsError returns err as an *Error when it is one.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
