package main

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// The three calls behind Studio's /authorize page, the consent screen for OAuth sign-in of MCP
// clients: describe a pending authorization request, approve it for an organization, decline it.
// The requests are listed in the config (`authorizations`), because the mock has no authorization
// server to make them. Approving answers with the redirect URL the client would get, which the
// browser then follows to callbackPath on the mock itself.

// callbackPath stands in for the MCP client's redirect URI target.
const callbackPath = "/oauth-callback"

// Authorization is one pending authorization request the mock serves.
type Authorization struct {
	// ID is the auth_id in the page's URL.
	ID string `json:"id"`
	// Name is the client's name as it registered it.
	Name string `json:"name"`
	// RedirectURI is where approving sends the browser: http or https, with the code, the state
	// and the issuer added to its query.
	RedirectURI string `json:"redirect_uri"`
	// Scopes are shown on the consent page (the default is a read and write database set).
	Scopes []string `json:"scopes"`
}

var authorizationIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

func (a *Authorization) normalize() error {
	if !authorizationIDRe.MatchString(a.ID) {
		return fmt.Errorf("authorization id %q must be 8 to 64 letters, digits or dashes", a.ID)
	}
	if a.Name == "" {
		return fmt.Errorf("authorization %s: name is required", a.ID)
	}
	u, err := url.Parse(a.RedirectURI)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Fragment != "" {
		return fmt.Errorf("authorization %s: redirect_uri %q must be an http(s) URL without a fragment", a.ID, a.RedirectURI)
	}
	if len(a.Scopes) == 0 {
		a.Scopes = []string{"organizations:read", "projects:read", "database:read", "database:write"}
	}
	return nil
}

// authzState is a request and what happened to it.
type authzState struct {
	Authorization
	approvedAt time.Time
	orgSlug    string
}

func (s *server) registerOAuth() {
	s.handle("GET", "/platform/oauth/authorizations/{id}", s.describeAuthorization)
	s.handle("POST", "/platform/organizations/{slug}/oauth/authorizations/{id}", s.withOrg(s.approveAuthorization))
	s.handle("DELETE", "/platform/organizations/{slug}/oauth/authorizations/{id}", s.withOrg(s.declineAuthorization))
}

func (s *server) describeAuthorization(w *respWriter, r *http.Request, c *reqCtx) {
	s.mu.Lock()
	a, ok := s.authz[c.params["id"]]
	var out map[string]any
	if ok {
		out = a.describe()
	}
	s.mu.Unlock()
	if !ok {
		w.message(http.StatusNotFound, "Authorization request not found")
		return
	}
	w.json(http.StatusOK, out)
}

// describe is the page's view: hosted lists the requester's domain next to its name, and a
// dynamically registered client has no icon and no website.
func (a *authzState) describe() map[string]any {
	u, _ := url.Parse(a.RedirectURI)
	out := map[string]any{
		"name": a.Name, "website": "", "icon": nil, "domain": u.Hostname(), "redirect_uri": a.RedirectURI,
		"scopes": a.Scopes, "expires_at": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		"approved_at": nil, "registration_type": "dynamic",
	}
	if !a.approvedAt.IsZero() {
		out["approved_at"] = a.approvedAt.UTC().Format(time.RFC3339)
		out["approved_organization_slug"] = a.orgSlug
	}
	return out
}

func (s *server) approveAuthorization(w *respWriter, r *http.Request, c *reqCtx) {
	s.mu.Lock()
	a, ok := s.authz[c.params["id"]]
	decided := ok && !a.approvedAt.IsZero()
	if ok && !decided {
		a.approvedAt, a.orgSlug = time.Now(), c.params["slug"]
	}
	s.mu.Unlock()
	switch {
	case !ok:
		w.message(http.StatusNotFound, "Authorization request not found")
		return
	case decided:
		w.message(http.StatusConflict, "Authorization request was already approved")
		return
	}
	// The same answer the real server gives with skip_browser_redirect: the URL to go to.
	u, _ := url.Parse(a.RedirectURI)
	q := u.Query()
	q.Set("code", "mock-code")
	q.Set("state", "mock-state")
	q.Set("iss", s.cfg.Scheme+"://"+r.Host)
	u.RawQuery = q.Encode()
	w.json(http.StatusCreated, map[string]any{"url": u.String()})
}

func (s *server) declineAuthorization(w *respWriter, r *http.Request, c *reqCtx) {
	id := c.params["id"]
	s.mu.Lock()
	a, ok := s.authz[id]
	approved := ok && !a.approvedAt.IsZero()
	if ok && !approved {
		delete(s.authz, id)
	}
	s.mu.Unlock()
	switch {
	case !ok:
		w.message(http.StatusNotFound, "Authorization request not found")
	case approved:
		w.message(http.StatusConflict, "Authorization request was already approved")
	default:
		w.json(http.StatusOK, map[string]any{"id": id})
	}
}

// serveCallback is the page the browser lands on after approving: the client's redirect target.
func serveCallback(w *respWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("<!doctype html><title>Authorization received</title><p>Authorization received.</p>\n"))
}
