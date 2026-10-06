// Package secrets seals values stored in the registry with the node master key and
// generates every credential sbctl hands out (JWT secrets, API keys, PATs, passwords).
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Secrets seals and opens small blobs. Implementations must be safe for concurrent use.
type Secrets interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(ciphertext []byte) ([]byte, error)
}

const (
	keyLen      = 32
	sealedV1    = 0x01
	nonceLen    = 12
	keyFileMode = 0o600
)

var ErrCorrupt = errors.New("secrets: ciphertext is corrupt or was sealed with a different key")

// AESGCM is Secrets over AES-256-GCM. Sealed format: 0x01 || nonce(12) || ciphertext+tag.
type AESGCM struct{ aead cipher.AEAD }

// New builds an AESGCM from a 32-byte key.
func New(key []byte) (*AESGCM, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("secrets: key must be %d bytes, got %d", keyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &AESGCM{aead: aead}, nil
}

// LoadOrCreate reads the hex-encoded master key at path, creating it (0600) if missing.
func LoadOrCreate(path string) (*AESGCM, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		key := make([]byte, keyLen)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFileMode)
		if err != nil {
			return nil, err
		}
		if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		return New(key)
	}
	if err != nil {
		return nil, err
	}
	return Load(b)
}

// Load parses a hex-encoded key file body.
func Load(body []byte) (*AESGCM, error) {
	key, err := hex.DecodeString(strings.TrimSpace(string(body)))
	if err != nil {
		return nil, fmt.Errorf("secrets: master key is not hex: %w", err)
	}
	return New(key)
}

func (s *AESGCM) Seal(plaintext []byte) ([]byte, error) {
	out := make([]byte, 1+nonceLen, 1+nonceLen+len(plaintext)+s.aead.Overhead())
	out[0] = sealedV1
	if _, err := rand.Read(out[1 : 1+nonceLen]); err != nil {
		return nil, err
	}
	return s.aead.Seal(out, out[1:1+nonceLen], plaintext, nil), nil
}

func (s *AESGCM) Open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < 1+nonceLen+s.aead.Overhead() || ciphertext[0] != sealedV1 {
		return nil, ErrCorrupt
	}
	pt, err := s.aead.Open(nil, ciphertext[1:1+nonceLen], ciphertext[1+nonceLen:], nil)
	if err != nil {
		return nil, ErrCorrupt
	}
	return pt, nil
}
