package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/registry"
)

// The organization's OAuth Apps page (Studio's pages/org/[slug]/apps.tsx): the authorized and
// published lists, creating, updating, deleting and revoking an app, and an app's client secrets.
//
// The rules live in internal/oauth (Service.ListAuthorizedApps and the methods beside it); these
// handlers resolve the organization, parse the body, call the service with the caller's id as the
// actor and write the answer in the shape of the platform spec. The Service writes the audit events.
//
// Who may call: Owners and Administrators, for every operation, reads included. The rules of
// authz.go for /platform/organizations/{slug}/oauth/** ask for ActUpdate on oauth_apps to write,
// which only those two roles hold, and ActRead to read, which every role holds. The page lists the
// clients that act for the organization and the aliases of its secrets, which is for the people who
// manage them, so each handler asks for ActUpdate itself (oauthAppsCaller), as requireBranchData does
// for what the route table cannot say. The routes are under /platform, so only a dashboard session
// reaches them: a personal access token or an OAuth token is refused before the handler runs.
//
// A client secret leaves the server once, in the answer that creates it, with Cache-Control:
// no-store. Every other answer carries the alias ("sba_1a2b********") and never the hash.

// maxOAuthAppsBody bounds the JSON body of create and update: ten redirect URIs of 2 KiB, a name, a
// website, an icon and 24 scopes fit with room to spare.
const maxOAuthAppsBody = 64 << 10

// oauthAppsNilUUID stands in for created_by when a secret has no creator on record; the spec wants a uuid.
const oauthAppsNilUUID = "00000000-0000-0000-0000-000000000000"

// routesOAuthApps registers the eight operations OAuthApps* and OAuthAppClientSecrets* of the
// platform spec. It is called whether or not [api] disable_oauth is set; the handlers answer 404
// while it is (oauthRoutes).
func (s *Server) routesOAuthApps(add func(string, handlerFunc)) {
	add = s.oauthRoutes(add)
	const apps = "/platform/organizations/{slug}/oauth/apps"
	add("GET "+apps, s.listOAuthApps)
	add("POST "+apps, s.createOAuthApp)
	add("PUT "+apps+"/{id}", s.updateOAuthApp)
	add("DELETE "+apps+"/{id}", s.deleteOAuthApp)
	add("POST "+apps+"/{id}/revoke", s.revokeOAuthApp)
	add("GET "+apps+"/{app_id}/client-secrets", s.listOAuthClientSecrets)
	add("POST "+apps+"/{app_id}/client-secrets", s.createOAuthClientSecret)
	add("DELETE "+apps+"/{app_id}/client-secrets/{secret_id}", s.deleteOAuthClientSecret)
}

// ---- shared pieces ------------------------------------------------------------------------------

// oauthAppsCaller is the start of every handler (OAuth is on by then, see oauthRoutes): the
// organization of the path exists, a dashboard user is signed in and that user may manage the
// organization's OAuth apps.
func (s *Server) oauthAppsCaller(r *http.Request) (*registry.Organization, *Principal, error) {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return nil, nil, err
	}
	p := principalFrom(r.Context())
	if p == nil {
		return nil, nil, errUnauthorized
	}
	ok, err := s.can(r, org, "", members.ActUpdate, "oauth_apps")
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, errf(http.StatusForbidden, "Your role does not allow this action (OAuth apps need the Owner or Administrator role)")
	}
	return org, p, nil
}

// oauthAppsPathID reads a UUID from the path. The ids of apps and secrets are UUIDs; any other text
// names nothing (the Store treats it the same way), so it is a 404 with the message of the caller.
func oauthAppsPathID(r *http.Request, name, notFound string) (string, error) {
	id := r.PathValue(name)
	if !uuidRe.MatchString(id) {
		return "", errf(http.StatusNotFound, "%s", notFound)
	}
	return strings.ToLower(id), nil
}

