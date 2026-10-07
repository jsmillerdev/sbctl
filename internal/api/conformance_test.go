package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// validateAgainstSpec checks body against the success schema of the operation key.
// The Supabase CLI and Studio decode strictly: a response that fails this check is
// a bug in the server.
func validateAgainstSpec(t *testing.T, key string, body []byte) {
	t.Helper()
	op := operationByKey(key)
	if op == nil {
		t.Fatalf("%s is not an operation of the pinned specs", key)
	}
	if !op.JSON || op.Response == nil {
		return
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Errorf("%s: response is not JSON: %v: %q", key, err, body)
		return
	}
	if err := op.Response.Value.VisitJSON(v, openapi3.MultiErrors()); err != nil {
		if strings.Contains(err.Error(), "error parsing regexp") {
			// The validator's regexp engine lacks look-ahead, which some spec patterns use.
			t.Logf("%s: not fully validated: %s", key, truncate(err.Error(), 120))
			return
		}
		t.Errorf("%s: response does not satisfy the spec: %v\nbody: %s", key, truncate(strings.SplitN(err.Error(), "\nSchema:", 2)[0], 500), truncate(string(body), 600))
	}
}

type step struct {
	key    string // spec operation, "METHOD /template"
	path   string // concrete path; defaults to the template with {ref} = testRef
	body   any
	status int // expected status; defaults to the operation's success status
	hdr    []string
	check  func(t *testing.T, rec *httptest.ResponseRecorder)
}

func (f *fixture) run(t *testing.T, steps []step) {
	t.Helper()
	for _, st := range steps {
		method, tmpl, _ := strings.Cut(st.key, " ")
		path := st.path
		if path == "" {
			path = strings.ReplaceAll(tmpl, "{ref}", testRef)
		}
		t.Run(st.key+" "+strings.TrimPrefix(path, tmpl[:min(len(tmpl), 0)]), func(t *testing.T) {
			rec := f.do(method, path, st.body, st.hdr...)
			want := st.status
			if want == 0 {
				want = operationByKey(st.key).Status
			}
			if rec.Code != want {
				t.Fatalf("status %d, want %d: %s", rec.Code, want, truncate(rec.Body.String(), 400))
			}
			if rec.Header().Get("X-Sbctl-Stub") != "" {
				t.Errorf("%s is served by a stub but is expected to be implemented", st.key)
			}
			if rec.Body.Len() > 0 && rec.Code < 300 {
				validateAgainstSpec(t, st.key, rec.Body.Bytes())
			}
			if st.check != nil {
				st.check(t, rec)
			}
		})
	}
}

func jsonField(t *testing.T, rec *httptest.ResponseRecorder, path string) any {
	t.Helper()
	v := decodeBody(t, rec)
	if path == "" {
		return v
	}
	for _, p := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			v = x[p]
		case []any:
			var i int
			fmt.Sscanf(p, "%d", &i)
			v = x[i]
		default:
			t.Fatalf("path %q: cannot descend into %T", path, v)
		}
	}
	return v
}

func want(path string, expected any) func(*testing.T, *httptest.ResponseRecorder) {
	return func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		got := jsonField(t, rec, path)
		switch x := got.(type) {
		case []any, map[string]any:
			b, _ := json.Marshal(got)
			got = string(b)
		case float64:
			got = strconv.FormatFloat(x, 'f', -1, 64)
		}
		if fmt.Sprint(got) != fmt.Sprint(expected) {
			t.Errorf("%s = %v, want %v", path, got, expected)
		}
	}
}

