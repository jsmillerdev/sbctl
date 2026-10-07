package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
)

const requestIDHeader = "X-Request-Id"

// TenantHeader tells the edge runtime's main service which project a /functions/v1
// request belongs to. It is always overwritten by the proxy.
const TenantHeader = "X-Sbctl-Project-Ref"

// ServeHTTP is the whole edge: it dispatches on the Host header to the Management
// API, Studio, or a project's API.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rid := requestID(r)
	r.Header.Set(requestIDHeader, rid)
	w.Header().Set(requestIDHeader, rid)
	sw := &statusWriter{ResponseWriter: w}

	host := normalizeHost(r.Host)
	which, ref := "", ""
	switch {
	case s.apiHost != "" && host == s.apiHost:
		which = "api"
		s.serveAPI(sw, r)
	case s.studioHost != "" && host == s.studioHost:
		which = "studio"
		s.serveStudio(sw, r)
	default:
		p, ok := s.table.lookup(host)
		if !ok {
			which = "unknown"
			writeJSON(sw, http.StatusNotFound, "Not Found")
			break
		}
		which, ref = "project", p.ref
		s.serveProject(sw, r, p)
	}

	lvl := slogLevel(sw.code())
	s.log.Log(r.Context(), lvl, "request", "id", rid, "kind", which, "host", host, "ref", ref,
		"method", r.Method, "path", r.URL.Path, "status", sw.code(), "bytes", sw.bytes,
		"dur_ms", time.Since(start).Milliseconds(), "remote", clientIP(r))
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	if p := r.URL.Path; p == "/auth/v1" || strings.HasPrefix(p, "/auth/v1/") {
		s.serveDashboardAuth(w, r)
		return
	}
	if s.opts.APIHandler == nil {
		writeJSON(w, http.StatusServiceUnavailable, "Management API is not available")
		return
	}
	// /internal/ is for the loopback listener (a project's GoTrue fetching its mail
	// templates); nothing that arrives through the edge may reach it, whoever the client is.
	if c := path.Clean("/" + r.URL.Path); c == "/internal" || strings.HasPrefix(c, "/internal/") {
		writeJSON(w, http.StatusNotFound, "Not Found")
		return
	}
	s.opts.APIHandler.ServeHTTP(w, r)
}

func (s *Server) serveStudio(w http.ResponseWriter, r *http.Request) {
	if answerStudioLocally(w, r) {
		return
	}
	s.forward(w, r, &target{
		addr: s.upstream(svcStudio, project{}), path: r.URL.Path, rawPath: r.URL.EscapedPath(), rawQuery: r.URL.RawQuery,
		fwdHost: r.Host, timeout: 60 * time.Second, studio: true,
	})
}

