package sso

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"

	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// SecretSigningKey is the name of the sealed project secret that holds a GoTrue's SAML signing
// key: the system project's for the dashboard, a project's own for its end users. It is never
// shared between projects, so one project's identity providers cannot be told apart from
// another's by anything they hold in common, and rotating one touches nothing else.
const SecretSigningKey = "saml_private_key"

// SigningKeyBits is the size of the RSA key. GoTrue refuses anything under 2048 bits and
// requires the public exponent 65537, which crypto/rsa always uses.
const SigningKeyBits = 2048

// NewSigningKey returns a new RSA key in the form GOTRUE_SAML_PRIVATE_KEY takes: PKCS#1 DER,
// standard Base64, no line breaks (an environment file cannot carry them).
func NewSigningKey() (string, error) {
	k, err := rsa.GenerateKey(rand.Reader, SigningKeyBits)
	if err != nil {
		return "", fmt.Errorf("sso: generate SAML signing key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(k)), nil
}

// ValidSigningKey reports whether s is a key GoTrue accepts as GOTRUE_SAML_PRIVATE_KEY.
func ValidSigningKey(s string) error {
	der, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return errors.New("not standard Base64")
	}
	k, err := x509.ParsePKCS1PrivateKey(der)
	if err != nil {
		return errors.New("not a PKCS#1 RSA key")
	}
	if k.E != 0x10001 {
		return errors.New("public exponent is not 65537")
	}
	if k.N.BitLen() < 2048 {
		return errors.New("shorter than 2048 bits")
	}
	return nil
}

var ensureMu sync.Mutex

// EnsureSigningKey returns the SAML signing key of ref (system or a project), creating and
// sealing it in the registry on first use. Two processes that ask at once (the daemon and a
// CLI command) end up with the key of whichever stored first: the loser reads it back.
// The project row must exist (the secret is stored under it).
func EnsureSigningKey(ctx context.Context, reg registry.Registry, sec secrets.Secrets, ref string) (string, error) {
	ensureMu.Lock()
	defer ensureMu.Unlock()
	open := func() (string, error) {
		sealed, err := reg.GetSecret(ctx, ref, SecretSigningKey)
		if err != nil {
			return "", err
		}
		pt, err := sec.Open(sealed)
		if err != nil {
			return "", fmt.Errorf("sso: open the SAML signing key of %s: %w", ref, err)
		}
		return string(pt), nil
	}
	key, err := open()
	if !errors.Is(err, registry.ErrNotFound) {
		return key, err
	}
	fresh, err := NewSigningKey()
	if err != nil {
		return "", err
	}
	sealed, err := sec.Seal([]byte(fresh))
	if err != nil {
		return "", err
	}
	if _, err := registry.PutSecretIfAbsent(ctx, reg, ref, SecretSigningKey, sealed); err != nil {
		return "", fmt.Errorf("sso: store the SAML signing key of %s: %w", ref, err)
	}
	return open()
}
