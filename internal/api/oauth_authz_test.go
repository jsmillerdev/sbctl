package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// The tests of how the API authenticates and authorizes OAuth access tokens (oauth_authn.go,
// oauth_scopes.go). The OAuth service is a fake with the contract of oauth.Authority.LookupAccess:
// ErrNotFound for every kind of unusable token. What the real service does with expiry, revoked
// grants and deleted apps is its own tests' business (internal/oauth); what these tests prove is
// that the API turns each of those answers into a 401 and applies everything else it owes a
// token: no /platform, the scope gate, one organization, live roles.

// oauthAuthzFake is the oauth.Authority these tests put in Server.oauth.
type oauthAuthzFake struct {
	oauth.Authority
	mu      sync.Mutex
	next    int64
	tokens  map[string]oauth.AccessInfo
	dead    map[int64]bool // grant id -> ended
	err     error          // returned by LookupAccess when set
	lookups int
	touches []int64 // token ids, in order
	revokes []oauthRevokeCall
}

type oauthRevokeCall struct {
	Grant         int64
	Reason, Actor string
}

func newOAuthAuthzFake() *oauthAuthzFake {
	return &oauthAuthzFake{tokens: map[string]oauth.AccessInfo{}, dead: map[int64]bool{}}
}

// issue registers a live token of a grant of userID in the organization with the scopes, and
// returns the token. The grant's scopes and the app's are the same list.
func (f *oauthAuthzFake) issue(userID string, orgID int64, orgSlug string, scopes ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	tok := fmt.Sprintf("%s%040x", oauth.AccessTokenPrefix, 0xabc0000+f.next)
	f.tokens[tok] = oauth.AccessInfo{
		TokenID: f.next, GrantID: 1000 + f.next, AppID: "7b1e0b1c-5d1c-4f7e-9d61-0a1b2c3d4e5f", AppName: "Test client",
		UserID: userID, OrgID: orgID, OrgSlug: orgSlug, ExpiresAt: time.Now().Add(time.Hour),
		GrantScopes: scopes, AppScopes: scopes, Scopes: oauth.NormalizeScopes(scopes),
	}
	return tok
}

// edit changes the stored access info of a token (a grant narrowed, a resource bound).
func (f *oauthAuthzFake) edit(tok string, fn func(*oauth.AccessInfo)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.tokens[tok]
	fn(&i)
	f.tokens[tok] = i
}

func (f *oauthAuthzFake) grantOf(tok string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[tok].GrantID
}

// kill ends a grant the way a revocation, an app deletion or an expiry does for the real service.
func (f *oauthAuthzFake) kill(grant int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dead[grant] = true
}

func (f *oauthAuthzFake) LookupAccess(_ context.Context, token string) (*oauth.AccessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.err != nil {
		return nil, f.err
	}
	i, ok := f.tokens[token]
	if !ok || f.dead[i.GrantID] {
		return nil, oauth.ErrNotFound
	}
	return &i, nil
}

func (f *oauthAuthzFake) TouchAccess(_ context.Context, tokenID, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches = append(f.touches, tokenID)
	return nil
}

func (f *oauthAuthzFake) RevokeGrant(_ context.Context, grantID int64, reason, actor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokes = append(f.revokes, oauthRevokeCall{grantID, reason, actor})
	if f.dead[grantID] {
		return oauth.ErrNotFound
	}
	f.dead[grantID] = true
	return nil
}

func (f *oauthAuthzFake) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

const (
	authzOtherRef = "cccccccccccccccccccc"
	// authzSomeAuthID is the id of an authorization request the routes under test never find.
	authzSomeAuthID = "0f0c8e8a-7b3d-4c55-9a5e-2d6a1b7c9e10"
)

// oauthAuthzFixture is the roles fixture (one user per role in the organization "default") with a
// second organization "other" that the Owner also owns, and the fake service installed.
type oauthAuthzFixture struct {
	*rolesFixture
	fake  *oauthAuthzFake
	other *registry.Organization
}

func newOAuthAuthzFixture(t testing.TB) *oauthAuthzFixture {
	t.Helper()
	rf := newRolesFixture(t)
	ctx := context.Background()
	other, err := rf.reg.CreateOrganization(ctx, "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	if err := rf.srv.members.EnsureOwner(ctx, members.OrgRef{ID: other.ID, Slug: other.Slug}, rf.userID); err != nil {
		t.Fatal(err)
	}
	rf.mgr.addProject(t, authzOtherRef, "Project of the other organization", other.ID, registry.StatusActiveHealthy)
	x := &oauthAuthzFixture{rolesFixture: rf, fake: newOAuthAuthzFake(), other: other}
	rf.srv.oauth = x.fake
	return x
}

// token issues a token for the user of role (a name of roleNames) in the organization "default".
func (x *oauthAuthzFixture) token(role string, scopes ...string) string {
	return x.fake.issue(x.ids[role], x.org.ID, x.org.Slug, scopes...)
}

func (x *oauthAuthzFixture) req(token, method, path string, body any) *httptest.ResponseRecorder {
	x.t.Helper()
	return x.doAs(token, method, path, body)
}

func authzRefused(rec *httptest.ResponseRecorder) bool { return rec.Code == 401 || rec.Code == 403 }

// authzMCPScopes are the scopes of a dynamic app: what Claude Code and the other MCP clients get.
var authzMCPScopes = oauth.AdvertisedScopes

