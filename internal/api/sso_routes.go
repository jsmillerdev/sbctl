package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/OWNER/sbctl/internal/members"
	"github.com/OWNER/sbctl/internal/projectconfig"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/sso"
)

func (s *Server) routesSSO(add func(string, handlerFunc)) {
	// The organization's single sign-on, as Studio's organization settings page calls it: one
	// provider per organization, and "join the organization on sign-up" as the default role.
	add("GET /platform/organizations/{slug}/sso", s.orgSSOGet)
	add("POST /platform/organizations/{slug}/sso", s.orgSSOCreate)
	add("PUT /platform/organizations/{slug}/sso", s.orgSSOUpdate)
	add("DELETE /platform/organizations/{slug}/sso", s.orgSSODelete)
	// sbctl's own routes: several providers per organization, and the people waiting to be let in.
	add("GET /platform/organizations/{slug}/sso/providers", s.orgSSOProviders)
	add("POST /platform/organizations/{slug}/sso/providers", s.orgSSOProviderCreate)
	add("GET /platform/organizations/{slug}/sso/providers/{provider_id}", s.orgSSOProviderGet)
	add("PUT /platform/organizations/{slug}/sso/providers/{provider_id}", s.orgSSOProviderUpdate)
	add("DELETE /platform/organizations/{slug}/sso/providers/{provider_id}", s.orgSSOProviderDelete)
	add("GET /platform/organizations/{slug}/sso/pending", s.orgSSOPending)
	add("POST /platform/organizations/{slug}/sso/pending/{user_id}", s.orgSSOApprove)
	add("DELETE /platform/organizations/{slug}/sso/pending/{user_id}", s.orgSSODeny)

	// A project's own identity providers, proxied to the project's GoTrue.
	add("POST /v1/projects/{ref}/config/auth/sso/providers", s.projectSSOCreate)
	add("GET /v1/projects/{ref}/config/auth/sso/providers", s.projectSSOList)
	add("GET /v1/projects/{ref}/config/auth/sso/providers/{provider_id}", s.projectSSOGet)
	add("PUT /v1/projects/{ref}/config/auth/sso/providers/{provider_id}", s.projectSSOUpdate)
	add("DELETE /v1/projects/{ref}/config/auth/sso/providers/{provider_id}", s.projectSSODelete)
}

// ---- bodies ------------------------------------------------------------------

// ssoProviderBody is the body of sbctl's provider routes: the Management API's
// CreateProviderBody and UpdateProviderBody, and the role a first-time user gets.
type ssoProviderBody struct {
	Type             string                `json:"type"`
	MetadataURL      string                `json:"metadata_url"`
	MetadataXML      string                `json:"metadata_xml"`
	Domains          *[]string             `json:"domains"`
	AttributeMapping *sso.AttributeMapping `json:"attribute_mapping"`
	NameIDFormat     *string               `json:"name_id_format"`
	// DefaultRole is a role name or slug ("developer", "Read-only"); "" or "none": the user waits
	// for approval. Absent on an update: unchanged.
	DefaultRole *string `json:"default_role"`
}

func parseDefaultRole(s string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none":
		return 0, nil
	}
	r, err := members.ParseRole(s)
	if err != nil {
		return 0, errf(http.StatusBadRequest, "%v", err)
	}
	return r.ID, nil
}

// providerJSON is how sbctl's routes show a provider: the Management API's provider object plus
// where it belongs and what its first-time users get.
func providerJSON(p *DashboardProvider) map[string]any {
	out := map[string]any{
		"id": p.ID, "saml": p.SAML, "domains": p.Domains, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
		"registered": p.Registered,
	}
	if p.OrgSlug != "" {
		out["organization_slug"] = p.OrgSlug
	}
	if p.DefaultRole != 0 {
		out["default_role"] = members.RoleName(p.DefaultRole)
	} else {
		out["default_role"] = nil
	}
	return out
}

func (b *ssoProviderBody) metadata() (sso.Metadata, bool) {
	return sso.Metadata{URL: b.MetadataURL, XML: b.MetadataXML}, b.MetadataURL != "" || b.MetadataXML != ""
}

// ---- Studio's organization SSO page -------------------------------------------

// orgSSOBody is the body of POST and PUT /platform/organizations/{slug}/sso.
type orgSSOBody struct {
	Enabled                *bool    `json:"enabled"`
	MetadataXMLURL         string   `json:"metadata_xml_url"`
	MetadataXMLFile        string   `json:"metadata_xml_file"`
	Domains                []string `json:"domains"`
	EmailMapping           []string `json:"email_mapping"`
	UserNameMapping        []string `json:"user_name_mapping"`
	FirstNameMapping       []string `json:"first_name_mapping"`
	LastNameMapping        []string `json:"last_name_mapping"`
	JoinOrgOnSignupEnabled *bool    `json:"join_org_on_signup_enabled"`
	JoinOrgOnSignupRole    string   `json:"join_org_on_signup_role"`
}

