package backup

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// The passwords inside base backups are sealed with the node's master key
// (/etc/supavise/master.key), so a backend alone cannot rebuild a node. The key never goes
// to the backend in the clear. With an operator passphrase it can go there encrypted:
// the key and config.toml are sealed with AES-256-GCM under a key that argon2id derives
// from the passphrase, and stored at EscrowKeyFor(key id). The passphrase is the operator's
// to keep; nothing on the node holds it, so the nightly timer never touches the escrow.
//
// Each master key has its own escrow object. A node rebuilt from a bucket gets a new key
// when it is installed, and a single shared object would let that install overwrite the
// only copy of the old node's key, the one the existing backups need. With one object per
// key id, no write can destroy another key's copy.

// EscrowPrefix starts the key of every escrow in the backend. "_node" is not a project
// ref, so prune and the restore-as-new check never mistake it for a project's tree.
const EscrowPrefix = "_node/key-escrow-"

// EscrowKeyFor is the backend key of the escrow of the master key with the given KeyID.
func EscrowKeyFor(keyID string) string { return EscrowPrefix + keyID + ".json" }

// validKeyID reports whether s looks like a KeyID: 16 lowercase hex digits.
func validKeyID(s string) bool {
	if len(s) != 16 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}

// MinPassphraseLen is the shortest passphrase accepted, in characters.
const MinPassphraseLen = 12

const (
	escrowVersion = 1
	escrowAAD     = "supavise/key-escrow/v1"
	kdfArgon2id   = "argon2id"
	cipherAESGCM  = "aes-256-gcm"
)

// escrowKDF are the argon2id cost parameters new escrows use: time 3, 64 MiB, 4 threads,
// RFC 9106's second recommended option. They are stored in the file, so raising them
// later does not strand an older escrow. Tests lower them.
var escrowKDF = struct {
	Time    uint32
	Memory  uint32 // KiB
	Threads uint8
}{Time: 3, Memory: 64 * 1024, Threads: 4}

// Bounds on the parameters read from a stored escrow: the file comes from the backend, and
// a tampered one must not be able to make restore-key allocate without limit.
const (
	maxKDFMemoryKiB = 1 << 20 // 1 GiB
	maxKDFTime      = 16
	maxKDFThreads   = 16
)

// EscrowContents is what the escrow protects.
type EscrowContents struct {
	// MasterKey is the content of master.key: the key as hex.
	MasterKey string `json:"master_key"`
	// ConfigTOML is the node's config.toml, as it was when the escrow was made.
	ConfigTOML string `json:"config_toml,omitempty"`
}

