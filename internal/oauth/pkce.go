package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// PKCE (RFC 7636). Only the S256 method exists here: "plain" sends the secret in the clear and is
// not accepted by MCP clients that follow the specification either.

const (
	// pkceMinLen and pkceMaxLen bound a code_verifier (RFC 7636 section 4.1) and, as the design fixes
	// it, a code_challenge.
	pkceMinLen = 43
	pkceMaxLen = 128
)

// unreservedChar reports whether c is in the unreserved set of RFC 3986: ALPHA, DIGIT, "-", ".", "_", "~".
func unreservedChar(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	}
	return c == '-' || c == '.' || c == '_' || c == '~'
}

// validPKCEString reports whether s is 43 to 128 characters of the unreserved set: the shape of a
// code_verifier, and of a code_challenge.
func validPKCEString(s string) bool {
	if len(s) < pkceMinLen || len(s) > pkceMaxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !unreservedChar(s[i]) {
			return false
		}
	}
	return true
}

// pkceChallengeS256 is BASE64URL-ENCODE(SHA256(ASCII(verifier))) without padding.
func pkceChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// verifyPKCE reports whether verifier answers challenge. A verifier of the wrong shape never does.
// The comparison takes the same time whatever the first differing byte.
func verifyPKCE(challenge, verifier string) bool {
	if challenge == "" || !validPKCEString(verifier) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(pkceChallengeS256(verifier)), []byte(challenge)) == 1
}
