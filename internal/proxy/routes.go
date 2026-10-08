package proxy

import (
	"errors"
	"net/url"
	"path"
	"strings"
	"time"
)

// service names the upstream a route forwards to.
type service int

const (
	svcRest service = iota + 1
	svcAuth
	svcRealtime
	svcStorage
	svcFunctions
	svcStudio
)

func (s service) String() string {
	switch s {
	case svcRest:
		return "postgrest"
	case svcAuth:
		return "gotrue"
	case svcRealtime:
		return "realtime"
	case svcStorage:
		return "storage"
	case svcFunctions:
		return "edge-runtime"
	case svcStudio:
		return "studio"
	}
	return "unknown"
}

// access is what a route demands of the caller before anything is forwarded.
type access int

const (
	// accessOpen forwards without checking the apikey (GoTrue verify and callback,
	// Storage, S3). A valid key is still translated when one is present.
	accessOpen access = iota
	// accessKey needs a valid anon or service_role credential (401 otherwise).
	accessKey
	// accessAdmin needs a service_role credential (401 without a key, 403 for anon).
	accessAdmin
	// accessBlocked is never forwarded (403).
	accessBlocked
)

// keyMode selects how credentials are rewritten for the upstream.
type keyMode int

const (
	// keyBearer replaces apikey with the project JWT and fills Authorization when
	// the caller did not send a real JWT (PostgREST, GoTrue, Storage, Realtime REST).
	keyBearer keyMode = iota
	// keyRealtime is keyBearer for the Realtime socket: it also sets x-api-key and
	// rewrites the apikey query parameter, which is where Realtime reads the token.
	keyRealtime
	// keyFunctions puts the translated JWT in a raw sb-api-key header and leaves
	// apikey, Authorization and the query string alone.
	keyFunctions
	// keyNone forwards credentials untouched.
	keyNone
	// keyS3 forwards credentials untouched (S3 requests carry an AWS SigV4 Authorization header
	// that must not be rewritten) but refuses a legacy key sent as the S3 session token while
	// the project's legacy keys are disabled.
	keyS3
)

// route is one entry of the project API route table. The table mirrors the
// self-hosted gateway at the pinned upstream (docker/volumes/api/envoy/lds.template.yaml
// and kong.yml) and the dockerless stack proxy (supabase/cli packages/stack).
type route struct {
	name string
	// prefix is the client path prefix, matched on segment boundaries.
	prefix string
	// exact limits the route to prefix itself (with or without a trailing slash).
	exact bool
	svc   service
	// upstream replaces prefix in the forwarded path. Empty strips the prefix.
	upstream string
	access   access
	keys     keyMode
	// fwdPrefix is sent as X-Forwarded-Prefix, as the upstream gateway does.
	fwdPrefix string
	// setHeaders are forced onto the upstream request.
	setHeaders [][2]string
	// timeout bounds the wait for upstream response headers.
	timeout time.Duration
	// balance lets a GET or HEAD on a project's load balancer host go to a replica; every other
	// request there goes to the primary like on the project's own host.
	balance bool
}

const (
	defaultTimeout   = 30 * time.Second
	storageTimeout   = 120 * time.Second
	functionsTimeout = 410 * time.Second // upstream: the runtime's 400 s wall clock expires first
)

