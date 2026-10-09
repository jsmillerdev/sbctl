package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// shape describes the documented success response of a route (see shapes_gen.go).
// field is a required response field and the neutral value to use for it: n null, s string,
// i number, b boolean, a array, o object, or 'x for the string literal x.
type field struct{ Name, Zero string }

type shape struct {
	Status   int
	Kind     string // "array", "object" or "empty"
	Required []field
}

// reqCtx is what a handler gets besides the request.
type reqCtx struct {
	params map[string]string
	user   *user
	body   []byte
	sql    string // set by the pg-meta handler, for the request log
	err    string // upstream error text for the request log
}

type handlerFunc func(w *respWriter, r *http.Request, c *reqCtx)

// route is a compiled path template.
type route struct {
	method   string
	template string // as in the specs, e.g. /platform/projects/{ref}
	segs     []string
	params   int // number of {param} segments, fewer means more specific
	handler  handlerFunc
	shape    *shape
	real     bool // a hand-written handler, as opposed to a generic stub
}

func compileRoute(method, template string, h handlerFunc, sh *shape) *route {
	rt := &route{method: method, template: template, segs: strings.Split(strings.Trim(template, "/"), "/"), handler: h, shape: sh}
	for _, s := range rt.segs {
		if isParam(s) {
			rt.params++
		}
	}
	return rt
}

func isParam(seg string) bool { return strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") }

func (rt *route) match(method string, segs []string) (map[string]string, bool) {
	if rt.method != method || len(segs) != len(rt.segs) {
		return nil, false
	}
	var params map[string]string
	for i, s := range rt.segs {
		if isParam(s) {
			if segs[i] == "" {
				return nil, false
			}
			if params == nil {
				params = map[string]string{}
			}
			params[s[1:len(s)-1]] = segs[i]
			continue
		}
		if s != segs[i] {
			return nil, false
		}
	}
	return params, true
}

// server is the mock's http.Handler.
type server struct {
	cfg     *Config
	routes  []*route // real handlers first, then one stub route per documented operation
	log     *requestLog
	client  *http.Client
	auth    *authState
	mu      sync.Mutex
	content map[string]map[string]any // SQL snippets per project ref, by id
	authz   map[string]*authzState    // authorization requests by id (oauth.go)
}

func newServer(cfg *Config) (*server, error) {
	s := &server{
		cfg: cfg,
		// postgres-meta (fastify) closes idle keep-alive connections after 5 s; closing ours sooner
		// avoids a request landing on a connection the server just closed (seen as 502 EOF).
		client:  &http.Client{Timeout: 70 * time.Second, Transport: &http.Transport{IdleConnTimeout: 2 * time.Second, MaxIdleConnsPerHost: 8}},
		auth:    newAuthState(cfg),
		content: map[string]map[string]any{},
		authz:   map[string]*authzState{},
	}
	for _, a := range cfg.Authorizations {
		s.authz[a.ID] = &authzState{Authorization: a}
	}
	var err error
	if s.log, err = newRequestLog(cfg.RequestLog); err != nil {
		return nil, err
	}
	s.registerReal()
	s.registerStubs()
	return s, nil
}

func (s *server) close() { s.log.close() }

func (s *server) handle(method, template string, h handlerFunc) {
	var sh *shape
	if v, ok := shapes[method+" "+template]; ok {
		sh = &v
	}
	rt := compileRoute(method, template, h, sh)
	rt.real = true
	s.routes = append(s.routes, rt)
}

// registerStubs adds a route per operation in the specs that has no real handler yet.
func (s *server) registerStubs() {
	have := map[string]bool{}
	for _, r := range s.routes {
		have[r.method+" "+r.template] = true
	}
	keys := make([]string, 0, len(shapes))
	for k := range shapes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if have[k] {
			continue
		}
		method, template, _ := strings.Cut(k, " ")
		sh := shapes[k]
		s.routes = append(s.routes, compileRoute(method, template, stubHandler, &sh))
	}
	// Literal segments win over parameters: /members/invitations before /members/{gotrue_id}.
	sort.SliceStable(s.routes, func(i, j int) bool { return s.routes[i].params < s.routes[j].params })
}

