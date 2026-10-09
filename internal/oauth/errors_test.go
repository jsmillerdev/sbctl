package oauth

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestErrorStatus(t *testing.T) {
	cases := map[string]int{
		CodeInvalidRequest:          http.StatusBadRequest,
		CodeInvalidClient:           http.StatusUnauthorized,
		CodeInvalidGrant:            http.StatusBadRequest,
		CodeUnsupportedGrantType:    http.StatusBadRequest,
		CodeUnsupportedResponseType: http.StatusBadRequest,
		CodeInvalidScope:            http.StatusBadRequest,
		CodeInvalidTarget:           http.StatusBadRequest,
		CodeInvalidClientMetadata:   http.StatusBadRequest,
		CodeInvalidRedirectURI:      http.StatusBadRequest,
		CodeServerError:             http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := NewError(code, "x").HTTPStatus(); got != want {
			t.Errorf("%s: status %d, want %d", code, got, want)
		}
	}
	if e := (&Error{Code: CodeInvalidGrant, Status: 418}); e.HTTPStatus() != 418 {
		t.Error("an explicit status wins")
	}
	if got := Errorf(CodeInvalidScope, "scope %q", "x").Error(); got != `invalid_scope: scope "x"` {
		t.Error(got)
	}
	if NewError(CodeInvalidRequest, "").Error() != "invalid_request" {
		t.Error("no description")
	}
}

func TestReuseError(t *testing.T) {
	var err error = &ReuseError{GrantID: 7, AppID: "a", UserID: "u", OrgID: 3}
	wrapped := fmt.Errorf("rotate: %w", err)
	if !errors.Is(wrapped, ErrRefreshReused) {
		t.Fatal("a *ReuseError matches ErrRefreshReused")
	}
	var re *ReuseError
	if !errors.As(wrapped, &re) || re.GrantID != 7 {
		t.Fatal("errors.As finds the grant")
	}
	if errors.Is(wrapped, ErrNotFound) {
		t.Fatal("it is not ErrNotFound")
	}
}

func TestRedirectError(t *testing.T) {
	var err error = &RedirectError{RedirectURI: "http://127.0.0.1:1/cb", State: "s", Err: NewError(CodeInvalidScope, "no scope left")}
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != CodeInvalidScope {
		t.Fatal("a *RedirectError unwraps to its *Error")
	}
}

func TestValidReason(t *testing.T) {
	for _, r := range []string{ReasonUser, ReasonAdmin, ReasonOperator, ReasonClient, ReasonAppDeleted, ReasonSuperseded,
		ReasonRefreshReuse, ReasonCodeReuse, ReasonMembership, ReasonUserRemoved} {
		if !ValidReason(r) {
			t.Errorf("%q must be valid", r)
		}
	}
	if ValidReason("") || ValidReason("other") {
		t.Error("only the constants are valid reasons")
	}
}

func TestGrantFilterIsEmpty(t *testing.T) {
	if !(GrantFilter{}).IsEmpty() || !(GrantFilter{Live: true, All: true, Limit: 5}).IsEmpty() {
		t.Error("Live, All and Limit do not select grants")
	}
	for _, f := range []GrantFilter{{ID: 1}, {AppID: "a"}, {UserID: "u"}, {OrgID: 2}} {
		if f.IsEmpty() {
			t.Errorf("%+v selects grants", f)
		}
	}
}
