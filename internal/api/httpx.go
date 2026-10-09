package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Error is an API error: an HTTP status and the message of the Management API error
// envelope {"message": "..."}.
type Error struct {
	Status  int
	Message string
	// Header is added to the response of this error (WWW-Authenticate on an insufficient_scope
	// refusal). Nil for nearly every error. Shared errors such as errUnauthorized never carry one:
	// build a new Error to add a header.
	Header http.Header
}

func (e *Error) Error() string { return e.Message }

// errf builds an *Error.
func errf(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

var (
	errUnauthorized = &Error{Status: http.StatusUnauthorized, Message: "Unauthorized"}
	errForbidden    = &Error{Status: http.StatusForbidden, Message: "Forbidden action"}
)

// handlerFunc is a handler that reports failure by returning an error. Returned
// *Error values keep their status; anything else is a 500 whose detail is logged,
// not sent.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// errorBody is the Management API error envelope.
type errorBody struct {
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, errf(http.StatusInternalServerError, "failed to encode response"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, e *Error) {
	for k, vs := range e.Header {
		w.Header()[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
	writeJSON(w, e.Status, errorBody{Message: e.Message})
}

// maxBody bounds request bodies the API reads into memory (SQL, config, snippets).
// Function deployments use their own, larger limit.
const maxBody = 8 << 20

// decode reads a JSON request body into dst. Unknown fields are ignored: the specs
// grow, and clients may send newer fields.
func decode(r *http.Request, dst any) error {
	body := http.MaxBytesReader(nil, r.Body, maxBody)
	b, err := io.ReadAll(body)
	if err != nil {
		return errf(http.StatusBadRequest, "failed to read request body: %v", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return errf(http.StatusBadRequest, "invalid request body: %v", err)
	}
	return nil
}

// asError converts any error to an *Error for the envelope.
func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return errf(http.StatusRequestEntityTooLarge, "request body too large")
	}
	return errf(http.StatusInternalServerError, "Internal server error")
}
