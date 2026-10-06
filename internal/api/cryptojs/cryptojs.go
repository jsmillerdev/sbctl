// Package cryptojs implements the passphrase mode of crypto-js AES, which is the
// format postgres-meta expects in the x-connection-encrypted request header
// (src/server/routes/index.ts: CryptoJS.AES.decrypt(header, CRYPTO_KEY)).
//
// crypto-js with a string key is OpenSSL's "enc -aes-256-cbc -md md5 -salt -a"
// compatible: the output is base64("Salted__" || salt[8] || ciphertext), with the
// 32-byte key and 16-byte IV derived from passphrase+salt by EVP_BytesToKey (MD5,
// one iteration) and PKCS#7 padding. This is a wire-compatibility shim for one
// header, not a recommendation: the key protects a loopback hop only.
package cryptojs

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" //nolint:gosec // required by the crypto-js key derivation
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

const (
	magic   = "Salted__"
	saltLen = 8
	keyLen  = 32
	ivLen   = 16
)

// ErrFormat is returned by Decrypt for input that is not crypto-js passphrase output
// or was encrypted with a different passphrase.
var ErrFormat = errors.New("cryptojs: malformed or wrongly keyed ciphertext")

// Encrypt returns CryptoJS.AES.encrypt(plaintext, passphrase).toString().
func Encrypt(plaintext, passphrase string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}
	return encryptWithSalt(plaintext, passphrase, salt)
}

func encryptWithSalt(plaintext, passphrase string, salt []byte) (string, error) {
	key, iv := evpBytesToKey([]byte(passphrase), salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	buf := append([]byte(plaintext), bytes.Repeat([]byte{byte(pad)}, pad)...)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(buf, buf)
	out := make([]byte, 0, len(magic)+saltLen+len(buf))
	out = append(out, magic...)
	out = append(out, salt...)
	out = append(out, buf...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt is CryptoJS.AES.decrypt(encrypted, passphrase).toString(CryptoJS.enc.Utf8).
func Decrypt(encrypted, passphrase string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil || len(raw) < len(magic)+saltLen+aes.BlockSize || string(raw[:len(magic)]) != magic {
		return "", ErrFormat
	}
	salt, ct := raw[len(magic):len(magic)+saltLen], raw[len(magic)+saltLen:]
	if len(ct)%aes.BlockSize != 0 {
		return "", ErrFormat
	}
	key, iv := evpBytesToKey([]byte(passphrase), salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	pad := int(pt[len(pt)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(pt) {
		return "", ErrFormat
	}
	for _, b := range pt[len(pt)-pad:] {
		if int(b) != pad {
			return "", ErrFormat
		}
	}
	return string(pt[:len(pt)-pad]), nil
}

// evpBytesToKey is OpenSSL's EVP_BytesToKey with MD5 and a count of 1.
func evpBytesToKey(pass, salt []byte) (key, iv []byte) {
	var d, prev []byte
	for len(d) < keyLen+ivLen {
		h := md5.New() //nolint:gosec
		h.Write(prev)
		h.Write(pass)
		h.Write(salt)
		prev = h.Sum(nil)
		d = append(d, prev...)
	}
	return d[:keyLen], d[keyLen : keyLen+ivLen]
}
