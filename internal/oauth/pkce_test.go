package oauth

import (
	"strings"
	"testing"
)

// TestPKCE checks S256 against the vectors of RFC 7636 appendix B and the shapes it refuses (C1).
func TestPKCE(t *testing.T) {
	const (
		verifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
		challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	)
	if got := pkceChallengeS256(verifier); got != challenge {
		t.Fatalf("challenge of the RFC verifier = %q, want %q", got, challenge)
	}
	if !verifyPKCE(challenge, verifier) {
		t.Fatal("the RFC verifier does not answer the RFC challenge")
	}

	// RFC 7636 appendix A: the octet sequence that the example verifier encodes.
	octets := []byte{116, 24, 223, 180, 151, 153, 224, 37, 79, 250, 96, 125, 216, 173, 187, 186, 22, 212, 37, 77, 105, 214, 191, 240, 91, 88, 5, 88, 83, 132, 141, 121}
	if got := pkceEncodeForTest(octets); got != verifier {
		t.Errorf("appendix A encoding = %q, want %q", got, verifier)
	}

	long := strings.Repeat("a", pkceMaxLen)
	for name, v := range map[string]string{
		"wrong verifier":    strings.Repeat("A", 43),
		"empty":             "",
		"the challenge":     challenge, // a "plain" method would accept this
		"42 characters":     verifier[:42],
		"129 characters":    strings.Repeat("a", pkceMaxLen+1),
		"a reserved char":   verifier[:42] + "=",
		"a space":           verifier[:42] + " ",
		"a slash":           verifier[:42] + "/",
		"a plus":            verifier[:42] + "+",
		"non-ASCII":         verifier[:42] + "é",
		"trailing newline":  verifier + "\n",
		"upper-cased":       strings.ToUpper(verifier),
		"128 valid, wrong":  long,
		"the right one, +x": verifier + "x",
	} {
		if verifyPKCE(challenge, v) {
			t.Errorf("%s: verifier accepted", name)
		}
	}
	if verifyPKCE("", verifier) {
		t.Error("an empty challenge accepts a verifier")
	}
	// Both bounds are inclusive, and the unreserved set is the whole alphabet.
	for _, n := range []int{pkceMinLen, pkceMaxLen} {
		v := strings.Repeat("a", n)
		if !validPKCEString(v) || !verifyPKCE(pkceChallengeS256(v), v) {
			t.Errorf("a verifier of %d characters is refused", n)
		}
	}
	if !validPKCEString("ABCxyz0123456789-._~ABCxyz0123456789-._~ABC") {
		t.Error("the unreserved set is refused")
	}
}

// pkceEncodeForTest is the base64url encoding of appendix A without padding, written out by hand so that
// the test does not share an implementation with the code under test.
func pkceEncodeForTest(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out []byte
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		v := uint(chunk[0])<<16 | uint(chunk[1])<<8 | uint(chunk[2])
		out = append(out, alphabet[v>>18&63], alphabet[v>>12&63])
		if n > 1 {
			out = append(out, alphabet[v>>6&63])
		}
		if n > 2 {
			out = append(out, alphabet[v&63])
		}
	}
	return string(out)
}