// serveProject routes a request on <ref>.api.<domain>.
func (s *Server) serveProject(w http.ResponseWriter, r *http.Request, p project) {
	setCORS(w.Header(), r)
	if isPreflight(r) {
		writePreflight(w, r)
		return
	}
	pth, trailing, err := cleanPath(r.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, "Bad Request")
		return
	}
	rt := matchRoute(pth)
	if rt == nil {
		writeJSON(w, http.StatusNotFound, "no Route matched with those values")
		return
	}
	if rt.access == accessBlocked {
		writeText(w, http.StatusForbidden, msgForbidden)
		return
	}

	res := authResult{rawQuery: r.URL.RawQuery}
	var guardKeys [][]byte
	if rt.keys != keyNone {
		k, err := s.table.projectKeys(r.Context(), p.ref)
		switch {
		case err != nil && rt.access == accessOpen:
			// An open route (GoTrue verify and callback, public storage objects) needs no
			// key to be allowed through; upstream's Envoy forwards these with static keys.
			// A registry hiccup, a decrypt error or a project whose secrets are not written
			// yet must not take them down. The request goes on with its credentials
			// untouched, and the status gate below still holds back an inactive project.
			if !errors.Is(err, registry.ErrNotFound) {
				s.log.Warn("proxy: project keys unavailable; forwarding an open route without key translation", "ref", p.ref, "path", pth, "err", err)
			}
			k = nil
		case errors.Is(err, registry.ErrNotFound):
			writeJSON(w, http.StatusNotFound, "Project not found")
			return
		case err != nil:
			s.log.Error("proxy: loading project keys", "ref", p.ref, "err", err)
			writeJSON(w, http.StatusServiceUnavailable, "Project credentials are unavailable")
			return
		}
		if k != nil {
			if rt.keys == keyRealtime {
				guardKeys = legacyNeedles(k)
			}
			res = authorize(rt, k, p.ref, r.Header, r.URL.RawQuery)
			if res.status != 0 {
				if res.ctype != "" {
					w.Header().Set("Content-Type", res.ctype)
					w.WriteHeader(res.status)
					_, _ = w.Write([]byte(res.body))
					return
				}
				writeText(w, res.status, res.body)
				return
			}
		}
	}

	if !servable(p.status) {
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, "Project is not active ("+string(p.status)+")")
		return
	}
	if rt.svc == svcFunctions && !s.opts.FunctionsEnabled {
		writeJSON(w, http.StatusServiceUnavailable, "Edge Functions are not enabled on this node")
		return
	}
	if err := s.waker.Start(r.Context(), p.ref); err != nil {
		s.log.Warn("proxy: waker failed", "ref", p.ref, "err", err)
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusServiceUnavailable, "Project is starting, retry shortly")
		return
	}

	tg := &target{
		addr: s.upstream(rt.svc, p), path: rt.upstreamPath(pth, trailing), rawPath: escapedUpstreamPath(r.URL, pth, rt, trailing), rawQuery: res.rawQuery,
		fwdHost: r.Host, fwdPrefix: rt.fwdPrefix, timeout: rt.timeout, set: res.set, del: res.del,
	}
	for _, h := range rt.setHeaders {
		if tg.set == nil {
			tg.set = map[string]string{}
		}
		tg.set[h[0]] = h[1]
	}
	switch rt.svc {
	case svcRealtime:
		// Realtime resolves the tenant from the first label of Host.
		tg.host = config.RealtimeInternalHost(p.ref)
	case svcStorage:
		// Storage (MULTI_TENANT) resolves the tenant from x-forwarded-host only.
		tg.fwdHost = s.cfg.ProjectHost(p.ref)
	case svcFunctions:
		if tg.set == nil {
			tg.set = map[string]string{}
		}
		tg.set[TenantHeader] = p.ref
	}
	tg.dropTenant = rt.svc != svcFunctions
	if len(guardKeys) > 0 {
		// Legacy keys are disabled: watch what the client sends into the socket (or the long-poll
		// body) for a legacy key. A compressing extension would hide the text, so it is not offered
		// to Realtime.
		tg.guardKeys = guardKeys
		tg.del = append(tg.del[:len(tg.del):len(tg.del)], "Sec-Websocket-Extensions")
		tg.onGuard = func(reason string) {
			s.log.Warn("proxy: closed a Realtime connection that carried a legacy API key", "ref", p.ref, "reason", reason)
		}
	}
	s.forward(w, r, tg)
}

// servable reports whether a project in status st may receive traffic.
func servable(st registry.Status) bool {
	switch st {
	case registry.StatusInactive, registry.StatusPausing, registry.StatusGoingDown,
		registry.StatusRemoved, registry.StatusInitFailed:
		return false
	}
	return true
}

// target describes one upstream request.
type target struct {
	addr string
	path string
	// rawPath is path as the client escaped it; empty lets Go re-escape path. S3
	// SigV4 signatures cover the client's canonical URI, so it must reach Storage unchanged.
	rawPath  string
	rawQuery string
	// host overrides the upstream Host header; empty keeps the client's.
	host      string
	fwdHost   string
	fwdPrefix string
	set       map[string]string
	del       []string
	timeout   time.Duration
	studio    bool
	// dashboardAuth marks the dashboard GoTrue route: its own CORS headers are replaced
	// like the project API's, but the policy is set by serveDashboardAuth.
	dashboardAuth bool
	// dropTenant removes a client-supplied TenantHeader (only functions sets it).
	dropTenant bool
	// guardKeys, when set, are legacy keys that must not travel from the client to the upstream
	// inside a WebSocket text message or a request body (see wsguard.go); onGuard is told of a hit.
	guardKeys [][]byte
	onGuard   func(reason string)
}

