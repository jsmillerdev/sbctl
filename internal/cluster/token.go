package cluster

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// TokenPrefix starts every join token. The rest is base64url of a JSON object.
const TokenPrefix = "svj1."

// tokenLabel is the derivation label of a token's secret; the token's id completes it.
const tokenLabel = "supavise/join-token/v1/"

// Token is what `supavise node token` prints and `supavise node join` reads.
type Token struct {
	V int `json:"v"`
	// ID names the registry row of the token.
	ID string `json:"id"`
	// Leader is host:port of the leader's peer listener.
	Leader string `json:"leader"`
	// CAFpr is the hex SHA-256 of the cluster CA certificate. The joiner accepts only a leader whose
	// chain ends in a root with this hash.
	CAFpr string `json:"ca_fpr"`
	// Secret is the token's secret, base64url. It proves the joiner holds the token; it never
	// travels, only a keyed hash of what the joiner sends does.
	Secret string `json:"secret"`
	// Exp is when the token stops working, in Unix seconds.
	Exp int64 `json:"exp"`
	// Region is the joiner's default region when it names none. The leader accepts any known region.
	Region string `json:"region,omitempty"`
	// Name is the one node name the token admits, repeated here for the joiner; the registry row
	// is the authority.
	Name string `json:"name,omitempty"`
}

// Expires is Exp as a time.
func (t Token) Expires() time.Time { return time.Unix(t.Exp, 0).UTC() }

// Encode writes the token as svj1.<base64url>.
func (t Token) Encode() string {
	b, err := json.Marshal(t)
	if err != nil { // plain strings and numbers
		panic(err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ParseToken reads a token as `node token` printed it. Surrounding white space is ignored, so a
// token read from a file with a trailing newline parses.
func ParseToken(s string) (Token, error) {
	s = strings.TrimSpace(s)
	body, ok := strings.CutPrefix(s, TokenPrefix)
	if !ok {
		return Token{}, errors.New("cluster: that is not a join token (it should start with svj1.)")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Token{}, errors.New("cluster: the join token is damaged (not base64url)")
	}
	var t Token
	if err := json.Unmarshal(raw, &t); err != nil {
		return Token{}, errors.New("cluster: the join token is damaged (not JSON)")
	}
	if t.V != 1 {
		return Token{}, fmt.Errorf("cluster: the join token is version %d; this release reads version 1", t.V)
	}
	if t.ID == "" || t.CAFpr == "" || t.Secret == "" {
		return Token{}, errors.New("cluster: the join token lacks its id, CA fingerprint or secret")
	}
	if _, _, err := net.SplitHostPort(t.Leader); err != nil {
		return Token{}, fmt.Errorf("cluster: the join token's leader address %q is not host:port", t.Leader)
	}
	return t, nil
}

// newTokenID is a random id for a token row: 13 lower case letters and digits.
func newTokenID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// tokenSecret is the secret of the token with this id: derived from the master key, so that the
// leader, which stores only its hash, can still check a keyed proof of it, and so that nobody who can
// read the registry (the replicated database, a backup of it) can forge one.
func tokenSecret(d Deriver, id string) []byte { return d.Derive(tokenLabel + id) }

// SecretHash is what the registry keeps of a token's secret.
func SecretHash(secret []byte) []byte {
	h := sha256.Sum256(secret)
	return h[:]
}

// Proof is the joiner's proof of the token: HMAC-SHA256 keyed with the secret over the nonce the
// leader gave, the certificate request and the node name. Each part is length-prefixed, so that no
// request can be moved across the boundary between two parts.
func Proof(secret, nonce, csr []byte, name string) []byte {
	m := hmac.New(sha256.New, secret)
	for _, part := range [][]byte{nonce, csr, []byte(name)} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(part)))
		m.Write(n[:])
		m.Write(part)
	}
	return m.Sum(nil)
}

// proofOK compares in constant time.
func proofOK(got, want []byte) bool { return subtle.ConstantTimeCompare(got, want) == 1 }