type escrowFile struct {
	Version int       `json:"version"`
	Created time.Time `json:"created"`
	// KeyID identifies the master key inside without revealing it (see KeyID).
	KeyID      string `json:"key_id"`
	KDF        string `json:"kdf"`
	Time       uint32 `json:"time"`
	MemoryKiB  uint32 `json:"memory_kib"`
	Threads    uint8  `json:"threads"`
	Salt       []byte `json:"salt"`
	Cipher     string `json:"cipher"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

// EscrowInfo describes one escrow in the backend without opening it.
type EscrowInfo struct {
	Key     string // backend key
	Created time.Time
	KeyID   string
	Size    int64
}

// KeyID is a short fingerprint of a master key (hex of its file content): the first eight
// bytes of a SHA-256 over it, which reveals nothing about a random 256-bit key. It lets an
// operator check that an escrow and a node hold the same key.
func KeyID(masterKey string) string {
	sum := sha256.Sum256([]byte("supavise/key-id/v1\x00" + strings.TrimSpace(masterKey)))
	return hex.EncodeToString(sum[:8])
}

// CheckPassphrase refuses a passphrase too short to protect a master key.
func CheckPassphrase(p []byte) error {
	if !utf8.Valid(p) {
		return errors.New("the passphrase is not valid UTF-8")
	}
	if n := utf8.RuneCount(p); n < MinPassphraseLen {
		return fmt.Errorf("the passphrase has %d characters; use at least %d (a few random words work well)", n, MinPassphraseLen)
	}
	return nil
}

// SealEscrow encrypts c under passphrase and returns the escrow file.
func SealEscrow(passphrase []byte, c EscrowContents, now time.Time) ([]byte, error) {
	if err := CheckPassphrase(passphrase); err != nil {
		return nil, err
	}
	if strings.TrimSpace(c.MasterKey) == "" {
		return nil, errors.New("backup: the escrow needs a master key")
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	f := escrowFile{Version: escrowVersion, Created: now.UTC(), KeyID: KeyID(c.MasterKey), KDF: kdfArgon2id,
		Time: escrowKDF.Time, MemoryKiB: escrowKDF.Memory, Threads: escrowKDF.Threads, Cipher: cipherAESGCM,
		Salt: make([]byte, 16), Nonce: make([]byte, 12)}
	if _, err := io.ReadFull(rand.Reader, f.Salt); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(rand.Reader, f.Nonce); err != nil {
		return nil, err
	}
	aead, err := escrowAEAD(passphrase, &f)
	if err != nil {
		return nil, err
	}
	f.Ciphertext = aead.Seal(nil, f.Nonce, plain, []byte(escrowAAD))
	return json.MarshalIndent(f, "", "  ")
}

func escrowAEAD(passphrase []byte, f *escrowFile) (cipher.AEAD, error) {
	if f.KDF != kdfArgon2id || f.Cipher != cipherAESGCM {
		return nil, fmt.Errorf("backup: the escrow uses %s and %s, which this version does not know", f.KDF, f.Cipher)
	}
	if f.Time == 0 || f.Time > maxKDFTime || f.MemoryKiB < 8 || f.MemoryKiB > maxKDFMemoryKiB || f.Threads == 0 || f.Threads > maxKDFThreads || len(f.Salt) < 8 {
		return nil, errors.New("backup: the escrow's key derivation parameters are out of range; the file is damaged")
	}
	key := argon2.IDKey(passphrase, f.Salt, f.Time, f.MemoryKiB, f.Threads, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ErrEscrowPassphrase is returned when the passphrase does not open the escrow (or the file
// was altered: AES-GCM cannot tell the two apart).
var ErrEscrowPassphrase = errors.New("backup: wrong passphrase, or the escrow file is damaged")

// OpenEscrow decrypts an escrow file.
func OpenEscrow(passphrase, blob []byte) (*EscrowContents, error) {
	f, err := parseEscrow(blob)
	if err != nil {
		return nil, err
	}
	aead, err := escrowAEAD(passphrase, f)
	if err != nil {
		return nil, err
	}
	if len(f.Nonce) != aead.NonceSize() {
		return nil, errors.New("backup: the escrow file is damaged")
	}
	plain, err := aead.Open(nil, f.Nonce, f.Ciphertext, []byte(escrowAAD))
	if err != nil {
		return nil, ErrEscrowPassphrase
	}
	var c EscrowContents
	if err := json.Unmarshal(plain, &c); err != nil {
		return nil, fmt.Errorf("backup: the escrow's content is not what this version writes: %w", err)
	}
	if KeyID(c.MasterKey) != f.KeyID {
		return nil, errors.New("backup: the escrow's key does not match its key id; the file is damaged")
	}
	return &c, nil
}

func parseEscrow(blob []byte) (*escrowFile, error) {
	var f escrowFile
	if err := json.Unmarshal(blob, &f); err != nil {
		return nil, fmt.Errorf("backup: the escrow file is not valid: %w", err)
	}
	if f.Version != escrowVersion {
		return nil, fmt.Errorf("backup: the escrow file has version %d; this version reads %d", f.Version, escrowVersion)
	}
	return &f, nil
}

// PutKeyEscrow encrypts c under passphrase and stores it in the backend at the key of c's
// master key, and returns that backend key. It replaces an earlier escrow of the same key
// (for example after a config change or a new passphrase) and never touches the escrow of
// another key.
func PutKeyEscrow(ctx context.Context, st Store, passphrase []byte, c EscrowContents, now time.Time) (string, error) {
	blob, err := SealEscrow(passphrase, c, now)
	if err != nil {
		return "", err
	}
	key := EscrowKeyFor(KeyID(c.MasterKey))
	return key, st.Put(ctx, key, bytes.NewReader(append(blob, '\n')))
}

// AmbiguousEscrowError is what GetKeyEscrow returns when the backend holds escrows of more
// than one master key and no key id was named.
type AmbiguousEscrowError struct{ Escrows []EscrowInfo }

func (e *AmbiguousEscrowError) Error() string {
	var parts []string
	for _, i := range e.Escrows {
		parts = append(parts, fmt.Sprintf("%s (made %s)", i.KeyID, i.Created.UTC().Format(time.RFC3339)))
	}
	return fmt.Sprintf("backup: the backend holds escrows of %d different master keys: %s; name the one to open by its key id", len(e.Escrows), strings.Join(parts, ", "))
}

// GetKeyEscrow reads and opens the escrow of the master key with the given key id. An empty
// keyID means the only escrow the backend holds; with several it returns an
// *AmbiguousEscrowError. ErrNotFound means there is none.
func GetKeyEscrow(ctx context.Context, st Store, passphrase []byte, keyID string) (*EscrowContents, error) {
	if keyID == "" {
		all, err := ListKeyEscrows(ctx, st)
		if err != nil {
			return nil, err
		}
		switch len(all) {
		case 0:
			return nil, ErrNotFound
		case 1:
			keyID = all[0].KeyID
		default:
			return nil, &AmbiguousEscrowError{Escrows: all}
		}
	}
	if !validKeyID(keyID) {
		return nil, fmt.Errorf("backup: %q is not a key id (16 hex digits, as `supavise system export-key` prints it)", keyID)
	}
	blob, err := readEscrowObject(ctx, st, EscrowKeyFor(keyID))
	if err != nil {
		return nil, err
	}
	c, err := OpenEscrow(passphrase, blob)
	if err != nil {
		return nil, err
	}
	if KeyID(c.MasterKey) != keyID {
		return nil, errors.New("backup: the escrow stored under this key id holds another key; the file is damaged")
	}
	return c, nil
}

func readEscrowObject(ctx context.Context, st Store, key string) ([]byte, error) {
	rc, err := st.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 4<<20))
}

// ListKeyEscrows describes the escrows in the backend without a passphrase, oldest first.
// The list is empty when there are none.
func ListKeyEscrows(ctx context.Context, st Store) ([]EscrowInfo, error) {
	objs, err := st.List(ctx, "_node/")
	if err != nil {
		return nil, err
	}
	var out []EscrowInfo
	for _, o := range objs {
		if !strings.HasPrefix(o.Key, EscrowPrefix) || !strings.HasSuffix(o.Key, ".json") {
			continue
		}
		blob, err := readEscrowObject(ctx, st, o.Key)
		if err != nil {
			return nil, err
		}
		f, err := parseEscrow(blob)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", o.Key, err)
		}
		out = append(out, EscrowInfo{Key: o.Key, Created: f.Created, KeyID: f.KeyID, Size: int64(len(blob))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}
