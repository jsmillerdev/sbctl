package oauth

import "testing"

func TestURLs(t *testing.T) {
	const iss = "https://api.example.com"
	for _, issuer := range []string{iss, iss + "/"} {
		cases := map[string]string{
			ResourceURL(issuer):                    iss + "/mcp",
			ProtectedResourceMetadataURL(issuer):   iss + "/.well-known/oauth-protected-resource/mcp",
			AuthorizationServerMetadataURL(issuer): iss + "/.well-known/oauth-authorization-server",
			AuthorizeURL(issuer):                   iss + "/v1/oauth/authorize",
			TokenURL(issuer):                       iss + "/v1/oauth/token",
			RevokeURL(issuer):                      iss + "/v1/oauth/revoke",
			RegisterURL(issuer):                    iss + "/platform/oauth/apps/register",
		}
		for got, want := range cases {
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		}
	}
	if got := ConsentURL("https://studio.example.com/", "1b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed", ""); got !=
		"https://studio.example.com/authorize?auth_id=1b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed" {
		t.Error(got)
	}
	if got := ConsentURL("https://studio.example.com", "x", "my-org"); got !=
		"https://studio.example.com/authorize?auth_id=x&organization_slug=my-org" {
		t.Error(got)
	}
}

func TestAppendQuery(t *testing.T) {
	cases := []struct {
		base string
		kv   []string
		want string
	}{
		{"https://app.example.com/cb", []string{"code", "sbc_ab", "state", "s t", "iss", "https://api.example.com"},
			"https://app.example.com/cb?code=sbc_ab&state=s+t&iss=https%3A%2F%2Fapi.example.com"},
		// An existing query stays, first.
		{"https://app.example.com/cb?x=1&y=%20", []string{"code", "c"}, "https://app.example.com/cb?x=1&y=%20&code=c"},
		// Loopback with a port.
		{"http://127.0.0.1:53124/callback", []string{"error", "invalid_scope", "error_description", "no scope left"},
			"http://127.0.0.1:53124/callback?error=invalid_scope&error_description=no+scope+left"},
		{"http://[::1]:8080/cb", nil, "http://[::1]:8080/cb"},
		// A state that tries to add parameters is one value.
		{"https://a.example/cb", []string{"state", "x&code=evil#frag"}, "https://a.example/cb?state=x%26code%3Devil%23frag"},
	}
	for _, c := range cases {
		got, err := AppendQuery(c.base, c.kv...)
		if err != nil || got != c.want {
			t.Errorf("AppendQuery(%q, %q) = %q, %v; want %q", c.base, c.kv, got, err, c.want)
		}
	}
	if _, err := AppendQuery("https://a.example/cb", "odd"); err == nil {
		t.Error("an odd number of strings must be refused")
	}
	if _, err := AppendQuery("http://[::1", "a", "b"); err == nil {
		t.Error("an unparsable URL must be refused")
	}
}
