package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/branching"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
)

// branchEngine adds DeleteWith to the fake manager.
type branchEngine struct{ *fakeManager }

func (e branchEngine) DeleteWith(ctx context.Context, ref string, _ lifecycle.DeleteOptions) error {
	return e.Delete(ctx, ref)
}

// fakeBranchDB is the branching.Database of the API tests: an in-memory migration history.
type fakeBranchDB struct {
	mu   sync.Mutex
	migs map[string][]branching.Migration
}

func newFakeBranchDB() *fakeBranchDB { return &fakeBranchDB{migs: map[string][]branching.Migration{}} }

func (d *fakeBranchDB) Migrations(_ context.Context, ref string) ([]branching.Migration, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]branching.Migration(nil), d.migs[ref]...), nil
}

func (d *fakeBranchDB) Apply(_ context.Context, ref string, ms []branching.Migration, _ branching.ApplyOptions) (branching.ApplyResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.migs[ref] = append(d.migs[ref], ms...)
	return branching.ApplyResult{Applied: len(ms)}, nil
}

func (d *fakeBranchDB) RunScript(context.Context, string, string) error { return nil }

func (f *fixture) waitBranch(t *testing.T, name, wantStatus string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := f.do("GET", "/v1/projects/"+testRef+"/branches/"+name, nil)
		if rec.Code != 200 {
			t.Fatalf("get branch %s: %d %s", name, rec.Code, rec.Body)
		}
		b := decodeBody(t, rec).(map[string]any)
		if b["status"] == wantStatus {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("branch %s is %v (%v), want %s", name, b["status"], b, wantStatus)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBranches(t *testing.T) {
	f := newFixture(t)
	f.bdb.migs[testRef] = []branching.Migration{{Version: "20260101000000", Name: "init", Statements: []string{"create table t (id int)"}}}

	// Create: the body of the CLI and of the MCP server's create_branch.
	rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{
		"branch_name": "feature/login", "is_default": false, "git_branch": "feat/login", "persistent": false, "with_data": false,
		"desired_instance_size": "micro", "notify_url": "https://example.com/hook",
	})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "POST /v1/projects/{ref}/branches", rec.Body.Bytes())
	br := decodeBody(t, rec).(map[string]any)
	ref, id := br["project_ref"].(string), br["id"].(string)
	if br["name"] != "feature/login" || br["parent_project_ref"] != testRef || br["is_default"] != false || br["with_data"] != false || br["git_branch"] != "feat/login" {
		t.Fatalf("branch = %v", br)
	}
	got := f.waitBranch(t, "feature%2Flogin", "MIGRATIONS_PASSED")
	if got["preview_project_status"] != "ACTIVE_HEALTHY" {
		t.Fatalf("branch = %v", got)
	}
	validateAgainstSpec(t, "GET /v1/projects/{ref}/branches/{name}", mustJSON(got))
	if m := f.bdb.migs[ref]; len(m) != 1 || m[0].Version != "20260101000000" {
		t.Fatalf("migrations replayed into the branch: %v", m)
	}

	f.run(t, []step{
		{key: "GET /v1/projects/{ref}/branches", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if jsonField(t, rec, "0.name") != "main" || jsonField(t, rec, "0.is_default") != true || jsonField(t, rec, "0.project_ref") != testRef ||
				jsonField(t, rec, "1.name") != "feature/login" || jsonField(t, rec, "1.project_ref") != ref {
				t.Errorf("list = %s", rec.Body)
			}
		}},
		{key: "GET /v1/projects/{ref}/branches/{name}", path: "/v1/projects/" + testRef + "/branches/main", check: want("is_default", true)},
		{key: "GET /v1/projects/{ref}/branches/{name}", path: "/v1/projects/" + testRef + "/branches/nope", status: 404},
		{key: "GET /v1/branches/{branch_id_or_ref}", path: "/v1/branches/" + id, check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if jsonField(t, rec, "ref") != ref || jsonField(t, rec, "db_user") != "postgres."+ref || jsonField(t, rec, "status") != "ACTIVE_HEALTHY" ||
				jsonField(t, rec, "db_pass") == "" || jsonField(t, rec, "jwt_secret") == "" || jsonField(t, rec, "postgres_engine") != "17" {
				t.Errorf("detail = %s", rec.Body)
			}
		}},
		{key: "GET /v1/branches/{branch_id_or_ref}", path: "/v1/branches/" + ref, check: want("ref", ref)},
		{key: "GET /v1/branches/{branch_id_or_ref}", path: "/v1/branches/zzzzzzzzzzzzzzzzzzzz", status: 404},
		{key: "PATCH /v1/branches/{branch_id_or_ref}", path: "/v1/branches/" + id, body: map[string]any{"persistent": true, "git_branch": "feat/other"}, check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if jsonField(t, rec, "persistent") != true || jsonField(t, rec, "git_branch") != "feat/other" {
				t.Errorf("patch = %s", rec.Body)
			}
		}},
		{key: "PATCH /v1/branches/{branch_id_or_ref}", path: "/v1/branches/" + id, body: map[string]any{"branch_name": "no good"}, status: 400},
	})

	// The branch is hidden from the project lists, and the parent shows it.
	rec = f.do("GET", "/v1/projects", nil)
	if strings.Contains(rec.Body.String(), ref) {
		t.Fatalf("a branch is listed as a project: %s", rec.Body)
	}
	rec = f.do("GET", "/platform/projects", nil)
	if strings.Contains(rec.Body.String(), `"ref":"`+ref+`"`) || !strings.Contains(rec.Body.String(), `"preview_branch_refs":["`+ref+`"]`) || !strings.Contains(rec.Body.String(), `"is_branch_enabled":true`) {
		t.Fatalf("platform projects: %s", rec.Body)
	}
	rec = f.do("GET", "/platform/projects/"+ref, nil)
	if rec.Code != 200 || jsonField(t, rec, "parent_project_ref") != testRef {
		t.Fatalf("platform project of a branch: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /platform/projects/{ref}", rec.Body.Bytes())

	// merge, push, reset: 201 with a run id; the branch status follows.
	f.bdb.migs[ref] = append(f.bdb.migs[ref], branching.Migration{Version: "20260110000000", Name: "feature", Statements: []string{"create table f (id int)"}})
	for _, op := range []string{"merge", "push", "reset"} {
		key := "POST /v1/branches/{branch_id_or_ref}/" + op
		rec := f.do("POST", "/v1/branches/"+id+"/"+op, map[string]any{})
		if rec.Code != 201 {
			t.Fatalf("%s: %d %s", op, rec.Code, rec.Body)
		}
		validateAgainstSpec(t, key, rec.Body.Bytes())
		if run := jsonField(t, rec, "workflow_run_id"); run == "" || !branching.IsUUID(run.(string)) {
			t.Fatalf("%s run id = %v", op, run)
		}
		f.waitBranch(t, "feature%2Flogin", "MIGRATIONS_PASSED")
	}
	if m := f.bdb.migs[testRef]; len(m) != 2 || m[1].Version != "20260110000000" {
		t.Fatalf("merge did not reach the parent: %v", m)
	}
	// An empty body and an unknown branch.
	if rec := f.do("POST", "/v1/branches/"+id+"/merge", nil); rec.Code != 201 {
		t.Fatalf("merge without a body: %d %s", rec.Code, rec.Body)
	}
	f.waitBranch(t, "feature%2Flogin", "MIGRATIONS_PASSED")
	if rec := f.do("POST", "/v1/branches/"+ref+"x/merge", nil); rec.Code != 404 {
		t.Fatalf("merge unknown: %d", rec.Code)
	}
	if rec := f.do("POST", "/v1/branches/"+testRef+"/merge", nil); rec.Code != 400 {
		t.Fatalf("merge of the default branch: %d %s", rec.Code, rec.Body)
	}

	rec = f.do("GET", "/v1/branches/"+id+"/diff", nil)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || !strings.Contains(rec.Body.String(), "nothing to merge") {
		t.Fatalf("diff: %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}

	// Soft delete and restore, then delete for real.
	rec = f.do("DELETE", "/v1/branches/"+id+"?force=false", nil)
	if rec.Code != 200 {
		t.Fatalf("soft delete: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "DELETE /v1/branches/{branch_id_or_ref}", rec.Body.Bytes())
	if b := f.waitBranch(t, "feature%2Flogin", "MIGRATIONS_PASSED"); b["deletion_scheduled_at"] == nil {
		t.Fatalf("not scheduled: %v", b)
	}
	rec = f.do("POST", "/v1/branches/"+id+"/restore", nil)
	if rec.Code != 201 || jsonField(t, rec, "message") != "Branch restoration initiated" {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "POST /v1/branches/{branch_id_or_ref}/restore", rec.Body.Bytes())
	if rec := f.do("DELETE", "/v1/branches/"+id, nil); rec.Code != 200 || jsonField(t, rec, "message") != "ok" {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/v1/branches/"+id, nil); rec.Code != 404 {
		t.Fatalf("deleted branch: %d", rec.Code)
	}

	// Errors: duplicate names, a branch of a branch, a project that does not exist,
	// disabling branching.
	f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "dup"})
	f.waitBranch(t, "dup", "MIGRATIONS_PASSED")
	if rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "dup"}); rec.Code != 409 {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": ""}); rec.Code != 400 {
		t.Fatalf("empty name: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/v1/projects/zzzzzzzzzzzzzzzzzzzz/branches", map[string]any{"branch_name": "x"}); rec.Code != 404 {
		t.Fatalf("unknown project: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/v1/projects/zzzzzzzzzzzzzzzzzzzz/branches", nil); rec.Code != 404 {
		t.Fatalf("list of an unknown project: %d", rec.Code)
	}
	if rec := f.do("DELETE", "/v1/projects/"+testRef+"/branches", nil); rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/branches", nil); len(decodeBody(t, rec).([]any)) != 1 {
		t.Fatalf("branches after disable: %s", rec.Body)
	}
	// The Management API shape of the error envelope.
	if rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "main"}); rec.Code != 400 || jsonField(t, rec, "message") == "" {
		t.Fatalf("reserved name: %d %s", rec.Code, rec.Body)
	}
}

