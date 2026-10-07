package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/OWNER/sbctl/internal/members"
	"github.com/OWNER/sbctl/internal/registry"
)

// Members, roles, invitations and permissions: /platform/organizations/{slug}/members*,
// /roles, /platform/profile/permissions, the /v1 members route the CLI and MCP server use
// and the /v2 organization routes. The model and its rules live in internal/members; the
// permission to call each route is checked in authorize (authz.go), and the checks that
// depend on the role being changed (who may add or remove an Owner) are made by the model
// itself, with the caller's permissions.

func (s *Server) routesMembers(add func(string, handlerFunc)) {
	add("GET /platform/organizations/{slug}/members", s.platformMembers)
	add("PATCH /platform/organizations/{slug}/members/{gotrue_id}", s.assignMemberRole)
	add("DELETE /platform/organizations/{slug}/members/{gotrue_id}", s.removeMember)
	add("PUT /platform/organizations/{slug}/members/{gotrue_id}/roles/{role_id}", s.updateMemberRole)
	add("DELETE /platform/organizations/{slug}/members/{gotrue_id}/roles/{role_id}", s.unassignMemberRole)
	add("GET /platform/organizations/{slug}/members/reached-free-project-limit", s.freeProjectLimit)
	add("GET /platform/organizations/{slug}/members/invitations", s.listInvitations)
	add("POST /platform/organizations/{slug}/members/invitations", s.createInvitations)
	add("DELETE /platform/organizations/{slug}/members/invitations/{id}", s.deleteInvitation)
	add("GET /platform/organizations/{slug}/members/invitations/{token}", s.invitationByToken)
	add("POST /platform/organizations/{slug}/members/invitations/{token}", s.acceptInvitation)
	add("GET /platform/organizations/{slug}/members/mfa/enforcement", s.getMFA)
	add("PATCH /platform/organizations/{slug}/members/mfa/enforcement", s.setMFA)
	add("GET /platform/organizations/{slug}/roles", s.platformRoles)
	add("GET /platform/profile/permissions", s.permissions)
	add("GET /platform/projects/{ref}/members", s.projectMembers)

	add("GET /v1/organizations/{slug}/members", s.v1Members)
	add("GET /v2/organizations/{slug}/members", s.v2Members)
	add("PATCH /v2/organizations/{slug}/members/{user_id}/roles", s.v2AssignRole)
	add("GET /v2/organizations/{slug}/roles", s.v2Roles)
	add("POST /v2/organizations/{slug}/members/invitations", s.v2CreateInvitations)
	add("DELETE /v2/organizations/{slug}/members/invitations", s.v2DeleteInvitations)
}

func orgRef(o *registry.Organization) members.OrgRef { return members.OrgRef{ID: o.ID, Slug: o.Slug} }

// memberErr maps a model error to the API's error envelope.
func memberErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, members.ErrForbidden):
		msg := "Your role does not allow this change"
		if m := strings.TrimPrefix(err.Error(), members.ErrForbidden.Error()+": "); m != err.Error() {
			msg = m
		}
		return errf(http.StatusForbidden, "%s", msg)
	case errors.Is(err, members.ErrNotFound):
		return errf(http.StatusNotFound, "Not found")
	case errors.Is(err, members.ErrLastOwner):
		return errf(http.StatusBadRequest, "%s", members.ErrLastOwner.Error())
	case errors.Is(err, members.ErrAlreadyMember):
		return errf(http.StatusConflict, "The user is already a member of this organization")
	case errors.Is(err, members.ErrInvalid):
		return errf(http.StatusBadRequest, "%s", strings.TrimPrefix(err.Error(), members.ErrInvalid.Error()+": "))
	}
	return err
}

// userDirectory maps dashboard user ids to their stored profile.
func (s *Server) userDirectory(ctx context.Context) (map[string]User, error) {
	us, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]User, len(us))
	for _, u := range us {
		m[u.UserID] = u
	}
	return m, nil
}

func nilIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func usernameOf(u User, id string) string {
	if u.Username != "" {
		return u.Username
	}
	if u.Email != "" {
		return strings.SplitN(u.Email, "@", 2)[0]
	}
	return id
}

