package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/jsmillerdev/supavise/internal/api/cryptojs"
	"github.com/jsmillerdev/supavise/internal/branching"
	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/members"
	"github.com/jsmillerdev/supavise/internal/projectconfig"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
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
	// Settings and password calls.
	applied   []string
	applyOpts []lifecycle.ApplyOptions
	applyErr  map[projectconfig.Service]error
	applyHook func(projectconfig.Service) error
	pending   bool
	passwords []string
	pwErr     error
	// hook, when set, runs inside Pause, Resume and Delete before they act.
	hook func(op string, ctx context.Context)
	// restores receives one record per finished restore; gate, when set, holds Run until it is
	// closed or sent to; restoreErr is what Run returns, beginErr what BeginRestore returns.
	restores   chan restoreRecord
	gate       chan struct{}
	restoreErr error
	beginErr   error
}

// restoreRecord is one restore the fakeManager ran.
type restoreRecord struct {
	ref string
	req lifecycle.RestoreRequest
	// deadline is whether the context Run got carried a deadline.
	deadline bool
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
	return &fakeManager{reg: reg, sec: sec, dsn: "postgres://postgres:pw@127.0.0.1:1/postgres?connect_timeout=1", keys: map[string]*secrets.ProjectKeys{}, applyErr: map[projectconfig.Service]error{}, keyCalls: map[string]int{}}
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

// BeginRestore makes fakeManager a lifecycle.DatabaseRestorer: like the Engine it refuses a
// project that is not active, and holds it RESTORING until Run ends.
func (m *fakeManager) BeginRestore(ctx context.Context, ref string) (lifecycle.RestoreRun, error) {
	m.mu.Lock()
	err := m.beginErr
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	p, err := m.reg.GetProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	if p.Status != registry.StatusActiveHealthy && p.Status != registry.StatusActiveUnhealthy && p.Status != registry.StatusRestoreFailed {
		return nil, fmt.Errorf("%w: cannot restore %s while it is %s", lifecycle.ErrInvalidState, ref, p.Status)
	}
	if err := m.reg.SetProjectStatus(ctx, ref, registry.StatusRestoring); err != nil {
		return nil, err
	}
	return &fakeRestore{m: m, ref: ref}, nil
}

type fakeRestore struct {
	m   *fakeManager
	ref string
}

func (r *fakeRestore) Run(ctx context.Context, req lifecycle.RestoreRequest) error {
	m := r.m
	m.mu.Lock()
	gate, err, out := m.gate, m.restoreErr, m.restores
	m.mu.Unlock()
	if gate != nil {
		<-gate
	}
	status := registry.StatusActiveHealthy
	if err != nil {
		status = registry.StatusRestoreFailed
	}
	_ = m.reg.SetProjectStatus(context.WithoutCancel(ctx), r.ref, status)
	if out != nil {
		_, hasDeadline := ctx.Deadline()
		out <- restoreRecord{ref: r.ref, req: req, deadline: hasDeadline}
	}
	return err
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
		// Like the real Engine, report the key records and the legacy switch stored in the
		// registry (the API writes them there).
		sealed, err := m.reg.GetSecrets(context.Background(), ref)
		if err != nil {
			return nil, err
		}
		extra := map[string]string{}
		for name, blob := range sealed {
			if strings.HasPrefix(name, secrets.NamePrefixAPIKey) || name == secrets.NameLegacyKeys {
				pt, err := m.sec.Open(blob)
				if err != nil {
					return nil, err
				}
				extra[name] = string(pt)
			}
		}
		kk := *k
		loaded := secrets.KeysFromMap(extra)
		kk.Records, kk.LegacyDisabled = loaded.Records, loaded.LegacyDisabled
		return &kk, nil
	}
	return nil, registry.ErrNotFound
}

