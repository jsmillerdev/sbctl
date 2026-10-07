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

	body, ctype := functionUpload(t, "index.ts", "console.log(1)")
	if rec := f.do("POST", base+"/functions/deploy?slug=hello", body, "Content-Type", ctype); rec.Code != 201 {
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
	body, ctype := functionUpload(t, "index.ts", "console.log(1)")
	rec := f.do("POST", "/v1/projects/"+testRef+"/functions/deploy?slug=hello", body, "Content-Type", ctype)
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
