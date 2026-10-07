package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/members"
	"github.com/OWNER/sbctl/internal/registry"
)

var (
	opIndexOnce sync.Once
	opIndex     map[string]*Operation
)

func operationByKey(key string) *Operation {
	opIndexOnce.Do(func() {
		ops, _ := Operations()
		opIndex = make(map[string]*Operation, len(ops))
		for _, o := range ops {
			opIndex[o.Key()] = o
		}
	})
	return opIndex[key]
}

// base returns a fresh minimal valid instance of the success response of the
// operation "METHOD /path", as a map. Handlers overwrite the fields they know with
// set, so the response stays valid against the spec when it grows required fields
// that this server does not model yet.
func base(key string) map[string]any {
	op := operationByKey(key)
	if op == nil || !op.JSON {
		panic("api: no JSON response schema for " + key)
	}
	m, ok := MinimalValue(op.Response).(map[string]any)
	if !ok {
		panic("api: response of " + key + " is not an object")
	}
	return m
}

// set writes v at the dotted path of nested objects in m, creating objects on the way.
func set(m map[string]any, path string, v any) {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}

// setAll applies set for every entry of kv.
func setAll(m map[string]any, kv map[string]any) map[string]any {
	for k, v := range kv {
		set(m, k, v)
	}
	return m
}

// ts formats a time the way the Management API does (RFC 3339 with milliseconds, UTC).
func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func queryInt(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

var versionRe = regexp.MustCompile(`\d+(?:\.\d+)+`)

// pgVersion extracts "17.11.0.004" from an artifact tag such as "postgres-17.11.0.004-r1".
func pgVersion(p *registry.Project) string {
	if v := versionRe.FindString(p.Versions[config.SvcPostgres]); v != "" {
		return v
	}
	return "17.0.0"
}

// pgMajor is the major version of the project's Postgres.
func pgMajor(p *registry.Project) int {
	major, _, _ := strings.Cut(pgVersion(p), ".")
	n, _ := strconv.Atoi(major)
	if n == 0 {
		return 17
	}
	return n
}

// publicScheme is the scheme of sbctl's public listeners.
func (s *Server) publicScheme() string {
	if s.cfg.TLS.Mode == "off" {
		return "http"
	}
	return "https"
}

// projectURL is the project's public API origin.
func (s *Server) projectURL(ref string) string {
	return s.publicScheme() + "://" + s.cfg.ProjectHost(ref)
}

// dbHost is the direct database host the CLI derives for a project
// (db.<ref>.<project_host> with project_host = the API host).
func (s *Server) dbHost(ref string) string { return "db." + s.cfg.ProjectHost(ref) }

// projectKey is the stable numeric id Studio's types carry for a project.
func projectNumID(p *registry.Project) float32 { return float32(p.Seq) }

// loadProject returns the user project ref. The system project and unknown refs are
// reported as 404 like any other missing project.
func (s *Server) loadProject(ctx context.Context, ref string) (*registry.Project, error) {
	if ref == config.SystemRef {
		return nil, errNoProject
	}
	p, err := s.reg.GetProject(ctx, ref)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, errNoProject
	}
	return p, err
}

var errNoProject = &Error{Status: http.StatusNotFound, Message: "Project not found"}

// running returns the project when it is ACTIVE_HEALTHY or ACTIVE_UNHEALTHY and
// reports 4xx errors that explain why SQL cannot run otherwise.
func (s *Server) running(ctx context.Context, ref string) (*registry.Project, error) {
	p, err := s.loadProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	switch p.Status {
	case registry.StatusActiveHealthy, registry.StatusActiveUnhealthy:
		return p, nil
	case registry.StatusInactive:
		return nil, errf(http.StatusConflict, "Project is paused; restore it first")
	default:
		return nil, errf(http.StatusServiceUnavailable, "Project is not ready (status %s)", p.Status)
	}
}

// defaultOrg returns the first organization, creating "default" on a fresh install.
func (s *Server) defaultOrg(ctx context.Context) (*registry.Organization, error) {
	orgs, err := s.reg.ListOrganizations(ctx)
	if err != nil {
		return nil, err
	}
	if len(orgs) > 0 {
		return &orgs[0], nil
	}
	o, err := s.reg.CreateOrganization(ctx, "default", "Default")
	if errors.Is(err, registry.ErrConflict) {
		return s.reg.GetOrganization(ctx, "default")
	}
	return o, err
}

