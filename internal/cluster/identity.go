package cluster

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh"
)

// ErrNoIdentity: the node has no certificate yet (it never joined a cluster and has not been
// given one as the founder).
var ErrNoIdentity = errors.New("cluster: this node has no certificate")

// Joined reports whether the cluster directory holds a node certificate: the sign that this server
// is part of a cluster as far as the files can say. A server that never joined has none, and every
// code path that reads it behaves as the single server it is.
func Joined(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, config.NodeCertFile))
	return err == nil
}

// NewKey generates a node key.
func NewKey() (ed25519.PrivateKey, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	return key, err
}

// Store keeps the node's credentials: they are loaded from the cluster directory, and a renewal
// replaces them in memory and on disk. Creds is what the mesh asks for on every handshake.
type Store struct {
	dir string
	cur atomic.Pointer[mesh.Credentials]
}

// OpenStore loads the node's key, certificate and the CA from dir. ErrNoIdentity when node.crt
// does not exist.
func OpenStore(dir string) (*Store, error) {
	s := &Store{dir: dir}
	c, err := LoadCredentials(dir)
	if err != nil {
		return nil, err
	}
	s.cur.Store(c)
	return s, nil
}

// NewStore returns a Store that holds c and persists replacements under dir.
func NewStore(dir string, c *mesh.Credentials) *Store {
	s := &Store{dir: dir}
	s.cur.Store(c)
	return s
}

// Creds returns the current credentials; nil if the store holds none.
func (s *Store) Creds() *mesh.Credentials { return s.cur.Load() }

// Dir is the cluster directory.
func (s *Store) Dir() string { return s.dir }

// ErrIdentityReadOnly: the daemon cannot write to the cluster directory (the unit may not allow it),
// so a renewed certificate could not be kept.
var ErrIdentityReadOnly = errors.New("cluster: the daemon cannot write to the cluster directory")

// Writable checks that a file can be created in the cluster directory, which keeping a renewed
// certificate takes. It creates a file and removes it.
func (s *Store) Writable() error {
	f, err := os.CreateTemp(s.dir, ".write-check-*")
	if err != nil {
		return fmt.Errorf("%w (%s): %v", ErrIdentityReadOnly, s.dir, err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("%w (%s): %v", ErrIdentityReadOnly, s.dir, err)
	}
	return nil
}

// ErrNotKept: a renewed certificate is in use but its file could not be written, so a restart of the
// daemon would load the old one, which the peers no longer admit.
var ErrNotKept = errors.New("cluster: the renewed certificate was not written to the cluster directory")

// Prepare checks a new certificate (DER) for the node's key and returns the credentials that use it.
// Nothing changes until Use and Keep are called.
func (s *Store) Prepare(certDER []byte) (*mesh.Credentials, error) {
	old := s.cur.Load()
	if old == nil {
		return nil, ErrNoIdentity
	}
	key, ok := old.Cert.PrivateKey.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("cluster: the node key is not an Ed25519 key")
	}
	c, err := mesh.NewCredentials(certDER, key, old.CA)
	if err != nil {
		return nil, err
	}
	if c.NodeID != old.NodeID {
		return nil, fmt.Errorf("cluster: the new certificate names node %s, not %s", c.NodeID, old.NodeID)
	}
	return c, nil
}

// Use makes c (from Prepare) the credentials of every handshake from now on.
func (s *Store) Use(c *mesh.Credentials) { s.cur.Store(c) }

// Keep writes a new certificate (DER) to the cluster directory, which is what a restart of the daemon
// loads. By the time a renewal calls it the leader has recorded the new serial, and peers admit a node
// by the serial in the registry, so the old file stops working for everyone once their copies catch up:
// the file is written as soon as the certificate arrives, and the switch in memory waits for the copies.
// The renewer checks Writable before it asks for a certificate, so that a directory the daemon cannot
// write to is found while the current certificate still works.
func (s *Store) Keep(certDER []byte) error {
	if err := writeFile(filepath.Join(s.dir, config.NodeCertFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0o644); err != nil {
		return fmt.Errorf("%w (%s): %v", ErrNotKept, config.NodeCertFile, err)
	}
	return nil
}

// Resync writes the certificate in use to the cluster directory when the file holds another one,
// which is what a renewal that could not write the file leaves behind. It reports whether it wrote.
// The renewer calls it between renewals, never during one: a renewal's new file is ahead of the
// certificate in use for the length of its grace.
func (s *Store) Resync() (bool, error) {
	c := s.cur.Load()
	if c == nil {
		return false, ErrNoIdentity
	}
	der := c.Cert.Certificate[0]
	have, err := os.ReadFile(filepath.Join(s.dir, config.NodeCertFile))
	if err == nil {
		if blk, _ := pem.Decode(have); blk != nil && bytes.Equal(blk.Bytes, der) {
			return false, nil
		}
	}
	if err := s.Keep(der); err != nil {
		return false, err
	}
	return true, nil
}

// Replace is Prepare, Use and Keep in one: it uses a new certificate (DER) for the node's key from now
// on and writes it to the cluster directory. A write that fails leaves the new certificate in use in
// memory only.
func (s *Store) Replace(certDER []byte) error {
	c, err := s.Prepare(certDER)
	if err != nil {
		return err
	}
	s.Use(c)
	return s.Keep(certDER)
}

// SaveIdentity writes the node's key (0600), certificate and the CA certificate to dir.
func SaveIdentity(dir string, key ed25519.PrivateKey, certDER, caDER []byte) error {
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := mkdirAllOwned(dir); err != nil {
		return err
	}
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{config.NodeKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600},
		{config.ClusterCAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644},
		// The certificate last: its presence is what Joined looks for.
		{config.NodeCertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0o644},
	}
	for _, f := range files {
		if err := writeFile(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return fmt.Errorf("cluster: writing %s: %w", f.name, err)
		}
	}
	return nil
}

// LoadCredentials reads node.key, node.crt and ca.crt from dir.
func LoadCredentials(dir string) (*mesh.Credentials, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, config.NodeCertFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoIdentity
	}
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, config.NodeKeyFile))
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, config.ClusterCAFile))
	if err != nil {
		return nil, err
	}
	certDER, err := decodePEM(certPEM, "CERTIFICATE")
	if err != nil {
		return nil, fmt.Errorf("cluster: %s: %w", config.NodeCertFile, err)
	}
	caDER, err := decodePEM(caPEM, "CERTIFICATE")
	if err != nil {
		return nil, fmt.Errorf("cluster: %s: %w", config.ClusterCAFile, err)
	}
	keyDER, err := decodePEM(keyPEM, "PRIVATE KEY")
	if err != nil {
		return nil, fmt.Errorf("cluster: %s: %w", config.NodeKeyFile, err)
	}
	k, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("cluster: %s: %w", config.NodeKeyFile, err)
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("cluster: %s is a %T key, want Ed25519", config.NodeKeyFile, k)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	return mesh.NewCredentials(certDER, key, ca)
}

func decodePEM(b []byte, typ string) ([]byte, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != typ {
		return nil, fmt.Errorf("no %s block", typ)
	}
	return blk.Bytes, nil
}
