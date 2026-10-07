package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/OWNER/sbctl/internal/members"
	"github.com/OWNER/sbctl/internal/registry"
)

// Authorization. Authentication (auth.go) says who calls; this file says what that caller
// may do. Every route of the three specs (implemented, stubbed and unknown) resolves to a
// requirement in the table below: an action and a resource of hosted's PermissionAction
// model, checked against the permissions of the caller's roles (internal/members) exactly the
// way Studio checks them against GET /platform/profile/permissions, so the dashboard and the
// server cannot disagree. A personal access token carries the permissions of its owner.
//
// Default deny: a write that no rule names needs the permission to update the organization
// (Owner) or the project's settings (Administrator), and a write outside any organization or
// project needs the Owner role somewhere.

type needKind int

const (
	// needCheck: the caller needs action on resource in the route's organization or project.
	needCheck needKind = iota
	// needAny: any signed-in user (their own profile, tokens, telemetry, ...).
	needAny
	// needSelf: the handler decides (the organization comes from the body, or the check depends
	// on the role being changed); the middleware only requires that the caller can see the
	// organization or project.
	needSelf
	// needOwner: the caller must hold the Owner role organization-wide in some organization.
	needOwner
)

// need is the requirement of one route.
type need struct {
	kind             needKind
	action, resource string
	// resourceFn derives the resource from the request when it depends on the path.
	resourceFn func(*http.Request) string
	// own makes the middleware check the action as the caller's own saved item: the permissions
	// on saved content are conditioned on the item's owner, which only the handler knows, so it
	// repeats the check with the real item (content.go).
	own bool
}

func (n need) res(r *http.Request) string {
	if n.resourceFn != nil {
		return n.resourceFn(r)
	}
	return n.resource
}

func chk(action, resource string) need { return need{action: action, resource: resource} }

// chkOwn is chk for a write on saved content, which the handler checks again against the item.
func chkOwn(action, resource string) need { return need{action: action, resource: resource, own: true} }

// data is the condition data of the middleware's check.
func (n need) data() map[string]any {
	if n.own {
		return members.OwnContentData("sql", "project", 1, 1)
	}
	return nil
}

var (
	nAny   = need{kind: needAny}
	nSelf  = need{kind: needSelf}
	nOwner = need{kind: needOwner}
)

// routeRule maps routes to a need. methods is "" (any), "R" (GET and HEAD), "W" (the other
// methods) or a comma-separated list of methods.
type routeRule struct {
	methods string
	re      *regexp.Regexp
	n       need
}

func (rr routeRule) matches(method, path string) bool {
	switch rr.methods {
	case "":
	case "R":
		if method != http.MethodGet && method != http.MethodHead {
			return false
		}
	case "W":
		if method == http.MethodGet || method == http.MethodHead {
			return false
		}
	default:
		if !strings.Contains(","+rr.methods+",", ","+method+",") {
			return false
		}
	}
	return rr.re.MatchString(path)
}

// pat compiles a path template: {x} is one segment, a trailing /** is the rest of the path
// (including nothing).
func pat(p string) *regexp.Regexp {
	rest := strings.HasSuffix(p, "/**")
	p = strings.TrimSuffix(p, "/**")
	re := regexp.QuoteMeta(p)
	re = regexp.MustCompile(`\\\{[a-z_]+\\\}`).ReplaceAllString(re, `[^/]+`)
	if rest {
		re += `(/.*)?`
	}
	return regexp.MustCompile("^" + re + "$")
}

func rule(methods, path string, n need) routeRule { return routeRule{methods, pat(path), n} }

// pgmetaResource names the pg-meta object a request is about by the path segment after the ref:
// /platform/pg-meta/{ref}/tables is "tables".
func pgmetaResource(r *http.Request) string {
	rest := strings.TrimPrefix(r.URL.Path, "/platform/pg-meta/"+r.PathValue("ref")+"/")
	seg, _, _ := strings.Cut(rest, "/")
	if seg == "" {
		return members.ResAny
	}
	return seg
}