func TestOAuthTokenCannotReachPlatform(t *testing.T) { // U1
	x := newOAuthAuthzFixture(t)
	tok := x.token("owner", oauth.AllScopes...)
	routes := []struct{ method, path string }{
		{"GET", "/platform/profile"},
		{"GET", "/platform/profile/permissions"},
		{"POST", "/platform/profile/access-tokens"},
		{"GET", "/platform/organizations"},
		{"GET", "/platform/projects"},
		{"GET", "/platform/projects/" + testRef},
		{"POST", "/platform/pg-meta/" + testRef + "/query"},
		{"GET", "/platform/oauth/authorizations/" + authzSomeAuthID},
		{"POST", "/platform/organizations/default/oauth/authorizations/" + authzSomeAuthID},
		{"DELETE", "/platform/organizations/default/oauth/authorizations/" + authzSomeAuthID},
		{"GET", "/platform/organizations/default/oauth/apps"},
		{"POST", "/platform/organizations/default/oauth/apps"},
	}
	for _, c := range routes {
		rec := x.req(tok, c.method, c.path, map[string]any{})
		if rec.Code != 401 {
			t.Errorf("%s %s with an OAuth token: %d %s, want 401", c.method, c.path, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
	if n := x.fake.lookupCount(); n != 0 {
		t.Errorf("a /platform route looked the token up %d times; it must be refused before any lookup", n)
	}
	// Nor can a personal access token decide an authorization request: approving mints a credential, so
	// it takes a signed-in session (K5).
	pat := x.pat("owner")
	for _, c := range routes[7:10] {
		if rec := x.req(pat, c.method, c.path, map[string]any{}); rec.Code != 401 {
			t.Errorf("%s %s with a personal access token: %d, want 401", c.method, c.path, rec.Code)
		}
	}
	// The dashboard's own session still works on the same routes.
	if rec := x.doAs(x.jwt, "GET", "/platform/profile", nil); rec.Code != 200 {
		t.Errorf("session on /platform/profile: %d", rec.Code)
	}
	// A /platform path no spec lists is answered by the catch-all, which takes any credential; an
	// OAuth token must not get the stub's empty 200 there either.
	for _, m := range []string{"GET", "POST"} {
		rec := x.req(tok, m, "/platform/zzz-unknown", map[string]any{})
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "not available to OAuth tokens") {
			t.Errorf("%s /platform/zzz-unknown with an OAuth token: %d %s", m, rec.Code, rec.Body)
		}
	}
	if rec := x.doAs(x.jwt, "GET", "/platform/zzz-unknown", nil); rec.Code != 200 {
		t.Errorf("session on an unknown /platform path: %d", rec.Code)
	}
}

func TestOAuthScopeEnforced(t *testing.T) { // U2
	x := newOAuthAuthzFixture(t)
	p := "/v1/projects/" + testRef
	sql := map[string]any{"query": "select 1"}
	x.withStorage(t)
	prm := `resource_metadata="https://api.example.test/.well-known/oauth-protected-resource/mcp"`

	type tc struct {
		name   string
		scopes []string
		method string
		path   string
		body   any
		// want: 0 allowed (not 401 or 403); "scope:<name>" an insufficient_scope refusal naming it;
		// "na" a refusal because no scope opens the route.
		want string
	}
	cases := []tc{
		{"read-only SQL with database:read", []string{"database:read"}, "POST", p + "/database/query/read-only", sql, ""},
		{"SQL with database:read is refused", []string{"database:read"}, "POST", p + "/database/query", sql, "scope:database:write"},
		{"write does not imply read", []string{"database:write"}, "POST", p + "/database/query/read-only", sql, "scope:database:read"},
		{"SQL with database:write", []string{"database:write"}, "POST", p + "/database/query", sql, ""},
		{"no scope at all", nil, "POST", p + "/database/query/read-only", sql, "scope:database:read"},
		{"a list that any user may call needs its scope", []string{"projects:read"}, "GET", "/v1/organizations", nil, "scope:organizations:read"},
		{"organizations:read", []string{"organizations:read"}, "GET", "/v1/organizations", nil, ""},
		{"projects:read lists projects", []string{"projects:read"}, "GET", "/v1/projects", nil, ""},
		{"projects:read does not pause", []string{"projects:read"}, "POST", p + "/pause", nil, "scope:projects:write"},
		{"migrations are database:read", []string{"database:read"}, "GET", p + "/database/migrations", nil, ""},
		{"applying one is database:write", []string{"database:read"}, "POST", p + "/database/migrations", map[string]any{"name": "m", "query": "select 1"}, "scope:database:write"},
		{"logs are analytics:read", []string{"database:read"}, "GET", p + "/analytics/endpoints/logs.all", nil, "scope:analytics:read"},
		{"edge functions", []string{"edge_functions:read"}, "GET", p + "/functions", nil, ""},
		{"api keys are secrets:read", []string{"projects:read"}, "GET", p + "/api-keys", nil, "scope:secrets:read"},
		// The storage config is unannotated in the specs; the override maps it to storage:read and storage:write.
		{"storage config read", []string{"storage:read"}, "GET", p + "/config/storage", nil, ""},
		{"storage config write", []string{"storage:write"}, "PATCH", p + "/config/storage", map[string]any{"fileSizeLimit": 1000}, ""},
		{"storage:read does not write the config", []string{"storage:read"}, "PATCH", p + "/config/storage", map[string]any{"fileSizeLimit": 1000}, "scope:storage:write"},
		{"storage:write does not read the config", []string{"storage:write"}, "GET", p + "/config/storage", nil, "scope:storage:read"},
		// Unannotated operations are denied to every scope, all 24 included.
		{"profile", oauth.AllScopes, "GET", "/v1/profile", nil, "na"},
		{"creating an organization", oauth.AllScopes, "POST", "/v1/organizations", map[string]any{"name": "x"}, "na"},
		{"billing add-ons", oauth.AllScopes, "GET", p + "/billing/addons", nil, "na"},
		{"disk config", oauth.AllScopes, "GET", p + "/config/disk", nil, "na"},
		{"read replicas", oauth.AllScopes, "POST", p + "/read-replicas/setup", map[string]any{"read_replica_region": "eu-west-1"}, "na"},
		{"the node's health in detail", oauth.AllScopes, "GET", "/healthz/detail", nil, "na"},
		{"project claim", oauth.AllScopes, "GET", "/v1/oauth/authorize/project-claim", nil, "na"},
		{"changing a member's role", oauth.AllScopes, "PATCH", "/v2/organizations/default/members/" + x.ids["dev"] + "/roles", map[string]any{"role_id": 3}, "na"},
		{"a route that is in no spec", oauth.AllScopes, "GET", "/v1/zzz-unknown", nil, "na"},
		{"a route that is in no spec (write)", oauth.AllScopes, "DELETE", "/v1/zzz-unknown", nil, "na"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := x.req(x.token("owner", c.scopes...), c.method, c.path, c.body)
			challenge := rec.Header().Get("WWW-Authenticate")
			switch {
			case c.want == "":
				if authzRefused(rec) {
					t.Fatalf("%s %s refused: %d %s", c.method, c.path, rec.Code, rec.Body)
				}
			case c.want == "na":
				if rec.Code != 403 || !strings.Contains(rec.Body.String(), "This operation is not available to OAuth tokens") {
					t.Fatalf("%s %s: %d %s, want the 403 of an operation no scope opens", c.method, c.path, rec.Code, rec.Body)
				}
				if challenge != "" {
					t.Errorf("a route that no scope opens must not suggest a scope to ask for: WWW-Authenticate %q", challenge)
				}
			default:
				scope := strings.TrimPrefix(c.want, "scope:")
				if rec.Code != 403 {
					t.Fatalf("%s %s: %d %s, want 403", c.method, c.path, rec.Code, rec.Body)
				}
				want := fmt.Sprintf(`Bearer error="insufficient_scope", scope=%q, %s`, scope, prm)
				if challenge != want {
					t.Errorf("WWW-Authenticate\n got %s\nwant %s", challenge, want)
				}
				if msg, _ := jsonField(t, rec, "message").(string); !strings.Contains(msg, scope) {
					t.Errorf("message %q does not name the scope", msg)
				}
			}
		})
	}

	// A token whose lookup carries no effective scopes (the service computes them; one that did not
	// would fail closed) can call nothing.
	bare := x.token("owner", "projects:read")
	x.fake.edit(bare, func(i *oauth.AccessInfo) { i.Scopes = nil })
	if rec := x.req(bare, "GET", "/v1/projects", nil); rec.Code != 403 {
		t.Errorf("a token without effective scopes: %d", rec.Code)
	}
	// Shrinking the app's scopes applies to the next request (the service's intersection).
	narrow := x.token("owner", "projects:read", "database:read")
	if rec := x.req(narrow, "POST", p+"/database/query/read-only", sql); authzRefused(rec) {
		t.Fatalf("before narrowing: %d", rec.Code)
	}
	x.fake.edit(narrow, func(i *oauth.AccessInfo) { i.Scopes = []string{"projects:read"} })
	if rec := x.req(narrow, "POST", p+"/database/query/read-only", sql); rec.Code != 403 {
		t.Errorf("after narrowing: %d", rec.Code)
	}

	// The gate is the OAuth token's alone: a session and a personal access token reach the same routes.
	pat := x.pat("owner")
	for name, cred := range map[string]string{"session": x.jwt, "personal access token": pat} {
		for _, path := range []string{"/v1/profile", p + "/config/disk", "/v1/organizations"} {
			if rec := x.req(cred, "GET", path, nil); authzRefused(rec) {
				t.Errorf("%s on GET %s: %d %s", name, path, rec.Code, rec.Body)
			}
		}
	}
}

// authzGoldenScopes renders the table that gates OAuth tokens: one line per operation that a scope opens,
// sorted, with the two operations the specs leave unannotated marked.
func authzGoldenScopes(t *testing.T) string {
	t.Helper()
	ops, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, op := range ops {
		if op.Scope != "" {
			if op.Spec == "platform" {
				t.Errorf("%s: the platform spec is annotated; OAuth tokens never reach /platform", op.Key())
			}
			lines = append(lines, fmt.Sprintf("%s\t%s", op.Key(), op.Scope))
		}
	}
	for key, scope := range oauthScopeOverrides {
		lines = append(lines, fmt.Sprintf("%s\t%s\t(override)", key, scope))
	}
	sort.Strings(lines)
	head := "# The scope an OAuth access token needs for each operation (x-oauth-scope of the pinned specs).\n" +
		"# An operation that is not listed is not available to OAuth tokens.\n" +
		"# Regenerate with SUPAVISE_UPDATE_GOLDEN=1 go test ./internal/api -run TestOAuthScopeGolden\n"
	return head + strings.Join(lines, "\n") + "\n"
}

func TestOAuthScopeGolden(t *testing.T) { // U2
	got := authzGoldenScopes(t)
	path := filepath.Join("testdata", "oauth_scopes.golden")
	if os.Getenv("SUPAVISE_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("the scopes of the operations changed (a new Supabase spec?). Review the diff, then regenerate %s with SUPAVISE_UPDATE_GOLDEN=1.\n%s", path, authzFirstDifference(string(want), got))
	}
	// Every scope is one the vocabulary knows, so no spec value silently becomes an unusable one.
	for _, l := range strings.Split(got, "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Split(l, "\t")
		if !oauth.ValidScope(f[1]) {
			t.Errorf("%s: %q is not one of the 24 scopes", f[0], f[1])
		}
	}
	// The map the gate reads is the table, nothing more: nothing is open that is not listed.
	if n, m := strings.Count(got, "\n")-3, len(oauthScopeTable()); n != m {
		t.Errorf("the gate's table has %d entries, the golden file lists %d", m, n)
	}
}

// authzFirstDifference names the first line two texts differ at.
func authzFirstDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			return fmt.Sprintf("first difference at line %d:\n  golden: %q\n  now:    %q", i+1, a, b)
		}
	}
	return ""
}

