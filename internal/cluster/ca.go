package cluster

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/registry"
)

// CALabel is the label the cluster CA's key is derived under (secrets.AESGCM.Derive). Rotating the
// CA is a new label ("v2") and a reissue of every node certificate.
const CALabel = "supavise/cluster-ca/v1"

// Deriver is the part of the node's secrets the CA needs: a key bound to the master key and a
// label. *secrets.AESGCM is one.
type Deriver interface {
	Derive(label string) []byte
}

// The CA certificate has a fixed subject, serial and validity so that every node that holds the
// master key computes the same bytes, and a pin of their hash means the same on all of them.
var (
	caNotBefore = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	caNotAfter  = time.Date(2045, 1, 1, 0, 0, 0, 0, time.UTC)
)

// NodeCertTTL is how long a node certificate is valid. A node renews it when RenewBefore is left.
const (
	NodeCertTTL = 365 * 24 * time.Hour
	RenewBefore = 30 * 24 * time.Hour
	// certBackdate is how far before issue a certificate starts, so that a node whose clock runs
	// slightly behind its issuer's still accepts it.
	certBackdate = 5 * time.Minute
)

// CA is the cluster's certificate authority: an Ed25519 key derived from the master key and the
// self-signed certificate over it. Every node can build it, so a promoted node issues certificates
// and a new node can join after a failover.
type CA struct {
	key  ed25519.PrivateKey
	cert *x509.Certificate
}

// NewCA derives the CA from the master key. The result is the same on every node that holds the key.
func NewCA(d Deriver) (*CA, error) {
	seed, err := caSeed(d.Derive(CALabel))
	if err != nil {
		return nil, err
	}
	key := ed25519.NewKeyFromSeed(seed)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Supavise cluster CA", Organization: []string{"Supavise"}},
		NotBefore:             caNotBefore,
		NotAfter:              caNotAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("cluster: building the CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{key: key, cert: cert}, nil
}

// caSeed turns what Derive returned into an Ed25519 seed: 32 bytes are used as they are, anything
// else goes through HKDF-SHA256 so that a Derive of another length still gives one seed.
func caSeed(b []byte) ([]byte, error) {
	switch {
	case len(b) == ed25519.SeedSize:
		return b, nil
	case len(b) == 0:
		return nil, errors.New("cluster: the master key derived nothing")
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := io.ReadFull(hkdf.Expand(sha256.New, b, []byte(CALabel+"/seed")), seed); err != nil {
		return nil, err
	}
	return seed, nil
}

// Cert is the CA certificate.
func (ca *CA) Cert() *x509.Certificate { return ca.cert }

// DER is the CA certificate in DER form: the bytes whose hash is the pin of a join token.
func (ca *CA) DER() []byte { return ca.cert.Raw }

// PEM is the CA certificate as a PEM block.
func (ca *CA) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// Fingerprint is the hex SHA-256 of the CA certificate, the pin a join token carries.
func (ca *CA) Fingerprint() string { return Fingerprint(ca.cert.Raw) }

// Fingerprint is the hex SHA-256 of a DER certificate.
func Fingerprint(der []byte) string { return mesh.Fingerprint(der) }

// Issued is a signed node certificate.
type Issued struct {
	DER      []byte
	Serial   string
	NotAfter time.Time
}

// PEM is the certificate as a PEM block.
func (i *Issued) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: i.DER})
}

// Issue signs a certificate for the node's public key: SAN URI supavise://node/<id>, valid for
// ttl from now, usable as a server and as a client certificate.
func (ca *CA) Issue(pub ed25519.PublicKey, nodeID string, now time.Time, ttl time.Duration) (*Issued, error) {
	if !registry.ValidNodeID(nodeID) {
		return nil, fmt.Errorf("cluster: %q is not a node id", nodeID)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	serial.Add(serial, big.NewInt(2)) // never 1, the CA's, and never 0
	uri, _ := url.Parse(mesh.NodeURI(nodeID))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "supavise node " + nodeID, Organization: []string{"Supavise"}},
		URIs:         []*url.URL{uri},
		NotBefore:    now.Add(-certBackdate).UTC(),
		NotAfter:     now.Add(ttl).UTC(),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		return nil, fmt.Errorf("cluster: signing the certificate of %s: %w", nodeID, err)
	}
	return &Issued{DER: der, Serial: mesh.SerialString(serial), NotAfter: tmpl.NotAfter}, nil
}

// IssueCSR signs the certificate request csr (DER) for node nodeID. Only the public key of the
// request is used: the node's name and address are the leader's to set, not the requester's. The
// key must be Ed25519 and the request must be signed with it.
func (ca *CA) IssueCSR(csr []byte, nodeID string, now time.Time, ttl time.Duration) (*Issued, error) {
	req, err := x509.ParseCertificateRequest(csr)
	if err != nil {
		return nil, fmt.Errorf("cluster: the certificate request is not valid: %w", err)
	}
	if err := req.CheckSignature(); err != nil {
		return nil, fmt.Errorf("cluster: the certificate request is not signed by its key: %w", err)
	}
	pub, ok := req.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("cluster: the certificate request has a %T key, want Ed25519", req.PublicKey)
	}
	return ca.Issue(pub, nodeID, now, ttl)
}

// NewCSR returns a certificate request (DER) for key.
func NewCSR(key ed25519.PrivateKey, name string) ([]byte, error) {
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: name},
	}, key)
}