// ---- members ----------------------------------------------------------------

func (s *Server) platformMembers(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	mv, err := s.members.Members(r.Context(), org.ID)
	if err != nil {
		return err
	}
	dir, err := s.userDirectory(r.Context())
	if err != nil {
		return err
	}
	rows := make([]map[string]any, 0, len(mv))
	for _, m := range mv {
		u := dir[m.UserID]
		rows = append(rows, setAll(elem("GET /platform/organizations/{slug}/members", ""), map[string]any{
			"gotrue_id": m.UserID, "primary_email": nilIfEmpty(u.Email), "username": usernameOf(u, m.UserID),
			"role_ids": m.RoleIDs, "mfa_enabled": false, "is_sso_user": false, "metadata": map[string]any{}, "avatar_url": nil,
		}))
	}
	writeJSON(w, http.StatusOK, rows)
	return nil
}

// refsInOrg checks that every ref names a project of the organization.
func (s *Server) refsInOrg(ctx context.Context, org *registry.Organization, refs []string) error {
	for _, ref := range refs {
		p, err := s.loadProject(ctx, ref)
		if err != nil {
			return errf(http.StatusBadRequest, "Project %s does not exist", ref)
		}
		po, err := s.orgOf(ctx, p)
		if err != nil {
			return err
		}
		if po.ID != org.ID {
			return errf(http.StatusBadRequest, "Project %s does not belong to this organization", ref)
		}
	}
	return nil
}