const (
	P  = members.ResProjects
	O  = members.ResOrg
	UC = members.ResUserContent
)

// routeRules is the route-to-permission table, first match wins. The patterns are the OpenAPI
// templates of the specs.
var routeRules = []routeRule{
	// ---- the caller's own account ----
	rule("", "/platform/profile/**", nAny),
	rule("", "/v1/profile", nAny),
	rule("", "/platform/cli/login/**", nAny),
	rule("", "/platform/notifications/**", nAny),
	rule("", "/platform/telemetry/**", nAny),
	rule("", "/platform/feedback/**", nAny),
	rule("", "/platform/support/**", nAny),
	rule("", "/platform/status", nAny),
	rule("", "/platform/reset-password", nAny),
	rule("", "/platform/update-email", nAny),
	rule("", "/platform/signup", nAny),
	rule("", "/platform/plans/features", nAny),
	rule("", "/platform/projects-resource-warnings", nAny),
	rule("", "/platform/projects/available-regions", nAny),
	rule("", "/v1/projects/available-regions", nAny),
	rule("", "/platform/mcp-tools-permissions", nAny),
	rule("", "/platform/workflow-runs/**", nAny),
	rule("", "/platform/organizations/preview-creation", nAny),
	rule("", "/platform/organizations/onboarding-survey", nAny),
	rule("", "/v1/oauth/**", nAny),
	rule("", "/v1/snippets/**", nAny),
	rule("R", "/platform/integrations/{slug}", nAny),
	// Lists are filtered to what the caller may see; creating needs the organization of the body.
	rule("R", "/platform/organizations", nAny),
	rule("R", "/v1/organizations", nAny),
	rule("R", "/platform/projects", nAny),
	rule("R", "/v1/projects", nAny),
	rule("POST", "/platform/organizations", nSelf),
	rule("POST", "/platform/projects", nSelf),
	rule("POST", "/v1/projects", nSelf),

	// ---- organizations ----
	// Whoever holds an invitation token may look it up and accept it; the token is the proof.
	rule("GET,POST", "/platform/organizations/{slug}/members/invitations/{token}", nAny),
	rule("R", "/platform/organizations/{slug}/members/invitations", chk(members.ActRead, members.ResUserInvites)),
	rule("POST", "/platform/organizations/{slug}/members/invitations", nSelf),
	rule("DELETE", "/platform/organizations/{slug}/members/invitations/{id}", nSelf),
	rule("PATCH", "/platform/organizations/{slug}/members/mfa/enforcement", chk(members.ActUpdate, O)),
	rule("W", "/platform/organizations/{slug}/members/**", nSelf),
	rule("W", "/v2/organizations/{slug}/members/**", nSelf),
	rule("R", "/platform/organizations/{slug}/members/**", chk(members.ActRead, O)),
	rule("DELETE", "/platform/organizations/{slug}", chk(members.ActDelete, O)),
	rule("W", "/platform/organizations/{slug}", chk(members.ActUpdate, O)),
	rule("R", "/platform/organizations/{slug}/billing/**", chk(members.ActBillingRead, "stripe.subscriptions")),
	rule("W", "/platform/organizations/{slug}/billing/**", chk(members.ActBillingWrite, "stripe.subscriptions")),
	rule("R", "/platform/organizations/{slug}/payments/**", chk(members.ActBillingRead, "stripe.payment_methods")),
	rule("W", "/platform/organizations/{slug}/payments/**", chk(members.ActBillingWrite, "stripe.payment_methods")),
	rule("R", "/platform/organizations/{slug}/customer", chk(members.ActBillingRead, "stripe.customer")),
	rule("W", "/platform/organizations/{slug}/customer", chk(members.ActBillingWrite, "stripe.customer")),
	rule("R", "/platform/organizations/{slug}/tax-ids", chk(members.ActBillingRead, "stripe.tax_ids")),
	rule("R", "/platform/organizations/{slug}/usage/**", chk(members.ActBillingRead, "stripe.subscriptions")),
	rule("R", "/platform/organizations/{slug}/oauth/**", chk(members.ActRead, "oauth_apps")),
	rule("W", "/platform/organizations/{slug}/oauth/**", chk(members.ActUpdate, "oauth_apps")),
	rule("R", "/platform/organizations/{slug}/apps/**", chk(members.ActRead, "oauth_apps")),
	rule("W", "/platform/organizations/{slug}/apps/**", chk(members.ActUpdate, "oauth_apps")),
	rule("POST", "/platform/organizations/{slug}/available-versions", chk(members.ActRead, O)),

	// ---- projects: lifecycle ----
	rule("DELETE", "/v1/projects/{ref}", chk(members.ActDelete, P)),
	rule("DELETE", "/platform/projects/{ref}", chk(members.ActDelete, P)),
	rule("POST", "/platform/projects/{ref}/transfer/preview", chk(members.ActRead, P)),
	rule("POST", "/v2/projects/{ref}/transfers/previews", chk(members.ActRead, P)),
	rule("W", "/platform/projects/{ref}/transfer", chk(members.ActUpdate, members.ResProjectTransfer)),
	rule("W", "/v2/projects/{ref}/transfers", chk(members.ActUpdate, members.ResProjectTransfer)),
	rule("POST", "/v1/projects/{ref}/pause", chk(members.ActInfraExecute, "queue_jobs.projects.pause")),
	rule("POST", "/platform/projects/{ref}/pause", chk(members.ActInfraExecute, "queue_jobs.projects.pause")),
	rule("POST", "/v1/projects/{ref}/restore", chk(members.ActInfraExecute, "queue_jobs.projects.initialize_or_resume")),
	rule("POST", "/platform/projects/{ref}/restore", chk(members.ActInfraExecute, "queue_jobs.projects.initialize_or_resume")),
	rule("POST", "/platform/projects/{ref}/wake", chk(members.ActInfraExecute, "queue_jobs.projects.initialize_or_resume")),
	rule("POST", "/v1/projects/{ref}/restart", chk(members.ActInfraExecute, "reboot")),
	rule("POST", "/platform/projects/{ref}/restart", chk(members.ActInfraExecute, "reboot")),
	rule("POST", "/platform/projects/{ref}/restart-services", chk(members.ActInfraExecute, "reboot")),
	rule("W", "/platform/database/{ref}/backups/**", chk(members.ActInfraExecute, "queue_job.walg.prepare_restore")),
	rule("W", "/v1/projects/{ref}/database/backups/**", chk(members.ActInfraExecute, "queue_job.walg.prepare_restore")),
	rule("W", "/v1/projects/{ref}/restore/**", chk(members.ActInfraExecute, "queue_job.restore.prepare")),

	// ---- projects: keys and secrets ----
	rule("POST", "/platform/projects/{ref}/api-keys/temporary", chk(members.ActRead, members.ResServiceKeys)),
	rule("R", "/v1/projects/{ref}/api-keys/**", chk(members.ActRead, P)), // secret values: redacted in the handler
	rule("POST", "/v1/projects/{ref}/api-keys", chk(members.ActCreate, members.ResServiceKeys)),
	rule("PATCH", "/v1/projects/{ref}/api-keys/{id}", chk(members.ActUpdate, members.ResServiceKeys)),
	rule("DELETE", "/v1/projects/{ref}/api-keys/{id}", chk(members.ActDelete, members.ResServiceKeys)),
	rule("W", "/v1/projects/{ref}/api-keys/**", chk(members.ActUpdate, members.ResServiceKeys)),
	rule("W", "/v1/projects/{ref}/config/auth/signing-keys/**", chk(members.ActUpdate, members.ResServiceKeys)),
	rule("W", "/platform/projects/{ref}/config/secrets", chk(members.ActUpdate, members.ResServiceKeys)),
	rule("R", "/v1/projects/{ref}/secrets", chk(members.ActSecretsRead, members.ResAny)),
	rule("W", "/v1/projects/{ref}/secrets", chk(members.ActSecretsWrite, members.ResAny)),
	rule("PATCH", "/v1/projects/{ref}/database/password", chk(members.ActUpdate, P)),
	rule("PATCH", "/platform/projects/{ref}/db-password", chk(members.ActUpdate, P)),

	// ---- projects: settings ----
	rule("W", "/v1/projects/{ref}/config/auth/**", chk(members.ActUpdate, "custom_config_gotrue")),
	rule("W", "/platform/auth/{ref}/config/**", chk(members.ActUpdate, "custom_config_gotrue")),
	rule("W", "/platform/auth/{ref}/templates/**", chk(members.ActUpdate, "custom_config_gotrue")),
	rule("W", "/v1/projects/{ref}/postgrest", chk(members.ActUpdate, "custom_config_postgrest")),
	rule("W", "/platform/projects/{ref}/config/postgrest", chk(members.ActUpdate, "custom_config_postgrest")),
	rule("W", "/v1/projects/{ref}/config/realtime/**", chk(members.ActUpdate, "custom_config_realtime")),
	rule("W", "/platform/projects/{ref}/config/realtime/**", chk(members.ActUpdate, "custom_config_realtime")),
	rule("W", "/v1/projects/{ref}/config/storage", chk(members.ActUpdate, "custom_config_storage")),
	rule("W", "/platform/projects/{ref}/config/storage", chk(members.ActUpdate, "custom_config_storage")),
	rule("W", "/v1/projects/{ref}/config/database/**", chk(members.ActUpdate, "custom_config_postgres")),
	rule("W", "/platform/projects/{ref}/config/**", chk(members.ActUpdate, "custom_config_postgres")),

	// ---- projects: database ----
	rule("POST", "/v1/projects/{ref}/database/query", chk(members.ActSQLQuery, members.ResAny)),
	rule("POST", "/v1/projects/{ref}/database/query/read-only", chk(members.ActSQLQuery, members.ResAny)),
	rule("POST", "/platform/pg-meta/{ref}/query", chk(members.ActSQLQuery, members.ResAny)),
	rule("R", "/platform/pg-meta/{ref}/**", need{action: members.ActSQLAdminRead, resourceFn: pgmetaResource}),
	rule("R", "/v1/projects/{ref}/database/migrations/**", chk(members.ActSQLSelect, members.ResAny)),
	rule("W", "/v1/projects/{ref}/database/migrations/**", chk(members.ActSQLAdminWrite, "migrations")),
	rule("R", "/v1/projects/{ref}/types/typescript", chk(members.ActSQLAdminRead, "schemas")),
	// The login role is read-only for a caller who cannot write SQL (handler). Deleting the
	// project's login roles drops those of every member, read-write ones included: it needs the
	// right to change database roles.
	rule("POST", "/v1/projects/{ref}/cli/login-role", chk(members.ActSQLQuery, members.ResAny)),
	rule("DELETE", "/v1/projects/{ref}/cli/login-role", chk(members.ActSQLAdminWrite, members.ResAny)),
	rule("POST", "/v1/projects/{ref}/database/webhooks/enable", chk(members.ActSQLAdminWrite, "triggers")),
	rule("POST", "/platform/database/{ref}/hook-enable", chk(members.ActSQLAdminWrite, "triggers")),
	rule("POST", "/platform/projects/{ref}/api/graphql", chk(members.ActSQLInsert, members.ResAny)),
	rule("POST", "/v2/projects/{ref}/advisors/run", chk(members.ActSQLAdminRead, "schemas")),

	// ---- projects: auth admin ----
	rule("POST", "/platform/auth/{ref}/users", chk(members.ActAuthExecute, "create_user")),
	rule("POST", "/platform/auth/{ref}/invite", chk(members.ActAuthExecute, "invite_user")),
	rule("POST", "/platform/auth/{ref}/magiclink", chk(members.ActAuthExecute, "send_magic_link")),
	rule("POST", "/platform/auth/{ref}/otp", chk(members.ActAuthExecute, "send_otp")),
	rule("POST", "/platform/auth/{ref}/recover", chk(members.ActAuthExecute, "send_recovery")),
	rule("POST", "/platform/auth/{ref}/validate/spam", chk(members.ActRead, P)),
	rule("DELETE", "/platform/auth/{ref}/users/{id}/factors", chk(members.ActSQLDelete, "auth.mfa_factors")),
	rule("DELETE", "/platform/auth/{ref}/users/{id}", chk(members.ActSQLDelete, "auth.users")),
	rule("PATCH", "/platform/auth/{ref}/users/{id}", chk(members.ActAuthExecute, "update_user")),
	rule("R", "/platform/auth/{ref}/users/**", chk(members.ActSQLAdminRead, "auth.users")),

	// ---- projects: storage ----
	rule("R", "/platform/storage/{ref}/credentials/**", chk(members.ActRead, members.ResS3Credentials)),
	rule("POST", "/platform/storage/{ref}/credentials", chk(members.ActCreate, members.ResS3Credentials)),
	rule("DELETE", "/platform/storage/{ref}/credentials/{id}", chk(members.ActDelete, members.ResS3Credentials)),
	rule("POST", "/platform/storage/{ref}/buckets/{id}/objects/copy", chk(members.ActStorageAdminWrite, members.ResAny)),
	rule("POST", "/platform/storage/{ref}/buckets/{id}/objects/move", chk(members.ActStorageAdminWrite, members.ResAny)),
	// The other object POSTs (list, list-v2, sign, sign-multi, public-url) read.
	rule("POST", "/platform/storage/{ref}/buckets/{id}/objects/{op}", chk(members.ActStorageAdminRead, members.ResAny)),
	rule("R", "/platform/storage/{ref}/**", chk(members.ActStorageAdminRead, members.ResAny)),
	rule("W", "/platform/storage/{ref}/jwks/**", chk(members.ActUpdate, "custom_config_storage")),
	rule("W", "/platform/storage/{ref}/cdn/**", chk(members.ActUpdate, "custom_config_storage")),
	rule("W", "/platform/storage/{ref}/**", chk(members.ActStorageAdminWrite, members.ResAny)),

	// ---- projects: functions ----
	rule("R", "/v1/projects/{ref}/functions/**", chk(members.ActFunctionsRead, members.ResAny)),
	rule("W", "/v1/projects/{ref}/functions/**", chk(members.ActFunctionsWrite, members.ResAny)),

	// ---- projects: saved content (SQL snippets, reports, notebooks): a Developer or Read-only member may change only their own; the handlers check the item ----
	rule("R", "/platform/projects/{ref}/content/**", chk(members.ActRead, UC)),
	rule("POST", "/platform/projects/{ref}/content/**", chk(members.ActCreate, UC)),
	rule("PUT,PATCH", "/platform/projects/{ref}/content/**", chkOwn(members.ActUpdate, UC)),
	rule("DELETE", "/platform/projects/{ref}/content/**", chkOwn(members.ActDelete, UC)),
	rule("R", "/v2/projects/{ref}/notebooks/**", chk(members.ActRead, UC)),
	rule("POST", "/v2/projects/{ref}/notebooks", chk(members.ActCreate, UC)),
	rule("PATCH", "/v2/projects/{ref}/notebooks/{id}", chkOwn(members.ActUpdate, UC)),
	rule("DELETE", "/v2/projects/{ref}/notebooks/{id}", chkOwn(members.ActDelete, UC)),

	// ---- projects: logs, branches ----
	rule("POST", "/platform/projects/{ref}/analytics/endpoints/**", chk(members.ActAnalyticsRead, "logflare")),
	rule("R", "/platform/projects/{ref}/analytics/**", chk(members.ActAnalyticsRead, "logflare")),
	rule("W", "/platform/projects/{ref}/analytics/**", chk(members.ActAnalyticsAdminWrite, "logflare")),
	rule("W", "/v2/projects/{ref}/analytics/**", chk(members.ActAnalyticsAdminWrite, "logflare")),
	rule("R", "/v1/projects/{ref}/branches/**", chk(members.ActRead, members.ResPreviewBranches)),
	rule("POST", "/v1/projects/{ref}/branches", chk(members.ActCreate, members.ResPreviewBranches)),
	rule("POST", "/v2/projects/{ref}/branches", chk(members.ActCreate, members.ResPreviewBranches)),
	rule("DELETE", "/v1/projects/{ref}/branches", chk(members.ActDelete, members.ResPreviewBranches)),
	// A branch is named by id or ref and governed by its parent project (authorize).
	rule("R", "/v1/branches/{branch_id_or_ref}/**", chk(members.ActRead, members.ResPreviewBranches)),
	rule("DELETE", "/v1/branches/{branch_id_or_ref}", chk(members.ActDelete, members.ResPreviewBranches)),
	rule("W", "/v1/branches/{branch_id_or_ref}/**", chk(members.ActUpdate, members.ResPreviewBranches)),

	// ---- projects: network, billing, everything else ----
	rule("POST", "/v1/projects/{ref}/network-bans/retrieve/**", chk(members.ActRead, P)),
	rule("POST", "/v1/projects/{ref}/vanity-subdomain/check-availability", chk(members.ActRead, P)),
	rule("W", "/v1/projects/{ref}/billing/**", chk(members.ActBillingWrite, "stripe.subscriptions")),
	rule("W", "/platform/projects/{ref}/billing/**", chk(members.ActBillingWrite, "stripe.subscriptions")),
}