// The overrides are for operations the specs leave unannotated. One the spec annotates later is the
// spec's now.
func TestOAuthScopeOverrides(t *testing.T) {
	ops, _ := Operations()
	byKey := map[string]*Operation{}
	for _, op := range ops {
		byKey[op.Key()] = op
	}
	for key, scope := range oauthScopeOverrides {
		op := byKey[key]
		if op == nil {
			t.Errorf("override %s: no such operation in the specs", key)
			continue
		}
		if op.Scope != "" {
			t.Errorf("override %s: the spec annotates it with %q now; delete the override", key, op.Scope)
		}
		if !oauth.IsAdvertised(scope) {
			t.Errorf("override %s: %s is not an advertised scope", key, scope)
		}
	}
}

// authzMCPCalls is every Management API call the Supabase MCP server (packages/mcp-server-supabase,
// platform/api-platform.ts) makes for its tools, by the route the server registers. Read-only mode
// uses the read-only SQL route. Unlike a project-scoped tool, the account tools run with no project.
var authzMCPCalls = []string{
	"GET /v1/organizations",
	"GET /v1/organizations/{slug}",
	"GET /v1/projects",
	"GET /v1/projects/{ref}",
	"POST /v1/projects",
	"POST /v1/projects/{ref}/pause",
	"POST /v1/projects/{ref}/restore",
	"POST /v1/projects/{ref}/database/query",
	"POST /v1/projects/{ref}/database/query/read-only",
	"GET /v1/projects/{ref}/database/migrations",
	"POST /v1/projects/{ref}/database/migrations",
	"GET /v1/projects/{ref}/analytics/endpoints/logs.all",
	"GET /v1/projects/{ref}/advisors/security",
	"GET /v1/projects/{ref}/advisors/performance",
	"GET /v1/projects/{ref}/api-keys",
	"GET /v1/projects/{ref}/api-keys/legacy",
	"GET /v1/projects/{ref}/types/typescript",
	"GET /v1/projects/{ref}/functions",
	"GET /v1/projects/{ref}/functions/{function_slug}",
	"GET /v1/projects/{ref}/functions/{function_slug}/body",
	"POST /v1/projects/{ref}/functions/deploy",
	"GET /v1/projects/{ref}/branches",
	"POST /v1/projects/{ref}/branches",
	"DELETE /v1/branches/{branch_id_or_ref}",
	"POST /v1/branches/{branch_id_or_ref}/merge",
	"POST /v1/branches/{branch_id_or_ref}/reset",
	"POST /v1/branches/{branch_id_or_ref}/push",
	"GET /v1/projects/{ref}/storage/buckets",
	"GET /v1/projects/{ref}/config/storage",
	"PATCH /v1/projects/{ref}/config/storage",
	"GET /v1/projects/{ref}/secrets",
}

