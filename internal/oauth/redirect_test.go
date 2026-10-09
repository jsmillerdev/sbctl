package oauth

import (
	"net/url"
	"slices"
	"strings"
	"testing"
)

// TestRedirectMatch is the table of C5: exact comparison, a port-free match only for registered http
// loopback URIs, and nothing else.
func TestRedirectMatch(t *testing.T) {
	const (
		web   = "https://app.example.test/oauth/callback"
		v4    = "http://127.0.0.1/callback"
		v6    = "http://[::1]/callback"
		local = "http://localhost/callback"
		query = "http://127.0.0.1/callback?flow=a"
	)
	tests := []struct {
		name       string
		registered []string
		requested  string
		want       bool
	}{
		{"exact https", []string{web}, web, true},
		{"exact among several", []string{"https://other.example.test/cb", web}, web, true},
		{"empty request", []string{web}, "", false},
		{"nothing registered", nil, web, false},
		{"another path", []string{web}, "https://app.example.test/oauth/callback2", false},
		{"trailing slash", []string{web}, web + "/", false},
		{"upper-cased host", []string{web}, "https://APP.example.test/oauth/callback", false},
		{"upper-cased scheme", []string{web}, "HTTPS://app.example.test/oauth/callback", false},
		{"added query", []string{web}, web + "?x=1", false},
		{"added fragment", []string{web}, web + "#frag", false},
		{"added port on https", []string{web}, "https://app.example.test:8443/oauth/callback", false},
		{"https may not drop to http", []string{web}, "http://app.example.test/oauth/callback", false},
		{"userinfo", []string{web}, "https://user@app.example.test/oauth/callback", false},
		{"userinfo hides the host", []string{web}, "https://app.example.test@evil.example.test/oauth/callback", false},
		{"suffix lookalike", []string{web}, "https://app.example.test.evil.example.test/oauth/callback", false},
		{"backslash trick", []string{web}, `https://app.example.test\@evil.example.test/oauth/callback`, false},

		// Loopback: any port, nothing else.
		{"v4 with a port", []string{v4}, "http://127.0.0.1:53123/callback", true},
		{"v4 with another port", []string{v4}, "http://127.0.0.1:1/callback", true},
		{"v4 highest port", []string{v4}, "http://127.0.0.1:65535/callback", true},
		{"v4 without a port", []string{v4}, v4, true},
		{"v6 with a port", []string{v6}, "http://[::1]:8080/callback", true},
		{"localhost with a port", []string{local}, "http://localhost:3000/callback", true},
		{"registered port may differ", []string{"http://127.0.0.1:9000/callback"}, "http://127.0.0.1:9001/callback", true},
		{"registered port, request none", []string{"http://127.0.0.1:9000/callback"}, v4, true},
		{"localhost is not 127.0.0.1", []string{local}, "http://127.0.0.1:3000/callback", false},
		{"127.0.0.1 is not localhost", []string{v4}, "http://localhost:3000/callback", false},
		{"v6 is not v4", []string{v6}, "http://127.0.0.1:3000/callback", false},
		{"port 0", []string{v4}, "http://127.0.0.1:0/callback", false},
		{"port too large", []string{v4}, "http://127.0.0.1:65536/callback", false},
		{"port not a number", []string{v4}, "http://127.0.0.1:abc/callback", false},
		{"loopback other path", []string{v4}, "http://127.0.0.1:3000/other", false},
		{"loopback path prefix", []string{v4}, "http://127.0.0.1:3000/callback/x", false},
		{"loopback path case", []string{v4}, "http://127.0.0.1:3000/Callback", false},
		{"loopback trailing slash", []string{v4}, "http://127.0.0.1:3000/callback/", false},
		{"loopback query kept", []string{query}, "http://127.0.0.1:3000/callback?flow=a", true},
		{"loopback query differs", []string{query}, "http://127.0.0.1:3000/callback?flow=b", false},
		{"loopback query added", []string{v4}, "http://127.0.0.1:3000/callback?flow=a", false},
		{"loopback query dropped", []string{query}, "http://127.0.0.1:3000/callback", false},
		{"loopback fragment", []string{v4}, "http://127.0.0.1:3000/callback#x", false},
		{"loopback userinfo", []string{v4}, "http://user@127.0.0.1:3000/callback", false},
		{"loopback to https", []string{v4}, "https://127.0.0.1:3000/callback", false},
		{"loopback with a different host", []string{v4}, "http://127.0.0.2:3000/callback", false},
		{"localhost lookalike suffix", []string{local}, "http://localhost.evil.example.test:3000/callback", false},
		{"localhost lookalike prefix", []string{local}, "http://evillocalhost:3000/callback", false},
		{"localhost userinfo trick", []string{local}, "http://localhost:3000@evil.example.test/callback", false},
		{"localhost dotted", []string{local}, "http://localhost.:3000/callback", false},
		{"127.0.0.1 lookalike", []string{v4}, "http://127.0.0.1.evil.example.test:3000/callback", false},
		{"decimal loopback", []string{v4}, "http://2130706433:3000/callback", false},
		{"percent-encoded dot", []string{local}, "http://localhost%2eevil.example.test/callback", false},
		{"space in the request", []string{v4}, "http://127.0.0.1:3000/call back", false},
		{"newline in the request", []string{v4}, "http://127.0.0.1:3000/callback\n", false},
		{"https registered with a port does not float", []string{"https://app.example.test:8443/cb"}, "https://app.example.test:9443/cb", false},
		{"a registered https URI is not loopback", []string{"https://localhost/callback"}, "https://localhost:3000/callback", false},
		{"custom scheme", []string{"cursor://anysphere.cursor-retrieval/oauth"}, "cursor://anysphere.cursor-retrieval/oauth", true}, // exact: only a registered string matches
		{"custom scheme, not registered", []string{web}, "cursor://anysphere.cursor-retrieval/oauth", false},
		{"javascript", []string{web}, "javascript:alert(1)", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchRedirectURI(tc.registered, tc.requested); got != tc.want {
				t.Errorf("matchRedirectURI(%q, %q) = %v, want %v", tc.registered, tc.requested, got, tc.want)
			}
		})
	}
}

