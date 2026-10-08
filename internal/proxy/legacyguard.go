package proxy

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/secrets"
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

// recheckRetries and recheckRetry bound how long a failed key lookup keeps tracked connections
// from being checked again before the next invalidation or periodic reload.
const (
	recheckRetries   = 6
	recheckRetryWait = 5 * time.Second
)

// recheckRealtime closes the project's tracked WebSockets when its legacy keys are now disabled.
// ref "" checks every project that has any. A failed key lookup is retried a few times (and the
// next invalidation or the periodic reload checks again).
func (s *Server) recheckRealtime(ref string) { s.recheckRealtimeN(ref, 0) }

func (s *Server) recheckRealtimeN(ref string, attempt int) {
	for _, r := range s.sockets.refs(ref) {
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
	}
}