// TestImplementedRoutesMatchSpec drives every hand-written route and validates the
// response against its schema.
func TestImplementedRoutesMatchSpec(t *testing.T) {
	f := newFixture(t)
	f.meta.Rules = []pgmetaRule{
		{Contains: "to_regclass('supabase_migrations.schema_migrations') is not null as e", Status: 200, Body: `[{"e":true}]`},
		{Contains: "from supabase_migrations.schema_migrations order by", Status: 200, Body: `[{"version":"20240101000000","name":"init"},{"version":"20240102000000","name":null}]`},
		{Contains: "syntax error", Status: 400, Body: `{"error":"syntax error at or near \"selec\""}`},
		{Contains: "select 1", Status: 200, Body: `[{"?column?":1}]`},
	}
	fnSlug := "hello"
	multipartBody, ctype := functionUpload(t, "index.ts", `Deno.serve(() => new Response("hi"))`)
	var folderID, contentID string
	contentID = "5f1b7b6e-3c0a-4c2f-9d6b-0e0e0e0e0e0e"

	f.run(t, []step{
		// identity
		{key: "GET /v1/profile", check: want("username", "dev")},
		{key: "GET /platform/profile", check: want("gotrue_id", f.userID)},
		{key: "POST /platform/profile"},
		{key: "PATCH /platform/profile", body: map[string]any{"first_name": "Dee"}, check: want("first_name", "Dee")},
		{key: "GET /platform/profile/permissions", check: want("0.actions.0", "%")},
		// organizations
		{key: "GET /v1/organizations", check: want("0.slug", "default")},
		{key: "GET /v1/organizations/{slug}", path: "/v1/organizations/default"},
		{key: "GET /v1/organizations/{slug}/entitlements", path: "/v1/organizations/default/entitlements"},
		{key: "GET /v1/organizations/{slug}/members", path: "/v1/organizations/default/members"},
		{key: "GET /platform/organizations", check: want("0.plan.id", "enterprise")},
		{key: "GET /platform/organizations/{slug}", path: "/platform/organizations/default"},
		{key: "PATCH /platform/organizations/{slug}", path: "/platform/organizations/default", body: map[string]any{"name": "Team"}, check: want("name", "Team")},
		{key: "GET /platform/organizations/{slug}/entitlements", path: "/platform/organizations/default/entitlements"},
		{key: "GET /platform/organizations/{slug}/billing/subscription", path: "/platform/organizations/default/billing/subscription"},
		{key: "GET /platform/organizations/{slug}/members", path: "/platform/organizations/default/members"},
		{key: "GET /platform/organizations/{slug}/projects", path: "/platform/organizations/default/projects", check: want("projects.0.ref", testRef)},
		// projects
		{key: "GET /v1/projects", check: want("0.ref", testRef)},
		{key: "GET /v1/projects/{ref}", check: want("database.version", "17.11.0.004")},
		{key: "GET /v1/projects/{ref}/health", path: "/v1/projects/" + testRef + "/health?services=auth,db,rest", check: want("2.healthy", false)},
		{key: "GET /v1/projects/{ref}/branches", check: want("0.name", "main")},
		{key: "GET /v1/projects/{ref}/config/database/pooler", check: want("0.identifier", testRef)},
		{key: "GET /platform/projects", check: want("pagination.count", 1)},
		{key: "GET /platform/projects/{ref}", check: want("connectionString", "postgresql://postgres:[YOUR-PASSWORD]@db.abcdefghijklmnopqrst.api.example.test:5432/postgres")},
		{key: "GET /platform/projects/{ref}/status", status: 200, check: want("status", "ACTIVE_HEALTHY")},
		{key: "GET /platform/projects/{ref}/settings", check: want("app_config.endpoint", testRef+".api.example.test")},
		{key: "GET /platform/projects/{ref}/config/postgrest"},
		{key: "POST /platform/projects/{ref}/api-keys/temporary"},
		{key: "GET /v2/projects/{ref}/config", check: want("data.id", testRef)},
		{key: "GET /platform/projects/{ref}/config/storage", check: want("fileSizeLimit", 52428800)},
		{key: "GET /v1/projects/{ref}/config/storage", check: want("fileSizeLimit", 52428800)},
		{key: "GET /platform/projects/{ref}/config/pgbouncer", check: want("pool_mode", "transaction")},
		{key: "GET /platform/projects/{ref}/config/supavisor", check: want("0.identifier", testRef)},
		{key: "GET /platform/database/{ref}/backups", path: "/platform/database/" + testRef + "/backups"},
		// keys
		{key: "GET /v1/projects/{ref}/api-keys", path: "/v1/projects/" + testRef + "/api-keys?reveal=true", check: want("0.name", "anon")},
		{key: "GET /v1/projects/{ref}/api-keys/legacy", check: want("enabled", true)},
		// database
		{key: "POST /v1/projects/{ref}/database/query", body: map[string]any{"query": "select 1"}, status: 201, check: want("0.?column?", 1)},
		{key: "POST /v1/projects/{ref}/database/query", body: map[string]any{"query": "selec 1 syntax error"}, status: 400, check: want("message", `syntax error at or near "selec"`)},
		{key: "POST /v1/projects/{ref}/database/query/read-only", body: map[string]any{"query": "select 1"}, status: 201},
		{key: "GET /v1/projects/{ref}/database/migrations", check: want("1.version", "20240102000000")},
		{key: "POST /v1/projects/{ref}/database/migrations", body: map[string]any{"query": "create table t(id int)", "name": "t"}, status: 200},
		{key: "GET /v1/projects/{ref}/types/typescript", check: want("types", "export type Json = string | number | boolean | null\n")},
		{key: "POST /v1/projects/{ref}/cli/login-role", body: map[string]any{"read_only": false}, check: want("ttl_seconds", 3600)},
		{key: "DELETE /v1/projects/{ref}/cli/login-role", check: want("message", "ok")},
		{key: "GET /v1/projects/{ref}/advisors/security", check: want("lints", "[]")},
		{key: "GET /v1/projects/{ref}/advisors/performance"},
		// secrets
		{key: "POST /v1/projects/{ref}/secrets", body: []map[string]string{{"name": "FOO", "value": "bar"}}, status: 201},
		{key: "GET /v1/projects/{ref}/secrets", check: want("0.name", "FOO")},
		{key: "DELETE /v1/projects/{ref}/secrets", body: []string{"FOO"}, status: 200},
		// functions
		{key: "POST /v1/projects/{ref}/functions/deploy", path: "/v1/projects/" + testRef + "/functions/deploy?slug=" + fnSlug, body: multipartBody, hdr: []string{"Content-Type", ctype}, check: want("version", 1)},
		{key: "GET /v1/projects/{ref}/functions", check: want("0.slug", fnSlug)},
		{key: "GET /v1/projects/{ref}/functions/{function_slug}", path: "/v1/projects/" + testRef + "/functions/" + fnSlug},
		{key: "PATCH /v1/projects/{ref}/functions/{function_slug}", path: "/v1/projects/" + testRef + "/functions/" + fnSlug, body: map[string]any{"verify_jwt": false}, check: want("verify_jwt", false)},
		{key: "DELETE /v1/projects/{ref}/functions/{function_slug}", path: "/v1/projects/" + testRef + "/functions/" + fnSlug},
		// content
		{key: "PUT /platform/projects/{ref}/content", body: map[string]any{"id": contentID, "name": "q1", "type": "sql", "visibility": "user", "content": map[string]any{"sql": "select 1"}}, status: 200},
		{key: "GET /platform/projects/{ref}/content", check: want("data.0.owner.username", "dev")},
		{key: "GET /platform/projects/{ref}/content/count", check: want("private", 1)},
		{key: "GET /platform/projects/{ref}/content/item/{id}", path: "/platform/projects/" + testRef + "/content/item/" + contentID, check: want("content.sql", "select 1")},
		{key: "POST /platform/projects/{ref}/content/folders", body: map[string]any{"name": "f1"}, check: func(t *testing.T, rec *httptest.ResponseRecorder) { folderID = jsonField(t, rec, "id").(string) }},
		{key: "GET /platform/projects/{ref}/content/folders", check: want("data.folders.0.name", "f1")},
	})
	f.run(t, []step{
		{key: "GET /platform/projects/{ref}/content/folders/{id}", path: "/platform/projects/" + testRef + "/content/folders/" + folderID},
		{key: "PATCH /platform/projects/{ref}/content/folders/{id}", path: "/platform/projects/" + testRef + "/content/folders/" + folderID, body: map[string]any{"name": "f2"}},
		{key: "DELETE /platform/projects/{ref}/content/folders", path: "/platform/projects/" + testRef + "/content/folders?ids=" + folderID},
		{key: "DELETE /platform/projects/{ref}/content", path: "/platform/projects/" + testRef + "/content?ids=" + contentID, check: want("0.id", contentID)},
	})
}