// oauthAppsErr maps the Service's sentinel errors to the API's. notFound is the message of a 404.
// Anything else is returned as it is and becomes a 500 whose detail is logged, not sent.
func oauthAppsErr(w http.ResponseWriter, err error, notFound string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, oauth.ErrNotFound):
		return errf(http.StatusNotFound, "%s", notFound)
	case errors.Is(err, oauth.ErrInvalid):
		return errf(http.StatusBadRequest, "%s", oauthAppsWrapped(err, oauth.ErrInvalid, "Invalid request"))
	case errors.Is(err, oauth.ErrLimit):
		// A cap, not a rate: the wait is a hint to try again later, as the other 429 answers give.
		w.Header().Set("Retry-After", "60")
		return errf(http.StatusTooManyRequests, "%s", oauthAppsWrapped(err, oauth.ErrLimit, "Limit reached"))
	}
	return err
}

// oauthAppsWrapped is the text a Service error carries after its sentinel ("oauth: invalid request:
// <text>"), which the Service keeps safe to show, or fallback when it carries none.
func oauthAppsWrapped(err, sentinel error, fallback string) string {
	_, text, ok := strings.Cut(err.Error(), sentinel.Error()+": ")
	if !ok || strings.TrimSpace(text) == "" {
		return fallback
	}
	return text
}

// oauthAppsInput is the body of create and update. A pointer tells a missing field from an empty
// one: the spec requires all of name, website, scopes and redirect_uris. icon is optional.
type oauthAppsInput struct {
	Name         *string   `json:"name"`
	Website      *string   `json:"website"`
	Icon         *string   `json:"icon"`
	Scopes       *[]string `json:"scopes"`
	RedirectURIs *[]string `json:"redirect_uris"`
}

// readOAuthAppsInput reads and checks the shape of the body of create and update. What the values
// may be (lengths, schemes, the 24 scopes) is the Service's to judge: it answers ErrInvalid.
func readOAuthAppsInput(w http.ResponseWriter, r *http.Request) (*oauthAppsInput, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOAuthAppsBody))
	if err != nil {
		return nil, asError(err)
	}
	var in oauthAppsInput
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &in); err != nil {
			return nil, errf(http.StatusBadRequest, "invalid request body")
		}
	}
	switch {
	case in.Name == nil:
		return nil, errf(http.StatusBadRequest, "name is required")
	case in.Website == nil:
		return nil, errf(http.StatusBadRequest, "website is required")
	case in.Scopes == nil:
		return nil, errf(http.StatusBadRequest, "scopes is required")
	case in.RedirectURIs == nil:
		return nil, errf(http.StatusBadRequest, "redirect_uris is required")
	}
	return &in, nil
}

func (in *oauthAppsInput) icon() string {
	if in.Icon == nil {
		return ""
	}
	return *in.Icon
}

// ---- answers ------------------------------------------------------------------------------------

// oauthAppsWebsite and oauthAppsIcon are what the page may show of an app. A dynamic app describes
// itself (client_uri, logo_uri), so its website is left empty and its icon is never returned: a
// self-asserted logo spoofs consent pages and tracks viewers. A manual app's are the publisher's.
func oauthAppsWebsite(a oauth.App) string {
	if a.RegistrationType == oauth.RegistrationDynamic {
		return ""
	}
	return a.Website
}

func oauthAppsIcon(a oauth.App) string {
	if a.RegistrationType == oauth.RegistrationDynamic {
		return ""
	}
	return a.Icon
}

// oauthAppsItem is one app of either list.
func oauthAppsItem(a oauth.App) map[string]any {
	m := setAll(elem("GET /platform/organizations/{slug}/oauth/apps", ""), map[string]any{
		"id": a.ID, "client_id": a.ID, "name": a.Name, "website": oauthAppsWebsite(a),
		"registration_type": a.RegistrationType, "created_at": ts(a.CreatedAt),
		"redirect_uris": nonNil(a.RedirectURIs), "scopes": nonNil(a.Scopes),
	})
	if icon := oauthAppsIcon(a); icon != "" {
		m["icon"] = icon
	}
	return m
}