// forward proxies r to tg, streaming in both directions and passing WebSocket
// upgrades through.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, tg *target) {
	if len(tg.guardKeys) > 0 {
		if r.Header.Get("Upgrade") != "" {
			w = &guardedWriter{ResponseWriter: w, wrap: func(c net.Conn, brw *bufio.ReadWriter) net.Conn {
				return &wsGuardConn{Conn: c, r: brw.Reader, insp: newWSInspector(tg.guardKeys), onBlock: tg.onGuard}
			}}
		} else if ok, hit := checkBody(w, r, tg.guardKeys); !ok {
			if hit && tg.onGuard != nil {
				tg.onGuard("request body")
			}
			return
		}
	}
	rp := &httputil.ReverseProxy{
		Transport:     s.transport(tg.timeout),
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL.Scheme = "http"
			out.URL.Host = tg.addr
			out.URL.Path = tg.path
			out.URL.RawPath = tg.rawPath
			out.URL.RawQuery = tg.rawQuery
			out.Host = pr.In.Host
			if tg.host != "" {
				out.Host = tg.host
			}
			h := out.Header
			// ReverseProxy already dropped X-Forwarded-For/Host/Proto and Forwarded;
			// drop the remaining spoofable ones and set our own.
			h.Del("X-Forwarded-Port")
			h.Del("X-Forwarded-Prefix")
			h.Del("X-Real-Ip")
			if tg.dropTenant {
				h.Del(TenantHeader)
			}
			h.Set("X-Forwarded-For", clientIP(pr.In))
			h.Set("X-Forwarded-Host", tg.fwdHost)
			h.Set("X-Forwarded-Proto", scheme(pr.In))
			h.Set("X-Forwarded-Port", localPort(pr.In))
			if tg.fwdPrefix != "" {
				h.Set("X-Forwarded-Prefix", tg.fwdPrefix)
			}
			for _, name := range tg.del {
				h.Del(name)
			}
			for name, v := range tg.set {
				h.Set(name, v)
			}
		},
		ModifyResponse: func(res *http.Response) error {
			if tg.studio {
				rewriteStudioResponse(res.Header)
			} else {
				// One CORS policy, ours: upstream services add their own and the browser rejects duplicates.
				for name := range res.Header {
					if strings.HasPrefix(http.CanonicalHeaderKey(name), "Access-Control-") {
						res.Header.Del(name)
					}
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var ne net.Error
			switch {
			case errors.Is(err, context.Canceled):
				// The client went away; nothing to answer.
			case errors.As(err, &ne) && ne.Timeout():
				s.log.Warn("proxy: upstream timeout", "upstream", tg.addr, "err", err)
				writeJSON(w, http.StatusGatewayTimeout, "Upstream timed out")
			default:
				s.log.Warn("proxy: upstream error", "upstream", tg.addr, "err", err)
				writeJSON(w, http.StatusBadGateway, "Upstream unavailable")
			}
		},
	}
	rp.ServeHTTP(w, r)
}

// transport returns the shared upstream transport for a response-header timeout.
func (s *Server) transport(timeout time.Duration) *http.Transport {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.transports[timeout]; ok {
		return t
	}
	t := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: timeout,
		MaxIdleConnsPerHost:   64,
		// Shorter than the 5 s default keep-alive of the Node upstreams, so a pooled
		// connection is never reused after the upstream closed it (POSTs are not retried).
		IdleConnTimeout:    4 * time.Second,
		DisableCompression: true,
	}
	s.transports[timeout] = t
	return t
}

