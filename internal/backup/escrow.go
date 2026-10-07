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
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// The passwords inside base backups are sealed with the node's master key
// (/etc/supavise/master.key), so a backend alone cannot rebuild a node. The key never goes
// to the backend in the clear. With an operator passphrase it can go there encrypted:
// the key and config.toml are sealed with AES-256-GCM under a key that argon2id derives
// from the passphrase, and stored once at EscrowKey. The passphrase is the operator's to
// keep; nothing on the node holds it, so the nightly timer never touches the escrow.

// EscrowKey is the escrow's key in the backend. "_node" is not a project ref, so prune and
// the restore-as-new check never mistake it for a project's tree.
const EscrowKey = "_node/key-escrow.json"

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

// EscrowInfo describes the escrow in the backend without opening it.
type EscrowInfo struct {
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

// PutKeyEscrow encrypts c under passphrase and stores it in the backend, replacing an
// earlier escrow.
func PutKeyEscrow(ctx context.Context, st Store, passphrase []byte, c EscrowContents, now time.Time) error {
	blob, err := SealEscrow(passphrase, c, now)
	if err != nil {
		return err
	}
	return st.Put(ctx, EscrowKey, bytes.NewReader(append(blob, '\n')))
}

// GetKeyEscrow reads and opens the escrow of the backend. ErrNotFound means there is none.
func GetKeyEscrow(ctx context.Context, st Store, passphrase []byte) (*EscrowContents, error) {
	rc, err := st.Get(ctx, EscrowKey)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	blob, err := io.ReadAll(io.LimitReader(rc, 4<<20))
	if err != nil {
		return nil, err
	}
	return OpenEscrow(passphrase, blob)
}

// KeyEscrowInfo describes the backend's escrow without a passphrase, or returns nil when
// there is none.
func KeyEscrowInfo(ctx context.Context, st Store) (*EscrowInfo, error) {
	rc, err := st.Get(ctx, EscrowKey)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	blob, err := io.ReadAll(io.LimitReader(rc, 4<<20))
	if err != nil {
		return nil, err
	}
	f, err := parseEscrow(blob)
	if err != nil {
		return nil, err
	}
	return &EscrowInfo{Created: f.Created, KeyID: f.KeyID, Size: int64(len(blob))}, nil
}