func TestBranchRequestsThatCannotBeHonoredAreRefused(t *testing.T) {
	f := newFixture(t)
	create := func(body map[string]any) *httptest.ResponseRecorder {
		body["branch_name"] = "x"
		return f.do("POST", "/v1/projects/"+testRef+"/branches", body)
	}
	for name, body := range map[string]map[string]any{
		"secrets":         {"secrets": map[string]string{"A": "b"}},
		"release channel": {"release_channel": "preview"},
		"postgres engine": {"postgres_engine": "15"},
	} {
		if rec := create(body); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := f.do("GET", "/v1/projects/"+testRef+"/branches", nil); len(decodeBody(t, rec).([]any)) != 1 {
		t.Fatalf("a refused create left a branch: %s", rec.Body)
	}
	// Empty secrets, the node's own engine and the ga channel are fine.
	if rec := create(map[string]any{"secrets": map[string]string{}, "release_channel": "ga", "postgres_engine": "17"}); rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	br := f.waitBranch(t, "x", "MIGRATIONS_PASSED")
	id := br["id"].(string)
	if rec := f.do("PATCH", "/v1/branches/"+id, map[string]any{"status": "CREATING_PROJECT"}); rec.Code != 400 {
		t.Errorf("setting the status: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("PATCH", "/v1/branches/"+id, map[string]any{"status": "MIGRATIONS_PASSED", "reset_on_push": true, "persistent": true}); rec.Code != 200 {
		t.Errorf("an unchanged status and the deprecated reset_on_push are accepted: %d %s", rec.Code, rec.Body)
	}
}

// with_data on a node that cannot clone and has no backup is something the user can fix: 400.
func TestBranchWithDataWithoutAWayIs400(t *testing.T) {
	f := newFixture(t)
	f.cfg.Branching.Clone = "backup"
	rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "d", "with_data": true})
	if rec.Code != 400 || !strings.Contains(jsonField(t, rec, "message").(string), "with_data is not possible") {
		t.Fatalf("with_data: %d %s", rec.Code, rec.Body)
	}
}

