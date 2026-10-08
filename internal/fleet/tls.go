package fleet

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/supavise/supavise/internal/config"
)

const (
	downstreamCertValidity = 10 * 365 * 24 * time.Hour
	// downstreamCertRenewBefore is how long before it expires the certificate is replaced.
	downstreamCertRenewBefore = 30 * 24 * time.Hour
)

// DownstreamCertPaths are the certificate and key Supavisor presents to pooler clients
// (GLOBAL_DOWNSTREAM_CERT_PATH and GLOBAL_DOWNSTREAM_KEY_PATH), inside the unit's own state
// directory because that is the only place of the node's state the unit sees.
func DownstreamCertPaths(cfg *config.Config) (cert, key string) {
	dir := filepath.Join(cfg.Paths().System(config.SvcSupavisor), "tls")
	return filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
}

// ensureDownstreamCert makes sure a usable pooler certificate exists and returns the
// SHA-256 of its DER encoding (hex), which the unit's environment carries so that Start
// restarts Supavisor when the certificate is replaced.
//
// The certificate is self-signed and node-generated: Supavisor serves TLS on the Postgres
// wire protocol (SSLRequest) from files, and the CertMagic certificates live outside the
// unit's namespace. Clients that verify the chain (sslmode=verify-ca or verify-full) must
// be given this certificate; sslmode=require and prefer, which is what psql, pgx and the
// Supabase CLI use by default, encrypt without verifying it. An existing certificate is
// kept until it is within 30 days of its end, does not list the pooler host, or its key
// does not match.
func ensureDownstreamCert(cfg *config.Config, now time.Time) (string, error) {
	crt, key := DownstreamCertPaths(cfg)
	host := cfg.PoolerHost()
	if sum, ok := validDownstreamCert(crt, key, host, now); ok {
		return sum, nil
	}
	if err := os.MkdirAll(filepath.Dir(crt), 0o750); err != nil {
		return "", fmt.Errorf("fleet: pooler certificate: %w", err)
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return "", err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{"supavise"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(downstreamCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host, "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		return "", err
	}
	// The key first: a crash between the two writes leaves a pair that does not match, which
	// validDownstreamCert detects on the next call.
	if err := writeFileAtomic(key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		return "", err
	}
	if err := writeFileAtomic(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o640); err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// validDownstreamCert reports whether the files hold a pair that matches, lists host, and
// stays valid for at least downstreamCertRenewBefore more.
func validDownstreamCert(crtPath, keyPath, host string, now time.Time) (string, bool) {
	pair, err := tls.LoadX509KeyPair(crtPath, keyPath)
	if err != nil || len(pair.Certificate) == 0 {
		return "", false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "", false
	}
	if now.Add(downstreamCertRenewBefore).After(leaf.NotAfter) || now.Before(leaf.NotBefore) || leaf.VerifyHostname(host) != nil {
		return "", false
	}
	sum := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(sum[:]), true
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
