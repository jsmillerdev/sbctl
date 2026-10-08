package mesh

import (
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// NodeURIScheme and NodeURIHost make the subject alternative name of a node certificate:
// supavise://node/<id>. The id in the name is the node's identity; no other field is read.
const (
	NodeURIScheme = "supavise"
	NodeURIHost   = "node"
)

// NodeURI is the URI a certificate of node id carries as its subject alternative name.
func NodeURI(id string) string { return NodeURIScheme + "://" + NodeURIHost + "/" + id }

// NodeIDOf returns the node id a certificate names, from its supavise://node/<id> URI.
func NodeIDOf(cert *x509.Certificate) (string, bool) {
	for _, u := range cert.URIs {
		if u.Scheme == NodeURIScheme && u.Host == NodeURIHost && len(u.Path) > 1 && strings.Count(u.Path, "/") == 1 {
			return u.Path[1:], true
		}
	}
	return "", false
}

// Fingerprint is the hex SHA-256 of a DER certificate: the pin of a join token.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// SerialString is the form a certificate serial takes in registry.Node.CertSerial: lower case hex.
func SerialString(n *big.Int) string { return n.Text(16) }

// Credentials are what a node presents to its peers and trusts them against: its certificate
// (the leaf, then the CA certificate, so that a joiner can pin the root it is shown) and the
// cluster CA.
type Credentials struct {
	Cert tls.Certificate
	CA   *x509.Certificate
	// NodeID, Serial and NotAfter describe the leaf.
	NodeID   string
	Serial   string
	NotAfter time.Time
}

// NewCredentials builds Credentials from a signed leaf (DER), its private key and the CA.
func NewCredentials(leafDER []byte, key crypto.PrivateKey, ca *x509.Certificate) (*Credentials, error) {
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return nil, fmt.Errorf("mesh: the node certificate is not valid: %w", err)
	}
	id, ok := NodeIDOf(leaf)
	if !ok {
		return nil, errors.New("mesh: the node certificate names no node (no supavise://node/<id> URI)")
	}
	return &Credentials{
		Cert:     tls.Certificate{Certificate: [][]byte{leafDER, ca.Raw}, PrivateKey: key, Leaf: leaf},
		CA:       ca,
		NodeID:   id,
		Serial:   SerialString(leaf.SerialNumber),
		NotAfter: leaf.NotAfter,
	}, nil
}

// ErrNotAdmitted is wrapped by the error of a handshake whose peer holds a certificate the
// cluster no longer accepts: the node was removed, or the serial is not the registry's.
var ErrNotAdmitted = errors.New("mesh: the registry does not admit that certificate")

// AdmitFunc is the registry's say on a certificate whose chain verified: the state of the node
// that the certificate names, or an error wrapping ErrNotAdmitted.
type AdmitFunc func(nodeID, serial string) (registry.NodeState, error)

// AdmitFromTopology admits a certificate when its node has a row that is joining, active or
// fenced, and the serial is the one recorded in it. A joining node must reach the leader to
// stream its first copy of the system cluster; a fenced node must hear that it is fenced. What
// each state may then do is the stream and request policy's business (Authorizer, rpcAllowed).
// A node that is left, unknown or holds an older certificate is refused at the handshake.
func AdmitFromTopology(t Topology) AdmitFunc {
	return func(id, serial string) (registry.NodeState, error) {
		for _, n := range t.Nodes() {
			if n.ID != id {
				continue
			}
			switch n.State {
			case registry.NodeJoining, registry.NodeActive, registry.NodeFenced:
			default:
				return "", fmt.Errorf("%w: node %s is %s", ErrNotAdmitted, id, n.State)
			}
			if n.CertSerial == "" || n.CertSerial != serial {
				return "", fmt.Errorf("%w: the serial of node %s is not the registry's", ErrNotAdmitted, id)
			}
			return n.State, nil
		}
		return "", fmt.Errorf("%w: no node %s", ErrNotAdmitted, id)
	}
}

