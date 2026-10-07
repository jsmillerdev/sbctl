package proxy

import (
	"net/url"
	"testing"
)

func TestCleanPath(t *testing.T) {
	cases := []struct {
		in       string
		want     string
		trailing bool
		bad      bool
	}{
		{in: "/rest/v1/todos", want: "/rest/v1/todos"},
		{in: "/rest/v1/", want: "/rest/v1", trailing: true},
		{in: "/rest/v1//todos", want: "/rest/v1/todos"},
		{in: "/rest/v1/../../auth/v1/user", want: "/auth/v1/user"},
		{in: "/realtime/v1/api/./tenants", want: "/realtime/v1/api/tenants"},
		{in: "/realtime/v1/api/x/../tenants/abc", want: "/realtime/v1/api/tenants/abc"},
		{in: "/", want: "/"},
		{in: "/rest/v1/a%20b", want: "/rest/v1/a b"},
		{in: "/rest/v1/a%2Fb", bad: true},
		{in: "/rest/v1/a%2fb", bad: true},
		{in: "/rest/v1/a%5Cb", bad: true},
		{in: "/rest/v1/a%00b", bad: true},
	}
	for _, c := range cases {
		u, err := url.ParseRequestURI(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		got, trailing, err := cleanPath(u)
		if c.bad {
			if err == nil {
				t.Errorf("%q: want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want || trailing != c.trailing {
			t.Errorf("%q: got (%q, %v, %v), want (%q, %v)", c.in, got, trailing, err, c.want, c.trailing)
		}
	}
}

func TestMatchAndRewrite(t *testing.T) {
	cases := []struct {
		path     string
		trailing bool
		route    string
		upstream string
	}{
		{"/auth/v1/verify", false, "auth-v1-verify", "/verify"},
		{"/auth/v1/callback", false, "auth-v1-callback", "/callback"},
		{"/auth/v1/authorize", false, "auth-v1-authorize", "/authorize"},
		{"/auth/v1/.well-known/jwks.json", false, "auth-v1-jwks", "/.well-known/jwks.json"},
		{"/auth/v1/sso/saml/acs", false, "auth-v1-sso-acs", "/sso/saml/acs"},
		{"/auth/v1/sso/saml/metadata", false, "auth-v1-sso-metadata", "/sso/saml/metadata"},
		{"/.well-known/oauth-authorization-server", false, "well-known-oauth", "/.well-known/oauth-authorization-server"},
		{"/auth/v1/token", false, "auth-v1", "/token"},
		{"/auth/v1/user", false, "auth-v1", "/user"},
		{"/auth/v1", false, "auth-v1", "/"},
		// "verifyx" is not the verify route: segment boundaries, unlike a string prefix.
		{"/auth/v1/verifyx", false, "auth-v1", "/verifyx"},
		{"/rest/v1", false, "rest-v1-openapi", "/"},
		{"/rest/v1", true, "rest-v1-openapi", "/"},
		{"/rest/v1/todos", false, "rest-v1", "/todos"},
		{"/rest/v1/rpc/f", false, "rest-v1", "/rpc/f"},
		{"/rest/v1/todos", true, "rest-v1", "/todos/"},
		{"/graphql/v1", false, "graphql-v1", "/rpc/graphql"},
		{"/storage/v1/object/public/b/f.png", false, "storage-v1", "/object/public/b/f.png"},
		{"/storage/v1/s3/bucket/key", false, "storage-v1-s3", "/s3/bucket/key"},
		{"/storage/v1/s3", false, "storage-v1-s3", "/s3"},
		{"/functions/v1/hello", false, "functions-v1", "/hello"},
		{"/functions/v1", false, "functions-v1", "/"},
		{"/realtime/v1/websocket", false, "realtime-v1-ws", "/socket/websocket"},
		{"/realtime/v1", false, "realtime-v1-ws", "/socket"},
		{"/realtime/v1/api/broadcast", false, "realtime-v1-api", "/api/broadcast"},
		{"/realtime/v1/api/tenants", false, "realtime-v1-api-tenants", ""},
		{"/realtime/v1/api/tenants/abc/reload", false, "realtime-v1-api-tenants", ""},
		{"/realtime/v1/api/openapi", false, "realtime-v1-api-openapi", ""},
	}
	for _, c := range cases {
		rt := matchRoute(c.path)
		if rt == nil {
			t.Errorf("%s: no route", c.path)
			continue
		}
		if rt.name != c.route {
			t.Errorf("%s: route %s, want %s", c.path, rt.name, c.route)
			continue
		}
		if rt.access == accessBlocked {
			continue
		}
		if got := rt.upstreamPath(c.path, c.trailing); got != c.upstream {
			t.Errorf("%s: upstream %q, want %q", c.path, got, c.upstream)
		}
	}
	for _, p := range []string{"/", "/foo", "/pg/tables", "/mcp", "/api/mcp", "/rest/v2/x", "/restx/v1", "/realtime/v2", "/.well-known/other"} {
		if rt := matchRoute(p); rt != nil {
			t.Errorf("%s: unexpected route %s", p, rt.name)
		}
	}
}

func TestStripAPIKey(t *testing.T) {
	cases := []struct{ in, key, rest string }{
		{"", "", ""},
		{"select=*", "", "select=*"},
		{"apikey=abc", "abc", ""},
		{"select=*&apikey=abc", "abc", "select=*"},
		{"apikey=abc&select=*&order=id.desc", "abc", "select=*&order=id.desc"},
		{"a=1&apikey=&b=2", "", "a=1&b=2"},
		{"apikey=first&apikey=second&x=1", "first", "x=1"},
		{"apikey=&apikey=second", "second", ""},
		{"%61pikey=enc&x=1", "enc", "x=1"},
		{"apikey=sb_publishable_ab%2Bc", "sb_publishable_ab+c", ""},
		// Other parameters are untouched, byte for byte.
		{"or=(a.eq.1,b.eq.2)&name=ilike.*%25x*&apikey=k", "k", "or=(a.eq.1,b.eq.2)&name=ilike.*%25x*"},
		{"apikeyx=1&xapikey=2", "", "apikeyx=1&xapikey=2"},
	}
	for _, c := range cases {
		key, rest := stripAPIKey(c.in)
		if key != c.key || rest != c.rest {
			t.Errorf("%q: got (%q, %q), want (%q, %q)", c.in, key, rest, c.key, c.rest)
		}
	}
}