// oauthAppsAuthor is the "Author" cell of the Authorized tab, which prints created_by as text. A
// manual app was published by an organization of this server, and that organization is its author. A
// dynamic app has no author; the host of its first redirect URI says where it sends the codes, which
// is what an administrator reading the list wants to know. orgs caches the names of one request.
func (s *Server) oauthAppsAuthor(r *http.Request, a oauth.App, orgs map[int64]string) string {
	if a.RegistrationType == oauth.RegistrationDynamic {
		if len(a.RedirectURIs) == 0 {
			return ""
		}
		u, err := url.Parse(a.RedirectURIs[0])
		if err != nil {
			return ""
		}
		return u.Hostname()
	}
	if name, ok := orgs[a.OrgID]; ok {
		return name
	}
	name := ""
	if o, err := s.reg.GetOrganizationByID(r.Context(), a.OrgID); err == nil {
		name = o.Name
	}
	orgs[a.OrgID] = name
	return name
}

// oauthAppsSecretItem is a client secret as the listing and the create answer show it. The plaintext
// and the hash are never part of it.
func oauthAppsSecretItem(m map[string]any, sec oauth.AppSecret) map[string]any {
	createdBy := sec.CreatedBy
	if createdBy == "" {
		createdBy = oauthAppsNilUUID
	}
	var lastUsed any
	if sec.LastUsedAt != nil {
		lastUsed = ts(*sec.LastUsedAt)
	}
	return setAll(m, map[string]any{
		"id": sec.ID, "oauth_app_id": sec.AppID, "client_secret_alias": sec.Alias,
		"created_by": createdBy, "created_at": ts(sec.CreatedAt), "last_used_at": lastUsed,
	})
}

// ---- the lists ----------------------------------------------------------------------------------

// listOAuthApps serves both tabs of the page: type=authorized, the apps that hold a live grant in
// the organization (including dynamic apps such as an MCP client), and type=published, the manual
// apps the organization owns.
func (s *Server) listOAuthApps(w http.ResponseWriter, r *http.Request) error {
	org, _, err := s.oauthAppsCaller(r)
	if err != nil {
		return err
	}
	switch r.URL.Query().Get("type") {
	case "authorized":
		apps, err := s.oauth.ListAuthorizedApps(r.Context(), org.ID)
		if err != nil {
			return oauthAppsErr(w, err, "Organization not found")
		}
		orgs := map[int64]string{}
		out := make([]map[string]any, 0, len(apps))
		for _, it := range apps {
			m := oauthAppsItem(it.App)
			m["app_id"] = it.App.ID
			m["authorized_at"] = ts(it.AuthorizedAt)
			m["scopes"] = nonNil(it.Scopes)
			if author := s.oauthAppsAuthor(r, it.App, orgs); author != "" {
				m["created_by"] = author
			}
			out = append(out, m)
		}
		writeJSON(w, http.StatusOK, out)
	case "published":
		apps, err := s.oauth.ListPublishedApps(r.Context(), org.ID)
		if err != nil {
			return oauthAppsErr(w, err, "Organization not found")
		}
		out := make([]map[string]any, 0, len(apps))
		for _, a := range apps {
			m := oauthAppsItem(a)
			if a.CreatedBy != "" {
				m["created_by"] = a.CreatedBy
			}
			out = append(out, m)
		}
		writeJSON(w, http.StatusOK, out)
	default:
		return errf(http.StatusBadRequest, "type must be published or authorized")
	}
	return nil
}

// ---- publishing -----------------------------------------------------------------------------------