func functionUpload(t *testing.T, name, src string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("metadata", `{"entrypoint_path":"`+name+`","verify_jwt":true,"name":"hello"}`)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write([]byte(src))
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

// TestStubsMatchSpec calls every operation no handler claims and checks that its
// stub is the success status of the spec with a body that satisfies the schema.
func TestStubsMatchSpec(t *testing.T) {
	f := newFixture(t)
	ops, _ := Operations()
	impl := f.srv.implemented()
	paramRe := regexp.MustCompile(`\{[^}]+\}`)
	n := 0
	for _, op := range ops {
		if _, ok := impl[op.Key()]; ok {
			continue
		}
		path := paramRe.ReplaceAllStringFunc(op.Path, func(p string) string {
			if p == "{ref}" {
				return testRef
			}
			return "x1"
		})
		rec := f.do(op.Method, path, nil)
		if rec.Header().Get("X-Sbctl-Stub") == "" {
			continue // a more specific implemented route (or an ambiguous pair) answered
		}
		n++
		want := op.Status
		if want == 0 {
			want = 200
		}
		if rec.Code != want {
			t.Errorf("%s: status %d, want %d", op.Key(), rec.Code, want)
			continue
		}
		if rec.Body.Len() > 0 {
			validateAgainstSpec(t, op.Key(), rec.Body.Bytes())
		}
	}
	t.Logf("%d stubbed operations checked", n)
	if n < 300 {
		t.Fatalf("only %d stubs were exercised", n)
	}
}
