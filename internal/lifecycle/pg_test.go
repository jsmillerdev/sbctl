package lifecycle

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

// TestScramVerifierRFC7677 checks the verifier against the RFC 7677 SCRAM-SHA-256 example
// (user "user", password "pencil"): the ServerKey in the verifier must reproduce the
// RFC's ServerSignature, and the StoredKey must verify the RFC's ClientProof.
func TestScramVerifierRFC7677(t *testing.T) {
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	v, err := scramVerifierWithSalt("pencil", salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	rest, ok := strings.CutPrefix(v, "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$")
	if !ok {
		t.Fatalf("bad verifier prefix: %s", v)
	}
	keys := strings.SplitN(rest, ":", 2)
	stored, _ := base64.StdEncoding.DecodeString(keys[0])
	server, _ := base64.StdEncoding.DecodeString(keys[1])

	authMsg := "n=user,r=rOprNGfwEbeRWgbNEkqO,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096,c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	h := hmac.New(sha256.New, server)
	h.Write([]byte(authMsg))
	if got, want := base64.StdEncoding.EncodeToString(h.Sum(nil)), "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="; got != want {
		t.Fatalf("server signature = %s, want %s", got, want)
	}

	proof, _ := base64.StdEncoding.DecodeString("dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=")
	h = hmac.New(sha256.New, stored)
	h.Write([]byte(authMsg))
	sig := h.Sum(nil)
	clientKey := make([]byte, len(sig))
	for i := range sig {
		clientKey[i] = proof[i] ^ sig[i]
	}
	if sk := sha256.Sum256(clientKey); string(sk[:]) != string(stored) {
		t.Fatal("stored key does not verify the RFC client proof")
	}
}

func TestScramVerifierRandomSalt(t *testing.T) {
	a, err := ScramVerifier("pw")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ScramVerifier("pw")
	if a == b || strings.Contains(a, "pw$") {
		t.Fatalf("verifiers should differ per call: %s %s", a, b)
	}
}
