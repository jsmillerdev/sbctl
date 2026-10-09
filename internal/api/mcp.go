package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// The remote MCP endpoint, api.<domain>/mcp, is Studio's bundled MCP server behind the edge. The proxy
// asks MCPGate about every request to that path and forwards the ones it lets through to Studio's
// loopback /api/mcp. The gate sits at the edge and not in the API mux so that the loopback admin
// listener, which serves the same handler, never answers /mcp, and so that the proxy's forward()
// (streaming, X-Forwarded-* of its own) is the only way in.
//
// The gate is the only check that sees a request whose tools make no Management API call
// (initialize, tools/list): without it a revoked token would never get its 401. It does not decide
// what a token may do. Studio's server calls the Management API with the same bearer, and that
// authenticates and authorizes every call (organization binding, scopes, the member's role).

const (
	// mcpMaxBody bounds a request body: deploying an Edge Function sends the files, and Studio's own
	// route takes 8 MB.
	mcpMaxBody = 8 << 20
	// mcpRatePerMinute and mcpMaxInFlight are the limits of one grant or personal access token on this
	// node.
	mcpRatePerMinute = 600
	mcpMaxInFlight   = 8
	mcpRateWindow    = time.Minute
	// mcpMaxCallers bounds the limiter's memory: the number of principals with a live window.
	mcpMaxCallers = 10000
	// mcpMaxParam is the longest value of a query parameter the gate passes on.
	mcpMaxParam = 1024
)

// What the gate says. The first is the answer hosted gives a request without a token.
const (
	mcpMsgNoToken       = "No access token provided"
	mcpMsgBadToken      = "The access token is invalid, expired or revoked"
	mcpMsgOtherResource = "The access token was issued for another resource"
)

// mcpAllow lists the methods of the endpoint. Studio's route answers POST only (it is stateless: no
// session to resume with GET, none to end with DELETE), but a client may try the others.
const mcpAllow = "GET, POST, DELETE, OPTIONS"

// mcpQueryKeys are the query parameters that reach Studio: the ones its route reads. Anything else a
// client sends, an access_token included, is dropped.
var mcpQueryKeys = [...]string{"project_ref", "read_only", "features", "skip_elicitations"}

// mcpOAuthTokenRe is the shape of an OAuth access token: "sbp_oauth_" and 40 hex digits.
var mcpOAuthTokenRe = regexp.MustCompile(`^sbp_oauth_[a-f0-9]{40}$`)

// MCPGate is proxy.Options.MCPGate: the gate in front of the remote MCP endpoint (api.<domain>/mcp)
// that the proxy asks before it forwards the request to Studio. It answers OPTIONS and every refusal
// itself and returns the query to forward. ok false means the answer has been written and nothing
// is forwarded.
//
// In order: [api] disable_oauth answers 404. A method the endpoint does not know answers 405, and
// OPTIONS answers the open CORS preflight. The bearer comes from Authorization only; none is 401
// with the discovery challenge, and one that is not a live OAuth access token or personal access
// token (a dashboard session included), or whose grant is bound to another resource, is 401
// invalid_token, which is the signal for a client to refresh. The grant or token then passes its
// limits (429 with Retry-After). The query is rebuilt from the four parameters Studio reads, and the
// body is capped.
//
// The request is "in flight" until the proxy has served it, that is, until the server cancels the
// request context after ServeHTTP returns.
func (s *Server) MCPGate(w http.ResponseWriter, r *http.Request) (rawQuery string, ok bool) {
	if s.oauthDisabled() {
		writeError(w, errf(http.StatusNotFound, "Not Found"))
		return "", false
	}
	// The open CORS policy of the OAuth endpoints (any origin, no credentials), so that browser clients
	// can read the refusals, WWW-Authenticate included. The proxy strips Studio's own Access-Control
	// headers.
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "WWW-Authenticate, Mcp-Session-Id")
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodDelete:
	case http.MethodOptions:
		h.Set("Access-Control-Allow-Methods", mcpAllow)
		h.Set("Access-Control-Allow-Headers", "authorization, content-type, mcp-protocol-version, mcp-session-id, last-event-id")
		w.WriteHeader(http.StatusNoContent)
		return "", false
	default:
		h.Set("Allow", mcpAllow)
		writeError(w, errf(http.StatusMethodNotAllowed, "Method Not Allowed"))
		return "", false
	}

	token := mcpBearer(r)
	if token == "" {
		s.mcpUnauthorized(w, "")
		return "", false
	}
	// Two credentials are not one request: the gate would check the first and Studio might read the other.
	if len(r.Header.Values("Authorization")) > 1 {
		s.mcpUnauthorized(w, mcpMsgBadToken)
		return "", false
	}
	caller, desc, err := s.mcpAuthenticate(r.Context(), token)
	switch {
	case err != nil:
		s.log.Error("MCP gate: could not check the access token", "error", err)
		writeError(w, asError(err))
		return "", false
	case caller == nil:
		s.mcpUnauthorized(w, desc)
		return "", false
	}

	release, wait, admitted := s.mcpLimiter().acquire(caller.key, s.now())
	if !admitted {
		h.Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		writeError(w, errf(http.StatusTooManyRequests, "Too many requests"))
		return "", false
	}
	query, qerr := mcpQuery(r.URL.RawQuery)
	if qerr == nil && r.ContentLength > mcpMaxBody {
		qerr = errf(http.StatusRequestEntityTooLarge, "request body too large")
	}
	if qerr != nil {
		release()
		writeError(w, qerr)
		return "", false
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, mcpMaxBody)
	}
	// The proxy forwards the request after this returns and has nothing to call when it is done, so the
	// slot is given back when the server cancels the request context.
	context.AfterFunc(r.Context(), release)
	return query, true
}

