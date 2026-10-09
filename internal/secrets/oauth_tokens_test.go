package secrets

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
)

func TestOAuthSecretFormats(t *testing.T) {
	cases := []struct {
		name   string
		make   func() string
		prefix string
		hexLen int
		is     func(string) bool
	}{
		{"access token", NewOAuthAccessToken, "sbp_oauth_", 40, IsOAuthAccessToken},
		{"refresh token", NewOAuthRefreshToken, "sbr_", 64, IsOAuthRefreshToken},
		{"authorization code", NewAuthCode, "sbc_", 64, IsAuthCode},
		{"client secret", NewClientSecret, "sba_", 64, IsClientSecret},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.make()
			if !strings.HasPrefix(got, c.prefix) || len(got) != len(c.prefix)+c.hexLen {
				t.Fatalf("%q is not %s + %d hex", got, c.prefix, c.hexLen)
			}
			if !c.is(got) {
				t.Errorf("the validator refuses %q", got)
			}
			if other := c.make(); other == got {
				t.Errorf("two calls returned %q", got)
			}
			// Every other shape is refused: wrong case, wrong length, wrong alphabet, a neighbor's prefix.
			rest := strings.TrimPrefix(got, c.prefix)
			for _, bad := range []string{
				"", c.prefix, got + "0", got[:len(got)-1], c.prefix + strings.ToUpper(rest),
				c.prefix + rest[:len(rest)-1] + "g", c.prefix + rest[:len(rest)-1] + " ", " " + got, got + "\n",
				"x" + got[1:],
			} {
				if c.is(bad) {
					t.Errorf("the validator accepts %q", bad)
				}
			}
		})
	}
}

// The prefixes must not be mistaken for each other: an access token is not a refresh token, a code or
// a client secret, and a personal access token is not an OAuth token.
func TestOAuthSecretShapesAreDisjoint(t *testing.T) {
	all := map[string]string{
		"access": NewOAuthAccessToken(), "refresh": NewOAuthRefreshToken(),
		"code": NewAuthCode(), "client secret": NewClientSecret(), "pat": NewPAT(),
	}
	is := map[string]func(string) bool{
		"access": IsOAuthAccessToken, "refresh": IsOAuthRefreshToken, "code": IsAuthCode, "client secret": IsClientSecret,
	}
	for kind, secret := range all {
		for name, f := range is {
			if want := name == kind; f(secret) != want {
				t.Errorf("%s validator on a %s: got %v", name, kind, !want)
			}
		}
	}
}

// An OAuth access token matches the personal access token pattern of the CLI (sbp_ + lowercase hex,
// with the sbp_oauth_ marker), which is why it must never be stored where PATs are looked up.
func TestOAuthAccessTokenExtendsPAT(t *testing.T) {
	if !strings.HasPrefix(PrefixOAuthAccess, PrefixPAT) {
		t.Fatalf("%q does not extend %q", PrefixOAuthAccess, PrefixPAT)
	}
	if IsOAuthAccessToken(NewPAT()) {
		t.Error("a personal access token is not an OAuth access token")
	}
}

func TestClientSecretAlias(t *testing.T) {
	const secret = "sba_1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809"
	if got := ClientSecretAlias(secret); got != "sba_1a2b********" {
		t.Errorf("alias = %q", got)
	}
	if got := ClientSecretAlias(NewClientSecret()); len(got) != 16 || !strings.HasSuffix(got, "********") || !strings.HasPrefix(got, "sba_") {
		t.Errorf("alias = %q", got)
	}
	if got := TokenPrefix(NewOAuthAccessToken()); got != "sbp_oaut" {
		t.Errorf("an access token's stored prefix = %q, want the constant sbp_oaut", got)
	}
	if got := TokenPrefix("sbr"); got != "sbr" {
		t.Errorf("a short string is kept whole, got %q", got)
	}
}

// HashToken is how every one of these secrets is stored.
func TestOAuthSecretsHashWithHashToken(t *testing.T) {
	tok := NewOAuthAccessToken()
	sum := sha256.Sum256([]byte(tok))
	if !bytes.Equal(HashToken(tok), sum[:]) {
		t.Error("HashToken is not SHA-256")
	}
	if bytes.Equal(HashToken(tok), HashToken(NewOAuthAccessToken())) {
		t.Error("two tokens hash alike")
	}
}
