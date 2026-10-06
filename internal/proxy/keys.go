package proxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"

	"github.com/OWNER/sbctl/internal/secrets"
)

// Bodies of the gateway's own error responses, identical to the self-hosted Envoy
// gateway (plain text): "Unauthorized" for a missing or invalid apikey,
// "RBAC: access denied" for a valid key without the needed role.
const (
	msgUnauthorized = "Unauthorized"
	msgForbidden    = "RBAC: access denied"
	msgInvalidKey   = "Invalid API key"
	msgConflict     = "Conflicting API keys"
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
//     service_role and whose ref claim, if present, is this project.
func classify(k *secrets.ProjectKeys, ref, apikey string) (role, jwt string, ok bool) {
	if k == nil || apikey == "" {
		return "", "", false
	}
	if strings.HasPrefix(apikey, "sb_") {
		// Evaluate both comparisons so timing does not reveal which prefix matched.
		pub, sec := eqConst(apikey, k.PublishableKey), eqConst(apikey, k.SecretKey)
		switch {
		case pub && k.AnonKey != "":
			return secrets.RoleAnon, k.AnonKey, true
		case sec && k.ServiceRoleKey != "":
			return secrets.RoleServiceRole, k.ServiceRoleKey, true
		}
		return "", "", false
	}
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
	if !strings.HasPrefix(key, "sb_") {
		return res
	}
	_, jwt, ok := classify(k, ref, key)
	if !ok {
		return reject(http.StatusUnauthorized, msgInvalidKey)
	}
	res.set = map[string]string{"Sb-Api-Key": jwt}
	return res
}
