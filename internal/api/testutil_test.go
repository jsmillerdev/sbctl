package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/OWNER/sbctl/internal/api/cryptojs"
	"github.com/OWNER/sbctl/internal/branching"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// fakeManager is a lifecycle.Manager over a registry: projects are created
// ACTIVE_HEALTHY at once, and connection strings are a fixed DSN.
type fakeManager struct {
	reg registry.Registry
	sec secrets.Secrets
	dsn string

	mu       sync.Mutex
	keys     map[string]*secrets.ProjectKeys
	keyCalls map[string]int // Keys calls by ref
	created  []lifecycle.CreateRequest
	paused   []string
	resumed  []string
	deleted  []string
	createFn func(req lifecycle.CreateRequest) (*registry.Project, error)
	// hook, when set, runs inside Pause, Resume and Delete before they act.
	hook func(op string, ctx context.Context)
}

func (m *fakeManager) observe(op string, ctx context.Context) {
	m.mu.Lock()
	h := m.hook
	m.mu.Unlock()
	if h != nil {
		h(op, ctx)
	}
}

func newFakeManager(reg registry.Registry, sec secrets.Secrets) *fakeManager {
	return &fakeManager{reg: reg, sec: sec, dsn: "postgres://postgres:pw@127.0.0.1:5432/postgres", keys: map[string]*secrets.ProjectKeys{}, keyCalls: map[string]int{}}
}

