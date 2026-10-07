package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OWNER/sbctl/internal/branching"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// Deps are the collaborators of the API server.
type Deps struct {
	Registry registry.Registry
	Secrets  secrets.Secrets
	// Manager creates, pauses and deletes projects and hands out connection strings
	// and credentials. Implemented by the lifecycle workstream; fakes in tests.
	Manager lifecycle.Manager
	Config  *config.Config
	Logger  *slog.Logger

	// Store is the API's own state (users, device logins, functions, content).
	// Empty derives it from Registry: the registry's database for a Postgres
	// registry, memory otherwise.
	Store Store
	// HTTPClient is used for every upstream call (pg-meta, GoTrue, Storage).
	HTTPClient *http.Client
	// PGMetaURL overrides http://127.0.0.1:<ports.pgmeta>.
	PGMetaURL string
	// Upstream overrides the loopback base URL of a project service ("gotrue" or
	// "storage"); an empty result keeps the default. For tests.
	Upstream func(p *registry.Project, svc string) string
	// Now is the clock; empty means time.Now.
	Now func() time.Time
	// Branching serves the branch endpoints. Nil: a project has only its default branch
	// and creating one is refused.
	Branching *branching.Service
	// CreateWait bounds how long POST /v1/projects waits for the new project to show
	// up in the registry before answering 201 COMING_UP. Zero means 10 seconds.
	CreateWait time.Duration
}

// Server is the Management API. It implements http.Handler.
type Server struct {
	reg      registry.Registry
	sec      secrets.Secrets
	mgr      lifecycle.Manager
	branches *branching.Service
	cfg      *config.Config
	log      *slog.Logger
	store    Store
	hc       *http.Client
	now      func() time.Time
	auth     *authenticator

	pgmetaURL        string
	upstreamOverride func(p *registry.Project, svc string) string
	createWait       time.Duration

	pgmetaKeyMu    chan struct{} // 1-slot lock around pgmetaKeyCache
	pgmetaKeyCache string

	roMu      sync.Mutex                 // guards roEnsured and roLocks, never held across I/O
	roLocks   map[string]*sync.Mutex     // per-project role setup locks
	roEnsured map[string]readOnlyEnsured // by project ref

	handler http.Handler
}

// route is one served operation.
type route struct {
	auth authKind
	h    handlerFunc
}

// New returns the Management API handler. It panics when the embedded OpenAPI
// documents do not load or the route table is inconsistent: both are programming
// errors that the package tests catch.
func New(d Deps) http.Handler {
	s, err := NewServer(d)
	if err != nil {
		panic(err)
	}
	return s
}

