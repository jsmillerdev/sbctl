package cryptojs

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

type vector struct {
	Key       string `json:"key"`
	Plaintext string `json:"plaintext"`
	Encrypted string `json:"encrypted"`
}

// The vectors in testdata/cryptojs/vectors.json were produced by the real crypto-js
// (gen.mjs next to them), the library postgres-meta uses to decrypt the header.
func loadVectors(t *testing.T) []vector {
	t.Helper()
	b, err := os.ReadFile("../testdata/cryptojs/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v []vector
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v) == 0 {
		t.Fatal("no vectors")
	}
	return v
}

func TestDecryptCryptoJSVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		got, err := Decrypt(v.Encrypted, v.Key)
		if err != nil {
			t.Fatalf("Decrypt(%q): %v", v.Key, err)
		}
		if got != v.Plaintext {
			t.Errorf("Decrypt(%q) = %q, want %q", v.Key, got, v.Plaintext)
		}
	}
}

// Re-encrypting with the salt crypto-js chose must reproduce its output byte for byte:
// that proves our encrypt side matches, not only our decrypt side.
func TestEncryptMatchesCryptoJSWithSameSalt(t *testing.T) {
	for _, v := range loadVectors(t) {
		raw, err := base64.StdEncoding.DecodeString(v.Encrypted)
		if err != nil {
			t.Fatal(err)
		}
		got, err := encryptWithSalt(v.Plaintext, v.Key, raw[len(magic):len(magic)+saltLen])
		if err != nil {
			t.Fatal(err)
		}
		if got != v.Encrypted {
			t.Errorf("encrypt(%q) differs from crypto-js output", v.Key)
		}
	}
}

func TestRoundTripAndWrongKey(t *testing.T) {
	enc, err := Encrypt("postgres://u:p@127.0.0.1:1/db", "key-a")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Decrypt(enc, "key-a"); err != nil || got != "postgres://u:p@127.0.0.1:1/db" {
		t.Fatalf("round trip: %q, %v", got, err)
	}
	// A wrong key usually fails padding validation; the rare collision decrypts to
	// garbage but never to the plaintext.
	if got, err := Decrypt(enc, "key-b"); err == nil && got == "postgres://u:p@127.0.0.1:1/db" {
		t.Fatal("wrong key decrypted")
	}
	for _, bad := range []string{"", "not base64!", "AAAA", "U2FsdGVkX1"} {
		if _, err := Decrypt(bad, "k"); err == nil {
			t.Errorf("Decrypt(%q) accepted malformed input", bad)
		}
	}
}