func TestMCPCallsAreCoveredByAdvertisedScopes(t *testing.T) { // U2
	ops, _ := Operations()
	inSpec := map[string]bool{}
	for _, op := range ops {
		inSpec[op.Key()] = true
	}
	used := map[string]bool{}
	for _, key := range authzMCPCalls {
		if !inSpec[key] {
			t.Errorf("%s is not an operation of the pinned specs", key)
		}
		scope, ok := oauthScopeOf(key)
		if !ok {
			t.Errorf("%s: no scope opens it; an MCP client could not use that tool", key)
			continue
		}
		if !oauth.IsAdvertised(scope) {
			t.Errorf("%s needs %s, which the discovery documents do not advertise: a dynamic app could never hold it", key, scope)
		}
		used[scope] = true
	}
	// And the reverse: every scope the documents advertise opens at least one call of the MCP server.
	for _, s := range oauth.AdvertisedScopes {
		if !used[s] {
			t.Errorf("%s is advertised but no call of the MCP server needs it", s)
		}
	}
	if n := len(authzMCPCalls); n != 31 {
		t.Errorf("the list holds %d calls; update it together with the MCP server's tools and this count", n)
	}
}

func TestOAuthOrgBound(t *testing.T) { // U3
	x := newOAuthAuthzFixture(t)
	inA := x.token("owner", authzMCPScopes...)
	inB := x.fake.issue(x.userID, x.other.ID, x.other.Slug, authzMCPScopes...)

	// The same user, with their session, belongs to both: the restriction is the token's.
	both := x.doAs(x.jwt, "GET", "/v1/organizations", nil).Body.String()
	if !strings.Contains(both, `"default"`) || !strings.Contains(both, `"other"`) {
		t.Fatalf("fixture: the Owner should see both organizations: %s", both)
	}

	listsOf := func(tok string) (orgs, projects string) {
		o := x.req(tok, "GET", "/v1/organizations", nil)
		pr := x.req(tok, "GET", "/v1/projects", nil)
		if o.Code != 200 || pr.Code != 200 {
			t.Fatalf("lists: %d %d", o.Code, pr.Code)
		}
		return o.Body.String(), pr.Body.String()
	}
	orgs, projs := listsOf(inA)
	if !strings.Contains(orgs, `"default"`) || strings.Contains(orgs, `"other"`) {
		t.Errorf("a token for default lists organizations: %s", orgs)
	}
	if !strings.Contains(projs, testRef) || !strings.Contains(projs, secondRef) || strings.Contains(projs, authzOtherRef) {
		t.Errorf("a token for default lists projects: %s", projs)
	}
	orgs, projs = listsOf(inB)
	if strings.Contains(orgs, `"default"`) || !strings.Contains(orgs, `"other"`) {
		t.Errorf("a token for other lists organizations: %s", orgs)
	}
	if strings.Contains(projs, testRef) || strings.Contains(projs, secondRef) || !strings.Contains(projs, authzOtherRef) {
		t.Errorf("a token for other lists projects: %s", projs)
	}

	// Every route that takes an organization or a project of the other one is refused, whatever
	// the user's role there.
	sql := map[string]any{"query": "select 1"}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/projects/" + authzOtherRef, nil},
		{"GET", "/v1/projects/" + authzOtherRef + "/api-keys", nil},
		{"GET", "/v1/projects/" + authzOtherRef + "/functions", nil},
		{"POST", "/v1/projects/" + authzOtherRef + "/database/query", sql},
		{"POST", "/v1/projects/" + authzOtherRef + "/database/query/read-only", sql},
		{"GET", "/v1/projects/" + authzOtherRef + "/types/typescript", nil},
		{"POST", "/v1/projects/" + authzOtherRef + "/pause", nil},
		{"GET", "/v1/projects/" + authzOtherRef + "/branches", nil},
		{"GET", "/v1/organizations/other", nil},
		{"GET", "/v1/organizations/other/members", nil},
		{"GET", "/v1/organizations/other/entitlements", nil},
		{"GET", "/v2/organizations/other/members", nil},
		{"GET", "/v2/organizations/other/roles", nil},
		{"POST", "/v1/projects", map[string]any{"name": "x", "organization_slug": "other", "db_pass": "pw", "region": "local"}},
	} {
		rec := x.req(inA, c.method, c.path, c.body)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "not a member of this organization") {
			t.Errorf("%s %s with a token for default: %d %s, want 403 (not a member)", c.method, c.path, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
	// And the other way: a token for other has no access to default's project.
	if rec := x.req(inB, "GET", "/v1/projects/"+testRef, nil); rec.Code != 403 {
		t.Errorf("a token for other on a project of default: %d", rec.Code)
	}
	// Its own organization works, so the refusals above are about the organization.
	for _, path := range []string{"/v1/projects/" + authzOtherRef, "/v1/organizations/other", "/v1/organizations/other/members"} {
		if rec := x.req(inB, "GET", path, nil); authzRefused(rec) {
			t.Errorf("a token for other on GET %s: %d %s", path, rec.Code, rec.Body)
		}
	}

	// The principal's Access is restricted, and a route that takes no organization sees that one only.
	r := httptest.NewRequest("GET", "/v1/projects", nil)
	r.Header.Set("Authorization", "Bearer "+inA)
	p, err := x.srv.auth.authenticate(r, authAny)
	if err != nil {
		t.Fatal(err)
	}
	if p.access == nil || len(p.access.Memberships) != 1 || p.access.Memberships[0].OrgID != x.org.ID {
		t.Fatalf("the principal's access: %+v", p.access)
	}
	if p.OAuth.OrgSlug != "default" || p.OAuth.OrgID != x.org.ID {
		t.Errorf("OAuthInfo: %+v", p.OAuth)
	}
}

func TestOAuthRoleCap(t *testing.T) { // U4
	x := newOAuthAuthzFixture(t)
	p := "/v1/projects/" + testRef
	all := oauth.AllScopes // the scopes allow it; the user's role decides

	// A Read-only member's token with write scopes stays read-only. Their SQL runs as the read-only role.
	dsnUser := func() string {
		d := x.meta.last().DSN
		return strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(d, "postgres://"), "postgresql://"), ":", 2)[0]
	}
	ro := x.token("ro", all...)
	if rec := x.req(ro, "POST", p+"/database/query", map[string]any{"query": "select 1"}); rec.Code != 201 {
		t.Fatalf("Read-only SQL: %d %s", rec.Code, rec.Body)
	}
	if got := dsnUser(); got != roleReadOnly {
		t.Errorf("a Read-only member's token ran SQL as %s, want %s", got, roleReadOnly)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", p + "/pause"},
		{"PATCH", p + "/postgrest"},
		{"POST", p + "/secrets"},
		{"POST", p + "/functions/deploy?slug=f"},
		{"POST", p + "/branches"},
		{"DELETE", p + "/secrets"},
		{"POST", p + "/api-keys"},
	} {
		if rec := x.req(ro, c.method, c.path, map[string]any{}); rec.Code != 403 || !strings.HasPrefix(jsonField(t, rec, "message").(string), "Your role does not allow") {
			t.Errorf("Read-only token %s %s: %d %s, want the role's 403", c.method, c.path, rec.Code, rec.Body)
		}
	}
	// A Developer runs SQL as postgres, but is not an Administrator.
	dev := x.token("dev", all...)
	if rec := x.req(dev, "POST", p+"/database/query", map[string]any{"query": "select 1"}); rec.Code != 201 {
		t.Fatalf("Developer SQL: %d %s", rec.Code, rec.Body)
	}
	if got := dsnUser(); got != "postgres" {
		t.Errorf("a Developer's token ran SQL as %s", got)
	}
	if rec := x.req(dev, "PATCH", p+"/postgrest", map[string]any{"max_rows": 500}); rec.Code != 403 {
		t.Errorf("Developer token on PATCH postgrest: %d", rec.Code)
	}
	// A Developer scoped to one project reaches that project only.
	scoped := x.token("scoped", all...)
	if rec := x.req(scoped, "GET", p+"/functions", nil); authzRefused(rec) {
		t.Errorf("scoped Developer token on its project: %d %s", rec.Code, rec.Body)
	}
	if rec := x.req(scoped, "GET", "/v1/projects/"+secondRef+"/functions", nil); rec.Code != 403 {
		t.Errorf("scoped Developer token on another project of the organization: %d", rec.Code)
	}

	// The role is live: demote the user and the token that worked a moment ago is refused.
	adm := x.token("admin", all...)
	patch := func() int {
		return x.req(adm, "PATCH", p+"/postgrest", map[string]any{"max_rows": 500}).Code
	}
	if code := patch(); code == 401 || code == 403 {
		t.Fatalf("Administrator token before the demotion: %d", code)
	}
	org := members.OrgRef{ID: x.org.ID, Slug: x.org.Slug}
	if err := x.srv.members.SetOrgRole(context.Background(), nil, org, x.ids["admin"], members.RoleDeveloper); err != nil {
		t.Fatal(err)
	}
	if code := patch(); code != 403 {
		t.Errorf("the same token after the user became a Developer: %d, want 403", code)
	}
	// Promotion applies at once too: a token never holds a snapshot.
	if err := x.srv.members.SetOrgRole(context.Background(), nil, org, x.ids["admin"], members.RoleAdministrator); err != nil {
		t.Fatal(err)
	}
	if code := patch(); code == 401 || code == 403 {
		t.Errorf("after the user became an Administrator again: %d", code)
	}
	// A token for the Owner of a second organization is not Owner of the first: its grant's organization decides.
	inOther := x.fake.issue(x.userID, x.other.ID, x.other.Slug, all...)
	if rec := x.req(inOther, "PATCH", "/v1/projects/"+authzOtherRef+"/postgrest", map[string]any{"max_rows": 5}); authzRefused(rec) {
		t.Errorf("Owner token in its own organization: %d %s", rec.Code, rec.Body)
	}
}