// FuzzRedirectMatch checks the invariants that make a match safe, whatever the strings: a request
// matches only when it is a registered string or differs from a registered http loopback URI in its
// port alone.
func FuzzRedirectMatch(f *testing.F) {
	for _, s := range [][3]string{
		{"https://app.example.test/cb", "http://127.0.0.1/cb", "http://127.0.0.1:8080/cb"},
		{"http://localhost/cb", "http://[::1]/cb", "http://localhost:99999/cb"},
		{"https://a.example.test/", "http://127.0.0.1/cb?x=1", "http://127.0.0.1:1/cb?x=1"},
		{"http://127.0.0.1/cb", "http://localhost/cb", "http://localhost.evil.test:1/cb"},
		{"http://127.0.0.1/cb", "https://app.example.test/cb", "http://127.0.0.1:1@evil.test/cb"},
		{"http://127.0.0.1/cb", "http://localhost/cb", "http://127.0.0.1:1/cb#frag"},
		{"http://127.0.0.1/cb", "x", "http://127.0.0.1:1/\x00cb"},
	} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, reg1, reg2, requested string) {
		// Only URIs that registration accepts can be stored.
		var registered []string
		for _, r := range []string{reg1, reg2} {
			if _, err := parseRedirectURI(r); err == nil {
				registered = append(registered, r)
			}
		}
		if !matchRedirectURI(registered, requested) {
			return
		}
		if slices.Contains(registered, requested) {
			return
		}
		// Not an exact match, so it must be the port-free loopback case.
		req, err := parseRedirectURI(requested)
		if err != nil {
			t.Fatalf("%q matched %q though it breaks the registration rules: %v", requested, registered, err)
		}
		if req.Scheme != "http" || req.User != nil || req.Fragment != "" || strings.Contains(requested, "#") {
			t.Fatalf("%q matched %q: scheme %q, userinfo %v, fragment %q", requested, registered, req.Scheme, req.User, req.Fragment)
		}
		if !loopbackHosts[strings.ToLower(req.Hostname())] {
			t.Fatalf("%q matched %q but its host is not a loopback name", requested, registered)
		}
		for _, r := range registered {
			reg, _ := url.Parse(r)
			if reg.Scheme == "http" && strings.EqualFold(reg.Hostname(), req.Hostname()) &&
				reg.EscapedPath() == req.EscapedPath() && reg.RawQuery == req.RawQuery {
				return
			}
		}
		t.Fatalf("%q matched %q though no registered URI differs only in its port", requested, registered)
	})
}