// assignMemberRole is Studio's "change role" and "give a project-scoped role": role_id is the
// organization-wide role, or the base role of a project-scoped one when projects are listed.
func (s *Server) assignMemberRole(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	var in struct {
		RoleID  float64  `json:"role_id"`
		Project []string `json:"role_scoped_projects"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	return s.applyRole(w, r, org, actor, r.PathValue("gotrue_id"), int(in.RoleID), in.Project)
}

func (s *Server) applyRole(w http.ResponseWriter, r *http.Request, org *registry.Organization, actor *members.Access, userID string, roleID int, projects []string) error {
	if !members.ValidRoleID(roleID) {
		return errf(http.StatusBadRequest, "role_id must be one of the organization roles (1 to 4)")
	}
	var err error
	if len(projects) > 0 {
		if err := s.refsInOrg(r.Context(), org, projects); err != nil {
			return err
		}
		err = s.members.AssignProjectRole(r.Context(), actor, orgRef(org), userID, roleID, projects)
	} else {
		err = s.members.SetOrgRole(r.Context(), actor, orgRef(org), userID, roleID)
	}
	if err := memberErr(err); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	if err := memberErr(s.members.RemoveMember(r.Context(), actor, orgRef(org), r.PathValue("gotrue_id"))); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func roleIDParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("role_id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errf(http.StatusBadRequest, "role_id must be a number")
	}
	return id, nil
}

// updateMemberRole changes the projects of a member's project-scoped role.
func (s *Server) updateMemberRole(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	roleID, err := roleIDParam(r)
	if err != nil {
		return err
	}
	var in struct {
		Name     string   `json:"name"`
		Projects []string `json:"role_scoped_projects"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if roleID < members.ProjectRoleIDBase {
		return errf(http.StatusBadRequest, "Only project-scoped roles can be updated; change the member's role instead")
	}
	if err := s.refsInOrg(r.Context(), org, in.Projects); err != nil {
		return err
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	if err := memberErr(s.members.SetProjectRoleRefs(r.Context(), actor, orgRef(org), r.PathValue("gotrue_id"), roleID, in.Projects)); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) unassignMemberRole(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	roleID, err := roleIDParam(r)
	if err != nil {
		return err
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	if err := memberErr(s.members.RemoveRole(r.Context(), actor, orgRef(org), r.PathValue("gotrue_id"), roleID)); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// freeProjectLimit: there is no free tier, so nobody has reached a limit.
func (s *Server) freeProjectLimit(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, []any{})
	return nil
}

// ---- roles ------------------------------------------------------------------

func (s *Server) projectNames(ctx context.Context, refs []string) []map[string]any {
	out := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		if p, err := s.reg.GetProject(ctx, ref); err == nil {
			out = append(out, map[string]any{"ref": p.Ref, "name": p.Name})
		}
	}
	return out
}

func (s *Server) platformRoles(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	const key = "GET /platform/organizations/{slug}/roles"
	orgRoles := make([]any, 0, len(members.Roles))
	for _, ro := range members.Roles {
		orgRoles = append(orgRoles, setAll(elem(key, "org_scoped_roles"), map[string]any{
			"id": ro.ID, "name": ro.Name, "description": ro.Description, "projects": []any{}, "base_role_id": nil,
		}))
	}
	scoped, err := s.members.ScopedRoles(r.Context(), org.ID)
	if err != nil {
		return err
	}
	projRoles := make([]any, 0, len(scoped))
	for _, sr := range scoped {
		projects := s.projectNames(r.Context(), sr.Refs)
		if len(projects) == 0 {
			continue
		}
		base, _ := members.RoleByID(sr.BaseRoleID)
		projRoles = append(projRoles, setAll(elem(key, "project_scoped_roles"), map[string]any{
			"id": sr.ID, "name": base.Name + "_" + sr.Refs[0], "description": base.Description,
			"projects": projects, "base_role_id": sr.BaseRoleID,
		}))
	}
	resp := base(key)
	set(resp, "org_scoped_roles", orgRoles)
	set(resp, "project_scoped_roles", projRoles)
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// ---- permissions ------------------------------------------------------------

// memberOrgs returns the organizations the caller belongs to.
func (s *Server) memberOrgs(r *http.Request) ([]registry.Organization, error) {
	a, err := s.callerAccess(r)
	if err != nil {
		return nil, err
	}
	orgs, err := s.reg.ListOrganizations(r.Context())
	if err != nil {
		return nil, err
	}
	out := orgs[:0:0]
	for _, o := range orgs {
		if a.IsMember(o.ID) {
			out = append(out, o)
		}
	}
	return out, nil
}

// permissionRows renders a user's permission entries as AccessControlPermission objects.
func (s *Server) permissionRows(ctx context.Context, perms []members.Permission) []map[string]any {
	seq := map[string]float64{}
	if ps, err := s.reg.ListProjects(ctx); err == nil {
		for _, p := range ps {
			seq[p.Ref] = float64(projectNumID(&p))
		}
	}
	out := make([]map[string]any, 0, len(perms))
	for _, p := range perms {
		refs := p.ProjectRefs
		if refs == nil {
			refs = []string{}
		}
		ids := make([]float64, 0, len(refs))
		for _, ref := range refs {
			if id, ok := seq[ref]; ok {
				ids = append(ids, id)
			}
		}
		out = append(out, map[string]any{
			"actions": p.Actions, "resources": p.Resources, "condition": p.Condition,
			"organization_id": p.OrganizationID, "organization_slug": p.OrganizationSlug,
			"project_ids": ids, "project_refs": refs, "restrictive": p.Restrictive,
		})
	}
	return out
}

// permissions is the list Studio's permission checks run on: the entries of the caller's
// roles, the same ones the server enforces (authz.go).
func (s *Server) permissions(w http.ResponseWriter, r *http.Request) error {
	a, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	orgs, err := s.memberOrgs(r)
	if err != nil {
		return err
	}
	refs := make([]members.OrgRef, len(orgs))
	for i := range orgs {
		refs[i] = orgRef(&orgs[i])
	}
	writeJSON(w, http.StatusOK, s.permissionRows(r.Context(), a.Permissions(refs)))
	return nil
}

// projectMembers lists the users who can see a project: organization members with an
// organization-wide role and members with a project-scoped role on it.
func (s *Server) projectMembers(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	org, err := s.orgOf(r.Context(), p)
	if err != nil {
		return err
	}
	ms, err := s.members.Store.ListMembers(r.Context(), org.ID)
	if err != nil {
		return err
	}
	scoped, err := s.members.Store.ProjectRoles(r.Context(), org.ID)
	if err != nil {
		return err
	}
	onProject := map[string]bool{}
	for _, sr := range scoped {
		for _, ref := range sr.Refs {
			if ref == p.Ref {
				onProject[sr.UserID] = true
			}
		}
	}
	dir, err := s.userDirectory(r.Context())
	if err != nil {
		return err
	}
	list := make([]any, 0, len(ms))
	for _, m := range ms {
		if m.RoleID == 0 && !onProject[m.UserID] {
			continue
		}
		u := dir[m.UserID]
		list = append(list, setAll(elem("GET /platform/projects/{ref}/members", "members"), map[string]any{
			"primary_email": u.Email, "username": usernameOf(u, m.UserID), "user_id": m.UserID,
		}))
	}
	resp := base("GET /platform/projects/{ref}/members")
	set(resp, "members", list)
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// ---- invitations ------------------------------------------------------------

func (s *Server) listInvitations(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	invs, err := s.members.Invitations(r.Context(), org.ID)
	if err != nil {
		return err
	}
	list := make([]any, 0, len(invs))
	for _, i := range invs {
		list = append(list, setAll(elem("GET /platform/organizations/{slug}/members/invitations", "invitations"), map[string]any{
			"id": i.ID, "invited_at": ts(i.CreatedAt), "invited_email": i.Email, "role_id": i.ListedRoleID(),
		}))
	}
	resp := base("GET /platform/organizations/{slug}/members/invitations")
	set(resp, "invitations", list)
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// invitationRequest is one address of a create-invitations call, in either of the two body
// shapes the platform spec has (Studio sends emails + role_id; the API's own clients send data).
type invitationRequest struct {
	email  string
	roleID int
	refs   []string
	err    string
}

func parseInvitations(body map[string]any) ([]invitationRequest, error) {
	var out []invitationRequest
	num := func(v any) int {
		f, _ := v.(float64)
		return int(f)
	}
	strs := func(v any) []string {
		var l []string
		for _, x := range anySlice(v) {
			switch t := x.(type) {
			case string:
				l = append(l, t)
			case map[string]any:
				if ref, _ := t["ref"].(string); ref != "" {
					l = append(l, ref)
				}
			}
		}
		return l
	}
	for _, e := range anySlice(body["emails"]) {
		email, _ := e.(string)
		out = append(out, invitationRequest{email: email, roleID: num(body["role_id"]), refs: strs(body["role_scoped_projects"])})
	}
	for _, d := range anySlice(body["data"]) {
		dm, _ := d.(map[string]any)
		at, _ := dm["attributes"].(map[string]any)
		if at == nil {
			continue
		}
		email, _ := at["email"].(string)
		req := invitationRequest{email: email, roleID: num(at["role_id"]), refs: strs(at["projects"])}
		if name, _ := at["role"].(string); name != "" {
			if name == "no-access" {
				req.err = "the no-access role is not supported; invite with a role and projects"
			} else if ro, err := members.ParseRole(name); err != nil {
				req.err = err.Error()
			} else {
				req.roleID = ro.ID
			}
		}
		out = append(out, req)
	}
	if len(out) == 0 {
		return nil, errf(http.StatusBadRequest, "emails is required")
	}
	if len(out) > 50 {
		return nil, errf(http.StatusBadRequest, "at most 50 invitations at a time")
	}
	return out, nil
}

func anySlice(v any) []any {
	l, _ := v.([]any)
	return l
}

type invitationOutcome struct {
	email  string
	err    string
	status int
	result *InviteResult
}

// invite sends the requested invitations and reports each address. The error status is the
// one of the first refusal, for the callers that answer a single failure with it.
func (s *Server) invite(ctx context.Context, actor *members.Access, org *registry.Organization, reqs []invitationRequest) []invitationOutcome {
	out := make([]invitationOutcome, 0, len(reqs))
	for _, rq := range reqs {
		o := invitationOutcome{email: rq.email}
		switch {
		case rq.err != "":
			o.err, o.status = rq.err, http.StatusBadRequest
		default:
			if err := s.refsInOrg(ctx, org, rq.refs); err != nil {
				o.err, o.status = err.Error(), asError(err).Status
				break
			}
			res, err := s.accounts.InviteToOrganization(ctx, actor, orgRef(org), members.InviteInput{Email: rq.email, RoleID: rq.roleID, Refs: rq.refs})
			if err != nil {
				e := asError(memberErr(err))
				o.err, o.status = e.Message, e.Status
				if e.Status >= 500 {
					s.log.Error("invitation failed", "email", rq.email, "error", err)
					o.err = "Could not create the invitation"
				}
				break
			}
			o.result = res
		}
		out = append(out, o)
	}
	return out
}

func (s *Server) createInvitations(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	body, err := jsonBody(r)
	if err != nil {
		return err
	}
	reqs, err := parseInvitations(body)
	if err != nil {
		return err
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	outs := s.invite(r.Context(), actor, org, reqs)
	succeeded, failed, links := []string{}, []any{}, []any{}
	forbiddenAll := true
	for _, o := range outs {
		if o.err == "" {
			succeeded = append(succeeded, o.email)
			links = append(links, map[string]any{"email": o.email, "url": o.result.Link(), "emailed": o.result.Emailed})
			forbiddenAll = false
			continue
		}
		failed = append(failed, map[string]any{"email": o.email, "error": o.err})
		if o.status != http.StatusForbidden {
			forbiddenAll = false
		}
	}
	if forbiddenAll && len(outs) > 0 {
		return errf(http.StatusForbidden, "%s", outs[0].err)
	}
	// invite_links is sbctl's addition for an installation without mail: the links to give
	// the invitees (the specification's fields are untouched).
	writeJSON(w, http.StatusCreated, map[string]any{"succeeded": succeeded, "failed": failed, "invite_links": links})
	return nil
}

func (s *Server) deleteInvitation(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return errf(http.StatusBadRequest, "id must be a number")
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	if err := memberErr(s.members.RevokeInvitation(r.Context(), actor, orgRef(org), id)); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) invitationByToken(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	p := principalFrom(r.Context())
	st, err := s.members.InvitationState(r.Context(), orgRef(org), r.PathValue("token"), p.Email)
	if err != nil {
		return err
	}
	if st.Accepted {
		// Studio shows "This invite has already been accepted or declined" for this answer.
		return errf(http.StatusUnauthorized, "Failed to retrieve organization invitation: it was already accepted")
	}
	resp := base("GET /platform/organizations/{slug}/members/invitations/{token}")
	setAll(resp, map[string]any{
		"organization_name": org.Name, "token_does_not_exist": st.TokenNotFound, "sso_mismatch": false,
		"email_match": st.EmailMatch, "authorized_user": st.EmailMatch && !st.Expired && !st.TokenNotFound, "expired_token": st.Expired,
	})
	if !st.TokenNotFound {
		resp["invite_id"] = st.InviteID
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) acceptInvitation(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	p := principalFrom(r.Context())
	token := r.PathValue("token")
	st, err := s.members.InvitationState(r.Context(), orgRef(org), token, p.Email)
	if err != nil {
		return err
	}
	switch {
	case st.TokenNotFound:
		return errf(http.StatusNotFound, "Invitation not found")
	case st.Accepted:
		return errf(http.StatusConflict, "This invitation was already accepted")
	case st.Expired:
		return errf(http.StatusBadRequest, "This invitation has expired")
	case !st.EmailMatch:
		return errf(http.StatusForbidden, "This invitation was sent to a different email address")
	}
	// A dashboard session without a second factor cannot join an organization that requires one.
	if p.Via == "jwt" && p.AAL != "aal2" {
		if on, err := s.members.MFAEnforced(r.Context(), org.ID); err != nil {
			return err
		} else if on {
			return errMFARequired
		}
	}
	err = s.members.AcceptInvitation(r.Context(), orgRef(org), token, p.UserID, p.Email, func(refs []string) []string { return s.liveRefs(r.Context(), refs) })
	if err := memberErr(err); err != nil {
		return err
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

// ---- MFA enforcement --------------------------------------------------------

func (s *Server) getMFA(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	on, err := s.members.MFAEnforced(r.Context(), org.ID)
	if err != nil {
		return err
	}
	// The specification answers a read with 201.
	writeJSON(w, operationByKey("GET /platform/organizations/{slug}/members/mfa/enforcement").Status, map[string]any{"enforced": on})
	return nil
}

func (s *Server) setMFA(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	var in struct {
		Enforced bool `json:"enforced"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	p := principalFrom(r.Context())
	if in.Enforced && p.Via == "jwt" && p.AAL != "aal2" {
		// Requiring MFA from a session that has none would lock the caller out.
		return errf(http.StatusBadRequest, "Sign in with a second factor (MFA) before requiring it for the organization")
	}
	if err := s.members.SetMFAEnforced(r.Context(), org.ID, in.Enforced); err != nil {
		return err
	}
	writeJSON(w, operationByKey("PATCH /platform/organizations/{slug}/members/mfa/enforcement").Status, map[string]any{"enforced": in.Enforced})
	return nil
}

// ---- /v1 and /v2 ------------------------------------------------------------

// v1Members is the list the CLI and the MCP server read.
func (s *Server) v1Members(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	mv, err := s.members.Members(r.Context(), org.ID)
	if err != nil {
		return err
	}
	scoped, err := s.members.ScopedRoles(r.Context(), org.ID)
	if err != nil {
		return err
	}
	baseOf := map[int64]int{}
	for _, sr := range scoped {
		baseOf[sr.ID] = sr.BaseRoleID
	}
	dir, err := s.userDirectory(r.Context())
	if err != nil {
		return err
	}
	rows := make([]map[string]any, 0, len(mv))
	for _, m := range mv {
		u := dir[m.UserID]
		role := ""
		for _, id := range m.RoleIDs {
			if id < members.ProjectRoleIDBase {
				role = members.RoleName(int(id))
				break
			}
			if role == "" {
				role = members.RoleName(baseOf[id])
			}
		}
		rows = append(rows, setAll(elem("GET /v1/organizations/{slug}/members", ""), map[string]any{
			"user_id": m.UserID, "user_name": usernameOf(u, m.UserID), "email": u.Email, "role_name": role,
			"mfa_enabled": false, "avatar_url": nil,
		}))
	}
	writeJSON(w, http.StatusOK, rows)
	return nil
}

// v2Roles describes the roles of a member in the v2 shape.
func (s *Server) v2RoleList(ctx context.Context, org *registry.Organization, m members.MemberView, scopedByID map[int64]members.ScopedRole) []any {
	roles := []any{}
	for _, id := range m.RoleIDs {
		if id < members.ProjectRoleIDBase {
			ro, _ := members.RoleByID(int(id))
			roles = append(roles, map[string]any{"name": ro.Slug, "scope": "organization", "projects": []any{}})
			continue
		}
		sr, ok := scopedByID[id]
		if !ok {
			continue
		}
		ro, _ := members.RoleByID(sr.BaseRoleID)
		roles = append(roles, map[string]any{"name": ro.Slug, "scope": "project", "projects": s.projectNames(ctx, sr.Refs)})
	}
	return roles
}

func (s *Server) v2Members(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	mv, err := s.members.Members(r.Context(), org.ID)
	if err != nil {
		return err
	}
	scoped, err := s.members.ScopedRoles(r.Context(), org.ID)
	if err != nil {
		return err
	}
	byID := map[int64]members.ScopedRole{}
	for _, sr := range scoped {
		byID[sr.ID] = sr
	}
	dir, err := s.userDirectory(r.Context())
	if err != nil {
		return err
	}
	q := r.URL.Query()
	nameFilter, mailFilter := q.Get("filter[username]"), strings.ToLower(q.Get("filter[primary_email]"))
	data := make([]any, 0, len(mv))
	for _, m := range mv {
		u := dir[m.UserID]
		if nameFilter != "" && !strings.EqualFold(usernameOf(u, m.UserID), nameFilter) {
			continue
		}
		if mailFilter != "" && strings.ToLower(u.Email) != mailFilter {
			continue
		}
		data = append(data, map[string]any{"type": "organization_member", "id": m.UserID, "attributes": map[string]any{
			"username": nilIfEmpty(usernameOf(u, m.UserID)), "primary_email": nilIfEmpty(u.Email), "mfa_enabled": false,
			"is_sso_user": false, "avatar_url": nil, "roles": s.v2RoleList(r.Context(), org, m, byID),
		}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "links": map[string]any{"prev": nil, "next": nil}})
	return nil
}

func (s *Server) v2Roles(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.orgBySlug(r.Context(), r.PathValue("slug")); err != nil {
		return err
	}
	data := make([]any, 0, len(members.Roles))
	for _, ro := range members.Roles {
		data = append(data, map[string]any{"type": "organization_role", "attributes": map[string]any{"name": ro.Slug}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
	return nil
}

func (s *Server) v2AssignRole(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	var in struct {
		Data struct {
			Attributes struct {
				Role     string `json:"role"`
				Projects []struct {
					Ref string `json:"ref"`
				} `json:"projects"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	at := in.Data.Attributes
	ro, err := members.ParseRole(at.Role)
	if err != nil {
		return errf(http.StatusBadRequest, "%v", err)
	}
	refs := make([]string, 0, len(at.Projects))
	for _, p := range at.Projects {
		refs = append(refs, p.Ref)
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	rec := &statusRecorder{ResponseWriter: w}
	if err := s.applyRole(rec, r, org, actor, r.PathValue("user_id"), ro.ID, refs); err != nil {
		return err
	}
	scope, projects := "organization", []any{}
	if len(refs) > 0 {
		scope, projects = "project", func() []any {
			l := []any{}
			for _, p := range s.projectNames(r.Context(), refs) {
				l = append(l, p)
			}
			return l
		}()
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"type": "organization_member_role",
		"attributes": map[string]any{"name": ro.Slug, "scope": scope, "projects": projects}}})
	return nil
}

// statusRecorder swallows the empty answer applyRole writes, so a v2 handler can write its own.
type statusRecorder struct{ http.ResponseWriter }

func (*statusRecorder) WriteHeader(int)             {}
func (*statusRecorder) Write(b []byte) (int, error) { return len(b), nil }

func (s *Server) v2CreateInvitations(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	body, err := jsonBody(r)
	if err != nil {
		return err
	}
	delete(body, "emails")
	reqs, err := parseInvitations(body)
	if err != nil {
		return err
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	outs := s.invite(r.Context(), actor, org, reqs)
	data, failed := []any{}, []any{}
	status := 0
	for _, o := range outs {
		if o.err != "" {
			if status == 0 {
				status = o.status
			}
			failed = append(failed, map[string]any{"code": "validation_failed", "message": o.err, "meta": map[string]any{"email": o.email}})
			continue
		}
		data = append(data, map[string]any{"type": "organization_invitation", "attributes": map[string]any{"email": o.email}})
	}
	resp := map[string]any{"data": data}
	if len(failed) > 0 {
		if len(data) == 0 && status == http.StatusForbidden {
			return errf(http.StatusForbidden, "%s", outs[0].err)
		}
		resp["error"] = map[string]any{"code": "organization_invitations_partially_failed", "message": "Some invitations could not be created", "issues": failed}
	}
	writeJSON(w, http.StatusCreated, resp)
	return nil
}

func (s *Server) v2DeleteInvitations(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	var in struct {
		Data []struct {
			Attributes struct {
				Email string `json:"email"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	invs, err := s.members.Invitations(r.Context(), org.ID)
	if err != nil {
		return err
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	data := []any{}
	for _, d := range in.Data {
		email := strings.ToLower(strings.TrimSpace(d.Attributes.Email))
		for _, inv := range invs {
			if inv.Email != email {
				continue
			}
			if err := memberErr(s.members.RevokeInvitation(r.Context(), actor, orgRef(org), inv.ID)); err != nil {
				return err
			}
			data = append(data, map[string]any{"type": "organization_invitation", "attributes": map[string]any{"email": inv.Email}})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
	return nil
}
