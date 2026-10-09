package storetest

import (
	"bytes"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/oauth"
)

// The check functions compare field by field: a time is equal when it is the same instant (a
// database returns another location), a nil and an empty slice are equal, and a nil and a zero
// optional value are equal.

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func eqStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func eqBytes(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Errorf("%s = %x, want %x", what, got, want)
	}
}

func eqTime(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func eqTimePtr(t *testing.T, what string, got *time.Time, want time.Time) {
	t.Helper()
	switch {
	case want.IsZero() && got != nil:
		t.Errorf("%s = %v, want nil", what, *got)
	case !want.IsZero() && got == nil:
		t.Errorf("%s = nil, want %v", what, want)
	case !want.IsZero() && !got.Equal(want):
		t.Errorf("%s = %v, want %v", what, *got, want)
	}
}

func tp(t time.Time) *time.Time { return &t }

func ptrTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

func checkApp(t *testing.T, got *oauth.App, want oauth.App) {
	t.Helper()
	if got == nil {
		t.Fatal("app is nil")
	}
	eq(t, "app.ID", got.ID, want.ID)
	eq(t, "app.RegistrationType", got.RegistrationType, want.RegistrationType)
	eq(t, "app.OrgID", got.OrgID, want.OrgID)
	eq(t, "app.Name", got.Name, want.Name)
	eq(t, "app.Website", got.Website, want.Website)
	eq(t, "app.Icon", got.Icon, want.Icon)
	eqStrings(t, "app.RedirectURIs", got.RedirectURIs, want.RedirectURIs)
	eqStrings(t, "app.Scopes", got.Scopes, want.Scopes)
	eq(t, "app.TokenEndpointAuthMethod", got.TokenEndpointAuthMethod, want.TokenEndpointAuthMethod)
	eq(t, "app.CreatedBy", got.CreatedBy, want.CreatedBy)
	eqTime(t, "app.CreatedAt", got.CreatedAt, want.CreatedAt)
	eqTime(t, "app.UpdatedAt", got.UpdatedAt, want.UpdatedAt)
	eqTimePtr(t, "app.LastAuthorizedAt", got.LastAuthorizedAt, ptrTime(want.LastAuthorizedAt))
	eqTimePtr(t, "app.DeletedAt", got.DeletedAt, ptrTime(want.DeletedAt))
}

func checkSecret(t *testing.T, got oauth.AppSecret, want oauth.AppSecret) {
	t.Helper()
	eq(t, "secret.ID", got.ID, want.ID)
	eq(t, "secret.AppID", got.AppID, want.AppID)
	eq(t, "secret.Alias", got.Alias, want.Alias)
	eqBytes(t, "secret.Hash", got.Hash, want.Hash)
	eq(t, "secret.CreatedBy", got.CreatedBy, want.CreatedBy)
	eqTime(t, "secret.CreatedAt", got.CreatedAt, want.CreatedAt)
	eqTimePtr(t, "secret.LastUsedAt", got.LastUsedAt, ptrTime(want.LastUsedAt))
}

// checkAuth compares every field of an Authorization except OrgSlug, which env.checkSlug covers.
func checkAuth(t *testing.T, got *oauth.Authorization, want oauth.Authorization) {
	t.Helper()
	if got == nil {
		t.Fatal("authorization is nil")
	}
	eq(t, "auth.ID", got.ID, want.ID)
	eq(t, "auth.AppID", got.AppID, want.AppID)
	eq(t, "auth.RedirectURI", got.RedirectURI, want.RedirectURI)
	eqStrings(t, "auth.Scopes", got.Scopes, want.Scopes)
	eq(t, "auth.State", got.State, want.State)
	eq(t, "auth.CodeChallenge", got.CodeChallenge, want.CodeChallenge)
	eq(t, "auth.Resource", got.Resource, want.Resource)
	eq(t, "auth.OrgHint", got.OrgHint, want.OrgHint)
	eqTime(t, "auth.CreatedAt", got.CreatedAt, want.CreatedAt)
	eqTime(t, "auth.ExpiresAt", got.ExpiresAt, want.ExpiresAt)
	eq(t, "auth.Status", got.Status, want.Status)
	eq(t, "auth.DecidedBy", got.DecidedBy, want.DecidedBy)
	eqTimePtr(t, "auth.DecidedAt", got.DecidedAt, ptrTime(want.DecidedAt))
	eq(t, "auth.OrgID", got.OrgID, want.OrgID)
	eqBytes(t, "auth.CodeHash", got.CodeHash, want.CodeHash)
	eqTimePtr(t, "auth.CodeExpiresAt", got.CodeExpiresAt, ptrTime(want.CodeExpiresAt))
	eqTimePtr(t, "auth.CodeUsedAt", got.CodeUsedAt, ptrTime(want.CodeUsedAt))
	eq(t, "auth.GrantID", got.GrantID, want.GrantID)
}

func checkGrant(t *testing.T, got *oauth.Grant, want oauth.Grant) {
	t.Helper()
	if got == nil {
		t.Fatal("grant is nil")
	}
	eq(t, "grant.ID", got.ID, want.ID)
	eq(t, "grant.AppID", got.AppID, want.AppID)
	eq(t, "grant.UserID", got.UserID, want.UserID)
	eq(t, "grant.OrgID", got.OrgID, want.OrgID)
	eqStrings(t, "grant.Scopes", got.Scopes, want.Scopes)
	eq(t, "grant.Resource", got.Resource, want.Resource)
	eqTime(t, "grant.CreatedAt", got.CreatedAt, want.CreatedAt)
	eqTimePtr(t, "grant.LastUsedAt", got.LastUsedAt, ptrTime(want.LastUsedAt))
	eqTimePtr(t, "grant.RevokedAt", got.RevokedAt, ptrTime(want.RevokedAt))
	eq(t, "grant.RevokedReason", got.RevokedReason, want.RevokedReason)
}

// revoked returns g as a store shows it after a revocation.
func revoked(g oauth.Grant, reason string, at time.Time) oauth.Grant {
	g.RevokedAt, g.RevokedReason = tp(at), reason
	return g
}

// ids returns the ids of grants, in order.
func ids(gs []oauth.Grant) []int64 {
	out := make([]int64, len(gs))
	for i, g := range gs {
		out[i] = g.ID
	}
	return out
}

func infoIDs(gs []oauth.GrantInfo) []int64 {
	out := make([]int64, len(gs))
	for i, g := range gs {
		out[i] = g.Grant.ID
	}
	return out
}

func eqIDs(t *testing.T, what string, got, want []int64) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// sortedIDs returns the ids of grants in ascending order: the order of rows a revocation
// returns is not part of the contract.
func sortedIDs(gs []oauth.Grant) []int64 {
	out := ids(gs)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortGrants(gs []oauth.Grant) {
	sort.Slice(gs, func(i, j int) bool { return gs[i].ID < gs[j].ID })
}
