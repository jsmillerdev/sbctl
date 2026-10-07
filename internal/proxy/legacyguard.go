package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/OWNER/sbctl/internal/secrets"
)

// While a project's legacy keys are disabled, no request to its host may carry the signing
// input of its anon or service_role key, wherever the client put it. Chasing the encodings
// upstream services read a JWT from (a bearer with any scheme and spacing, a bare token, a
// duplicate header, an unrelated header, the query string) route by route does not end, so the
// proxy looks for the key itself: every value of every request header and the raw query string,
// decoded and undecoded, before any route-specific logic. The route-specific checks in keys.go
// stay as a second line.

// legacyWireNeedles returns the strings that give away a legacy key in a header value or query
// string, or nil when the legacy keys are enabled. For a JWT it is "<header>.<payload>", the
// signing input: it is what legacyNeedles finds in a socket ("<header>.<payload>."), without the
// trailing dot, so a key whose signature was cut off, re-encoded or separated by a different
// character is found too. Users' session JWTs share the header but not the payload.
func legacyWireNeedles(k *secrets.ProjectKeys) []string {
	if k == nil || !k.LegacyDisabled {
		return nil
	}
	var n []string
	for _, key := range []string{k.AnonKey, k.ServiceRoleKey} {
		switch in, ok := signingInput(key); {
		case key == "":
		case ok && strings.Count(key, ".") == 2:
			n = append(n, in)
		default:
			n = append(n, key)
		}
	}
	return n
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// percentDecode decodes every valid %XX escape and leaves the rest of s as it is, so a
// malformed escape elsewhere cannot keep a key from being found.
func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && unhex(s[i+1]) >= 0 && unhex(s[i+2]) >= 0 {
			b.WriteByte(byte(unhex(s[i+1])<<4 | unhex(s[i+2])))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// carriesLegacyKey reports whether a request's headers (every value of every header) or raw
// query string (as sent and percent-decoded) contain a legacy key of the project. It is false
// while the legacy keys are enabled.
func carriesLegacyKey(k *secrets.ProjectKeys, h http.Header, rawQuery string) bool {
	needles := legacyWireNeedles(k)
	if len(needles) == 0 {
		return false
	}
	for _, vs := range h {
		for _, v := range vs {
			if containsAny(v, needles) || containsAny(percentDecode(v), needles) {
				return true
			}
		}
	}
	return containsAny(rawQuery, needles) || containsAny(percentDecode(rawQuery), needles)
}

// refuseLegacy answers a request that carries a legacy key: 401 with a JSON message, or, on the S3
// route, 403 with Storage's own XML AccessDenied document (S3 clients parse it).
func refuseLegacy(w http.ResponseWriter, rt *route) {
	if rt.keys == keyS3 {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(s3AccessDeniedBody))
		return
	}
	writeJSON(w, http.StatusUnauthorized, msgInvalidKey)
}

// ---- Realtime long-poll sessions ----

// A Phoenix long-poll session is a process on the Realtime side that outlives single requests:
// the first GET (no token) connects with the credentials of that request and answers with a
// session token, and every later GET or POST resumes the session by its "token" query parameter
// without connecting again. A session opened while the legacy keys were enabled would therefore
// keep the legacy key it connected with (and authorize channel joins with it) after the switch.
// The proxy remembers the tokens of such sessions per project and, when the project's legacy keys
// change to disabled, turns every later request that carries one of them away with a 410, so the
// client starts a new session through the guarded path. As with WebSockets, only sessions opened
// while the keys were enabled are tracked; one opened later went through the guard.
const (
	lpLiveIdle     = 15 * time.Minute // a tracked session nobody polls is forgotten (Realtime ended it long ago)
	lpRevokedFor   = time.Hour        // how long a revoked token keeps being refused
	lpMaxLive      = 10000            // tracked sessions per project; the least recently used goes first
	lpMaxSessionRs = 64 << 10         // the response to a new session is a token and no messages
)

type lpProject struct {
	live    map[string]time.Time // token -> last seen
	revoked map[string]time.Time // token -> revoked at
}

type lpSessions struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]*lpProject
}

func (s *lpSessions) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *lpSessions) project(ref string) *lpProject {
	if s.m == nil {
		s.m = map[string]*lpProject{}
	}
	p := s.m[ref]
	if p == nil {
		p = &lpProject{live: map[string]time.Time{}, revoked: map[string]time.Time{}}
		s.m[ref] = p
	}
	return p
}

func (p *lpProject) prune(now time.Time) {
	for t, at := range p.live {
		if now.Sub(at) > lpLiveIdle {
			delete(p.live, t)
		}
	}
	for t, at := range p.revoked {
		if now.Sub(at) > lpRevokedFor {
			delete(p.revoked, t)
		}
	}
}

// add starts tracking a session of ref.
func (s *lpSessions) add(ref, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	p := s.project(ref)
	p.prune(now)
	for len(p.live) >= lpMaxLive {
		var oldest string
		var at time.Time
		for t, a := range p.live {
			if oldest == "" || a.Before(at) {
				oldest, at = t, a
			}
		}
		delete(p.live, oldest)
	}
	p.live[token] = now
}

