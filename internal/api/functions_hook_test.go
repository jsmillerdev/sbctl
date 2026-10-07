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

// A node that runs functions serves bundles only; a source upload would run from real
// files, where relative imports can reach other projects' files.
func TestSourceUploadsAreRefusedWhereFunctionsRun(t *testing.T) {
	f := newFixture(t)
	body, ctype := functionUpload(t, "index.ts", "console.log(1)")
	url := "/v1/projects/" + testRef + "/functions/deploy?slug=hello"
	// Without a runtime nothing runs the files, and the API keeps storing them.
	if rec := f.do("POST", url, body, "Content-Type", ctype); rec.Code != 201 {
		t.Fatalf("deploy without a runtime: %d %s", rec.Code, rec.Body)
	}
	h := &recordingHook{}
	withHook(t, f, h)
	rec := f.do("POST", url+"2", body, "Content-Type", ctype)
	if rec.Code != 400 {
		t.Fatalf("source deploy with a runtime: %d %s", rec.Code, rec.Body)
	}
	if msg, _ := jsonField(t, rec, "message").(string); !strings.Contains(msg, "without --use-api") {
		t.Fatalf("message %q does not say what to do", msg)
	}
	if h.count() != 0 {
		t.Fatal("a refused upload reached the hook")
	}
	if _, err := f.srv.store.GetFunction(context.Background(), testRef, "hello2"); err == nil {
		t.Fatal("a refused upload was stored")
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