func TestOAuthRevocationMatrix(t *testing.T) { // U5
	const path = "/v1/organizations"
	setup := func(t *testing.T) (*oauthAuthzFixture, string) {
		x := newOAuthAuthzFixture(t)
		tok := x.token("dev", authzMCPScopes...)
		if rec := x.req(tok, "GET", path, nil); rec.Code != 200 {
			t.Fatalf("baseline: %d %s", rec.Code, rec.Body)
		}
		return x, tok
	}
	want := func(t *testing.T, x *oauthAuthzFixture, tok string, code int) {
		t.Helper()
		if rec := x.req(tok, "GET", path, nil); rec.Code != code {
			t.Fatalf("after the change: %d %s, want %d", rec.Code, strings.TrimSpace(rec.Body.String()), code)
		}
	}

	t.Run("user removed", func(t *testing.T) {
		x, tok := setup(t)
		if err := x.srv.accounts.Store.MarkUserRemoved(context.Background(), x.ids["dev"], "dev@example.test"); err != nil {
			t.Fatal(err)
		}
		want(t, x, tok, 401)
	})
	t.Run("sso denied", func(t *testing.T) {
		x, tok := setup(t)
		x.srv.auth.ssoUser = func(context.Context, string) error { return errSSOPending }
		want(t, x, tok, 401) // a session gets 403; a token's refusal is "this credential no longer works"
	})
	t.Run("sso check that fails is not a refusal", func(t *testing.T) {
		x, tok := setup(t)
		x.srv.auth.ssoUser = func(context.Context, string) error { return errors.New("registry unreachable") }
		want(t, x, tok, 500)
		if len(x.fake.revokes) != 0 {
			t.Errorf("a failed check revoked a grant: %+v", x.fake.revokes)
		}
		x.srv.auth.ssoUser = nil
		want(t, x, tok, 200)
	})
	t.Run("removed from the organization", func(t *testing.T) {
		x, tok := setup(t)
		grant := x.fake.grantOf(tok)
		org := members.OrgRef{ID: x.org.ID, Slug: x.org.Slug}
		if err := x.srv.members.RemoveMember(context.Background(), nil, org, x.ids["dev"]); err != nil {
			t.Fatal(err)
		}
		want(t, x, tok, 401)
		if len(x.fake.revokes) != 1 || x.fake.revokes[0] != (oauthRevokeCall{grant, oauth.ReasonMembership, oauth.ActorSystem}) {
			t.Errorf("revocations: %+v, want the grant %d revoked as %q by %q", x.fake.revokes, grant, oauth.ReasonMembership, oauth.ActorSystem)
		}
		// The grant is dead for good: even if the user is added back, this token does not return.
		if err := x.srv.members.Store.Update(context.Background(), x.org.ID, func(ops members.Ops) error {
			return ops.PutMember(context.Background(), members.Member{OrgID: x.org.ID, UserID: x.ids["dev"], RoleID: members.RoleDeveloper})
		}); err != nil {
			t.Fatal(err)
		}
		want(t, x, tok, 401)
	})
	t.Run("member of another organization only", func(t *testing.T) {
		// A grant for an organization the user does not belong to (never admitted) is refused the same way.
		x := newOAuthAuthzFixture(t)
		tok := x.fake.issue(x.ids["dev"], x.other.ID, x.other.Slug, authzMCPScopes...)
		want(t, x, tok, 401)
	})
	for _, why := range []string{"grant revoked", "app deleted", "organization deleted", "token expired"} {
		t.Run(why, func(t *testing.T) { // the service answers ErrNotFound for each
			x, tok := setup(t)
			x.fake.kill(x.fake.grantOf(tok))
			want(t, x, tok, 401)
			if len(x.fake.revokes) != 0 {
				t.Errorf("the API revoked a grant the service had ended: %+v", x.fake.revokes)
			}
		})
	}
	t.Run("expired on the server's clock", func(t *testing.T) {
		x, tok := setup(t)
		x.fake.edit(tok, func(i *oauth.AccessInfo) { i.ExpiresAt = time.Now().Add(-time.Second) })
		want(t, x, tok, 401)
	})
	t.Run("wrapped not found", func(t *testing.T) {
		x, tok := setup(t)
		x.fake.err = fmt.Errorf("oauth store: lookup: %w", oauth.ErrNotFound)
		want(t, x, tok, 401)
	})
	t.Run("service failure is not a refusal", func(t *testing.T) {
		x, tok := setup(t)
		x.fake.err = errors.New("database unreachable")
		want(t, x, tok, 500)
		x.fake.err = nil
		want(t, x, tok, 200)
	})
	t.Run("a lookup that answers nothing", func(t *testing.T) {
		x, _ := setup(t)
		x.srv.oauth = authzNilLookup{}
		want(t, x, x.fake.issue(x.ids["dev"], x.org.ID, "default", authzMCPScopes...), 401)
	})
}

