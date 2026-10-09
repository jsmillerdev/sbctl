package oauth

import "strings"

// matchRedirectURI reports whether the redirect_uri of an authorization request matches one of the
// URIs the app registered. The rule (RFC 9700 section 4.1.3, RFC 8252 section 7.3):
//
//   - Exact string comparison, with no normalization: a different case, a trailing slash or an
//     added query is a different URI.
//   - The one exception is a registered loopback URI, http://localhost, http://127.0.0.1 or
//     http://[::1]: the request may name any port, because a native app takes a free port at run
//     time. Scheme, host, path and query must still be equal, and "localhost" is not "127.0.0.1".
//
// A request URI that breaks the registration rules (fragment, userinfo, a host that merely starts with
// "localhost", a custom scheme) matches nothing unless it is registered byte for byte, and no such URI
// can be registered.
func matchRedirectURI(registered []string, requested string) bool {
	if requested == "" {
		return false
	}
	for _, r := range registered {
		if r == requested {
			return true
		}
	}
	req, err := parseRedirectURI(requested)
	if err != nil || req.Scheme != "http" {
		return false
	}
	for _, r := range registered {
		reg, err := parseRedirectURI(r)
		if err != nil || reg.Scheme != "http" {
			continue
		}
		if strings.EqualFold(reg.Hostname(), req.Hostname()) &&
			reg.EscapedPath() == req.EscapedPath() && reg.RawQuery == req.RawQuery {
			return true
		}
	}
	return false
}
