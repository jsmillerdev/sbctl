package fleet

import (
	"strings"
	"testing"
)

// Expected value computed with Python's hashlib (pbkdf2_hmac and hmac), an independent
// implementation of RFC 5802 / PostgreSQL's pg_be_scram_build_secret.
func TestScramVerifierMatchesIndependentImplementation(t *testing.T) {
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = byte(i)
	}
	got, err := scramVerifierWithSalt("correct horse battery staple", salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	const want = "SCRAM-SHA-256$4096:AAECAwQFBgcICQoLDA0ODw==$ONYbSJBXtKl6bP6PVqw8pm9e7EiacprLnoUQPFS80Hw=:IPOtHuGJ2HifEQg74W2XXqqCrCyQG55GbPRHa6g6n9w="
	if got != want {
		t.Fatalf("verifier = %s\nwant       %s", got, want)
	}
	a, _ := scramVerifier("x")
	b, _ := scramVerifier("x")
	if a == b || !strings.HasPrefix(a, "SCRAM-SHA-256$4096:") {
		t.Fatalf("verifiers must carry a random salt: %s %s", a, b)
	}
}

func TestManagerPasswordIsStableAndDistinctPerProject(t *testing.T) {
	a, b := managerPassword("admin-password-1"), managerPassword("admin-password-2")
	if a == b || a != managerPassword("admin-password-1") || len(a) != 64 {
		t.Fatalf("%s %s", a, b)
	}
	if strings.Contains(a, "admin-password") {
		t.Fatal("derived password leaks its input")
	}
}