// authzNilLookup answers "no error, no token info".
type authzNilLookup struct{ oauth.Authority }

func (authzNilLookup) LookupAccess(context.Context, string) (*oauth.AccessInfo, error) {
	return nil, nil
}

func TestOAuthTokensNeverInAccessTokens(t *testing.T) { // U6
	x := newOAuthAuthzFixture(t)
	ctx := context.Background()
	tok := x.token("owner", authzMCPScopes...)
	before, err := x.reg.ListAccessTokens(ctx, x.userID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		x.req(tok, "GET", "/v1/projects", nil)
	}
	if _, err := x.reg.GetAccessTokenByHash(ctx, secrets.HashToken(tok)); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("an OAuth token is in access_tokens: %v", err)
	}
	after, _ := x.reg.ListAccessTokens(ctx, x.userID)
	if len(after) != len(before) {
		t.Errorf("using an OAuth token changed access_tokens: %d -> %d rows", len(before), len(after))
	}
	for _, a := range after {
		if a.LastUsedAt != nil {
			t.Errorf("using an OAuth token touched the personal access token %q", a.Name)
		}
	}

	// Even a row planted in access_tokens under an OAuth token's hash gives no access: the OAuth
	// branch asks the OAuth service only, as a release without OAuth answers 401 for the same token.
	rogue := fmt.Sprintf("%s%040x", oauth.AccessTokenPrefix, 0xdead)
	if err := x.reg.CreateAccessToken(ctx, &registry.AccessToken{UserID: x.userID, Name: "rogue", Hash: secrets.HashToken(rogue), Prefix: rogue[:8]}); err != nil {
		t.Fatal(err)
	}
	if rec := x.req(rogue, "GET", "/v1/projects", nil); rec.Code != 401 {
		t.Errorf("an sbp_oauth_ token that is only in access_tokens: %d, want 401", rec.Code)
	}
	// And a real personal access token never reaches the OAuth service.
	lookups := x.fake.lookupCount()
	if rec := x.req(x.pat("owner"), "GET", "/v1/projects", nil); rec.Code != 200 {
		t.Errorf("personal access token: %d", rec.Code)
	}
	if x.fake.lookupCount() != lookups {
		t.Error("a personal access token was looked up as an OAuth token")
	}
	if rec := x.req(x.pat("owner"), "GET", "/platform/profile", nil); rec.Code != 401 {
		t.Errorf("personal access token on /platform: %d, want 401 as before", rec.Code)
	}
}