// projectRoutes is the ordered route table; the first match wins.
var projectRoutes = []route{
	// Open GoTrue routes: browsers and identity providers call these without an apikey.
	{name: "auth-v1-verify", prefix: "/auth/v1/verify", svc: svcAuth, upstream: "/verify", access: accessOpen, fwdPrefix: "/auth/v1/verify", timeout: defaultTimeout},
	{name: "auth-v1-callback", prefix: "/auth/v1/callback", svc: svcAuth, upstream: "/callback", access: accessOpen, fwdPrefix: "/auth/v1/callback", timeout: defaultTimeout},
	{name: "auth-v1-authorize", prefix: "/auth/v1/authorize", svc: svcAuth, upstream: "/authorize", access: accessOpen, fwdPrefix: "/auth/v1/authorize", timeout: defaultTimeout},
	{name: "auth-v1-jwks", prefix: "/auth/v1/.well-known/jwks.json", svc: svcAuth, upstream: "/.well-known/jwks.json", access: accessOpen, fwdPrefix: "/auth/v1/.well-known/jwks.json", timeout: defaultTimeout},
	{name: "auth-v1-sso-acs", prefix: "/auth/v1/sso/saml/acs", svc: svcAuth, upstream: "/sso/saml/acs", access: accessOpen, fwdPrefix: "/auth/v1/sso/saml/acs", timeout: defaultTimeout},
	{name: "auth-v1-sso-metadata", prefix: "/auth/v1/sso/saml/metadata", svc: svcAuth, upstream: "/sso/saml/metadata", access: accessOpen, fwdPrefix: "/auth/v1/sso/saml/metadata", timeout: defaultTimeout},
	{name: "well-known-oauth", prefix: "/.well-known/oauth-authorization-server", svc: svcAuth, upstream: "/.well-known/oauth-authorization-server", access: accessOpen, fwdPrefix: "/.well-known/oauth-authorization-server", timeout: defaultTimeout},

	// Edge Functions: no key check, the runtime verifies JWTs itself.
	{name: "functions-v1", prefix: "/functions/v1", svc: svcFunctions, access: accessOpen, keys: keyFunctions, fwdPrefix: "/functions/v1/", timeout: functionsTimeout},

	// Storage: no key check (public objects, signed URLs, S3 SigV4).
	{name: "storage-v1-s3", prefix: "/storage/v1/s3", svc: svcStorage, upstream: "/s3", access: accessOpen, keys: keyS3, fwdPrefix: "/storage/v1", timeout: storageTimeout},
	{name: "storage-v1", prefix: "/storage/v1", svc: svcStorage, access: accessOpen, keys: keyBearer, fwdPrefix: "/storage/v1", timeout: storageTimeout},

	// Protected GoTrue and PostgREST.
	{name: "auth-v1", prefix: "/auth/v1", svc: svcAuth, access: accessKey, keys: keyBearer, fwdPrefix: "/auth/v1/", timeout: defaultTimeout},
	// The PostgREST OpenAPI document at the root is admin only (supabase discussion 42949).
	{name: "rest-v1-openapi", prefix: "/rest/v1", exact: true, svc: svcRest, access: accessAdmin, keys: keyBearer, fwdPrefix: "/rest/v1/", timeout: defaultTimeout, balance: true},
	{name: "rest-v1", prefix: "/rest/v1", svc: svcRest, access: accessKey, keys: keyBearer, fwdPrefix: "/rest/v1/", timeout: defaultTimeout, balance: true},
	{name: "graphql-v1", prefix: "/graphql/v1", svc: svcRest, upstream: "/rpc/graphql", access: accessKey, keys: keyBearer, fwdPrefix: "/graphql/v1",
		// Forced, not added when absent: a client-chosen profile would call graphql() in another schema.
		setHeaders: [][2]string{{"Content-Profile", "graphql_public"}}, timeout: defaultTimeout},

	// Realtime: the tenant-admin and OpenAPI endpoints are never public.
	{name: "realtime-v1-api-openapi", prefix: "/realtime/v1/api/openapi", svc: svcRealtime, access: accessBlocked, timeout: defaultTimeout},
	{name: "realtime-v1-api-tenants", prefix: "/realtime/v1/api/tenants", svc: svcRealtime, access: accessBlocked, timeout: defaultTimeout},
	{name: "realtime-v1-api", prefix: "/realtime/v1/api", svc: svcRealtime, upstream: "/api", access: accessKey, keys: keyBearer, fwdPrefix: "/realtime/v1/api", timeout: defaultTimeout},
	{name: "realtime-v1-ws", prefix: "/realtime/v1", svc: svcRealtime, upstream: "/socket", access: accessKey, keys: keyRealtime, fwdPrefix: "/realtime/v1/", timeout: defaultTimeout},
}

// matchRoute returns the first route whose prefix matches the cleaned path.
func matchRoute(p string) *route {
	for i := range projectRoutes {
		rt := &projectRoutes[i]
		if p == rt.prefix || (!rt.exact && strings.HasPrefix(p, rt.prefix+"/")) {
			return rt
		}
	}
	return nil
}

// upstreamPath maps a cleaned client path onto the upstream path.
func (rt *route) upstreamPath(p string, trailingSlash bool) string {
	suffix := strings.TrimPrefix(p, rt.prefix)
	if trailingSlash && !strings.HasSuffix(suffix, "/") {
		suffix += "/"
	}
	if rt.upstream == "" {
		if suffix == "" {
			return "/"
		}
		return suffix
	}
	return rt.upstream + suffix
}

// escapedUpstreamPath is the upstream path in the client's own escaping (for
// example %2B stays %2B and a raw "(" stays raw). cleanPath already validated u; the
// escaped form is used only when cleaning it gives the same path and route as the
// decoded form, otherwise "" makes the proxy re-escape the decoded path.
func escapedUpstreamPath(u *url.URL, decoded string, rt *route, trailing bool) string {
	clean := path.Clean(u.EscapedPath())
	if dec, err := url.PathUnescape(clean); err != nil || dec != decoded || matchRoute(clean) != rt {
		return ""
	}
	return rt.upstreamPath(clean, trailing)
}

var errBadPath = errors.New("proxy: malformed request path")

// cleanPath normalizes a request path the way the upstream gateway does
// (normalize_path, merge_slashes, REJECT_REQUEST on escaped slashes) so route
// matching cannot be bypassed with "..", "//" or "%2F". It returns the decoded
// path without a trailing slash and whether the original had one.
func cleanPath(u *url.URL) (string, bool, error) {
	esc := strings.ToLower(u.EscapedPath())
	if strings.Contains(esc, "%2f") || strings.Contains(esc, "%5c") {
		return "", false, errBadPath
	}
	p := u.Path
	if p == "" || p[0] != '/' || strings.ContainsRune(p, 0) {
		return "", false, errBadPath
	}
	trailing := len(p) > 1 && strings.HasSuffix(p, "/")
	return path.Clean(p), trailing, nil
}

// stripAPIKey removes every apikey parameter from a raw query string, leaving the
// other parameters byte for byte (PostgREST filters depend on their encoding). It
// returns the first non-empty apikey value.
func stripAPIKey(raw string) (value, rest string) {
	if raw == "" {
		return "", ""
	}
	parts := strings.Split(raw, "&")
	kept := parts[:0]
	for _, part := range parts {
		name, val, _ := strings.Cut(part, "=")
		if n, err := url.QueryUnescape(name); err == nil && n == "apikey" {
			if value == "" {
				if v, err := url.QueryUnescape(val); err == nil {
					value = v
				}
			}
			continue
		}
		if part != "" {
			kept = append(kept, part)
		}
	}
	return value, strings.Join(kept, "&")
}