// NewServer is New with an error instead of a panic.
func NewServer(d Deps) (*Server, error) {
	if d.Registry == nil || d.Manager == nil || d.Config == nil || d.Secrets == nil {
		return nil, fmt.Errorf("api: Deps needs Registry, Secrets, Manager and Config")
	}
	s := &Server{
		reg: d.Registry, sec: d.Secrets, mgr: d.Manager, branches: d.Branching, cfg: d.Config, log: d.Logger, store: d.Store,
		hc: d.HTTPClient, now: d.Now, pgmetaURL: d.PGMetaURL, upstreamOverride: d.Upstream, createWait: d.CreateWait,
		pgmetaKeyMu: make(chan struct{}, 1), roEnsured: map[string]readOnlyEnsured{},
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.hc == nil {
		// No overall timeout: pg-meta queries and function uploads may be long.
		// Callers bound work with request contexts.
		s.hc = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if s.pgmetaURL == "" {
		s.pgmetaURL = fmt.Sprintf("http://127.0.0.1:%d", d.Config.Ports.PGMeta)
	}
	if s.createWait == 0 {
		s.createWait = 10 * time.Second
	}
	if s.store == nil {
		if pg, ok := d.Registry.(*registry.Postgres); ok {
			s.store = NewPGStore(pg.Pool())
		} else {
			s.store = NewMemoryStore()
		}
	}
	s.auth = newAuthenticator(s.reg, s.mgr.Keys, s.store, s.now, s.cfg.API.Admins())
	h, err := s.build()
	if err != nil {
		return nil, err
	}
	s.handler = h
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// authFor is the credential kind of a spec path.
func authFor(path string) authKind {
	if strings.HasPrefix(path, "/platform/") {
		return authJWT
	}
	return authAny
}

// implemented returns every hand-written route, keyed "METHOD /path" in the form of
// the OpenAPI template. Keys that are not operations of the pinned specs are extra
// routes (Studio calls a few that the platform spec omits).
func (s *Server) implemented() map[string]route {
	m := map[string]route{}
	add := func(key string, h handlerFunc) {
		if _, dup := m[key]; dup {
			panic("api: duplicate route " + key)
		}
		_, path, _ := strings.Cut(key, " ")
		m[key] = route{auth: authFor(path), h: h}
	}
	s.routesProfile(add)
	s.routesOrganizations(add)
	s.routesProjects(add)
	s.routesBranches(add)
	s.routesKeys(add)
	s.routesDatabase(add)
	s.routesFunctions(add)
	s.routesPlatformProject(add)
	s.routesContent(add)
	s.routesProxies(add)
	s.routesLogin(add)
	// The device-login poll carries no credentials: the CLI has none yet.
	r := m["GET /platform/cli/login/{session_id}"]
	r.auth = authNone
	m["GET /platform/cli/login/{session_id}"] = r
	return m
}

// build assembles the route table: every operation of the three specs is served,
// by its handler when one exists and by a spec-derived stub otherwise.
func (s *Server) build() (http.Handler, error) {
	ops, err := Operations()
	if err != nil {
		return nil, err
	}
	impl := s.implemented()
	mux := &muxSet{}
	inSpec := map[string]bool{}
	for _, op := range ops {
		key := op.Key()
		inSpec[key] = true
		if r, ok := impl[key]; ok {
			mux.handle(key, s.wrap(r.auth, r.h))
			continue
		}
		stub := stubHandler(op)
		mux.handle(key, s.wrap(authFor(op.Path), func(w http.ResponseWriter, r *http.Request) error {
			stub(w, r)
			return nil
		}))
	}
	for key, r := range impl {
		if !inSpec[key] {
			mux.handle(key, s.wrap(r.auth, r.h))
		}
	}
	mux.fallback = s.wrap(authAny, s.unknown)
	return s.middleware(mux), nil
}

// unknown answers requests for routes that neither the specs nor we define. Studio
// calls a handful of platform paths that no spec lists; an empty answer keeps its
// pages alive (research/05 section 4.4). Everything else is a 404.
func (s *Server) unknown(w http.ResponseWriter, r *http.Request) error {
	s.log.Debug("unknown route", "method", r.Method, "path", r.URL.Path)
	if !strings.HasPrefix(r.URL.Path, "/platform/") {
		return errf(http.StatusNotFound, "Not Found")
	}
	w.Header().Set("X-Sbctl-Stub", "true")
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{})
		return nil
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// wrap authenticates and runs h, turning returned errors into the error envelope.
func (s *Server) wrap(kind authKind, h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kind != authNone {
			p, err := s.auth.authenticate(r, kind)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			r = r.WithContext(withPrincipal(r.Context(), p))
		}
		if err := h(w, r); err != nil {
			s.fail(w, r, err)
		}
	})
}

// fail writes err as the error envelope. Unexpected errors are logged with their
// detail and answered with a generic 500.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	e := asError(err)
	if e.Status >= 500 {
		s.log.Error("api request failed", "method", r.Method, "path", r.URL.Path, "status", e.Status, "err", err)
	}
	writeError(w, e)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// middleware adds the request id, CORS, panic recovery and debug logging.
func (s *Server) middleware(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range s.cfg.API.Origins() {
		allowed[o] = true
	}
	allowed[s.cfg.DashboardURL()] = true
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		if origin := r.Header.Get("Origin"); origin != "" && allowed[strings.TrimRight(origin, "/")] {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				reqHeaders := r.Header.Get("Access-Control-Request-Headers")
				if reqHeaders == "" {
					reqHeaders = "Authorization, Content-Type"
				}
				h.Set("Access-Control-Allow-Headers", reqHeaders)
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		sw := &statusWriter{ResponseWriter: w}
		start := s.now()
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("api handler panic", "method", r.Method, "path", r.URL.Path, "panic", fmt.Sprint(rec))
				if sw.status == 0 {
					writeError(sw, errf(http.StatusInternalServerError, "Internal server error"))
				}
			}
			s.log.Debug("api request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
				"ms", s.now().Sub(start).Milliseconds(), "id", id)
		}()
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

// muxSet is a chain of ServeMuxes. The Supabase specs contain path pairs that Go's
// pattern rules call ambiguous (for example /apps/{app_id}/signing-keys next to
// /apps/installations/{installation_id}); a pattern that conflicts with one already
// in a mux goes to the next mux of the chain, and requests try the muxes in order.
type muxSet struct {
	muxes    []*http.ServeMux
	fallback http.Handler
}

func (m *muxSet) handle(pattern string, h http.Handler) {
	for _, mux := range m.muxes {
		if tryHandle(mux, pattern, h) {
			return
		}
	}
	mux := http.NewServeMux()
	m.muxes = append(m.muxes, mux)
	mux.Handle(pattern, h)
}

func tryHandle(mux *http.ServeMux, pattern string, h http.Handler) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	mux.Handle(pattern, h)
	return true
}

func (m *muxSet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, mux := range m.muxes {
		if _, pattern := mux.Handler(r); pattern != "" {
			// Serve through the mux itself: only it records the path wildcards.
			mux.ServeHTTP(w, r)
			return
		}
	}
	m.fallback.ServeHTTP(w, r)
}