// seen reports whether any of tokens is a revoked session of ref, and refreshes the ones that
// are tracked live.
func (s *lpSessions) seen(ref string, tokens []string) (revoked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.m[ref]
	if p == nil {
		return false
	}
	now := s.clock()
	for _, t := range tokens {
		if at, ok := p.revoked[t]; ok {
			if now.Sub(at) <= lpRevokedFor {
				revoked = true
			}
			continue
		}
		if _, ok := p.live[t]; ok {
			p.live[t] = now
		}
	}
	return revoked
}

// revoke turns every tracked session of ref into a revoked one and reports how many there were.
func (s *lpSessions) revoke(ref string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.m[ref]
	if p == nil {
		return 0
	}
	now := s.clock()
	p.prune(now)
	n := len(p.live)
	for t := range p.live {
		p.revoked[t] = now
		delete(p.live, t)
	}
	if len(p.revoked) == 0 {
		delete(s.m, ref)
	}
	return n
}

// refs lists the projects with a tracked live session; with ref set, just that project if it has one.
func (s *lpSessions) refs(ref string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref != "" {
		if p := s.m[ref]; p != nil && len(p.live) > 0 {
			return []string{ref}
		}
		return nil
	}
	var out []string
	for r, p := range s.m {
		if len(p.live) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// queryValues returns every decoded value of the query parameter name in a raw query string.
func queryValues(raw, name string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, "&") {
		n, v, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		if dn, err := url.QueryUnescape(n); err != nil || dn != name {
			continue
		}
		if dv, err := url.QueryUnescape(v); err == nil {
			out = append(out, dv)
		} else {
			out = append(out, percentDecode(v))
		}
	}
	return out
}

// refuseRevokedSession answers a long-poll request that resumes a session revoked by the switch.
// The body has the shape of a Phoenix long-poll answer without a token, so the client starts over
// with a new session instead of treating the answer as an error.
func refuseRevokedSession(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusGone)
	_, _ = w.Write([]byte(`{"status":410,"messages":[],"message":"This session was opened before the legacy API keys were disabled for this project: start a new session"}`))
}

// captureSession reads the answer to a long-poll request that opened a session (while the legacy
// keys were enabled) and starts tracking the token in it. The body is passed on unchanged.
func (s *Server) captureSession(ref string, res *http.Response) {
	if res.Body == nil || res.Body == http.NoBody || res.ContentLength > lpMaxSessionRs || res.Header.Get("Content-Encoding") != "" {
		return
	}
	buf, err := io.ReadAll(io.LimitReader(res.Body, lpMaxSessionRs+1))
	res.Body = &joinedBody{Reader: io.MultiReader(bytes.NewReader(buf), res.Body), closer: res.Body}
	if err != nil || len(buf) > lpMaxSessionRs {
		return
	}
	var v struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(buf, &v) != nil || v.Token == "" {
		return
	}
	s.lp.add(ref, v.Token)
	// The keys may have been switched off while the session was being opened.
	go s.recheckRealtime(ref)
}

type joinedBody struct {
	io.Reader
	closer io.Closer
}

func (b *joinedBody) Close() error { return b.closer.Close() }

// recheckRetries and recheckRetry bound how long a failed key lookup keeps tracked connections
// from being checked again before the next invalidation or periodic reload.
const (
	recheckRetries   = 6
	recheckRetryWait = 5 * time.Second
)

// recheckRealtime closes the project's tracked WebSockets and revokes its tracked long-poll
// sessions when its legacy keys are now disabled. ref "" checks every project that has any. A
// failed key lookup is retried a few times (and the next invalidation or the periodic reload
// checks again).
func (s *Server) recheckRealtime(ref string) { s.recheckRealtimeN(ref, 0) }

func (s *Server) recheckRealtimeN(ref string, attempt int) {
	seen := map[string]bool{}
	for _, r := range append(s.sockets.refs(ref), s.lp.refs(ref)...) {
		if seen[r] {
			continue
		}
		seen[r] = true
		k, err := s.table.projectKeys(context.Background(), r)
		if err != nil {
			if attempt < recheckRetries {
				wait := s.recheckWait
				if wait == 0 {
					wait = recheckRetryWait
				}
				r := r
				time.AfterFunc(wait, func() { s.recheckRealtimeN(r, attempt+1) })
			}
			continue
		}
		if k == nil || !k.LegacyDisabled {
			continue
		}
		if n := s.sockets.closeAll(r, wsCloseReason); n > 0 {
			s.log.Warn("proxy: closed Realtime sockets opened before the legacy API keys were disabled", "ref", r, "sockets", n)
		}
		if n := s.lp.revoke(r); n > 0 {
			s.log.Warn("proxy: revoked Realtime long-poll sessions opened before the legacy API keys were disabled", "ref", r, "sessions", n)
		}
	}
}
