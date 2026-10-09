package oauth

import (
	"errors"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/secrets"
)

func TestNewUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := secrets.NewUUID()
		if len(id) != 36 || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
			t.Fatalf("%q is not a version 4 UUID", id)
		}
		if got, ok := canonUUID(id); !ok || got != id {
			t.Fatalf("canonUUID(%q) = %q, %v", id, got, ok)
		}
		if seen[id] {
			t.Fatalf("%q repeated", id)
		}
		seen[id] = true
	}
	if got, ok := canonUUID("AAAAAAAA-1111-4222-8333-ABCDEFABCDEF"); !ok || got != "aaaaaaaa-1111-4222-8333-abcdefabcdef" {
		t.Errorf("upper case is folded: %q, %v", got, ok)
	}
	for _, bad := range []string{"", "x", "aaaaaaaa-1111-4222-8333-abcdefabcde", "aaaaaaaa-1111-4222-8333-abcdefabcdeg", " aaaaaaaa-1111-4222-8333-abcdefabcdef", "aaaaaaaa11114222833 3abcdefabcdef0"} {
		if _, ok := canonUUID(bad); ok {
			t.Errorf("canonUUID accepts %q", bad)
		}
	}
}

func TestCleanClientName(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"Claude Code", "Claude Code", true},
		{"  Claude \t Code\n(supabase)  ", "Claude Code (supabase)", true},
		{"Evil‮Label", "EvilLabel", true},                                // right-to-left override dropped
		{"Zero​Width", "ZeroWidth", true},                                // zero-width space dropped
		{"a\x00b\x07c", "abc", true},                                     // control characters dropped
		{"line break", "line break", true},                               // line separator is whitespace
		{"non breaking", "non breaking", true},                           // no-break space is whitespace
		{"<script>alert(1)</script>", "<script>alert(1)</script>", true}, // kept: it is rendered as text
		{"日本語クライアント", "日本語クライアント", true},
		{"", "", false},
		{"   \t\n ", "", false},
		{"‮​", "", false},
		{string([]byte{0xff, 0xfe}), "", false},
		{strings.Repeat("a", 100), strings.Repeat("a", 100), true},
		{strings.Repeat("a", 101), "", false},
		{strings.Repeat("語", 100), strings.Repeat("語", 100), true}, // 100 characters, 300 bytes
		{strings.Repeat("語", 101), "", false},
		{strings.Repeat(" ", 50) + "x" + strings.Repeat(" ", 50), "x", true}, // length is of the cleaned name
	}
	for _, tc := range tests {
		got, ok := cleanClientName(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("cleanClientName(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseRedirectURI(t *testing.T) {
	good := []string{
		"https://app.example.test/callback",
		"https://app.example.test",
		"https://app.example.test:8443/cb?x=1&y=2",
		"https://claude.ai/api/mcp/auth_callback",
		"https://127.0.0.1/cb",
		"https://[2001:db8::1]/cb",
		"http://localhost/cb",
		"http://localhost:3000/cb",
		"http://127.0.0.1:53123/callback",
		"http://[::1]:8080/cb",
		"http://LOCALHOST:3000/cb",
		"https://sub-domain.example-site.test/a/b/c",
		"https://" + strings.Repeat("a", 63) + ".test/" + strings.Repeat("p", 1900),
	}
	for _, u := range good {
		if _, err := parseRedirectURI(u); err != nil {
			t.Errorf("parseRedirectURI(%.60q) = %v", u, err)
		}
	}
	bad := map[string]string{
		"":                                                  "empty",
		"javascript:alert(1)":                               "javascript scheme",
		"data:text/html,x":                                  "data scheme",
		"file:///etc/passwd":                                "file scheme",
		"cursor://anysphere.cursor-retrieval":               "custom scheme",
		"vscode://vscode.github-authentication":             "custom scheme",
		"ftp://example.test/cb":                             "ftp scheme",
		"//example.test/cb":                                 "no scheme",
		"/relative/path":                                    "relative",
		"example.test/cb":                                   "no scheme, no slashes",
		"https://":                                          "no host",
		"https:///cb":                                       "empty host",
		"http://app.example.test/cb":                        "plain http to a non-loopback host",
		"http://localhost.example.test/cb":                  "plain http to a lookalike",
		"http://192.168.0.5/cb":                             "plain http to a private address",
		"http://0.0.0.0/cb":                                 "plain http to the unspecified address",
		"http://[::ffff:127.0.0.1]/cb":                      "mapped loopback",
		"https://user:pw@example.test/cb":                   "userinfo",
		"https://user@example.test/cb":                      "user name",
		"https://example.test/cb#frag":                      "fragment",
		"https://example.test/cb#":                          "empty fragment",
		"https://example.test/c b":                          "space",
		"https://example.test/cb\n":                         "newline",
		"https://example.test/\x00":                         "NUL",
		`https://example.test\@evil.test/cb`:                "backslash",
		"https://exämple.test/cb":                           "non-ASCII host",
		"https://example.test:0/cb":                         "port 0",
		"https://example.test:99999/cb":                     "port too large",
		"https://example.test:http/cb":                      "port not numeric",
		"https://exa%6dple.test/cb":                         "percent-encoded host",
		"https://exa$mple.test/cb":                          "odd host character",
		"https://" + strings.Repeat("a", MaxRedirectURILen): "too long",
		"mailto:a@example.test":                             "mailto",
	}
	for u, why := range bad {
		if _, err := parseRedirectURI(u); err == nil {
			t.Errorf("parseRedirectURI accepts %.60q (%s)", u, why)
		}
	}
}

func TestValidateRedirectURIs(t *testing.T) {
	if _, err := validateRedirectURIs(nil); err == nil {
		t.Error("an empty list is accepted")
	}
	ten := make([]string, 10)
	for i := range ten {
		ten[i] = "https://app.example.test/cb" + string(rune('a'+i))
	}
	if got, err := validateRedirectURIs(ten); err != nil || len(got) != 10 {
		t.Errorf("ten URIs: %v, %v", got, err)
	}
	if _, err := validateRedirectURIs(append(ten, "https://app.example.test/cbk")); err == nil {
		t.Error("eleven URIs are accepted")
	}
	got, err := validateRedirectURIs([]string{"https://a.example.test/cb", "https://a.example.test/cb", "http://localhost/cb"})
	if err != nil || len(got) != 2 || got[0] != "https://a.example.test/cb" || got[1] != "http://localhost/cb" {
		t.Errorf("repeats are dropped, order kept: %v, %v", got, err)
	}
	_, err = validateRedirectURIs([]string{"https://a.example.test/cb", "javascript:alert(1)"})
	if err == nil || !strings.HasPrefix(err.Error(), "redirect_uris[1] ") {
		t.Errorf("the error names the entry: %v", err)
	}
}

func TestCheckWebURL(t *testing.T) {
	for _, tc := range []struct {
		url       string
		httpsOnly bool
		ok        bool
	}{
		{"https://example.test/logo.png", true, true},
		{"https://example.test", true, true},
		{"http://example.test/logo.png", true, false},
		{"http://example.test/logo.png", false, true},
		{"javascript:alert(1)", false, false},
		{"data:image/png;base64,AAAA", false, false},
		{"ftp://example.test/x", false, false},
		{"https://user:pw@example.test/", true, false},
		{"https://example.test/a b", true, false},
		{"https:///x", true, false},
		{"/relative.png", false, false},
		{"https://example.test/" + strings.Repeat("a", maxWebURLLen), true, false},
	} {
		if err := checkWebURL(tc.url, tc.httpsOnly); (err == nil) != tc.ok {
			t.Errorf("checkWebURL(%.50q, httpsOnly=%v) = %v, want ok=%v", tc.url, tc.httpsOnly, err, tc.ok)
		}
	}
}

func TestResolveResource(t *testing.T) {
	s := &Service{Issuer: "https://api.example.test"}
	mcp, root := "https://api.example.test/mcp", "https://api.example.test"
	for in, want := range map[string]string{
		"https://api.example.test/mcp":                mcp,
		"https://api.example.test/mcp/":               mcp,
		"https://API.example.test/mcp":                mcp,
		"https://api.example.test/mcp?project_ref=ab": mcp,
		"https://api.example.test/mcp#frag":           mcp,
		"https://api.example.test":                    root,
		"https://api.example.test/":                   root,
	} {
		if got, ok := s.resolveResource(in); !ok || got != want {
			t.Errorf("resolveResource(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{
		"", "mcp", "/mcp", "https://api.example.test/mcp2", "https://api.example.test/mcp//", "https://api.example.test/other",
		"http://api.example.test/mcp", "https://api.example.test:8443/mcp", "https://evil.example.test/mcp",
		"https://api.example.test.evil.test/mcp", "https://user@api.example.test/mcp", "javascript:alert(1)",
		"https://api.example.test/mcp space",
	} {
		if got, ok := s.resolveResource(bad); ok {
			t.Errorf("resolveResource(%q) = %q, accepted", bad, got)
		}
	}
	// The server's own spelling comes back, so that a grant compares equal to ResourceURL(issuer).
	s = &Service{Issuer: "https://API.example.test/"}
	if got, ok := s.resolveResource("https://api.example.test/mcp"); !ok || got != ResourceURL(s.Issuer) {
		t.Errorf("with a mixed-case issuer: %q, %v; want %q", got, ok, ResourceURL(s.Issuer))
	}
}

func TestErrorMessage(t *testing.T) {
	if got := ErrorMessage(invalidf("name is required")); got != "name is required" {
		t.Errorf("invalid: %q", got)
	}
	if got := ErrorMessage(limitf("too many %s", "apps")); got != "too many apps" {
		t.Errorf("limit: %q", got)
	}
	if got := ErrorMessage(errors.New("boom")); got != "boom" {
		t.Errorf("other: %q", got)
	}
	if err := invalidf("x"); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrLimit) {
		t.Errorf("invalidf is not ErrInvalid: %v", err)
	}
	if err := limitf("x"); !errors.Is(err, ErrLimit) {
		t.Errorf("limitf is not ErrLimit: %v", err)
	}
	if got := clip("ééééé", 5); got != "éé..." {
		t.Errorf("clip cuts inside a character: %q", got)
	}
}
