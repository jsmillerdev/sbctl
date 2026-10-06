package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// encryptConnString encrypts s the way crypto-js does for AES.encrypt(s, passphrase) and
// postgres-meta expects in x-connection-encrypted: the OpenSSL "Salted__" container, a key and
// IV derived from the passphrase and an 8-byte salt with EVP_BytesToKey (MD5, one iteration),
// AES-256-CBC with PKCS#7 padding, base64.
func encryptConnString(s, passphrase string) (string, error) {
	salt := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}
	key, iv := evpBytesToKey([]byte(passphrase), salt, 32, 16)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	pad := aes.BlockSize - len(s)%aes.BlockSize
	plain := append([]byte(s), bytes.Repeat([]byte{byte(pad)}, pad)...)
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, plain)
	out := append(append([]byte("Salted__"), salt...), ct...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// decryptConnString is the inverse, used by tests to check what the mock sends.
func decryptConnString(enc, passphrase string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw) < 16 || string(raw[:8]) != "Salted__" {
		return "", errors.New("not a crypto-js passphrase ciphertext")
	}
	key, iv := evpBytesToKey([]byte(passphrase), raw[8:16], 32, 16)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	ct := raw[16:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return "", errors.New("bad ciphertext length")
	}
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ct)
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(plain) {
		return "", errors.New("bad padding (wrong passphrase?)")
	}
	return string(plain[:len(plain)-pad]), nil
}

func evpBytesToKey(pass, salt []byte, keyLen, ivLen int) (key, iv []byte) {
	var out, prev []byte
	for len(out) < keyLen+ivLen {
		h := md5.New()
		h.Write(prev)
		h.Write(pass)
		h.Write(salt)
		prev = h.Sum(nil)
		out = append(out, prev...)
	}
	return out[:keyLen], out[keyLen : keyLen+ivLen]
}

// connectionString is the opaque value in a project's `connectionString` field: the DB URL,
// encrypted for postgres-meta. Studio sends it back unchanged as x-connection-encrypted.
func (s *server) connectionString(p *Project) string {
	enc, err := encryptConnString(p.DBURL, s.cfg.PgmetaCryptoKey)
	if err != nil {
		return ""
	}
	return enc
}

// pgMetaQuery implements POST /platform/pg-meta/{ref}/query: it builds the connection header
// from the project (as the real API will) and forwards the body to postgres-meta, returning its
// status and body unchanged.
func (s *server) pgMetaQuery(w *respWriter, r *http.Request, c *reqCtx) {
	p, _ := s.project(c.params["ref"])
	if p == nil {
		w.message(http.StatusNotFound, "Project not found")
		return
	}
	if s.cfg.PgmetaURL == "" {
		w.message(http.StatusServiceUnavailable, "pgmeta_url is not configured")
		return
	}
	if r.Header.Get("X-Connection-Encrypted") == "" {
		// Studio never sends a request without it once the project has a connectionString.
		w.message(http.StatusBadRequest, "missing x-connection-encrypted")
		return
	}
	var in struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(c.body, &in)
	c.sql = in.Query
	if len(c.sql) > 300 {
		c.sql = c.sql[:300]
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(s.cfg.PgmetaURL, "/")+"/query", bytes.NewReader(c.body))
	if err != nil {
		w.message(http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Connection-Encrypted", s.connectionString(p))
	if app := r.Header.Get("X-Pg-Application-Name"); app != "" {
		req.Header.Set("X-Pg-Application-Name", app)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		w.message(http.StatusBadGateway, "postgres-meta unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
