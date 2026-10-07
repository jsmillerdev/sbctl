package proxy

import (
	"net/http"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/notice"
)

// Studio is served unmodified (three patches, no fixups), so the two things its build
// gets wrong for a self-hosted node are corrected here, on the way through.

// Studio's own /api/incident-banner route asks incident.io, answers 500 without a key, and
// react-query retries it after 1, 4 and 16 seconds while the sign-in form awaits the query
// cache reset: sign-in took 22 seconds (docs/research/08 section 9). The proxy answers the route
// itself, instantly and with no outbound call. With nothing to announce the answer is
// {"incidents":[]}; while the operator has a maintenance window announced
// (`supavise maintenance announce`) or an upgrade is running (<state_dir>/system/upgrade.json)
// the answer lists it, as an incident that Studio shows to every signed-in user (internal/notice).
//
// Studio's banner code takes no text from this answer: it shows its own fixed wording
// ("We are investigating a technical issue") with a link to the status page. The announcement's
// message travels in the answer for other readers, and in `supavise status` for the operator.

// answerStudioLocally serves the routes supavise answers instead of Studio. It reports
// whether it wrote a response.
func (s *Server) answerStudioLocally(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/api/incident-banner" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	now := time.Now
	if s.noticeNow != nil {
		now = s.noticeNow
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(notice.BannerJSON(s.cfg.Paths(), now()))
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