// checkPeer verifies what a peer presented: the leaf chains to ca for usage and is valid at now,
// it names a node, and the registry admits that node and serial. It returns the node id.
func checkPeer(certs []*x509.Certificate, ca *x509.Certificate, usage x509.ExtKeyUsage, admit AdmitFunc, now time.Time) (string, error) {
	if len(certs) == 0 {
		return "", errors.New("mesh: the peer presented no certificate")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
		return "", fmt.Errorf("mesh: the peer's certificate does not verify against the cluster CA: %w", err)
	}
	id, ok := NodeIDOf(certs[0])
	if !ok {
		return "", errors.New("mesh: the peer's certificate names no node")
	}
	if admit != nil {
		if _, err := admit(id, SerialString(certs[0].SerialNumber)); err != nil {
			return "", err
		}
	}
	return id, nil
}

// serverTLS is the configuration of the peer listener: TLS 1.3, the mesh's ALPN, the node's
// certificate, and a client certificate that is asked for and verified when given. A client with
// none is a joiner; the request policy lets it reach the join endpoints and nothing else.
func serverTLS(creds func() *Credentials, admit AdmitFunc, now func() time.Time) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{ALPN},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c := creds()
			if c == nil {
				return nil, errors.New("mesh: this node has no certificate")
			}
			return &c.Cert, nil
		},
		ClientAuth: tls.RequestClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return nil
			}
			c := creds()
			if c == nil {
				return errors.New("mesh: this node has no certificate")
			}
			_, err := checkPeer(cs.PeerCertificates, c.CA, x509.ExtKeyUsageClientAuth, admit, now())
			return err
		},
	}
}

// clientTLS is the configuration for dialing node want: it presents this node's certificate and
// accepts the server only when it holds a certificate of the cluster CA that names want.
// InsecureSkipVerify turns off the standard host name check, which has no host to check (peers
// are dialed by address); VerifyConnection does the verification.
func clientTLS(creds func() *Credentials, want string, admit AdmitFunc, now func() time.Time) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{ALPN},
		InsecureSkipVerify: true,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			if c := creds(); c != nil {
				return &c.Cert, nil
			}
			return &tls.Certificate{}, nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			c := creds()
			if c == nil {
				return errors.New("mesh: this node has no certificate")
			}
			id, err := checkPeer(cs.PeerCertificates, c.CA, x509.ExtKeyUsageServerAuth, admit, now())
			if err != nil {
				return err
			}
			if want != "" && id != want {
				return fmt.Errorf("mesh: dialed node %s and reached node %s", want, id)
			}
			return nil
		},
	}
}

// PinnedTLS is the configuration of a joiner, which has no certificate and does not yet know the
// CA: it accepts the server when the root of the chain the server presents hashes to caFingerprint
// (hex SHA-256 of the CA certificate, the pin in the join token) and the leaf chains to that root.
// An impostor without the master key cannot present such a root. creds, when not nil, supplies
// a client certificate (a fenced node asking to rejoin has one).
func PinnedTLS(caFingerprint string, creds func() *Credentials, now func() time.Time) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{ALPN},
		InsecureSkipVerify: true,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			if creds != nil {
				if c := creds(); c != nil {
					return &c.Cert, nil
				}
			}
			return &tls.Certificate{}, nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			certs := cs.PeerCertificates
			if len(certs) < 2 {
				return errors.New("mesh: the server did not present its CA certificate")
			}
			root := certs[len(certs)-1]
			if !strings.EqualFold(Fingerprint(root.Raw), caFingerprint) {
				return errors.New("mesh: the server's CA is not the one the join token pins")
			}
			pool := x509.NewCertPool()
			pool.AddCert(root)
			if _, err := certs[0].Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
				return fmt.Errorf("mesh: the server's certificate does not verify against the pinned CA: %w", err)
			}
			if _, ok := NodeIDOf(certs[0]); !ok {
				return errors.New("mesh: the server's certificate names no node")
			}
			return nil
		},
	}
}