func (s *server) find(method, path string) (*route, map[string]string) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for _, rt := range s.routes {
		if p, ok := rt.match(method, segs); ok {
			return rt, p
		}
	}
	return nil, nil
}

// respWriter records status and size for the request log.
type respWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *respWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *respWriter) json(status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (w *respWriter) message(status int, msg string) { w.json(status, map[string]any{"message": msg}) }

func (s *server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	start := time.Now()
	w := &respWriter{ResponseWriter: rw}
	s.cors(w, r)
	entry := logEntry{Time: start.UTC().Format(time.RFC3339Nano), Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Origin: r.Header.Get("Origin")}

	defer func() {
		entry.Status = w.status
		entry.Bytes = w.bytes
		entry.Millis = float64(time.Since(start).Microseconds()) / 1000
		s.log.write(entry)
	}()

	if r.Method == http.MethodOptions {
		entry.Handler = "preflight"
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	r.Body = io.NopCloser(bytes.NewReader(body))
	entry.BodyBytes = len(body)
	entry.Headers = interestingHeaders(r)

	if strings.HasPrefix(r.URL.Path, "/auth/v1/") {
		entry.Handler = "builtin-auth"
		entry.Template = r.URL.Path
		if s.cfg.BuiltinAuth == nil {
			w.message(http.StatusNotFound, "built-in auth is not enabled")
			return
		}
		s.auth.serve(w, r, body)
		return
	}
	if r.URL.Path == callbackPath {
		// The query holds the authorization code; it stays out of the log.
		entry.Handler, entry.Template, entry.Query = "real", callbackPath, ""
		serveCallback(w)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/healthz") {
		entry.Handler = "health"
		w.json(http.StatusOK, map[string]any{"ok": true})
		return
	}

	u, authState := s.auth.verifyRequest(r)
	entry.Auth = authState
	if u != nil {
		entry.User = u.Email
	}
	api := strings.HasPrefix(r.URL.Path, "/platform/") || strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/v2/")
	if !api {
		entry.Handler = "unknown"
		entry.Template = r.URL.Path
		w.message(http.StatusNotFound, "not found")
		return
	}
	if u == nil {
		entry.Handler = "auth"
		entry.Template = r.URL.Path
		w.message(http.StatusUnauthorized, "Unauthorized")
		return
	}

	rt, params := s.find(r.Method, r.URL.Path)
	if rt == nil {
		// Baseline from the supastack mock: unknown GET gets {}, anything else 204.
		entry.Handler = "unknown"
		entry.Template = r.URL.Path
		if r.Method == http.MethodGet {
			w.json(http.StatusOK, map[string]any{})
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	entry.Template = rt.template
	entry.Handler = "stub"
	if ref := params["ref"]; ref != "" {
		entry.Ref = ref
	}
	c := &reqCtx{params: params, user: u, body: body}
	ctx := context.WithValue(r.Context(), routeKey{}, rt)
	rt.handler(w, r.WithContext(ctx), c)
	if rt.isReal() {
		entry.Handler = "real"
	}
	entry.SQL = c.sql
	entry.Err = c.err
}

type routeKey struct{}

func (rt *route) isReal() bool { return rt.real }

// cors answers for the configured Studio origins. Studio calls with credentials, so the origin
// is echoed, never "*".
func (s *server) cors(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	allowed := false
	for _, o := range s.cfg.StudioOrigins {
		if o == "*" || o == origin {
			allowed = true
		}
	}
	if !allowed {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "authorization, content-type, accept, x-request-id, x-connection-encrypted, x-pg-application-name, version, x-client-info, apikey")
	h.Set("Access-Control-Expose-Headers", "Retry-After, X-RateLimit-Reset, X-Total-Count")
	h.Set("Access-Control-Max-Age", "600")
	h.Add("Vary", "Origin")
}

func interestingHeaders(r *http.Request) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"X-Request-Id", "Version", "X-Pg-Application-Name", "Content-Type"} {
		if v := r.Header.Get(k); v != "" {
			out[strings.ToLower(k)] = v
		}
	}
	if r.Header.Get("X-Connection-Encrypted") != "" {
		out["x-connection-encrypted"] = "present"
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