// The detail of the default branch does not hand out the project's secrets, and a branch whose
// credentials are not stored yet answers without them instead of failing.
func TestBranchDetailSecrets(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/v1/branches/"+testRef, nil)
	if rec.Code != 200 || jsonField(t, rec, "ref") != testRef || jsonField(t, rec, "db_pass") != nil || jsonField(t, rec, "jwt_secret") != nil {
		t.Fatalf("default branch detail: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /v1/branches/{branch_id_or_ref}", rec.Body.Bytes())

	const ref = "wwwwwwwwwwwwwwwwwwww"
	if err := f.reg.CreateProject(context.Background(), &registry.Project{Ref: ref, Name: "w", Status: registry.StatusComingUp,
		Branch: &registry.BranchInfo{ID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", ParentRef: testRef, Name: "w", State: registry.BranchCreatingProject}}); err != nil {
		t.Fatal(err)
	}
	rec = f.do("GET", "/v1/branches/"+ref, nil)
	if rec.Code != 200 || jsonField(t, rec, "ref") != ref || jsonField(t, rec, "db_pass") != nil {
		t.Fatalf("branch without credentials yet: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /v1/branches/{branch_id_or_ref}", rec.Body.Bytes())
}

func TestBranchesWithoutService(t *testing.T) {
	f := newFixture(t)
	srv, err := NewServer(Deps{Registry: f.reg, Secrets: f.srv.sec, Manager: f.mgr, Config: f.cfg, PGMetaURL: f.meta.URL})
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	if rec := f.do("GET", "/v1/projects/"+testRef+"/branches", nil); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "x"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/v1/branches/"+testRef, nil); rec.Code != 404 {
		t.Fatalf("get: %d", rec.Code)
	}
}