// ApplyConfig and SetDatabasePassword make fakeManager a lifecycle.Reconfigurer.
func (m *fakeManager) ApplyConfig(_ context.Context, ref string, svc projectconfig.Service, opts lifecycle.ApplyOptions) (lifecycle.ApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applied = append(m.applied, ref+" "+string(svc))
	m.applyOpts = append(m.applyOpts, opts)
	if m.applyHook != nil {
		if err := m.applyHook(svc); err != nil {
			return lifecycle.ApplyResult{}, err
		}
	} else if err := m.applyErr[svc]; err != nil {
		return lifecycle.ApplyResult{}, err
	}
	return lifecycle.ApplyResult{Applied: true, PendingRestart: m.pending}, nil
}

func (m *fakeManager) SetDatabasePassword(_ context.Context, ref, password string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pwErr != nil {
		return m.pwErr
	}
	m.passwords = append(m.passwords, ref+":"+password)
	if k, ok := m.keys[ref]; ok {
		k.DBPassword = password
	}
	return nil
}

func (m *fakeManager) Health(context.Context, string) ([]lifecycle.ServiceHealth, error) {
	return []lifecycle.ServiceHealth{
		{Name: config.SvcPostgres, Healthy: true, Status: "ACTIVE_HEALTHY"},
		{Name: config.SvcGoTrue, Healthy: true, Status: "ACTIVE_HEALTHY"},
		{Name: config.SvcPostgREST, Healthy: false, Status: "UNHEALTHY", Error: "connection refused"},
	}, nil
}

func (m *fakeManager) ConnString(context.Context, string, string) (string, error) { return m.dsn, nil }

// fakePGMeta is an httptest stand-in for supavise-pgmeta: it decrypts the connection
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
	// gt is the stand-in for supavise-gotrue@system the server's account calls go to.
	gt *fakeGoTrue
	// pgt, when a test sets it, is the GoTrue of the project testRef.
	pgt *fakeGoTrue
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
	f.gt = newFakeGoTrue(t)
	f.srv, err = NewServer(Deps{
		Registry: reg, Secrets: sec, Manager: mgr, Config: cfg, PGMetaURL: f.meta.URL, Branching: bsvc,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), CreateWait: 2 * time.Second,
		Upstream: func(p *registry.Project, svc string) string {
			if p.Ref == config.SystemRef && svc == upGoTrue {
				return f.gt.URL
			}
			if p.Ref == testRef && svc == upGoTrue && f.pgt != nil {
				return f.pgt.URL
			}
			// Nothing of ours listens here, whatever else this machine runs on its default ports
			// (macOS answers on 5000).
			return "http://127.0.0.1:1"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.userID = "11111111-2222-4333-8444-555555555555"
	// The signed-in user owns the organization, as the claimed first user does.
	if err := f.srv.members.EnsureOwner(ctx, members.OrgRef{ID: f.org.ID, Slug: f.org.Slug}, f.userID); err != nil {
		t.Fatal(err)
	}
	f.jwt = f.signJWT(map[string]any{"sub": f.userID, "email": "dev@example.test", "role": "authenticated",
		"user_metadata": map[string]any{"full_name": "Dev Eloper"}})
	return f
}

func (f *fixture) signJWT(claims map[string]any) string {
	f.t.Helper()
	// Like the users supavise creates in supavise-gotrue@system, sessions carry the admin claim.
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

// addMember makes user a member of the fixture's organization with an organization-wide role.
func (f *fixture) addMember(user string, role int) {
	f.t.Helper()
	err := f.srv.members.Store.Update(context.Background(), f.org.ID, func(ops members.Ops) error {
		return ops.PutMember(context.Background(), members.Member{OrgID: f.org.ID, UserID: user, RoleID: role})
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

// ownerOn makes the fixture's signed-in user Owner of the fixture's organization on srv: a server
// built next to the fixture's has a member store of its own.
func (f *fixture) ownerOn(srv *Server) {
	f.t.Helper()
	if err := srv.members.EnsureOwner(context.Background(), members.OrgRef{ID: f.org.ID, Slug: f.org.Slug}, f.userID); err != nil {
		f.t.Fatal(err)
	}
}