// upstream is the loopback address of svc for project p.
func (s *Server) upstream(svc service, p project) string {
	if s.upstreamFn != nil {
		return s.upstreamFn(svc, p)
	}
	pp := s.cfg.PortsFor(p.ref, p.seq)
	var port int
	switch svc {
	case svcRest:
		port = pp.PostgREST
	case svcAuth:
		port = pp.GoTrue
	case svcRealtime:
		port = s.cfg.Ports.Realtime
	case svcStorage:
		port = s.cfg.Ports.Storage
	case svcFunctions:
		port = s.cfg.Ports.EdgeRuntime
	case svcStudio:
		port = s.cfg.Ports.Studio
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// ---- helpers ----

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func localPort(r *http.Request) string {
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, p, err := net.SplitHostPort(a.String()); err == nil {
			return p
		}
	}
	if r.TLS != nil {
		return "443"
	}
	return "80"
}

func requestID(r *http.Request) string {
	if id := r.Header.Get(requestIDHeader); validRequestID(id) {
		return id
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, msg string) {
	b, _ := json.Marshal(map[string]string{"message": msg})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeText(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

// ---- CORS ----

const allowMethods = "GET,POST,PUT,PATCH,DELETE,OPTIONS,HEAD,CONNECT,TRACE"

// setCORS sets the response CORS headers of the project API: any origin is
// allowed (the keys are the access control), as upstream's gateway does.
func setCORS(h http.Header, r *http.Request) {
	if o := r.Header.Get("Origin"); o != "" {
		h.Set("Access-Control-Allow-Origin", o)
		h.Add("Vary", "Origin")
	} else {
		h.Set("Access-Control-Allow-Origin", "*")
	}
	h.Set("Access-Control-Expose-Headers", "*")
}

func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Origin") != "" && r.Header.Get("Access-Control-Request-Method") != ""
}

func writePreflight(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", allowMethods)
	// "*" does not cover Authorization, so echo what the browser asks for.
	if v := r.Header.Get("Access-Control-Request-Headers"); v != "" {
		h.Set("Access-Control-Allow-Headers", v)
		h.Add("Vary", "Access-Control-Request-Headers")
	} else {
		h.Set("Access-Control-Allow-Headers", "authorization,apikey,content-type,x-client-info")
	}
	h.Set("Access-Control-Max-Age", "3600")
	w.WriteHeader(http.StatusNoContent)
}

// serveDashboardAuth forwards api.<domain>/auth/v1/* to the system project's GoTrue, the
// sign-in service of Studio (its NEXT_PUBLIC_GOTRUE_URL, and the API_EXTERNAL_URL the
// system GoTrue is configured with). It is the Kong route of upstream's self-hosted
// gateway for the dashboard: the prefix is stripped, and no apikey is needed or wanted,
// because gotrue-js in Studio sends none. CORS is limited to the dashboard's own origins
// (public dashboard URL and [api] allowed_origins); anything else gets no CORS headers.
func (s *Server) serveDashboardAuth(w http.ResponseWriter, r *http.Request) {
	pth, trailing, err := cleanPath(r.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, "Bad Request")
		return
	}
	if pth != "/auth/v1" && !strings.HasPrefix(pth, "/auth/v1/") {
		writeJSON(w, http.StatusNotFound, "Not Found") // ".." stepped out of the prefix
		return
	}
	origin := r.Header.Get("Origin")
	allowed := origin != "" && s.dashboardOrigin(origin)
	if allowed {
		w.Header().Add("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Expose-Headers", "*")
	}
	if isPreflight(r) {
		if allowed {
			writePreflight(w, r)
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
		return
	}
	rest := strings.TrimPrefix(pth, "/auth/v1")
	if rest == "" {
		rest = "/"
	} else if trailing && !strings.HasSuffix(rest, "/") {
		rest += "/"
	}
	tg := &target{
		addr: s.upstream(svcAuth, project{ref: config.SystemRef}), path: rest, rawQuery: r.URL.RawQuery,
		fwdHost: r.Host, fwdPrefix: "/auth/v1/", timeout: defaultTimeout, dashboardAuth: true,
	}
	s.forward(w, r, tg)
}

// dashboardOrigin reports whether a browser origin may call the dashboard's GoTrue.
func (s *Server) dashboardOrigin(origin string) bool {
	origin = strings.TrimRight(origin, "/")
	if strings.EqualFold(origin, s.cfg.DashboardURL()) {
		return true
	}
	for _, o := range s.cfg.API.Origins() {
		if strings.EqualFold(origin, o) {
			return true
		}
	}
	return false
}
