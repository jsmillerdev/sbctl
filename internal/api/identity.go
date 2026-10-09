package api

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strings"

	plat "github.com/supavise/supavise/internal/api/gen/platform"
	v1 "github.com/supavise/supavise/internal/api/gen/v1"
	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
)

// disabledFeatures are the dashboard features hidden through profile.disabled_features:
// billing, and the hosted-only infrastructure and integrations supavise does not offer.
// Keys are Studio's enabled-features keys (packages/common/enabled-features).
var disabledFeatures = []string{
	"billing:all", "database:replication", "database:restore_to_new_project", "database:network_restrictions",
	featureReadReplicas, "integrations:vercel", "integrations:aws_private_link", "integrations:partners",
	"organization:show_sso_settings", "project_addons:dedicated_ipv4_address", "project_addons:show_compute_price",
	"project_creation:show_high_availability", "project_settings:custom_domains", "project_settings:log_drains",
}

// featureReadReplicas is the key that hides Studio's read replica screens (the Infrastructure
// page, the database selector, the replica rows of the reports). It stays in disabledFeatures
// until a second server has joined (replicasOffered).
const featureReadReplicas = "infrastructure:read_replicas"

func (s *Server) routesProfile(add func(string, handlerFunc)) {
	add("GET /v1/profile", s.v1Profile)
	add("GET /platform/profile", s.platformProfile)
	add("POST /platform/profile", s.platformProfile)
	add("PATCH /platform/profile", s.platformUpdateProfile)
}

// currentUser returns the stored user behind the request's credentials.
func (s *Server) currentUser(r *http.Request) (*User, error) {
	p := principalFrom(r.Context())
	if p == nil {
		return nil, errUnauthorized
	}
	u, err := s.store.GetUser(r.Context(), p.UserID)
	if err == ErrNotFound {
		email := p.UserEmail()
		return s.store.UpsertUser(r.Context(), User{UserID: p.UserID, Email: email, Username: strings.SplitN(email, "@", 2)[0]})
	}
	return u, err
}

func (s *Server) v1Profile(w http.ResponseWriter, r *http.Request) error {
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, &v1.V1ProfileResponseOutput{GotrueId: u.UserID, PrimaryEmail: u.Email, Username: u.Username})
	return nil
}

func (s *Server) platformProfile(w http.ResponseWriter, r *http.Request) error {
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	writeJSON(w, status, s.profileOf(r.Context(), u))
	return nil
}

func (s *Server) profileOf(ctx context.Context, u *User) *plat.ProfileResponseOutput {
	opt := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	disabled := append([]string(nil), disabledFeatures...)
	if s.replicasOffered(ctx) {
		disabled = slices.DeleteFunc(disabled, func(f string) bool { return f == featureReadReplicas })
	}
	return &plat.ProfileResponseOutput{
		Auth0Id: u.UserID, DisabledFeatures: &disabled, FirstName: opt(u.FirstName), LastName: opt(u.LastName),
		GotrueId: u.UserID, Id: float32(u.ID), PrimaryEmail: u.Email, Username: u.Username,
	}
}

