package secrets

import (
	"encoding/hex"
	"strings"
)

// Prefixes and lengths of the secrets of the OAuth authorization server (internal/oauth). Every
// secret is its prefix plus lowercase hex from crypto/rand; it is shown once and stored as
// HashToken of the whole string. internal/oauth repeats the values as constants (types.go) and a
// test there keeps the two in step.
const (
	// PrefixOAuthAccess extends PrefixPAT on purpose: the Supabase CLI accepts it as a token. It must
	// never be looked up in access_tokens (see authenticate in internal/api).
	PrefixOAuthAccess  = "sbp_oauth_"
	PrefixOAuthRefresh = "sbr_"
	// PrefixAuthCode is also the prefix of the claim tokens of `supavise claim` (sbc_ plus 48 hex). The
	// two never meet: they are looked up in different tables, and a code is 64 hex.
	PrefixAuthCode     = "sbc_"
	PrefixClientSecret = "sba_"

	OAuthAccessHexLen  = 40
	OAuthRefreshHexLen = 64
	AuthCodeHexLen     = 64
	ClientSecretHexLen = 64

	// OAuthStoredPrefixLen is how many leading characters of a token the registry keeps next to its
	// hash (oauth_tokens.prefix, as access_tokens.prefix does for personal access tokens). It is also
	// how many characters of a client secret its alias shows.
	OAuthStoredPrefixLen = 8

	// clientSecretAliasStars are the asterisks that follow the visible prefix of a client secret alias.
	clientSecretAliasStars = "********"
)

// NewOAuthAccessToken returns an OAuth access token: "sbp_oauth_" + 40 lowercase hex. It matches the
// personal access token pattern sbp_[a-f0-9]... on purpose, see PrefixOAuthAccess.
func NewOAuthAccessToken() string {
	return PrefixOAuthAccess + hex.EncodeToString(RandomBytes(OAuthAccessHexLen/2))
}

// NewOAuthRefreshToken returns a refresh token: "sbr_" + 64 lowercase hex.
func NewOAuthRefreshToken() string {
	return PrefixOAuthRefresh + hex.EncodeToString(RandomBytes(OAuthRefreshHexLen/2))
}

// NewAuthCode returns an authorization code: "sbc_" + 64 lowercase hex.
func NewAuthCode() string { return PrefixAuthCode + hex.EncodeToString(RandomBytes(AuthCodeHexLen/2)) }

// NewClientSecret returns the secret of an OAuth app: "sba_" + 64 lowercase hex.
func NewClientSecret() string {
	return PrefixClientSecret + hex.EncodeToString(RandomBytes(ClientSecretHexLen/2))
}

// TokenPrefix is the part of a token that is stored beside its hash and shown in listings: its first
// OAuthStoredPrefixLen characters. For an OAuth access token that is the constant "sbp_oaut"; the
// column identifies nothing and is never used for lookup.
func TokenPrefix(token string) string {
	if len(token) <= OAuthStoredPrefixLen {
		return token
	}
	return token[:OAuthStoredPrefixLen]
}

// ClientSecretAlias is what lists show instead of a client secret: its first eight characters
// ("sba_" and four hex digits) and eight asterisks, "sba_1a2b********", the way personal access
// tokens are shown.
func ClientSecretAlias(secret string) string { return TokenPrefix(secret) + clientSecretAliasStars }

// IsOAuthAccessToken reports whether s has the exact shape of an OAuth access token. A string that
// does not cannot be a token, so callers refuse it without a lookup.
func IsOAuthAccessToken(s string) bool { return hasShape(s, PrefixOAuthAccess, OAuthAccessHexLen) }

// IsOAuthRefreshToken reports whether s has the exact shape of a refresh token.
func IsOAuthRefreshToken(s string) bool { return hasShape(s, PrefixOAuthRefresh, OAuthRefreshHexLen) }

// IsAuthCode reports whether s has the exact shape of an authorization code.
func IsAuthCode(s string) bool { return hasShape(s, PrefixAuthCode, AuthCodeHexLen) }

// IsClientSecret reports whether s has the exact shape of a client secret.
func IsClientSecret(s string) bool { return hasShape(s, PrefixClientSecret, ClientSecretHexLen) }

// hasShape reports whether s is prefix followed by exactly n lowercase hex digits.
func hasShape(s, prefix string, n int) bool {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok || len(rest) != n {
		return false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
