package proxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"

	"github.com/supavise/supavise/internal/secrets"
)

// Bodies of the gateway's own error responses, identical to the self-hosted Envoy
// gateway (plain text): "Unauthorized" for a missing or invalid apikey,
// "RBAC: access denied" for a valid key without the needed role.
const (
	msgUnauthorized = "Unauthorized"
	msgForbidden    = "RBAC: access denied"
	msgInvalidKey   = "Invalid API key"
	msgConflict     = "Conflicting API keys"

	// msgRealtimeLongPollOff answers any non-WebSocket request on the Realtime route while the
	// project's legacy keys are disabled.
	msgRealtimeLongPollOff = "Realtime long-polling is unavailable while legacy API keys are disabled; use WebSocket"
)

// eqConst compares two secrets in constant time with respect to their content and
// length (both are hashed first). An empty expected value never matches.
func eqConst(got, want string) bool {
	if want == "" {
		return false
	}
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// classify decides whether apikey is a credential of the project and returns the
// role it stands for and the JWT to forward upstream.
//
//   - sb_publishable_* maps to the anon JWT and sb_secret_* to the service_role JWT.
//   - A legacy key is accepted when it equals the project's anon or service_role
//     key, or is any HS256 JWT signed with the project secret whose role is anon or
//     service_role and whose ref claim, if present, is this project (the claim is optional:
//     the project secret's signature already binds the token to the project).
func classify(k *secrets.ProjectKeys, ref, apikey string) (role, jwt string, ok bool) {
	if k == nil || apikey == "" {
		return "", "", false
	}
	if strings.HasPrefix(apikey, "sb_") {
		// Compare against every active key, without stopping at the first match, so timing
		// reveals neither which key nor which prefix matched. A revoked key is simply not in
		// the list: the key cache is dropped when its record changes.
		var pub, sec bool
		for _, ok := range k.OpaqueKeys(ref) {
			if !eqConst(apikey, ok.Key) {
				continue
			}
			switch ok.Type {
			case secrets.KeyTypePublishable:
				pub = true
			case secrets.KeyTypeSecret:
				sec = true
			}
		}
		switch {
		case pub && k.AnonKey != "":
			return secrets.RoleAnon, k.AnonKey, true
		case sec && k.ServiceRoleKey != "":
			return secrets.RoleServiceRole, k.ServiceRoleKey, true
		}
		return "", "", false
	}
	if k.LegacyDisabled {
		return temporaryKey(k, ref, apikey)
	}
	return legacyKey(k, ref, apikey)
}

// legacyKey reports whether apikey is a legacy credential of the project: it equals the
// anon or service_role key, or is an HS256 JWT signed with the project secret.
func legacyKey(k *secrets.ProjectKeys, ref, apikey string) (role, jwt string, ok bool) {
	anon, svc := eqConst(apikey, k.AnonKey), eqConst(apikey, k.ServiceRoleKey)
	switch {
	case anon:
		return secrets.RoleAnon, apikey, true
	case svc:
		return secrets.RoleServiceRole, apikey, true
	}
	if k.JWTSecret == "" || strings.Count(apikey, ".") != 2 {
		return "", "", false
	}
	claims, err := secrets.ParseHS256(apikey, k.JWTSecret)
	if err != nil {
		return "", "", false
	}
	if r, present := claims["ref"]; present {
		if s, isStr := r.(string); !isStr || s != ref {
			return "", "", false
		}
	}
	switch r, _ := claims["role"].(string); r {
	case secrets.RoleAnon, secrets.RoleServiceRole:
		return r, apikey, true
	}
	return "", "", false
}

// signingInput returns the "<header>.<payload>" part of a JWT-shaped token: the bytes the
// HMAC covers. ok is false for a token without a dot.
func signingInput(tok string) (string, bool) {
	i := strings.IndexByte(tok, '.')
	if i < 0 {
		return "", false
	}
	if j := strings.IndexByte(tok[i+1:], '.'); j >= 0 {
		return tok[:i+1+j], true
	}
	return tok, true
}

// isLegacyKeyToken reports whether tok is the project's anon or service_role key in any
// encoding the upstream verifiers accept: its header and payload segments (the signing input)
// equal those of the key. The signature segment is not compared. It can be re-encoded without
// changing the bytes it stands for (flipped unused low bits of its last character, "="
// padding), verifiers decode it leniently, and a signature that does not verify gets the token
// nowhere anyway. Comparing the signing input exactly is what a canonical comparison needs:
// the HMAC covers those segments as text, so any other spelling of them fails verification.
func isLegacyKeyToken(k *secrets.ProjectKeys, tok string) bool {
	if k == nil || tok == "" {
		return false
	}
	in, ok := signingInput(tok)
	if !ok {
		return false
	}
	var hit bool
	for _, key := range []string{k.AnonKey, k.ServiceRoleKey} {
		want, _ := signingInput(key)
		// No early exit, as in eqConst.
		if eqConst(in, want) {
			hit = true
		}
	}
	return hit
}

// disabledLegacyBearer reports whether bearer is the project's anon or service_role key while
// the legacy keys are disabled (in any signature encoding, see isLegacyKeyToken). Such a token
// is what a leaked legacy key looks like on the wire, and the switch exists to make it stop
// working. Other JWTs signed with the project's secret (users' sessions, which GoTrue signs
// with it) stay valid in Authorization until the JWT secret is rotated, as on hosted.
func disabledLegacyBearer(k *secrets.ProjectKeys, bearer string) bool {
	return k != nil && k.LegacyDisabled && bearer != "" && !strings.HasPrefix(bearer, "sb_") &&
		isLegacyKeyToken(k, bearer)
}

// refusedLegacy reports whether key is a legacy credential of the project (the anon or
// service_role key in any signature encoding, or a JWT signed with the project secret for one
// of those roles) that the disabled legacy switch turns away. The dashboard's temporary key
// is spared. Opaque sb_ keys are never legacy.
func refusedLegacy(k *secrets.ProjectKeys, ref, key string) bool {
	if k == nil || !k.LegacyDisabled || key == "" || strings.HasPrefix(key, "sb_") {
		return false
	}
	if isLegacyKeyToken(k, key) {
		return true
	}
	if _, _, tmp := temporaryKey(k, ref, key); tmp {
		return false
	}
	_, _, legacy := legacyKey(k, ref, key)
	return legacy
}

// temporaryKey accepts, while the legacy keys are disabled, the short-lived service_role
// JWT the Management API issues for the dashboard's own calls (api-keys/temporary): it
// is signed with the project secret and carries the secrets.TemporaryClaim.
func temporaryKey(k *secrets.ProjectKeys, ref, apikey string) (role, jwt string, ok bool) {
	if k.JWTSecret == "" || strings.Count(apikey, ".") != 2 {
		return "", "", false
	}
	claims, err := secrets.ParseHS256(apikey, k.JWTSecret)
	if err != nil {
		return "", "", false
	}
	if tmp, _ := claims[secrets.TemporaryClaim].(bool); !tmp {
		return "", "", false
	}
	if r, _ := claims["ref"].(string); r != ref {
		return "", "", false
	}
	if r, _ := claims["role"].(string); r == secrets.RoleServiceRole {
		return r, apikey, true
	}
	return "", "", false
}

// bearerToken returns the token of an "Authorization: Bearer <token>" value.
func bearerToken(authz string) string {
	authz = strings.TrimSpace(authz)
	if len(authz) > 7 && strings.EqualFold(authz[:7], "bearer ") {
		return strings.TrimSpace(authz[7:])
	}
	return ""
}

// authResult is the outcome of authorize: either an early response (status != 0)
// or the credential rewrites to apply to the upstream request.
type authResult struct {
	status int
	body   string
	// ctype is the Content-Type of a refusal; empty means text/plain.
	ctype string

	set      map[string]string // canonical header name -> value
	del      []string
	rawQuery string
}

func reject(status int, body string) authResult { return authResult{status: status, body: body} }

// authorize applies the route's access rule to the request credentials and
// computes the header and query rewrites. It follows the self-hosted Envoy
// gateway filter chain: apikey from header, else query (stripped from the
// forwarded URL), else an "Authorization: Bearer sb_..." value; validation of
// protected routes; translation of opaque keys to the project's JWTs; and an
// Authorization header synthesized from apikey unless the caller sent a real JWT.
func authorize(rt *route, k *secrets.ProjectKeys, ref string, h http.Header, rawQuery string) authResult {
	switch rt.keys {
	case keyNone:
		return authResult{rawQuery: rawQuery}
	case keyFunctions:
		return authorizeFunctions(k, ref, h, rawQuery)
	case keyS3:
		return authorizeS3(k, ref, h, rawQuery)
	}

	queryKey, query := stripAPIKey(rawQuery)
	bearer := bearerToken(h.Get("Authorization"))
	key := strings.TrimSpace(h.Get("apikey"))
	if key == "" {
		key = queryKey
	}
	if key == "" && strings.HasPrefix(bearer, "sb_") {
		key = bearer
	}
	// With the legacy keys disabled, a leaked one must stop working on every route that
	// forwards credentials, including those that do not require a key at the gateway (Storage
	// verifies the JWT itself): refuse it as a bearer, and as an apikey where classify below
	// would not already refuse it.
	if disabledLegacyBearer(k, bearer) {
		return reject(http.StatusUnauthorized, msgInvalidKey)
	}
	if rt.access != accessKey && rt.access != accessAdmin && refusedLegacy(k, ref, key) {
		return reject(http.StatusUnauthorized, msgInvalidKey)
	}
	role, jwt, valid := classify(k, ref, key)

	switch rt.access {
	case accessKey, accessAdmin:
		if !valid {
			return reject(http.StatusUnauthorized, msgUnauthorized)
		}
		if rt.access == accessAdmin && role != secrets.RoleServiceRole {
			return reject(http.StatusForbidden, msgForbidden)
		}
	}

	res := authResult{rawQuery: query, set: map[string]string{}}
	if rt.keys == keyRealtime {
		// A client-supplied x-api-key wins over apikey in Realtime, so it is always replaced.
		res.del = append(res.del, "X-Api-Key")
	}
	if !valid {
		return res
	}
	res.set["Apikey"] = jwt
	switch rt.keys {
	case keyRealtime:
		res.set["X-Api-Key"] = jwt
		// Realtime also reads apikey from the socket's query string; upstream rewrites it to the JWT.
		if query != "" {
			query += "&"
		}
		res.rawQuery = query + "apikey=" + url.QueryEscape(jwt)
	default:
		if bearer == "" || strings.HasPrefix(bearer, "sb_") {
			res.set["Authorization"] = "Bearer " + jwt
		}
	}
	return res
}

// authorizeFunctions mirrors the Envoy functions filter: a client-supplied
// sb-api-key is always dropped; an opaque sb_ key (from apikey, else from an
// Authorization bearer) is translated into sb-api-key; an unknown sb_ key or an
// apikey that disagrees with an sb_ bearer is rejected; everything else passes
// through to the runtime, which verifies JWTs itself.
func authorizeFunctions(k *secrets.ProjectKeys, ref string, h http.Header, rawQuery string) authResult {
	res := authResult{rawQuery: rawQuery, del: []string{"Sb-Api-Key"}}
	if k == nil || k.PublishableKey == "" || k.SecretKey == "" {
		return res
	}
	apikey := strings.TrimSpace(h.Get("apikey"))
	bearer := bearerToken(h.Get("Authorization"))
	if apikey == "" && bearer == "" {
		return res
	}
	if strings.HasPrefix(bearer, "sb_") && apikey != "" && apikey != bearer {
		return reject(http.StatusUnauthorized, msgConflict)
	}
	key := apikey
	if key == "" && strings.HasPrefix(bearer, "sb_") {
		key = bearer
	}
	if disabledLegacyBearer(k, bearer) {
		return reject(http.StatusUnauthorized, msgInvalidKey)
	}
	if !strings.HasPrefix(key, "sb_") {
		// The runtime verifies JWTs itself and would accept a legacy key: refuse it here.
		if refusedLegacy(k, ref, key) {
			return reject(http.StatusUnauthorized, msgInvalidKey)
		}
		return res
	}
	_, jwt, ok := classify(k, ref, key)
	if !ok {
		return reject(http.StatusUnauthorized, msgInvalidKey)
	}
	res.set = map[string]string{"Sb-Api-Key": jwt}
	return res
}

// S3 refusals use Storage's own error document (S3 clients parse it), not plain text.
const (
	s3AccessDeniedBody = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<Error><Code>AccessDenied</Code><Message>The legacy API keys are disabled for this project: ` +
		`use S3 access keys or a user session as the session token</Message></Error>`
	s3AccessDeniedPostBody = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<Error><Code>AccessDenied</Code><Message>Browser-based POST uploads are not available while the ` +
		`legacy API keys are disabled for this project</Message></Error>`
)

func rejectS3(body string) authResult {
	return authResult{status: http.StatusForbidden, body: body, ctype: "application/xml"}
}

// authorizeS3 guards Storage's S3 endpoint. The request carries an AWS SigV4 signature that must
// reach Storage untouched, so nothing is rewritten. Storage accepts, besides its own S3
// credentials, any JWT of the project sent as the S3 session token (the X-Amz-Security-Token
// header or presigned query parameter) and acts with that token's claims, so a leaked legacy
// key would work here as well: with the legacy keys disabled such a session token is refused.
// A session token that is a user's session, or no session token at all (Storage's S3
// credentials), passes. The token of a browser POST upload travels in a multipart form field
// the proxy does not parse, so those uploads are refused while the legacy keys are disabled.
func authorizeS3(k *secrets.ProjectKeys, ref string, h http.Header, rawQuery string) authResult {
	res := authResult{rawQuery: rawQuery}
	if k == nil || !k.LegacyDisabled {
		return res
	}
	if disabledLegacyBearer(k, bearerToken(h.Get("Authorization"))) {
		return rejectS3(s3AccessDeniedBody)
	}
	for _, tok := range h.Values("X-Amz-Security-Token") {
		if refusedLegacy(k, ref, strings.TrimSpace(tok)) {
			return rejectS3(s3AccessDeniedBody)
		}
	}
	for _, part := range strings.Split(rawQuery, "&") {
		name, val, _ := strings.Cut(part, "=")
		if n, err := url.QueryUnescape(name); err != nil || !strings.EqualFold(n, "X-Amz-Security-Token") {
			continue
		}
		if v, err := url.QueryUnescape(val); err == nil && refusedLegacy(k, ref, strings.TrimSpace(v)) {
			return rejectS3(s3AccessDeniedBody)
		}
	}
	if ct := h.Get("Content-Type"); strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "multipart/form-data") {
		return rejectS3(s3AccessDeniedPostBody)
	}
	return res
}