// mcpBearer returns the token of an "Authorization: Bearer" header, or "" if there is none.
func mcpBearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// mcpUnauthorized answers 401 with the challenge of RFC 9728. desc "" is a request with no token: the
// challenge only points at the protected resource metadata, as hosted's does. Otherwise the token was
// presented and refused, and error="invalid_token" tells the client to refresh or sign in again.
func (s *Server) mcpUnauthorized(w http.ResponseWriter, desc string) {
	meta := oauth.ProtectedResourceMetadataURL(s.cfg.APIURL())
	msg := mcpMsgNoToken
	challenge := `Bearer resource_metadata="` + meta + `"`
	if desc != "" {
		msg = desc
		challenge = `Bearer error="invalid_token", error_description="` + desc + `", resource_metadata="` + meta + `"`
	}
	w.Header().Set("WWW-Authenticate", challenge)
	writeError(w, errf(http.StatusUnauthorized, "%s", msg))
}

// mcpCaller is who a request to /mcp comes from, as far as the limits go.
type mcpCaller struct {
	// key names the grant or the personal access token.
	key string
}

// mcpAuthenticate checks the bearer of a request to /mcp. The result is the caller, or a description
// of why the token is refused (the answer is 401 invalid_token), or an error when it could not be
// checked (the answer is 5xx: the gate fails closed).
//
// Only OAuth access tokens and personal access tokens are accepted (hosted takes a personal access
// token as a bearer too, so clients configured with a header work). A dashboard session is not, and
// it is refused without being parsed.
//
// It applies to a token what authenticate applies to it, without its writes. The gate also runs on a
// follower, whose registry is read-only, and the Management API touches last_used_at on the calls
// that follow.
func (s *Server) mcpAuthenticate(ctx context.Context, token string) (c *mcpCaller, desc string, err error) {
	switch {
	case strings.HasPrefix(token, "sbp_oauth_"):
		return s.mcpOAuthCaller(ctx, token)
	case strings.HasPrefix(token, secrets.PrefixPAT):
		return s.mcpPATCaller(ctx, token)
	}
	return nil, mcpMsgBadToken, nil
}

func (s *Server) mcpOAuthCaller(ctx context.Context, token string) (*mcpCaller, string, error) {
	if !mcpOAuthTokenRe.MatchString(token) {
		return nil, mcpMsgBadToken, nil
	}
	info, err := s.oauth.LookupAccess(ctx, token)
	switch {
	case errors.Is(err, oauth.ErrNotFound), err == nil && info == nil:
		return nil, mcpMsgBadToken, nil
	case err != nil:
		return nil, "", err
	case !info.ExpiresAt.IsZero() && !info.ExpiresAt.After(s.now()):
		return nil, mcpMsgBadToken, nil
	}
	// The user checks of every principal: not removed, still admitted by SSO, still a member of the
	// organization. A member who left takes the grant with them.
	switch err := s.oauthAdmit(ctx, info.UserID, info.OrgID); {
	case errors.Is(err, oauth.ErrNotAdmitted):
		return nil, mcpMsgBadToken, nil
	case errors.Is(err, oauth.ErrNotMember):
		if rerr := s.oauth.RevokeGrant(ctx, info.GrantID, oauth.ReasonMembership, oauth.ActorSystem); rerr != nil && !errors.Is(rerr, oauth.ErrNotFound) {
			s.log.Warn("MCP gate: could not revoke the grant of a user who left the organization", "grant", info.GrantID, "error", rerr)
		}
		return nil, mcpMsgBadToken, nil
	case err != nil:
		return nil, "", err
	}
	// A grant bound to a resource is good for that resource only (RFC 8707). The grant of a client that
	// sent none is not bound.
	if info.Resource != "" && info.Resource != oauth.ResourceURL(s.cfg.APIURL()) {
		return nil, mcpMsgOtherResource, nil
	}
	return &mcpCaller{key: "grant:" + strconv.FormatInt(info.GrantID, 10)}, "", nil
}