// routeNeed resolves a route (method and template path) to its need, applying the defaults.
func routeNeed(method, path string) need {
	for _, rr := range routeRules {
		if rr.matches(method, path) {
			return rr.n
		}
	}
	read := method == http.MethodGet || method == http.MethodHead
	switch {
	case strings.Contains(path, "{ref}"):
		if read {
			return chk(members.ActRead, P)
		}
		return chk(members.ActUpdate, P)
	case strings.Contains(path, "organizations/{slug}"):
		if read {
			return chk(members.ActRead, O)
		}
		return chk(members.ActUpdate, O)
	}
	if read {
		return nAny
	}
	return nOwner
}

// forbidden is the error of a refused permission check.
func forbidden(action, resource string) *Error {
	return errf(http.StatusForbidden, "Your role does not allow this action (%s on %s)", action, resource)
}

var errMFARequired = errf(http.StatusForbidden, "MFA required: sign in with a second factor to use this organization")

// authorize runs the permission check of a route for the authenticated caller p and returns
// the request context carrying the loaded access. key is the route in "METHOD /template"
// form, empty for a path that no spec lists.
func (s *Server) authorize(r *http.Request, key string, p *Principal) (context.Context, error) {
	method, tmpl := r.Method, r.URL.Path
	if key != "" {
		method, tmpl, _ = strings.Cut(key, " ")
	} else if strings.HasPrefix(tmpl, "/platform/") {
		// An unknown platform path: reads are harmless stubs, writes are default-deny.
		if method == http.MethodGet || method == http.MethodHead {
			return r.Context(), nil
		}
		tmpl = "/unknown"
	}
	n := routeNeed(method, tmpl)
	if n.kind == needAny {
		return r.Context(), nil
	}
	ctx := r.Context()
	access, err := s.accessOf(ctx, p)
	if err != nil {
		return ctx, err
	}
	switch {
	case strings.Contains(tmpl, "{ref}"):
		proj, err := s.loadProject(ctx, r.PathValue("ref"))
		if err != nil {
			return ctx, err
		}
		org, err := s.orgOf(ctx, proj)
		if err != nil {
			return ctx, err
		}
		ref := members.OrgRef{ID: org.ID, Slug: org.Slug}
		if err := s.gateOrg(ctx, p, access, ref); err != nil {
			return ctx, err
		}
		if n.kind == needSelf || n.kind == needOwner {
			n = chk(members.ActRead, P)
		}
		if res := n.res(r); !access.Can(ref, scopeRef(proj), n.action, res, n.data()) {
			return ctx, forbidden(n.action, res)
		}
	case strings.Contains(tmpl, "{branch_id_or_ref}"):
		// A branch belongs to its parent project: the caller needs the permission on the parent
		// (an organization-wide role, or a role scoped to the parent), whichever way the branch
		// is named, so neither the id of a branch nor its own ref is a way around a project scope.
		proj, err := s.branchParent(ctx, r.PathValue("branch_id_or_ref"))
		if err != nil {
			return ctx, err
		}
		org, err := s.orgOf(ctx, proj)
		if err != nil {
			return ctx, err
		}
		ref := members.OrgRef{ID: org.ID, Slug: org.Slug}
		if err := s.gateOrg(ctx, p, access, ref); err != nil {
			return ctx, err
		}
		if n.kind == needSelf || n.kind == needOwner {
			n = chk(members.ActRead, P)
		}
		if res := n.res(r); !access.Can(ref, proj.Ref, n.action, res, n.data()) {
			return ctx, forbidden(n.action, res)
		}
	case strings.Contains(tmpl, "organizations/{slug}"):
		org, err := s.orgBySlug(ctx, r.PathValue("slug"))
		if err != nil {
			return ctx, err
		}
		ref := members.OrgRef{ID: org.ID, Slug: org.Slug}
		if err := s.gateOrg(ctx, p, access, ref); err != nil {
			return ctx, err
		}
		if n.kind == needSelf || n.kind == needOwner {
			n = chk(members.ActRead, O)
		}
		if res := n.res(r); !access.Can(ref, "", n.action, res, n.data()) {
			return ctx, forbidden(n.action, res)
		}
	default:
		switch n.kind {
		case needOwner:
			if !access.IsOwnerAnywhere() {
				return ctx, errf(http.StatusForbidden, "Your role does not allow this action (needs the Owner role)")
			}
		case needSelf: // the handler checks (organization creation, project creation)
		default:
			return ctx, errf(http.StatusForbidden, "Your role does not allow this action")
		}
	}
	return ctx, nil
}