// createOAuthApp publishes a manual app with its first client secret, which this answer shows once.
func (s *Server) createOAuthApp(w http.ResponseWriter, r *http.Request) error {
	org, p, err := s.oauthAppsCaller(r)
	if err != nil {
		return err
	}
	in, err := readOAuthAppsInput(w, r)
	if err != nil {
		return err
	}
	created, err := s.oauth.CreateApp(r.Context(), oauth.CreateAppRequest{
		OrgID: org.ID, CreatedBy: p.UserID, Name: *in.Name, Website: *in.Website, Icon: in.icon(),
		Scopes: *in.Scopes, RedirectURIs: *in.RedirectURIs,
	})
	if err != nil {
		return oauthAppsErr(w, err, "Organization not found")
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, setAll(base("POST /platform/organizations/{slug}/oauth/apps"), map[string]any{
		"id": created.App.ID, "client_id": created.App.ID, "client_secret": created.ClientSecret,
		"client_secret_expires_at": 0, "redirect_uris": nonNil(created.App.RedirectURIs),
	}))
	return nil
}

// updateOAuthApp replaces a manual app's name, website, icon, scopes and redirect URIs. Grants that
// exist keep working, narrowed by the new scopes at use.
func (s *Server) updateOAuthApp(w http.ResponseWriter, r *http.Request) error {
	org, p, err := s.oauthAppsCaller(r)
	if err != nil {
		return err
	}
	id, err := oauthAppsPathID(r, "id", "OAuth app not found")
	if err != nil {
		return err
	}
	in, err := readOAuthAppsInput(w, r)
	if err != nil {
		return err
	}
	app, err := s.oauth.UpdateApp(r.Context(), oauth.UpdateAppRequest{
		OrgID: org.ID, AppID: id, Actor: p.UserID, Name: *in.Name, Website: *in.Website, Icon: in.icon(),
		Scopes: *in.Scopes, RedirectURIs: *in.RedirectURIs,
	})
	if err != nil {
		return oauthAppsErr(w, err, "OAuth app not found")
	}
	m := setAll(base("PUT /platform/organizations/{slug}/oauth/apps/{id}"), map[string]any{
		"id": app.ID, "client_id": app.ID, "created_at": ts(app.CreatedAt), "name": app.Name,
		"website": oauthAppsWebsite(*app), "redirect_uris": nonNil(app.RedirectURIs),
	})
	if icon := oauthAppsIcon(*app); icon != "" {
		m["icon"] = icon
	}
	writeJSON(w, http.StatusOK, m)
	return nil
}

// deleteOAuthApp deletes a manual app of the organization and, with it, every grant it holds. The
// answer describes the app as it was.
func (s *Server) deleteOAuthApp(w http.ResponseWriter, r *http.Request) error {
	org, p, err := s.oauthAppsCaller(r)
	if err != nil {
		return err
	}
	id, err := oauthAppsPathID(r, "id", "OAuth app not found")
	if err != nil {
		return err
	}
	app, err := s.oauth.DeleteApp(r.Context(), oauth.DeleteAppRequest{OrgID: org.ID, AppID: id, Actor: p.UserID})
	if err != nil {
		return oauthAppsErr(w, err, "OAuth app not found")
	}
	m := setAll(base("DELETE /platform/organizations/{slug}/oauth/apps/{id}"), map[string]any{
		"id": app.ID, "client_id": app.ID, "created_at": ts(app.CreatedAt), "name": app.Name,
		"website": oauthAppsWebsite(*app), "redirect_uris": nonNil(app.RedirectURIs),
	})
	if icon := oauthAppsIcon(*app); icon != "" {
		m["icon"] = icon
	}
	writeJSON(w, http.StatusOK, m)
	return nil
}

