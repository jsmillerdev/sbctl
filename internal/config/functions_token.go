package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FunctionsProxyTokenHeader carries the node's proxy secret from the proxy to the Edge
// Runtime's main service (and, on the Management API, marks a request from a trusted
// forwarder). The runtime listens on loopback, which a function worker can reach too, so
// the main service refuses every request without it.
const FunctionsProxyTokenHeader = "X-Sbctl-Proxy-Token"

// FunctionsProxyTokenFile is the node's proxy secret: 0600, owned by the sbctl user, in
// system/ itself, outside every unit's state directory, so no unit's mount namespace shows it
// (sb-edge-runtime gets the value in its environment file instead).
func (p Paths) FunctionsProxyTokenFile() string {
	return filepath.Join(p.Root, "system", "edge-runtime.token")
}

// LoadFunctionsProxyToken returns the node's proxy secret, creating it (32 random bytes,
// hex) when there is none. The proxy, the fleet step that renders the runtime's
// environment and the API all call it, from different processes: the file is created
// atomically (written aside, then linked into place), so they all read the same value.
func LoadFunctionsProxyToken(p Paths) (string, error) {
	path := p.FunctionsProxyTokenFile()
	read := func() (string, error) { return ReadFunctionsProxyToken(p) }
	if t, err := read(); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return t, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".edge-runtime.token.")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	return read()
}

// ReadFunctionsProxyToken returns the node's proxy secret without creating it; the error
// wraps fs.ErrNotExist when there is none.
func ReadFunctionsProxyToken(p Paths) (string, error) {
	path := p.FunctionsProxyTokenFile()
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if len(t) < 32 {
		return "", fmt.Errorf("config: %s holds no proxy secret (delete it to have a new one made)", path)
	}
	return t, nil
}