// scopeRef is the project ref a role scope is matched against: a branch is covered by the roles
// of its parent project, which is the project the roles were granted on.
func scopeRef(p *registry.Project) string {
	if p.Branch != nil && p.Branch.ParentRef != "" {
		return p.Branch.ParentRef
	}
	return p.Ref
}

// branchParent resolves a branch named by its id or ref (the default branch of a project is the
// project) to the parent project whose roles govern it. A branch that does not exist, and a node
// without branching, answer 404 like the handlers do.
func (s *Server) branchParent(ctx context.Context, idOrRef string) (*registry.Project, error) {
	if s.branches == nil {
		return nil, errf(http.StatusNotFound, "Branch not found")
	}
	b, err := s.branches.Resolve(ctx, idOrRef)
	if err != nil {
		return nil, mapBranchErr(err)
	}
	p, err := s.loadProject(ctx, b.ParentRef)
	if err != nil {
		return nil, errf(http.StatusNotFound, "Branch not found")
	}
	return p, nil
}

// gateOrg refuses callers who do not belong to the organization, and dashboard sessions
// without a second factor when the organization requires MFA. Personal access tokens are not
// interactive sessions and carry no aal; they are not held to the requirement.
func (s *Server) gateOrg(ctx context.Context, p *Principal, a *members.Access, org members.OrgRef) error {
	if !a.IsMember(org.ID) {
		return errf(http.StatusForbidden, "You are not a member of this organization")
	}
	if p.Via == "jwt" && p.AAL != "aal2" {
		on, err := s.members.MFAEnforced(ctx, org.ID)
		if err != nil {
			return err
		}
		if on {
			return errMFARequired
		}
	}
	return nil
}

