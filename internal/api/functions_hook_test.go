package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingHook struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (h *recordingHook) FunctionsChanged(_ context.Context, ref string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, ref)
	return h.err
}

func (h *recordingHook) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

// withHook replaces the fixture's server by one that reports to hook.
func withHook(t *testing.T, f *fixture, hook FunctionsHook) {
	t.Helper()
	srv, err := NewServer(Deps{
		Registry: f.reg, Secrets: f.srv.sec, Manager: f.mgr, Config: f.cfg, PGMetaURL: f.meta.URL,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), CreateWait: 2 * time.Second, Functions: hook,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	f.ownerOn(srv)
}

func TestFunctionsHookIsCalledAfterEveryChange(t *testing.T) {
	f := newFixture(t)
	h := &recordingHook{}
	withHook(t, f, h)
	base := "/v1/projects/" + testRef

	body := bundleBody("compressed eszip")
	if rec := f.do("POST", base+"/functions?slug=hello&entrypoint_path=file:///x/index.ts", body, "Content-Type", EszipMediaType); rec.Code != 201 {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}
	steps := []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"create", "POST", base + "/functions", map[string]any{"slug": "legacy", "name": "legacy"}, 201},
		{"patch", "PATCH", base + "/functions/hello", map[string]any{"verify_jwt": false}, 200},
		{"set secrets", "POST", base + "/secrets", []map[string]string{{"name": "FOO", "value": "bar"}}, 201},
		{"delete secrets", "DELETE", base + "/secrets", []string{"FOO"}, 200},
		{"delete function", "DELETE", base + "/functions/hello", nil, 200},
	}
	n := h.count()
	if n != 1 {
		t.Fatalf("deploy made %d hook calls, want 1", n)
	}
	for _, s := range steps {
		if rec := f.do(s.method, s.path, s.body); rec.Code != s.want {
			t.Fatalf("%s: %d %s", s.name, rec.Code, rec.Body)
		}
		if got := h.count(); got != n+1 {
			t.Fatalf("%s: %d hook calls, want %d", s.name, got, n+1)
		}
		n++
	}
	for _, ref := range h.calls {
		if ref != testRef {
			t.Fatalf("hook told about %q", ref)
		}
	}
	// Reads do not call it.
	if rec := f.do("GET", base+"/functions", nil); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if rec := f.do("GET", base+"/secrets", nil); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if h.count() != n {
		t.Fatal("a read called the hook")
	}
}

func TestFunctionsHookFailureSaysTheChangeIsStored(t *testing.T) {
	f := newFixture(t)
	h := &recordingHook{err: errors.New("disk full")}
	withHook(t, f, h)
	body := bundleBody("compressed eszip")
	rec := f.do("POST", "/v1/projects/"+testRef+"/functions?slug=hello&entrypoint_path=file:///x/index.ts", body, "Content-Type", EszipMediaType)
	if rec.Code != 500 {
		t.Fatalf("deploy with a failing hook: %d %s", rec.Code, rec.Body)
	}
	if msg := jsonField(t, rec, "message"); msg == nil || !strings.Contains(msg.(string), "stored") || !strings.Contains(msg.(string), "disk full") {
		t.Fatalf("message %v", msg)
	}
	// The deployment is stored all the same; the hook's reconcile applies it later.
	if rec := f.do("GET", "/v1/projects/"+testRef+"/functions/hello", nil); rec.Code != 200 {
		t.Fatalf("stored function: %d", rec.Code)
	}
}

// bundlingHook is a hook that can bundle: it records what it was given and answers with a
// canned bundle or error.
type bundlingHook struct {
	recordingHook
	in     []SourceBundle
	result *BundledSource
	fail   error
}

func (h *bundlingHook) BundleSources(_ context.Context, in SourceBundle) (*BundledSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.in = append(h.in, in)
	if h.fail != nil {
		return nil, h.fail
	}
	return h.result, nil
}