func (b *orgSSOBody) attributeMapping() *sso.AttributeMapping {
	keys := map[string]sso.Attribute{}
	for k, names := range map[string][]string{"email": b.EmailMapping, "user_name": b.UserNameMapping, "first_name": b.FirstNameMapping, "last_name": b.LastNameMapping} {
		if len(names) > 0 {
			keys[k] = sso.Attribute{Names: names}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return &sso.AttributeMapping{Keys: keys}
}

// defaultRole is the role of a first-time user: the role of join_org_on_signup_role while
// join_org_on_signup_enabled is on, none otherwise.
func (b *orgSSOBody) defaultRole() (int, error) {
	if b.JoinOrgOnSignupEnabled == nil || !*b.JoinOrgOnSignupEnabled {
		return 0, nil
	}
	return parseDefaultRole(b.JoinOrgOnSignupRole)
}

// orgProvider returns the first provider of the organization, errNoOrgSSO when there is none.
// Studio's page shows "not set up" for a 404 whose message contains this text.
func (s *Server) orgProvider(ctx context.Context, org *registry.Organization) (*DashboardProvider, error) {
	ps, err := s.sso.List(ctx, org.ID)
	if err != nil {
		return nil, err
	}
	if len(ps) == 0 {
		return nil, errf(http.StatusNotFound, "Failed to find an existing SSO Provider for this organization")
	}
	return s.sso.Get(ctx, ps[0].ID, org.ID)
}

func attributeNames(m *sso.AttributeMapping, key string) []string {
	if m == nil {
		return []string{}
	}
	a, ok := m.Keys[key]
	switch {
	case !ok:
		return []string{}
	case len(a.Names) > 0:
		return a.Names
	case a.Name != "":
		return []string{a.Name}
	}
	return []string{}
}

// orgSSOView is the page's view of a provider: GET, POST and PUT answer with it.
func orgSSOView(p *DashboardProvider) map[string]any {
	var am *sso.AttributeMapping
	file, url := "", ""
	if p.SAML != nil {
		am, file, url = p.SAML.AttributeMapping, p.SAML.MetadataXML, p.SAML.MetadataURL
	}
	email := attributeNames(am, "email")
	if len(email) == 0 {
		email = []string{"email"}
	}
	role := "None"
	if p.DefaultRole != 0 {
		role = members.RoleName(p.DefaultRole)
	}
	domains := p.DomainNames()
	if domains == nil {
		domains = []string{}
	}
	out := map[string]any{
		"enabled": p.Disabled == nil || !*p.Disabled, "metadata_xml_file": file, "domains": domains, "email_mapping": email,
		"user_name_mapping": attributeNames(am, "user_name"), "first_name_mapping": attributeNames(am, "first_name"),
		"last_name_mapping":          attributeNames(am, "last_name"),
		"join_org_on_signup_enabled": p.DefaultRole != 0, "join_org_on_signup_role": role,
		"idjag_issuer_url": nil,
	}
	if url != "" {
		out["metadata_xml_url"] = url
	}
	return out
}

func (s *Server) orgSSOGet(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	p, err := s.orgProvider(r.Context(), org)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, orgSSOView(p))
	return nil
}

func (s *Server) orgSSOCreate(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	var in orgSSOBody
	if err := decode(r, &in); err != nil {
		return err
	}
	if ps, err := s.sso.List(r.Context(), org.ID); err != nil {
		return err
	} else if len(ps) > 0 {
		return errf(http.StatusConflict, "This organization has a single sign-on provider already; update it, or add another with POST .../sso/providers")
	}
	role, err := in.defaultRole()
	if err != nil {
		return err
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	md := sso.Metadata{URL: in.MetadataXMLURL, XML: in.MetadataXMLFile}
	if md.URL != "" {
		md.XML = "" // the address wins when the page sends both: GoTrue refreshes it
	}
	p, err := s.sso.Add(r.Context(), actor, AddProvider{
		Org: orgRef(org), Metadata: md,
		Domains: in.Domains, DefaultRole: role, AttributeMapping: in.attributeMapping(), CreatedBy: principalFrom(r.Context()).UserID,
		Disabled: in.Enabled != nil && !*in.Enabled,
	})
	if err != nil {
		return err
	}
	// Add left the metadata as given; the page wants the document back.
	if full, err := s.sso.Get(r.Context(), p.ID, org.ID); err == nil {
		p = full
	}
	writeJSON(w, http.StatusCreated, orgSSOView(p))
	return nil
}

func (s *Server) orgSSOUpdate(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	var in orgSSOBody
	if err := decode(r, &in); err != nil {
		return err
	}
	cur, err := s.orgProvider(r.Context(), org)
	if err != nil {
		return err
	}
	role, err := in.defaultRole()
	if err != nil {
		return err
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	up := UpdateProvider{DefaultRole: &role, AttributeMapping: in.attributeMapping()}
	if in.Enabled != nil {
		disabled := !*in.Enabled
		up.Disabled = &disabled
	}
	if in.Domains != nil {
		up.Domains = &in.Domains
	}
	if in.MetadataXMLURL != "" || in.MetadataXMLFile != "" {
		// The page sends back what it was shown (the address, and the document GoTrue fetched
		// from it): metadata that did not change is not a change.
		curURL, curXML := "", ""
		if cur.SAML != nil {
			curURL, curXML = cur.SAML.MetadataURL, cur.SAML.MetadataXML
		}
		var md sso.Metadata
		switch {
		case in.MetadataXMLURL != "" && in.MetadataXMLURL != curURL:
			md = sso.Metadata{URL: in.MetadataXMLURL}
		case in.MetadataXMLURL == "" && in.MetadataXMLFile != curXML:
			md = sso.Metadata{XML: in.MetadataXMLFile}
		}
		if md != (sso.Metadata{}) {
			up.Metadata = &md
		}
	}
	p, err := s.sso.Update(r.Context(), actor, cur.ID, org.ID, up)
	if err != nil {
		return err
	}
	if full, err := s.sso.Get(r.Context(), p.ID, org.ID); err == nil {
		p = full
	}
	writeJSON(w, http.StatusOK, orgSSOView(p))
	return nil
}

func (s *Server) orgSSODelete(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	cur, err := s.orgProvider(r.Context(), org)
	if err != nil {
		return err
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	if _, err := s.sso.Remove(r.Context(), actor, cur.ID, org.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// ---- sbctl's organization routes ---------------------------------------------

func (s *Server) orgSSOProviders(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	ps, err := s.sso.List(r.Context(), org.ID)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		items = append(items, providerJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "service_provider": s.sso.ServiceProvider()})
	return nil
}

func (s *Server) orgSSOProviderCreate(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	var in ssoProviderBody
	if err := decode(r, &in); err != nil {
		return err
	}
	md, ok := in.metadata()
	if !ok {
		return errf(http.StatusBadRequest, "metadata_url or metadata_xml is required")
	}
	role := 0
	if in.DefaultRole != nil {
		if role, err = parseDefaultRole(*in.DefaultRole); err != nil {
			return err
		}
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	add := AddProvider{Org: orgRef(org), Metadata: md, DefaultRole: role, AttributeMapping: in.AttributeMapping, CreatedBy: principalFrom(r.Context()).UserID}
	if in.Domains != nil {
		add.Domains = *in.Domains
	}
	if in.NameIDFormat != nil {
		add.NameIDFormat = *in.NameIDFormat
	}
	p, err := s.sso.Add(r.Context(), actor, add)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, providerJSON(p))
	return nil
}

func (s *Server) orgSSOProviderGet(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	p, err := s.sso.Get(r.Context(), r.PathValue("provider_id"), org.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, providerJSON(p))
	return nil
}

func (s *Server) orgSSOProviderUpdate(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	var in ssoProviderBody
	if err := decode(r, &in); err != nil {
		return err
	}
	up := UpdateProvider{Domains: in.Domains, AttributeMapping: in.AttributeMapping, NameIDFormat: in.NameIDFormat}
	if md, ok := in.metadata(); ok {
		up.Metadata = &md
	}
	if in.DefaultRole != nil {
		role, err := parseDefaultRole(*in.DefaultRole)
		if err != nil {
			return err
		}
		up.DefaultRole = &role
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	p, err := s.sso.Update(r.Context(), actor, r.PathValue("provider_id"), org.ID, up)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, providerJSON(p))
	return nil
}

func (s *Server) orgSSOProviderDelete(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	p, err := s.sso.Remove(r.Context(), actor, r.PathValue("provider_id"), org.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, providerJSON(p))
	return nil
}

func (s *Server) orgSSOPending(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	us, err := s.sso.Pending(r.Context(), org.ID)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(us))
	for _, u := range us {
		items = append(items, map[string]any{
			"user_id": u.UserID, "email": u.Email, "provider_id": u.ProviderID,
			"first_seen": u.FirstSeen.UTC().Format(time.RFC3339), "last_seen": u.LastSeen.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Server) orgSSOApprove(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	var in struct {
		Role   string `json:"role"`
		RoleID int    `json:"role_id"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	role := in.RoleID
	if in.Role != "" {
		ro, err := members.ParseRole(in.Role)
		if err != nil {
			return errf(http.StatusBadRequest, "%v", err)
		}
		role = ro.ID
	}
	if role == 0 {
		return errf(http.StatusBadRequest, "role or role_id is required: the role the person joins with")
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	if err := s.sso.Approve(r.Context(), actor, orgRef(org), r.PathValue("user_id"), role); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"user_id": r.PathValue("user_id"), "role": members.RoleName(role)})
	return nil
}

func (s *Server) orgSSODeny(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	if err := s.sso.Deny(r.Context(), actor, orgRef(org), r.PathValue("user_id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// ---- a project's identity providers ------------------------------------------

// projectSSO returns the client of the project's GoTrue. The project must run, and have SAML
// enabled in its Auth settings (saml_enabled), as on hosted (the specs' 404: "SAML 2.0 support is
// not enabled for this project"). The provider endpoints of GoTrue work whether or not SAML is
// on, so the setting is checked here.
func (s *Server) projectSSO(ctx context.Context, ref string) (*sso.Client, error) {
	p, err := s.running(ctx, ref)
	if err != nil {
		return nil, err
	}
	st, err := s.settings.Get(ctx, p.Ref, projectconfig.Auth)
	if err != nil {
		return nil, err
	}
	if !st.Effective.Bool("saml_enabled") {
		return nil, errf(http.StatusNotFound, "SAML 2.0 support is not enabled for this project")
	}
	keys, err := s.mgr.Keys(ctx, p.Ref)
	if err != nil {
		return nil, err
	}
	return &sso.Client{BaseURL: s.upstream(p, upGoTrue), ServiceKey: keys.ServiceRoleKey, HTTP: s.hc}, nil
}

// projectSSOError maps GoTrue's refusal to the Management API's.
func (s *Server) projectSSOError(err error) error {
	var ae *sso.APIError
	if errors.As(err, &ae) {
		return ssoError(err)
	}
	s.log.Error("a project's GoTrue is unavailable", "err", err)
	return errf(http.StatusBadGateway, "auth is unavailable")
}

// projectProviderJSON is the provider as the Management API shows it: GoTrue's own object
// without the fields the specs do not have.
func projectProviderJSON(p *sso.Provider) *sso.Provider {
	q := *p
	q.Disabled = nil
	return q.Normalize()
}

func (s *Server) projectSSOList(w http.ResponseWriter, r *http.Request) error {
	c, err := s.projectSSO(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	ps, err := c.List(r.Context())
	if err != nil {
		return s.projectSSOError(err)
	}
	// GoTrue leaves the metadata document out of its list; the Management API's list has it.
	for i, p := range ps {
		if full, err := c.Get(r.Context(), p.ID); err == nil {
			p = full
		}
		ps[i] = projectProviderJSON(p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ps})
	return nil
}

func (s *Server) projectSSOCreate(w http.ResponseWriter, r *http.Request) error {
	c, err := s.projectSSO(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in sso.CreateBody
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Type != "saml" {
		return errf(http.StatusBadRequest, "Only 'saml' supported for SSO provider type")
	}
	if in.MetadataURL == "" && in.MetadataXML == "" {
		return errf(http.StatusBadRequest, "Either metadata_xml or metadata_url must be set")
	}
	p, err := c.Create(r.Context(), in)
	if err != nil {
		return s.projectSSOError(err)
	}
	writeJSON(w, http.StatusCreated, projectProviderJSON(p))
	return nil
}

func (s *Server) projectSSOGet(w http.ResponseWriter, r *http.Request) error {
	c, err := s.projectSSO(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	p, err := c.Get(r.Context(), r.PathValue("provider_id"))
	if err != nil {
		return s.projectSSOError(err)
	}
	writeJSON(w, http.StatusOK, projectProviderJSON(p))
	return nil
}

func (s *Server) projectSSOUpdate(w http.ResponseWriter, r *http.Request) error {
	c, err := s.projectSSO(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in struct {
		MetadataURL      string                `json:"metadata_url"`
		MetadataXML      string                `json:"metadata_xml"`
		Domains          *[]string             `json:"domains"`
		AttributeMapping *sso.AttributeMapping `json:"attribute_mapping"`
		NameIDFormat     *string               `json:"name_id_format"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	p, err := c.Update(r.Context(), r.PathValue("provider_id"), sso.UpdateBody{
		MetadataURL: in.MetadataURL, MetadataXML: in.MetadataXML, Domains: in.Domains,
		AttributeMapping: in.AttributeMapping, NameIDFormat: in.NameIDFormat,
	})
	if err != nil {
		return s.projectSSOError(err)
	}
	writeJSON(w, http.StatusOK, projectProviderJSON(p))
	return nil
}

func (s *Server) projectSSODelete(w http.ResponseWriter, r *http.Request) error {
	c, err := s.projectSSO(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	p, err := c.Delete(r.Context(), r.PathValue("provider_id"))
	if err != nil {
		return s.projectSSOError(err)
	}
	writeJSON(w, http.StatusOK, projectProviderJSON(p))
	return nil
}