// accessOf loads the caller's access once per request.
func (s *Server) accessOf(ctx context.Context, p *Principal) (*members.Access, error) {
	if p == nil {
		return nil, errUnauthorized
	}
	if p.access != nil {
		return p.access, nil
	}
	a, err := s.members.Access(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	p.access = a
	return a, nil
}

// callerAccess is accessOf for handlers: the principal is already in the request context.
func (s *Server) callerAccess(r *http.Request) (*members.Access, error) {
	return s.accessOf(r.Context(), principalFrom(r.Context()))
}

// can reports whether the caller of r may perform action on resource in the project (ref
// not empty) or organization.
func (s *Server) can(r *http.Request, org *registry.Organization, ref, action, resource string) (bool, error) {
	return s.canWith(r, org, ref, action, resource, nil)
}

// canWith is can for a permission conditioned on data (see members.OwnContentData).
func (s *Server) canWith(r *http.Request, org *registry.Organization, ref, action, resource string, data map[string]any) (bool, error) {
	a, err := s.callerAccess(r)
	if err != nil {
		return false, err
	}
	return a.Can(members.OrgRef{ID: org.ID, Slug: org.Slug}, ref, action, resource, data), nil
}

// require is can that returns the refusal as an error.
func (s *Server) require(r *http.Request, org *registry.Organization, ref, action, resource string) error {
	ok, err := s.can(r, org, ref, action, resource)
	if err != nil {
		return err
	}
	if !ok {
		return forbidden(action, resource)
	}
	return nil
}

// projectAccess returns p's organization and whether the caller may perform action on
// resource in it.
func (s *Server) projectCan(r *http.Request, p *registry.Project, action, resource string) (bool, error) {
	org, err := s.orgOf(r.Context(), p)
	if err != nil {
		return false, err
	}
	return s.can(r, org, scopeRef(p), action, resource)
}

// canContent reports whether the caller u may perform action (create, update or delete) on a
// saved item or folder of typ, visibility and owner in project p. Owners and Administrators may
// change anyone's shared items; every other role only its own.
func (s *Server) canContent(r *http.Request, p *registry.Project, u *User, action, typ, visibility string, ownerID int64) (bool, error) {
	org, err := s.orgOf(r.Context(), p)
	if err != nil {
		return false, err
	}
	return s.canWith(r, org, scopeRef(p), action, members.ResUserContent, members.OwnContentData(typ, visibility, ownerID, u.ID))
}

// canWriteSQL reports whether the caller may change data and schema; a caller who may only
// query has every statement run as the read-only database role.
func (s *Server) canWriteSQL(r *http.Request, p *registry.Project) (bool, error) {
	return s.projectCan(r, p, members.ActSQLInsert, members.ResAny)
}

// canReadSecrets reports whether the caller may read the project's service_role key and JWT
// secret (not the Read-only role).
func (s *Server) canReadSecrets(r *http.Request, p *registry.Project) (bool, error) {
	ok, err := s.projectCan(r, p, members.ActRead, members.ResServiceKeys)
	if err != nil || !ok {
		return false, err
	}
	return s.projectCan(r, p, members.ActRead, members.ResJWTSecret)
}

// requireBranchData is the second requirement of POST /v1/projects/{ref}/branches: a branch
// created with with_data copies the parent's data (every user, every row), which a Developer,
// who may create schema-only branches (the route's own rule), may not read in bulk outside the
// SQL editor. It needs the permission to update the project's settings, which is what makes
// an Owner or Administrator (also when the role is scoped to this project). The route table
// cannot see the body, so createBranch calls this once it knows with_data is set.
func (s *Server) requireBranchData(r *http.Request, p *registry.Project) error {
	ok, err := s.projectCan(r, p, members.ActUpdate, P)
	if err != nil {
		return err
	}
	if !ok {
		return errf(http.StatusForbidden, "Your role does not allow this action (creating a branch with data needs the Owner or Administrator role)")
	}
	return nil
}
