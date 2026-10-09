package proxy

import (
	"net/http"
	"path"
	"strings"

	"github.com/supavise/supavise/internal/notice"
)

// Studio is served unmodified (three patches, no fixups), so the two things its build
// gets wrong for a self-hosted node are corrected here, on the way through.

// Studio's own /api/incident-banner route asks incident.io, answers 500 without a key, and
// react-query retries it after 1, 4 and 16 seconds while the sign-in form awaits the query
// cache reset: sign-in took 22 seconds (docs/reference/studio-platform-calls.md, "Findings from running Studio"). The proxy answers the route
// itself, instantly and with no outbound call, with {"incidents":[]}.
//
// The answer stays empty even while a maintenance window is announced or an upgrade runs: this
// Studio build draws any incident as "We are investigating a technical issue" with a link to
// Supabase's status page, which would present planned work as an outage (internal/notice has the
// reasoning and the answer to serve once Studio can show the operator's own words). The
// operator's notices appear in `supavise status` and /healthz/detail.

// answerStudioLocally serves the routes supavise answers instead of Studio. It reports
// whether it wrote a response.
func (s *Server) answerStudioLocally(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/api/incident-banner" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(notice.BannerJSON())
	}
	return true
}

// blockedStudioHosts are third parties Studio contacts from the browser on every page load
// although this deployment configures none of them: Usercentrics, the consent-banner vendor,
// is requested at https://api.usercentrics.eu/settings//latest/languages.json even with
// NEXT_PUBLIC_USERCENTRICS_RULESET_ID unset (an empty settings id; the answer is a 403).
// Studio's Content-Security-Policy allows the host by default, so removing it from the policy
// makes the browser refuse the request without a fourth Studio patch. The call already fails
// soft, so nothing else changes. Proposed upstream: skip initialization without a ruleset id.
var blockedStudioHosts = []string{"usercentrics.eu"}

// scrubCSP removes every source in a Content-Security-Policy whose host is, or is under,
// one of blocked. It returns csp unchanged when nothing matched.
func scrubCSP(csp string, blocked []string) string {
	if csp == "" {
		return csp
	}
	changed := false
	directives := strings.Split(csp, ";")
	for i, d := range directives {
		fields := strings.Fields(d)
		if len(fields) < 2 {
			continue
		}
		kept := fields[:1]
		for _, src := range fields[1:] {
			if blockedSource(src, blocked) {
				changed = true
				continue
			}
			kept = append(kept, src)
		}
		directives[i] = " " + strings.Join(kept, " ")
	}
	if !changed {
		return csp
	}
	return strings.TrimSpace(strings.Join(directives, ";"))
}

// blockedSource reports whether a CSP source expression names a host in blocked:
// "https://*.usercentrics.eu", "app.usercentrics.eu", "https://api.usercentrics.eu/path".
func blockedSource(src string, blocked []string) bool {
	host := src
	if _, rest, ok := strings.Cut(host, "://"); ok {
		host = rest
	}
	host, _, _ = strings.Cut(host, "/")
	host, _, _ = strings.Cut(host, ":")
	host = strings.ToLower(strings.TrimPrefix(host, "*."))
	for _, b := range blocked {
		if host == b || strings.HasSuffix(host, "."+b) {
			return true
		}
	}
	return false
}

// rewriteStudioResponse adjusts the headers of a Studio response.
func rewriteStudioResponse(h http.Header) {
	for _, name := range []string{"Content-Security-Policy", "Content-Security-Policy-Report-Only"} {
		for i, v := range h.Values(name) {
			h[http.CanonicalHeaderKey(name)][i] = scrubCSP(v, blockedStudioHosts)
		}
	}
}

// isStudioMCPPath reports whether a request to Studio's host is for Studio's own MCP route, /api/mcp.
// The route serves the remote MCP endpoint, which is api.<domain>/mcp behind the Management API's
// gate (serveMCP); the host Studio is served on has no gate, so the route is not reachable here. The
// match ignores case and any prefix, and sees through dot segments, repeated slashes and an encoded
// slash, so that no spelling of the path gets by.
func isStudioMCPPath(p string) bool {
	return strings.Contains(strings.ToLower(path.Clean("/"+p))+"/", "/api/mcp/")
}

// isStudioConsentPath reports whether p is a Studio page that approves access for someone else:
// /authorize, where a user approves what an OAuth client may do, and /cli/login, where they approve
// the Supabase CLI. A page that another site frames can be made to look like something else under a
// click, so these two are served with framing forbidden (denyFraming).
func isStudioConsentPath(p string) bool {
	switch strings.ToLower(path.Clean("/" + p)) {
	case "/authorize", "/cli/login":
		return true
	}
	return false
}

// denyFraming forbids another site to frame the response: X-Frame-Options DENY for old browsers and
// frame-ancestors 'none' in the Content-Security-Policy, whether Studio sent one or not. A policy that
// already has a frame-ancestors directive gets its sources replaced, since a second directive of the
// name is ignored by the browser.
func denyFraming(h http.Header) {
	const none = "frame-ancestors 'none'"
	h.Set("X-Frame-Options", "DENY")
	vals := h.Values("Content-Security-Policy")
	if len(vals) == 0 {
		h.Set("Content-Security-Policy", none)
		return
	}
	for i, v := range vals {
		h[http.CanonicalHeaderKey("Content-Security-Policy")][i] = withFrameAncestorsNone(v)
	}
}

// withFrameAncestorsNone returns csp with frame-ancestors 'none' in place of its frame-ancestors
// directive, or added at the end if it has none.
func withFrameAncestorsNone(csp string) string {
	const none = "frame-ancestors 'none'"
	found := false
	var kept []string
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		switch fields := strings.Fields(d); {
		case len(fields) == 0:
		case strings.EqualFold(fields[0], "frame-ancestors"):
			if !found {
				kept = append(kept, none)
			}
			found = true
		default:
			kept = append(kept, d)
		}
	}
	if !found {
		kept = append(kept, none)
	}
	return strings.Join(kept, "; ")
}
