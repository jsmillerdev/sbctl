package selfupdate

import (
	"crypto/ed25519"
	_ "embed"
	"errors"
	"fmt"
)

// releaseKeyNextPEM is the optional second public key, release_key_next.pem. While it holds a key,
// this binary accepts a release signed by the current key or by the next one. A key rotation
// needs that overlap: the release that introduces the next key is signed with the current key
// and carries both; once nodes run it, a release signed with the next key verifies on them
// without anybody re-running the installer (deploy/README.md, "Rotating the release signing key").
//
//go:embed release_key_next.pem
var releaseKeyNextPEM []byte

// Key indexes in the list EmbeddedKeys returns, and in Verified.Key.
const (
	CurrentKey = 0
	NextKey    = 1
)

// EmbeddedKeys returns the public keys compiled into the binary: the current key, then the next
// key when one is set. It returns ErrNoKey when there is no current key; an unset next key is not
// an error, a next key that is set but not a valid ed25519 public key is.
func EmbeddedKeys() ([]ed25519.PublicKey, error) {
	return keysFromPEM(releaseKeyPEM, releaseKeyNextPEM)
}

func keysFromPEM(current, next []byte) ([]ed25519.PublicKey, error) {
	cur, err := ParsePublicKey(current)
	if err != nil {
		return nil, err
	}
	keys := []ed25519.PublicKey{cur}
	nk, err := ParsePublicKey(next)
	switch {
	case errors.Is(err, ErrNoKey):
		return keys, nil // no rotation under way
	case err != nil:
		return nil, fmt.Errorf("next release key: %w", err)
	case nk.Equal(cur):
		return nil, errors.New("the next release key is the current key: a rotation needs a new key")
	}
	return append(keys, nk), nil
}

// VerifySumsAny checks the signature of the checksum list against each of keys and returns the
// index of the key that verified it. It is VerifySums for a list of acceptable keys.
func VerifySumsAny(keys []ed25519.PublicKey, sums, sig []byte) (int, error) {
	if len(keys) == 0 {
		return -1, ErrNoKey
	}
	if len(sig) != ed25519.SignatureSize {
		return -1, fmt.Errorf("%s is %d bytes, want a %d-byte ed25519 signature", SigAsset, len(sig), ed25519.SignatureSize)
	}
	for i, k := range keys {
		if ed25519.Verify(k, sums, sig) {
			return i, nil
		}
	}
	return -1, errors.New("signature of SHA256SUMS does not verify against the release key: refusing to install")
}

// keys is the list of keys that verify a release: Options.Keys, or Options.Key, or the embedded ones.
func (o *Options) keys() ([]ed25519.PublicKey, error) {
	switch {
	case len(o.Keys) > 0:
		return o.Keys, nil
	case o.Key != nil:
		return []ed25519.PublicKey{o.Key}, nil
	}
	return EmbeddedKeys()
}

func keyName(i int) string {
	if i == NextKey {
		return "next key: this release was signed after a key rotation"
	}
	return "current key"
}