func TestOAuthTokenShape(t *testing.T) {
	x := newOAuthAuthzFixture(t)
	good := x.token("owner", authzMCPScopes...)
	for name, tok := range map[string]string{
		"prefix only":      oauth.AccessTokenPrefix,
		"short":            good[:len(good)-1],
		"long":             good + "0",
		"uppercase hex":    oauth.AccessTokenPrefix + strings.ToUpper(good[len(oauth.AccessTokenPrefix):]),
		"not hex":          oauth.AccessTokenPrefix + strings.Repeat("g", 40),
		"v0 token":         "sbp_v0_" + good[len(oauth.AccessTokenPrefix):],
		"refresh token":    oauth.RefreshTokenPrefix + strings.Repeat("a", 64),
		"authorization cd": oauth.AuthCodePrefix + strings.Repeat("a", 64),
	} {
		before := x.fake.lookupCount()
		rec := x.req(tok, "GET", "/v1/projects", nil)
		if rec.Code != 401 {
			t.Errorf("%s: %d, want 401", name, rec.Code)
		}
		if x.fake.lookupCount() != before {
			t.Errorf("%s: a token of the wrong shape was looked up", name)
		}
	}
	if rec := x.req(good, "GET", "/v1/projects", nil); rec.Code != 200 {
		t.Errorf("the well-formed token: %d", rec.Code)
	}
}

