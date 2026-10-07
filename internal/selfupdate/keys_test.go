package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func pemOf(t *testing.T, pub ed25519.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestKeysFromPEM(t *testing.T) {
	cur, _ := newKey(t)
	next, _ := newKey(t)
	placeholder := []byte("UNSET\n")

	if _, err := keysFromPEM(placeholder, placeholder); err != ErrNoKey {
		t.Errorf("no current key: %v", err)
	}
	if _, err := keysFromPEM(placeholder, pemOf(t, next)); err != ErrNoKey {
		t.Errorf("a next key without a current key is no key: %v", err)
	}
	ks, err := keysFromPEM(pemOf(t, cur), placeholder)
	if err != nil || len(ks) != 1 || !ks[0].Equal(cur) {
		t.Errorf("current only: %v %v", ks, err)
	}
	ks, err = keysFromPEM(pemOf(t, cur), pemOf(t, next))
	if err != nil || len(ks) != 2 || !ks[CurrentKey].Equal(cur) || !ks[NextKey].Equal(next) {
		t.Errorf("both: %v %v", ks, err)
	}
	if _, err := keysFromPEM(pemOf(t, cur), pemOf(t, cur)); err == nil || !strings.Contains(err.Error(), "needs a new key") {
		t.Errorf("next == current: %v", err)
	}
	// A next key that is PEM but not ed25519 is an error, not silence: a broken rotation must not pass unnoticed.
	rsaLike := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("garbage")})
	if _, err := keysFromPEM(pemOf(t, cur), rsaLike); err == nil || errors.Is(err, ErrNoKey) {
		t.Errorf("malformed next key: %v", err)
	}
}

func TestEmbeddedKeysOfThisCheckout(t *testing.T) {
	// Both files are placeholders until the maintainers commit keys; the second stays one outside a rotation.
	if _, err := EmbeddedKeys(); err != nil && err != ErrNoKey {
		t.Fatalf("EmbeddedKeys: %v", err)
	}
	if _, err := ParsePublicKey(releaseKeyNextPEM); err != ErrNoKey {
		t.Logf("a next key is set in this checkout (%v): a rotation is under way", err)
	}
}

func TestVerifySumsAny(t *testing.T) {
	k1, p1 := newKey(t)
	k2, p2 := newKey(t)
	k3, p3 := newKey(t)
	sums := []byte("deadbeef  file\n")
	keys := []ed25519.PublicKey{k1, k2}

	if i, err := VerifySumsAny(keys, sums, ed25519.Sign(p1, sums)); err != nil || i != CurrentKey {
		t.Errorf("signed by the current key: %d %v", i, err)
	}
	if i, err := VerifySumsAny(keys, sums, ed25519.Sign(p2, sums)); err != nil || i != NextKey {
		t.Errorf("signed by the next key: %d %v", i, err)
	}
	if _, err := VerifySumsAny(keys, sums, ed25519.Sign(p3, sums)); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Errorf("signed by a third key: %v", err)
	}
	// Only the current key is trusted: the next key's signature is refused, as on a binary from before the rotation began.
	if _, err := VerifySumsAny([]ed25519.PublicKey{k1}, sums, ed25519.Sign(p2, sums)); err == nil {
		t.Error("a binary without the next key accepted a signature by it")
	}
	// A changed list, a flipped signature bit and a short signature are all refused.
	sig := ed25519.Sign(p2, sums)
	if _, err := VerifySumsAny(keys, append([]byte("x"), sums...), sig); err == nil {
		t.Error("changed list accepted")
	}
	flipped := append([]byte(nil), sig...)
	flipped[7] ^= 0x10
	if _, err := VerifySumsAny(keys, sums, flipped); err == nil {
		t.Error("flipped signature accepted")
	}
	if _, err := VerifySumsAny(keys, sums, sig[:20]); err == nil || !strings.Contains(err.Error(), "64-byte") {
		t.Errorf("short signature: %v", err)
	}
	if _, err := VerifySumsAny(nil, sums, sig); err != ErrNoKey {
		t.Errorf("no keys: %v", err)
	}
	_ = k3
}

// The rotation, end to end: release N (signed by the current key) is installed by a node that
// trusts [current]; release N+1 is signed by the next key, which release N added to the binary
// as its second key, and the updated node installs it with no reinstall.
func TestKeyRotationNeedsNoReinstall(t *testing.T) {
	oldPub, oldPriv := newKey(t)
	newPub, newPriv := newKey(t)

	// A release signed by the new key, as the rotation's second release is.
	r := newReleaseServer(t, "v1.3.0", "binary signed after the rotation")
	r.priv, r.pub = newPriv, newPub
	r.rebuild(t)

	// A node whose binary has only the old key refuses it: it is a binary from before the rotation.
	o, exe := r.opts(t, "v1.1.0")
	o.Key, o.Keys = nil, []ed25519.PublicKey{oldPub}
	if _, err := Update(context.Background(), o); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("a binary that has not learned the next key: %v", err)
	}
	if read(t, exe) != "old binary" {
		t.Fatal("installed a release it could not verify")
	}

	// A node whose binary carries both keys (the release that introduced the next key) installs it.
	var said strings.Builder
	o, exe = r.opts(t, "v1.1.0")
	o.Key, o.Keys, o.Out = nil, []ed25519.PublicKey{oldPub, newPub}, &said
	res, err := Update(context.Background(), o)
	if err != nil || !res.Replaced {
		t.Fatalf("%+v %v", res, err)
	}
	if read(t, exe) != "binary signed after the rotation" {
		t.Fatal("not installed")
	}
	if !strings.Contains(said.String(), "next key") {
		t.Errorf("the output should say which key verified the release:\n%s", said.String())
	}

	// And the old key signing a release is still accepted while both keys are in the binary.
	r2 := newReleaseServer(t, "v1.2.0", "binary signed before the rotation")
	r2.priv, r2.pub = oldPriv, oldPub
	r2.rebuild(t)
	o, _ = r2.opts(t, "v1.1.0")
	o.Key, o.Keys = nil, []ed25519.PublicKey{oldPub, newPub}
	if _, err := Update(context.Background(), o); err != nil {
		t.Fatalf("current key during the overlap: %v", err)
	}
}