func (s *Server) platformUpdateProfile(w http.ResponseWriter, r *http.Request) error {
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	var in struct {
		FirstName *string `json:"first_name"`
		LastName  *string `json:"last_name"`
		Username  *string `json:"username"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.FirstName != nil {
		u.FirstName = *in.FirstName
	}
	if in.LastName != nil {
		u.LastName = *in.LastName
	}
	if in.Username != nil && *in.Username != "" {
		u.Username = *in.Username
	}
	if err := s.store.UpdateUser(r.Context(), u); err != nil {
		return mapErr(err)
	}
	writeJSON(w, http.StatusOK, s.profileOf(r.Context(), u))
	return nil
}

// allOrgs returns the organizations the caller belongs to. A node that has none yet gets the
// default organization, owned by the caller (the first dashboard user).
func (s *Server) allOrgs(r *http.Request) ([]registry.Organization, error) {
	existing, err := s.reg.ListOrganizations(r.Context())
	if err != nil {
		return nil, err
	}
	o, err := s.defaultOrg(r.Context())
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		if p := principalFrom(r.Context()); p != nil {
			if err := s.members.EnsureOwner(r.Context(), orgRef(o), p.UserID); err != nil {
				return nil, err
			}
		}
	}
	return s.memberOrgs(r)
}

func (s *Server) routesOrganizations(add func(string, handlerFunc)) {
	add("GET /v1/organizations", s.v1Orgs)
	add("GET /v1/organizations/{slug}", s.v1Org)
	add("GET /v1/organizations/{slug}/entitlements", s.entitlements("GET /v1/organizations/{slug}/entitlements"))

	add("GET /platform/organizations", s.platformOrgs)
	add("POST /platform/organizations", s.platformCreateOrg)
	add("GET /platform/organizations/{slug}", s.platformOrg)
	add("PATCH /platform/organizations/{slug}", s.platformUpdateOrg)
	add("DELETE /platform/organizations/{slug}", s.platformDeleteOrg)
	add("GET /platform/organizations/{slug}/entitlements", s.entitlements("GET /platform/organizations/{slug}/entitlements"))
	add("GET /platform/organizations/{slug}/billing/subscription", s.subscription)
}

func (s *Server) v1Orgs(w http.ResponseWriter, r *http.Request) error {
	orgs, err := s.allOrgs(r)
	if err != nil {
		return err
	}
	out := make([]v1.OrganizationResponseV1Output, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, v1.OrganizationResponseV1Output{Id: o.Slug, Name: o.Name, Slug: o.Slug})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) v1Org(w http.ResponseWriter, r *http.Request) error {
	o, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	resp := base("GET /v1/organizations/{slug}")
	setAll(resp, map[string]any{"id": o.Slug, "name": o.Name, "opt_in_tags": []any{}, "allowed_release_channels": []string{"ga"}})
	set(resp, "plan", "enterprise")
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// orgEntry is one organization of the platform list.
func (s *Server) orgEntry(o *registry.Organization, isOwner bool) map[string]any {
	row := elem("GET /platform/organizations", "")
	return setAll(row, map[string]any{
		"id": o.ID, "slug": o.Slug, "name": o.Name, "is_owner": isOwner, "opt_in_tags": []string{},
		"plan": map[string]any{"id": "enterprise", "name": "Self-hosted"}, "usage_billing_enabled": false,
		"billing_email": nil, "billing_partner": nil, "integration_source": nil, "stripe_customer_id": nil,
		"subscription_id": nil, "restriction_data": nil, "restriction_status": nil,
		"organization_missing_address": false, "organization_missing_tax_id": false,
		"organization_requires_mfa": false, "requires_indirect_tax_declaration": false,
	})
}

func (s *Server) platformOrgs(w http.ResponseWriter, r *http.Request) error {
	orgs, err := s.allOrgs(r)
	if err != nil {
		return err
	}
	a, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(orgs))
	for i := range orgs {
		e := s.orgEntry(&orgs[i], a.OrgRole(orgs[i].ID) == members.RoleOwner)
		if on, err := s.members.MFAEnforced(r.Context(), orgs[i].ID); err == nil {
			e["organization_requires_mfa"] = on
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) platformOrg(w http.ResponseWriter, r *http.Request) error {
	o, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	resp := base("GET /platform/organizations/{slug}")
	setAll(resp, map[string]any{
		"id": o.ID, "slug": o.Slug, "name": o.Name, "created_at": ts(o.CreatedAt), "has_oriole_project": false,
		"opt_in_tags": []string{}, "plan": map[string]any{"id": "enterprise", "name": "Self-hosted"},
		"usage_billing_enabled": false, "billing_email": nil, "billing_partner": nil, "integration_source": nil,
		"restriction_data": nil, "restriction_status": nil,
	})
	writeJSON(w, http.StatusOK, resp)
	return nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "org"
	}
	return s
}

func (s *Server) platformCreateOrg(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.Name) == "" {
		return errf(http.StatusBadRequest, "name is required")
	}
	// Organizations are made by Owners (or by anyone on a node that has none yet), and the
	// creator owns the new one, as on hosted.
	a, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	if existing, err := s.reg.ListOrganizations(r.Context()); err != nil {
		return err
	} else if len(existing) > 0 && !a.IsOwnerAnywhere() {
		return errf(http.StatusForbidden, "Only an Owner can create an organization")
	}
	o, err := s.reg.CreateOrganization(r.Context(), slugify(in.Name), strings.TrimSpace(in.Name))
	if err != nil {
		return mapErr(err)
	}
	if err := s.members.EnsureOwner(r.Context(), orgRef(o), principalFrom(r.Context()).UserID); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, s.orgEntry(o, true))
	return nil
}

func (s *Server) platformUpdateOrg(w http.ResponseWriter, r *http.Request) error {
	o, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if n := strings.TrimSpace(in.Name); n != "" {
		o.Name = n
		if err := s.reg.UpdateOrganization(r.Context(), o); err != nil {
			return mapErr(err)
		}
	}
	resp := base("PATCH /platform/organizations/{slug}")
	setAll(resp, map[string]any{"id": o.ID, "name": o.Name, "slug": o.Slug})
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// numericEntitlement and setEntitlement classify the entitlement keys of the spec
// enum. The spec lists keys and three kinds (boolean, numeric, set) but not which key
// has which kind, so the kind follows the key's name.
func entitlementKind(key string) string {
	switch {
	case strings.Contains(key, "available_"):
		return "set"
	case strings.Contains(key, "max_") && !strings.HasSuffix(key, ".configurable"),
		strings.HasSuffix(key, "_days"), strings.HasSuffix(key, "_limit"), strings.HasSuffix(key, "_mb"):
		return "numeric"
	}
	return "boolean"
}

// entitlements grants every feature of the spec's key enum: supavise has no plans.
func (s *Server) entitlements(key string) handlerFunc {
	op := operationByKey(key)
	keys := op.Response.Value.Properties["entitlements"].Value.Items.Value.Properties["feature"].Value.Properties["key"].Value.Enum
	list := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		name, _ := k.(string)
		kind := entitlementKind(name)
		e := map[string]any{"feature": map[string]any{"key": name, "type": kind}, "type": kind, "hasAccess": true}
		switch kind {
		case "numeric":
			e["config"] = map[string]any{"enabled": true, "value": 0, "unlimited": true, "unit": "count"}
		case "set":
			e["config"] = map[string]any{"enabled": true, "set": []string{}}
		default:
			e["config"] = map[string]any{"enabled": true}
		}
		list = append(list, e)
	}
	return func(w http.ResponseWriter, r *http.Request) error {
		if _, err := s.orgBySlug(r.Context(), r.PathValue("slug")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"entitlements": list})
		return nil
	}
}

func (s *Server) subscription(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.orgBySlug(r.Context(), r.PathValue("slug")); err != nil {
		return err
	}
	resp := base("GET /platform/organizations/{slug}/billing/subscription")
	set(resp, "plan", map[string]any{"id": "enterprise", "name": "Self-hosted"})
	writeJSON(w, http.StatusOK, resp)
	return nil
}