func (m *fakeManager) addProject(t testing.TB, ref, name string, orgID int64, status registry.Status) *registry.Project {
	t.Helper()
	p := &registry.Project{Ref: ref, OrgID: orgID, Name: name, Status: status, Region: "local", Engine: registry.EnginePostgres,
		Versions: map[string]string{"postgres": "17.11.0.004-r1"}}
	if err := m.reg.CreateProject(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	k, err := secrets.NewProjectKeys(ref, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.keys[ref] = k
	m.mu.Unlock()
	return p
}

func (m *fakeManager) Create(ctx context.Context, req lifecycle.CreateRequest) (*registry.Project, error) {
	m.mu.Lock()
	m.created = append(m.created, req)
	fn := m.createFn
	m.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	if req.OrgSlug == "" {
		req.OrgSlug = "default"
	}
	org, err := m.reg.GetOrganization(ctx, req.OrgSlug)
	if err != nil {
		return nil, err
	}
	p := &registry.Project{Ref: req.Ref, OrgID: org.ID, Name: req.Name, Region: req.Region, Status: registry.StatusActiveHealthy, Engine: registry.EnginePostgres}
	if req.Branch != nil {
		b := *req.Branch
		p.Branch = &b
	}
	if err := m.reg.CreateProject(ctx, p); err != nil {
		return nil, err
	}
	k, _ := secrets.NewProjectKeys(req.Ref, time.Now())
	m.mu.Lock()
	m.keys[req.Ref] = k
	m.mu.Unlock()
	return p, nil
}

func (m *fakeManager) Pause(ctx context.Context, ref string) error {
	m.observe("pause", ctx)
	m.mu.Lock()
	m.paused = append(m.paused, ref)
	m.mu.Unlock()
	return m.reg.SetProjectStatus(ctx, ref, registry.StatusInactive)
}

func (m *fakeManager) Resume(ctx context.Context, ref string) error {
	m.observe("resume", ctx)
	m.mu.Lock()
	m.resumed = append(m.resumed, ref)
	m.mu.Unlock()
	return m.reg.SetProjectStatus(ctx, ref, registry.StatusActiveHealthy)
}

func (m *fakeManager) Delete(ctx context.Context, ref string) error {
	m.observe("delete", ctx)
	m.mu.Lock()
	m.deleted = append(m.deleted, ref)
	m.mu.Unlock()
	return m.reg.DeleteProject(ctx, ref)
}

func (m *fakeManager) RotateKeys(context.Context, string) (*secrets.ProjectKeys, error) {
	return nil, lifecycle.ErrInvalidState
}

func (m *fakeManager) Keys(_ context.Context, ref string) (*secrets.ProjectKeys, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keyCalls[ref]++
	if k, ok := m.keys[ref]; ok {
		return k, nil
	}
	return nil, registry.ErrNotFound
}

func (m *fakeManager) Health(context.Context, string) ([]lifecycle.ServiceHealth, error) {
	return []lifecycle.ServiceHealth{
		{Name: config.SvcPostgres, Healthy: true, Status: "ACTIVE_HEALTHY"},
		{Name: config.SvcGoTrue, Healthy: true, Status: "ACTIVE_HEALTHY"},
		{Name: config.SvcPostgREST, Healthy: false, Status: "UNHEALTHY", Error: "connection refused"},
	}, nil
}

func (m *fakeManager) ConnString(context.Context, string, string) (string, error) { return m.dsn, nil }

// fakePGMeta is an httptest stand-in for sb-pgmeta: it decrypts the connection
// header like the real one, records requests and answers from canned rules.
type fakePGMeta struct {
	*httptest.Server
	key string

	mu       sync.Mutex
	Requests []pgmetaRequest
	// Rows answers POST /query: the first rule whose substring is in the query wins.
	Rules []pgmetaRule
	hits  map[int]int // matches per rule index, for Skip
}

type pgmetaRequest struct {
	Method, Path, RawQuery, DSN, Body string
}

type pgmetaRule struct {
	Contains string
	Status   int
	Body     string
	// Skip passes over the first Skip matching requests, so a rule can answer a later
	// request differently from an earlier one.
	Skip int
}

func newFakePGMeta(t testing.TB, key string) *fakePGMeta {
	f := &fakePGMeta{key: key}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		dsn, err := cryptojs.Decrypt(r.Header.Get("X-Connection-Encrypted"), f.key)
		if err != nil {
			http.Error(w, `{"error":"failed to process upstream connection details"}`, http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		f.Requests = append(f.Requests, pgmetaRequest{r.Method, r.URL.Path, r.URL.RawQuery, dsn, string(b)})
		rules := f.Rules
		f.mu.Unlock()
		skipped := func(i int) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.hits == nil {
				f.hits = map[int]int{}
			}
			f.hits[i]++
			return f.hits[i] <= rules[i].Skip
		}
		switch {
		case r.URL.Path == "/query":
			var in struct{ Query string }
			_ = json.Unmarshal(b, &in)
			for i, rule := range rules {
				if strings.Contains(in.Query, rule.Contains) && !skipped(i) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(rule.Status)
					_, _ = w.Write([]byte(rule.Body))
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/generators/typescript":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("export type Json = string | number | boolean | null\n"))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakePGMeta) last() pgmetaRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Requests[len(f.Requests)-1]
}

// fixture is a server over memory state with one org, one active project and a
// signed-in dashboard user.
type fixture struct {
	t       testing.TB
	srv     *Server
	reg     *registry.Memory
	mgr     *fakeManager
	meta    *fakePGMeta
	bdb     *fakeBranchDB
	cfg     *config.Config
	org     *registry.Organization
	project *registry.Project
	system  *secrets.ProjectKeys
	userID  string
	jwt     string
}

const testRef = "abcdefghijklmnopqrst"

func newFixture(t testing.TB) *fixture {
	t.Helper()
	reg := registry.NewMemory()
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Domain = "example.test"
	cfg.API.PGMetaCryptoKey = "test-crypto-key"
	mgr := newFakeManager(reg, sec)
	f := &fixture{t: t, reg: reg, mgr: mgr, cfg: cfg, meta: newFakePGMeta(t, cfg.API.PGMetaCryptoKey)}
	ctx := context.Background()
	if f.org, err = reg.CreateOrganization(ctx, "default", "Default"); err != nil {
		t.Fatal(err)
	}
	mgr.addProject(t, config.SystemRef, "system", 0, registry.StatusActiveHealthy)
	f.system, _ = mgr.Keys(ctx, config.SystemRef)
	f.project = mgr.addProject(t, testRef, "First project", f.org.ID, registry.StatusActiveHealthy)
	f.bdb = newFakeBranchDB()
	bsvc, err := branching.New(branching.Deps{Cfg: cfg, Registry: reg, Secrets: sec, Engine: branchEngine{mgr}, DB: f.bdb, CreateWait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bsvc.Drain(context.Background()) })
	f.srv, err = NewServer(Deps{
		Registry: reg, Secrets: sec, Manager: mgr, Config: cfg, PGMetaURL: f.meta.URL, Branching: bsvc,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), CreateWait: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.userID = "11111111-2222-4333-8444-555555555555"
	f.jwt = f.signJWT(map[string]any{"sub": f.userID, "email": "dev@example.test", "role": "authenticated",
		"user_metadata": map[string]any{"full_name": "Dev Eloper"}})
	return f
}

func (f *fixture) signJWT(claims map[string]any) string {
	f.t.Helper()
	// Like the users sbctl creates in sb-gotrue@system, sessions carry the admin claim.
	c := jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "aud": "authenticated",
		"app_metadata": map[string]any{AdminClaim: true}}
	for k, v := range claims {
		c[k] = v
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(f.system.JWTSecret))
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// do sends a request to the server with the dashboard JWT and returns the recorder.
func (f *fixture) do(method, path string, body any, headers ...string) *httptest.ResponseRecorder {
	return f.doAs(f.jwt, method, path, body, headers...)
}

// calledRoutes records every "METHOD path" the tests send, for the coverage check.
var calledRoutes sync.Map

func (f *fixture) doAs(token, method, path string, body any, headers ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	calledRoutes.Store(method+" "+strings.SplitN(path, "?", 2)[0], true)
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = strings.NewReader(string(b))
	default:
		j, err := json.Marshal(b)
		if err != nil {
			f.t.Fatal(err)
		}
		rd = strings.NewReader(string(j))
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t testing.TB, rec *httptest.ResponseRecorder) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body is not JSON: %v: %q", err, rec.Body.String())
	}
	return v
}