// A node that runs functions serves bundles only (a function that ran from real source
// files could import other projects' files through its relative imports), so an upload of
// sources is bundled first, by a hook that confines the bundler; a node that cannot bundle
// says so and what to do instead.
func TestSourceUploadsAreBundledWhereFunctionsRun(t *testing.T) {
	f := newFixture(t)
	body, ctype := functionUpload(t, "index.ts", "console.log(1)")
	url := "/v1/projects/" + testRef + "/functions/deploy?slug=hello"
	// Without a runtime nothing runs the files, and the API keeps storing them.
	if rec := f.do("POST", url, body, "Content-Type", ctype); rec.Code != 201 {
		t.Fatalf("deploy without a runtime: %d %s", rec.Code, rec.Body)
	}

	// A hook that cannot bundle: refused, with the way out.
	plain := &recordingHook{}
	withHook(t, f, plain)
	rec := f.do("POST", url+"2", body, "Content-Type", ctype)
	if rec.Code != 501 {
		t.Fatalf("source deploy with a runtime that cannot bundle: %d %s", rec.Code, rec.Body)
	}
	if msg, _ := jsonField(t, rec, "message").(string); !strings.Contains(msg, "supabase functions deploy") || !strings.Contains(msg, "Docker") {
		t.Fatalf("message %q does not say what to do", msg)
	}
	if plain.count() != 0 {
		t.Fatal("a refused upload reached the hook")
	}
	if _, err := f.srv.store.GetFunction(context.Background(), testRef, "hello2"); err == nil {
		t.Fatal("a refused upload was stored")
	}

	// A hook that bundles: the sources are stored with the bundle and the entrypoint in it.
	h := &bundlingHook{result: &BundledSource{Bundle: []byte("EZBRxyz"), Entrypoint: "file:///scratch/src/index.ts"}}
	withHook(t, f, h)
	rec = f.do("POST", url+"3", body, "Content-Type", ctype)
	if rec.Code != 201 {
		t.Fatalf("source deploy with a bundling runtime: %d %s", rec.Code, rec.Body)
	}
	if len(h.in) != 1 || h.in[0].Ref != testRef || h.in[0].Slug != "hello3" || h.in[0].Entrypoint != "index.ts" || len(h.in[0].Files) != 1 || string(h.in[0].Files[0].Content) != "console.log(1)" {
		t.Fatalf("the hook was given %+v", h.in)
	}
	if h.count() != 1 {
		t.Fatalf("%d calls of FunctionsChanged, want 1", h.count())
	}
	files, err := f.srv.store.FunctionFiles(context.Background(), testRef, "hello3")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, file := range files {
		got[file.Path] = string(file.Content)
	}
	if len(got) != 3 || got["index.ts"] != "console.log(1)" || got[BundleFileName] != "EZBRxyz" || !strings.Contains(got[BundleInfoFileName], "file:///scratch/src/index.ts") {
		t.Fatalf("stored %v", got)
	}
	// The sources can be read back; the node's own files are not part of them.
	rec = f.do("GET", "/v1/projects/"+testRef+"/functions/hello3/body", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "console.log(1)") || strings.Contains(rec.Body.String(), "EZBRxyz") || strings.Contains(rec.Body.String(), BundleFileName) {
		t.Fatalf("body: %d %s", rec.Code, rec.Body)
	}

	// A broken upload is the uploader's error, with the bundler's words.
	h.fail = &BundleError{Msg: "Module not found \"file:///x/missing.ts\""}
	rec = f.do("POST", url+"4", body, "Content-Type", ctype)
	if msg, _ := jsonField(t, rec, "message").(string); rec.Code != 400 || !strings.Contains(msg, "Module not found") {
		t.Fatalf("bundle error: %d %s", rec.Code, rec.Body)
	}
	if _, err := f.srv.store.GetFunction(context.Background(), testRef, "hello4"); err == nil {
		t.Fatal("an upload that failed to bundle was stored")
	}
	for err, want := range map[error]int{ErrBundlingBusy: 429, ErrBundlingUnavailable: 501, errors.New("unit did not run"): 500} {
		h.fail = err
		if rec := f.do("POST", url+"5", body, "Content-Type", ctype); rec.Code != want {
			t.Fatalf("%v: %d %s, want %d", err, rec.Code, rec.Body, want)
		}
	}
	// An entrypoint that is not among the files is a 400 before any bundling.
	h.fail, h.in = nil, nil
	bad, ctype2 := functionUpload(t, "index.ts", "x")
	bad = []byte(strings.Replace(string(bad), `"entrypoint_path":"index.ts"`, `"entrypoint_path":"other.ts"`, 1))
	if rec := f.do("POST", url+"6", bad, "Content-Type", ctype2); rec.Code != 400 || len(h.in) != 0 {
		t.Fatalf("entrypoint missing: %d %s (%d calls)", rec.Code, rec.Body, len(h.in))
	}
	// The names the node stores its own files under cannot be uploaded.
	reserved, ctype3 := functionUpload(t, BundleFileName, "x")
	if rec := f.do("POST", url+"7", reserved, "Content-Type", ctype3); rec.Code != 400 {
		t.Fatalf("reserved name: %d %s", rec.Code, rec.Body)
	}
	// The entrypoint and import map of a bundled-from-sources function are fixed at deploy.
	if rec := f.do("PATCH", "/v1/projects/"+testRef+"/functions/hello3", map[string]any{"entrypoint_path": "other.ts"}); rec.Code != 400 {
		t.Fatalf("patching the entrypoint: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("PATCH", "/v1/projects/"+testRef+"/functions/hello3", map[string]any{"verify_jwt": false}); rec.Code != 200 {
		t.Fatalf("patching verify_jwt: %d %s", rec.Code, rec.Body)
	}
}

func TestFunctionsHookHearsAboutPauseResumeAndDelete(t *testing.T) {
	f := newFixture(t)
	h := &recordingHook{}
	withHook(t, f, h)
	base := "/v1/projects/" + testRef
	for _, step := range []struct{ method, path string }{{"POST", "/pause"}, {"POST", "/restore"}, {"DELETE", ""}} {
		before := h.count()
		if rec := f.do(step.method, base+step.path, nil); rec.Code != 200 {
			t.Fatalf("%s %s: %d %s", step.method, step.path, rec.Code, rec.Body)
		}
		if h.count() != before+1 {
			t.Fatalf("%s %s: %d hook calls, want %d", step.method, step.path, h.count(), before+1)
		}
	}
	// A failing hook does not fail a lifecycle change that succeeded.
	g := newFixture(t)
	withHook(t, g, &recordingHook{err: errors.New("disk full")})
	if rec := g.do("POST", base+"/pause", nil); rec.Code != 200 {
		t.Fatalf("pause with a failing hook: %d %s", rec.Code, rec.Body)
	}
}