func TestOAuthPrincipal(t *testing.T) {
	x := newOAuthAuthzFixture(t)
	x.doAs(x.jwt, "GET", "/platform/profile", nil) // the user is on record now
	tok := x.token("owner", "projects:read", "database:read")
	x.fake.edit(tok, func(i *oauth.AccessInfo) { i.Resource = "https://api.example.test/mcp" })
	r := httptest.NewRequest("GET", "/v1/projects", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	p, err := x.srv.auth.authenticate(r, authAny)
	if err != nil {
		t.Fatal(err)
	}
	if p.Via != "oauth" || p.UserID != x.userID || p.UserEmail() != "dev@example.test" || p.AAL != "" || p.TokenID != 0 {
		t.Errorf("principal: %+v", p)
	}
	o := p.OAuth
	if o == nil || o.AppName != "Test client" || o.OrgID != x.org.ID || o.OrgSlug != "default" || o.Resource != "https://api.example.test/mcp" ||
		strings.Join(o.Scopes, " ") != "database:read projects:read" || o.GrantID == 0 || o.TokenID == 0 {
		t.Errorf("OAuthInfo: %+v", o)
	}
	// Only /v1-style routes take it.
	if _, err := x.srv.auth.authenticate(r, authJWT); err != errUnauthorized {
		t.Errorf("authJWT: %v", err)
	}
	if p, err := x.srv.auth.authenticate(r, authNone); p != nil || err != nil {
		t.Errorf("authNone: %v %v", p, err)
	}
	// No MFA gate: like a personal access token, a token is exempt (the second factor is checked at approval).
	if err := x.srv.members.Store.SetMFAEnforced(context.Background(), x.org.ID, true); err != nil {
		t.Fatal(err)
	}
	if rec := x.req(tok, "GET", "/v1/projects/"+testRef, nil); authzRefused(rec) {
		t.Errorf("an organization that requires MFA refused an OAuth token: %d %s", rec.Code, rec.Body)
	}
	if rec := x.doAs(x.jwt, "GET", "/v1/projects/"+testRef, nil); rec.Code != 403 {
		t.Errorf("fixture: the session below aal2 should be refused by an MFA organization, got %d", rec.Code)
	}
}

// An OAuth principal does not read its user from the store until a handler asks for the address.
func TestOAuthPrincipalLoadsItsEmailWhenAsked(t *testing.T) {
	x := newOAuthAuthzFixture(t)
	x.doAs(x.jwt, "GET", "/platform/profile", nil) // the user is on record now
	counter := &countingUserStore{Store: x.srv.auth.store}
	x.srv.auth.store = counter
	tok := x.token("owner", "projects:read")
	r := httptest.NewRequest("GET", "/v1/projects", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	p, err := x.srv.auth.authenticate(r, authAny)
	if err != nil {
		t.Fatal(err)
	}
	if n := counter.gets.Load(); n != 0 {
		t.Fatalf("authenticate read the user %d times", n)
	}
	if got := p.UserEmail(); got != "dev@example.test" {
		t.Errorf("UserEmail = %q", got)
	}
	if got := p.UserEmail(); got != "dev@example.test" || counter.gets.Load() != 1 {
		t.Errorf("a second UserEmail = %q after %d reads, want the kept answer after one", got, counter.gets.Load())
	}
}

// countingUserStore counts the reads of a user by GoTrue id.
type countingUserStore struct {
	Store
	gets atomic.Int32
}

func (c *countingUserStore) GetUser(ctx context.Context, userID string) (*User, error) {
	c.gets.Add(1)
	return c.Store.GetUser(ctx, userID)
}

func TestOAuthLastUsedIsThrottled(t *testing.T) {
	x := newOAuthAuthzFixture(t)
	clock := time.Now()
	x.srv.auth.now = func() time.Time { return clock }
	tok := x.token("owner", authzMCPScopes...)
	x.fake.edit(tok, func(i *oauth.AccessInfo) { i.ExpiresAt = clock.Add(24 * time.Hour) })
	other := x.token("owner", authzMCPScopes...)
	x.fake.edit(other, func(i *oauth.AccessInfo) { i.ExpiresAt = clock.Add(24 * time.Hour) })
	hit := func(tok string) {
		if rec := x.req(tok, "GET", "/v1/projects", nil); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	}
	for i := 0; i < 5; i++ {
		hit(tok)
	}
	if len(x.fake.touches) != 1 {
		t.Fatalf("5 requests within a minute touched %d times, want 1", len(x.fake.touches))
	}
	hit(other) // each token has its own throttle
	if len(x.fake.touches) != 2 {
		t.Errorf("a second token: %d touches", len(x.fake.touches))
	}
	clock = clock.Add(touchEvery + time.Second)
	hit(tok)
	hit(tok)
	if len(x.fake.touches) != 3 {
		t.Errorf("after a minute: %d touches, want 3", len(x.fake.touches))
	}
	// A refused request records nothing.
	x.fake.kill(x.fake.grantOf(other))
	clock = clock.Add(2 * touchEvery)
	n := len(x.fake.touches)
	if rec := x.req(other, "GET", "/v1/projects", nil); rec.Code != 401 {
		t.Fatalf("revoked: %d", rec.Code)
	}
	if len(x.fake.touches) != n {
		t.Error("a refused request touched the token")
	}
	// The throttle map does not grow without bound.
	o := x.srv.auth.oauth
	o.mu.Lock()
	for i := int64(0); i < oauthTouchMax; i++ {
		o.touchedAt[1_000_000+i] = clock.Add(-time.Hour)
	}
	o.mu.Unlock()
	clock = clock.Add(touchEvery + time.Second)
	hit(tok)
	o.mu.Lock()
	size := len(o.touchedAt)
	o.mu.Unlock()
	if size > 10 {
		t.Errorf("stale throttle entries were not dropped: %d left", size)
	}
}

func TestOAuthDisabledRefusesTokens(t *testing.T) {
	x := newOAuthAuthzFixture(t)
	tok := x.token("owner", authzMCPScopes...)
	if rec := x.req(tok, "GET", "/v1/projects", nil); rec.Code != 200 {
		t.Fatalf("enabled: %d", rec.Code)
	}
	x.cfg.API.DisableOAuth = true
	before := x.fake.lookupCount()
	// [api] disable_oauth is the kill switch: tokens already issued stop working, as on a release without OAuth.
	if rec := x.req(tok, "GET", "/v1/projects", nil); rec.Code != 401 {
		t.Errorf("disabled: %d, want 401", rec.Code)
	}
	if x.fake.lookupCount() != before {
		t.Error("a disabled server looked the token up")
	}
	// Nothing else changes: sessions and personal access tokens work.
	for _, cred := range []string{x.jwt, x.pat("owner")} {
		if rec := x.req(cred, "GET", "/v1/projects", nil); rec.Code != 200 {
			t.Errorf("credential after disable_oauth: %d", rec.Code)
		}
	}
	x.cfg.API.DisableOAuth = false
	if rec := x.req(tok, "GET", "/v1/projects", nil); rec.Code != 200 {
		t.Errorf("enabled again: %d", rec.Code)
	}
}

// A server built without the OAuth wiring (an authenticator with no resolver) refuses every OAuth token.
func TestOAuthWithoutResolver(t *testing.T) {
	x := newOAuthAuthzFixture(t)
	tok := x.token("owner", authzMCPScopes...)
	x.srv.auth.oauth = nil
	if rec := x.req(tok, "GET", "/v1/projects", nil); rec.Code != 401 {
		t.Errorf("%d", rec.Code)
	}
	if x.fake.lookupCount() != 0 {
		t.Error("looked up")
	}
}

func TestErrorHeaderIsWritten(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, &Error{Status: http.StatusForbidden, Message: "no", Header: http.Header{"www-authenticate": {`Bearer error="insufficient_scope"`}}})
	if rec.Code != 403 || rec.Header().Get("WWW-Authenticate") != `Bearer error="insufficient_scope"` || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("%d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	writeError(rec, errUnauthorized)
	if len(rec.Header().Values("WWW-Authenticate")) != 0 || errUnauthorized.Header != nil || errForbidden.Header != nil {
		t.Errorf("a shared error carries a header: %v", rec.Header())
	}
}

// Rights are read through accessOf, which a token's principal fills with the restricted Access. Code
// that reads memberships some other way would see every organization of the user and bypass the
// restriction, so the places that do are listed here; a new one fails this test until someone has
// decided that it cannot be reached by an OAuth token.
func TestOnlyKnownCodeReadsMembershipsDirectly(t *testing.T) {
	known := map[string]int{
		"authz.go":          1, // accessOf: the one place a request loads Access (oauth tokens arrive with theirs)
		"oauth_authn.go":    1, // loads the user's Access to restrict it to the grant's organization
		"server.go":         1, // oauthAdmit (members.IsMember): membership of one named organization, for the token endpoint
		"sso_dashboard.go":  2, // dashboard single sign-on: other users' roles (a /platform route) and first sight
		"members_wiring.go": 1, // `supavise users list`
		"standin.go":        1, // the sweep of `functions dev`'s stand-in user, at daemon start
	}
	re := regexp.MustCompile(`MembershipsOf\(|\bmembers\.Access\(|\bmembers\.IsMember\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(re.FindAll(b, -1)); n > 0 {
			got[f] = n
		}
	}
	for f, n := range got {
		if known[f] != n {
			t.Errorf("%s reads memberships directly %d times, expected %d: if the new reader can be reached by an OAuth token it must use accessOf", f, n, known[f])
		}
	}
	for f, n := range known {
		if got[f] != n {
			t.Errorf("%s: expected %d direct reads of memberships, found %d", f, n, got[f])
		}
	}
}