// orgOf returns the organization of p; projects without one belong to the default.
func (s *Server) orgOf(ctx context.Context, p *registry.Project) (*registry.Organization, error) {
	if p.OrgID != 0 {
		o, err := s.reg.GetOrganizationByID(ctx, p.OrgID)
		if err == nil {
			return o, nil
		}
		if !errors.Is(err, registry.ErrNotFound) {
			return nil, err
		}
	}
	return s.defaultOrg(ctx)
}

// userProjects lists the projects shown to the caller: everything but "system" and the
// branches (a branch is listed under its parent and opened by its ref, as on hosted Supabase)
// that the caller's roles let them see (an organization-wide role sees all projects of the
// organization, a project-scoped role only its projects). Without a caller in ctx it lists
// them all.
func (s *Server) userProjects(ctx context.Context) ([]registry.Project, error) {
	all, err := s.reg.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var access *members.Access
	if p := principalFrom(ctx); p != nil {
		if access, err = s.accessOf(ctx, p); err != nil {
			return nil, err
		}
	}
	orgs := map[int64]members.OrgRef{}
	// A dashboard session below aal2 does not see the projects of an organization that requires
	// MFA, as it cannot open them.
	pr := principalFrom(ctx)
	checkMFA := pr != nil && pr.Via == "jwt" && pr.AAL != "aal2"
	mfaOff := map[int64]bool{}
	out := all[:0:0]
	for _, p := range all {
		if p.Ref == config.SystemRef || p.Branch != nil {
			continue
		}
		if access != nil {
			org, err := s.orgOf(ctx, &p)
			if err != nil {
				return nil, err
			}
			ref, ok := orgs[org.ID]
			if !ok {
				ref = orgRef(org)
				orgs[org.ID] = ref
			}
			if !access.Can(ref, scopeRef(&p), members.ActRead, members.ResProjects, nil) {
				continue
			}
			if checkMFA {
				off, seen := mfaOff[org.ID]
				if !seen {
					on, err := s.members.MFAEnforced(ctx, org.ID)
					if err != nil {
						return nil, err
					}
					off = !on
					mfaOff[org.ID] = off
				}
				if !off {
					continue
				}
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// requireOrgMember refuses a caller who does not belong to the organization.
func (s *Server) requireOrgMember(r *http.Request, org *registry.Organization) error {
	a, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	if !a.IsMember(org.ID) {
		return errf(http.StatusForbidden, "You are not a member of this organization")
	}
	return nil
}

// defaultCreateOrg is the organization a project is created in when the request names none: the
// first one the caller may create projects in.
func (s *Server) defaultCreateOrg(r *http.Request) (*registry.Organization, error) {
	if _, err := s.defaultOrg(r.Context()); err != nil {
		return nil, err
	}
	orgs, err := s.memberOrgs(r)
	if err != nil {
		return nil, err
	}
	for i := range orgs {
		if ok, err := s.can(r, &orgs[i], "", members.ActCreate, members.ResProjects); err != nil {
			return nil, err
		} else if ok {
			return &orgs[i], nil
		}
	}
	return nil, errf(http.StatusForbidden, "Your role does not allow creating projects in any organization")
}

// orgBySlug resolves an organization slug of a path.
func (s *Server) orgBySlug(ctx context.Context, slug string) (*registry.Organization, error) {
	o, err := s.reg.GetOrganization(ctx, slug)
	if errors.Is(err, registry.ErrNotFound) {
		// Older clients pass the numeric id where the slug goes.
		if id, perr := strconv.ParseInt(slug, 10, 64); perr == nil {
			if o, err = s.reg.GetOrganizationByID(ctx, id); err == nil {
				return o, nil
			}
		}
	}
	if errors.Is(err, registry.ErrNotFound) {
		return nil, errf(http.StatusNotFound, "Organization not found")
	}
	return o, err
}

// sqlLiteral quotes s as a standard-conforming SQL string literal.
func sqlLiteral(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, "\x00", ""), "'", "''") + "'"
}

// sqlIdent quotes s as a SQL identifier.
func sqlIdent(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, "\x00", ""), `"`, `""`) + `"`
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func writeRaw(w http.ResponseWriter, status int, contentType string, b []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeNoContent answers 204.
func writeNoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// jsonBody decodes a request body into a map.
func jsonBody(r *http.Request) (map[string]any, error) {
	m := map[string]any{}
	if err := decode(r, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// mustJSON marshals v for embedding in a response built with set.
func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// withQuery returns u with the given query values added.
func withQuery(u string, kv url.Values) string {
	if len(kv) == 0 {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + kv.Encode()
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// truncate shortens s to at most n bytes plus an ellipsis, for log fields and messages.
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