// revokeOAuthApp ends the grants an app holds in this organization, for every user (the Revoke
// button of the Authorized tab). It reaches any live app, dynamic or published, and no other
// organization's grants. Asking again once nothing is left to end is not an error: the answer is
// the same, without authorized_at.
func (s *Server) revokeOAuthApp(w http.ResponseWriter, r *http.Request) error {
	org, p, err := s.oauthAppsCaller(r)
	if err != nil {
		return err
	}
	id, err := oauthAppsPathID(r, "id", "OAuth app not found")
	if err != nil {
		return err
	}
	res, err := s.oauth.RevokeApp(r.Context(), oauth.RevokeAppRequest{
		AppID: id, OrgID: org.ID, Reason: oauth.ReasonAdmin, Actor: p.UserID,
	})
	if err != nil {
		return oauthAppsErr(w, err, "OAuth app not found")
	}
	m := setAll(base("POST /platform/organizations/{slug}/oauth/apps/{id}/revoke"), map[string]any{
		"id": res.App.ID, "name": res.App.Name, "website": oauthAppsWebsite(res.App),
	})
	if icon := oauthAppsIcon(res.App); icon != "" {
		m["icon"] = icon
	}
	if !res.AuthorizedAt.IsZero() {
		m["authorized_at"] = ts(res.AuthorizedAt)
	}
	writeJSON(w, http.StatusCreated, m)
	return nil
}

// ---- client secrets ------------------------------------------------------------------------------

// listOAuthClientSecrets lists a published app's secrets by alias.
func (s *Server) listOAuthClientSecrets(w http.ResponseWriter, r *http.Request) error {
	org, _, err := s.oauthAppsCaller(r)
	if err != nil {
		return err
	}
	appID, err := oauthAppsPathID(r, "app_id", "OAuth app not found")
	if err != nil {
		return err
	}
	secs, err := s.oauth.ListClientSecrets(r.Context(), org.ID, appID)
	if err != nil {
		return oauthAppsErr(w, err, "OAuth app not found")
	}
	const key = "GET /platform/organizations/{slug}/oauth/apps/{app_id}/client-secrets"
	rows := make([]map[string]any, 0, len(secs))
	for _, sec := range secs {
		rows = append(rows, oauthAppsSecretItem(elem(key, "client_secrets"), sec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"client_secrets": rows})
	return nil
}

// createOAuthClientSecret adds a secret to a published app. The plaintext is in this answer and
// nowhere else, and the answer must not be cached.
func (s *Server) createOAuthClientSecret(w http.ResponseWriter, r *http.Request) error {
	org, p, err := s.oauthAppsCaller(r)
	if err != nil {
		return err
	}
	appID, err := oauthAppsPathID(r, "app_id", "OAuth app not found")
	if err != nil {
		return err
	}
	res, err := s.oauth.CreateClientSecret(r.Context(), oauth.CreateSecretRequest{OrgID: org.ID, AppID: appID, CreatedBy: p.UserID})
	if err != nil {
		return oauthAppsErr(w, err, "OAuth app not found")
	}
	m := oauthAppsSecretItem(base("POST /platform/organizations/{slug}/oauth/apps/{app_id}/client-secrets"), res.Secret)
	m["client_secret"] = res.ClientSecret
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, m)
	return nil
}

// deleteOAuthClientSecret removes one secret of a published app. A client that authenticates with it
// is refused from then on.
func (s *Server) deleteOAuthClientSecret(w http.ResponseWriter, r *http.Request) error {
	org, p, err := s.oauthAppsCaller(r)
	if err != nil {
		return err
	}
	appID, err := oauthAppsPathID(r, "app_id", "OAuth app not found")
	if err != nil {
		return err
	}
	secretID, err := oauthAppsPathID(r, "secret_id", "Client secret not found")
	if err != nil {
		return err
	}
	if err := s.oauth.DeleteClientSecret(r.Context(), oauth.DeleteSecretRequest{
		OrgID: org.ID, AppID: appID, SecretID: secretID, Actor: p.UserID,
	}); err != nil {
		return oauthAppsErr(w, err, "Client secret not found")
	}
	w.WriteHeader(http.StatusOK)
	return nil
}