func (s *Server) mcpPATCaller(ctx context.Context, token string) (*mcpCaller, string, error) {
	if !patRe.MatchString(token) {
		return nil, mcpMsgBadToken, nil
	}
	t, err := s.reg.GetAccessTokenByHash(ctx, secrets.HashToken(token))
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return nil, mcpMsgBadToken, nil
	case err != nil:
		return nil, "", err
	case t.ExpiresAt != nil && !t.ExpiresAt.After(s.now()):
		return nil, mcpMsgBadToken, nil
	}
	if gone, err := s.auth.isRemoved(ctx, t.UserID); err != nil {
		return nil, "", err
	} else if gone {
		return nil, mcpMsgBadToken, nil
	}
	if s.auth.ssoUser != nil {
		if err := s.auth.ssoUser(ctx, t.UserID); err != nil {
			if e := asError(err); e.Status < http.StatusInternalServerError {
				return nil, mcpMsgBadToken, nil
			}
			return nil, "", err
		}
	}
	return &mcpCaller{key: "token:" + strconv.FormatInt(t.ID, 10)}, "", nil
}

// mcpQuery rebuilds the query to forward from the parameters Studio's route reads, each with one
// value. A parameter given twice is refused: whichever value the gate kept, the client may have meant
// the other (read_only=false&read_only=true). Studio validates the values and fails closed.
func mcpQuery(raw string) (string, *Error) {
	if raw == "" {
		return "", nil
	}
	in, err := url.ParseQuery(raw)
	if err != nil {
		return "", errf(http.StatusBadRequest, "Invalid query string")
	}
	out := url.Values{}
	for _, key := range mcpQueryKeys {
		v := in[key]
		switch {
		case len(v) == 0:
			continue
		case len(v) > 1:
			return "", errf(http.StatusBadRequest, "Query parameter %s was given more than once", key)
		case len(v[0]) > mcpMaxParam:
			return "", errf(http.StatusBadRequest, "Query parameter %s is too long", key)
		}
		out.Set(key, v[0])
	}
	return out.Encode(), nil
}

// ---- limits ----------------------------------------------------------------------------------

// mcpLimiter returns the server's limiter (Server.mcp).
func (s *Server) mcpLimiter() *mcpLimiter { return &s.mcp }

// mcpLimiter bounds each grant and personal access token to mcpRatePerMinute requests per minute and
// mcpMaxInFlight requests at a time, in memory on this node (a cluster has the limits per node, as
// the other limiters do).
type mcpLimiter struct {
	mu      sync.Mutex
	callers map[string]*mcpWindow
}

type mcpWindow struct {
	start    time.Time
	n        int // requests admitted since start
	inFlight int
}

// acquire admits one request of key at now, or says how long to wait. The caller must call release,
// once, when the request is done. A request that is refused counts for nothing.
func (l *mcpLimiter) acquire(key string, now time.Time) (release func(), wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.callers[key]
	if w == nil {
		if l.callers == nil {
			l.callers = map[string]*mcpWindow{}
		}
		if len(l.callers) >= mcpMaxCallers {
			for k, o := range l.callers {
				if o.inFlight == 0 && now.Sub(o.start) >= mcpRateWindow {
					delete(l.callers, k)
				}
			}
		}
		w = &mcpWindow{start: now}
		if len(l.callers) < mcpMaxCallers {
			l.callers[key] = w
		} // else every slot is a caller of the last minute: this one is admitted and not counted
	}
	if now.Sub(w.start) >= mcpRateWindow {
		w.start, w.n = now, 0
	}
	switch {
	case w.inFlight >= mcpMaxInFlight:
		return nil, time.Second, false
	case w.n >= mcpRatePerMinute:
		return nil, w.start.Add(mcpRateWindow).Sub(now), false
	}
	w.n++
	w.inFlight++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			w.inFlight--
			l.mu.Unlock()
		})
	}, 0, true
}
