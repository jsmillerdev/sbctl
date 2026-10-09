package api

import (
	"errors"
	"net/http"

	"github.com/supavise/supavise/internal/oauth"
)

// The consent routes Studio's "Authorize API access" page calls: describe an authorization request,
// approve it and decline it. All three need a dashboard session (a personal access token or an
// OAuth token is refused before the handler runs) and no cookie, so a page of another site cannot
// make the browser approve anything.

// routesOAuthConsent registers GET /platform/oauth/authorizations/{id} and POST and DELETE
// /platform/organizations/{slug}/oauth/authorizations/{id}. routesOAuth calls it.
func (s *Server) routesOAuthConsent(add func(string, handlerFunc)) {
	add("GET /platform/oauth/authorizations/{id}", s.oauthDescribe)
	add("POST /platform/organizations/{slug}/oauth/authorizations/{id}", s.oauthApprove)
	add("DELETE /platform/organizations/{slug}/oauth/authorizations/{id}", s.oauthDecline)
}

var errAuthorizationNotFound = errf(http.StatusNotFound, "Authorization request not found")

// oauthConsentError gives an error of the Service the status the consent page expects.
func oauthConsentError(err error) error {
	switch {
	case errors.Is(err, oauth.ErrNotFound):
		return errAuthorizationNotFound
	case errors.Is(err, oauth.ErrAlreadyDecided):
		return errf(http.StatusConflict, "This authorization request was approved or declined already")
	case errors.Is(err, oauth.ErrExpired):
		return errf(http.StatusGone, "This authorization request has expired")
	case errors.Is(err, oauth.ErrOrgMismatch):
		return errf(http.StatusForbidden, "This authorization request was made for another organization")
	}
	return err
}

// oauthDescribeBody is what Studio's consent page reads. Optional fields are left out, as the
// spec has them optional; icon is never set for a dynamic app.
type oauthDescribeBody struct {
	Name                     string   `json:"name"`
	Website                  string   `json:"website"`
	Icon                     string   `json:"icon,omitempty"`
	Domain                   string   `json:"domain"`
	RedirectURI              string   `json:"redirect_uri"`
	ExpiresAt                string   `json:"expires_at"`
	ApprovedAt               string   `json:"approved_at,omitempty"`
	ApprovedOrganizationSlug string   `json:"approved_organization_slug,omitempty"`
	Scopes                   []string `json:"scopes"`
	RegistrationType         string   `json:"registration_type"`
}

// oauthDescribe answers GET /platform/oauth/authorizations/{id}: the application, the redirect host
// and the scopes the consent page shows. An expired or decided request is described too; Studio
// works out "expired" from expires_at.
func (s *Server) oauthDescribe(w http.ResponseWriter, r *http.Request) error {
	if s.oauthDisabled() {
		return errOAuthEndpointOff
	}
	// The route rule says the same (an Owner or Administrator somewhere). It is repeated here because
	// this page names the application that asks for access and the scopes it asks for: the route
	// table is the first line and this check the second. Owner decision 1 of the design (let any
	// member approve) changes both.
	a, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	if !a.IsOperatorAnywhere() {
		return errf(http.StatusForbidden, "Your role does not allow this action (needs the Owner or Administrator role)")
	}
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		return errAuthorizationNotFound
	}
	v, err := s.oauth.Describe(r.Context(), id)
	if err != nil {
		return oauthConsentError(err)
	}
	body := oauthDescribeBody{
		Name:                     v.Name,
		Website:                  v.Website,
		Icon:                     v.Icon,
		Domain:                   v.Domain,
		RedirectURI:              v.RedirectURI,
		ExpiresAt:                ts(v.ExpiresAt),
		ApprovedOrganizationSlug: v.ApprovedOrgSlug,
		Scopes:                   nonNil(v.Scopes),
		RegistrationType:         v.RegistrationType,
	}
	if v.ApprovedAt != nil {
		body.ApprovedAt = ts(*v.ApprovedAt)
	}
	if v.RegistrationType == oauth.RegistrationDynamic {
		// A logo a client asserts about itself would let anyone dress a request as another
		// application, and loading it tells the client who looked.
		body.Icon = ""
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, body)
	return nil
}

// oauthApprove answers POST /platform/organizations/{slug}/oauth/authorizations/{id}. The route rule
// has checked that the caller is an Owner or Administrator of the organization and, for a session
// below aal2, that the organization does not require MFA. skip_browser_redirect is accepted and
// ignored: the answer is always JSON, and Studio sends the browser to the URL in it.
func (s *Server) oauthApprove(w http.ResponseWriter, r *http.Request) error {
	if s.oauthDisabled() {
		return errOAuthEndpointOff
	}
	// An approval mints tokens that, like a personal access token, are exempt from the MFA
	// requirement afterwards, so the session that approves has to meet it in every organization of
	// the caller, not only the chosen one.
	if err := s.requireMintAAL(r); err != nil {
		return err
	}
	p := principalFrom(r.Context())
	if p == nil {
		return errUnauthorized
	}
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		return errAuthorizationNotFound
	}
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	res, err := s.oauth.Approve(r.Context(), oauth.ApproveRequest{AuthID: id, UserID: p.UserID, OrgID: org.ID, OrgSlug: org.Slug})
	if err != nil {
		return oauthConsentError(err)
	}
	// The URL holds the only copy of the authorization code. It goes to the page that sends the
	// browser there, and nowhere else: not a log line, not a cache.
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	writeJSON(w, http.StatusCreated, map[string]string{"url": res.RedirectURL})
	return nil
}

// oauthDecline answers DELETE /platform/organizations/{slug}/oauth/authorizations/{id}. Studio does
// not tell the client; the client gives up at its own timeout.
func (s *Server) oauthDecline(w http.ResponseWriter, r *http.Request) error {
	if s.oauthDisabled() {
		return errOAuthEndpointOff
	}
	p := principalFrom(r.Context())
	if p == nil {
		return errUnauthorized
	}
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		return errAuthorizationNotFound
	}
	if err := s.oauth.Decline(r.Context(), oauth.DeclineRequest{AuthID: id, UserID: p.UserID}); err != nil {
		return oauthConsentError(err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
	return nil
}
