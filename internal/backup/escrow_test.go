package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const testKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func cheapKDF(t *testing.T) {
	t.Helper()
	old := escrowKDF
	escrowKDF.Time, escrowKDF.Memory, escrowKDF.Threads = 1, 64, 1
	t.Cleanup(func() { escrowKDF = old })
}

func TestEscrowRoundTrip(t *testing.T) {
	cheapKDF(t)
	pass := []byte("correct horse battery staple")
	c := EscrowContents{MasterKey: testKey, ConfigTOML: "domain = \"example.com\"\n"}
	blob, err := SealEscrow(pass, c, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte(testKey)) || bytes.Contains(blob, []byte("example.com")) {
		t.Fatal("the escrow file holds the key or the config in the clear")
	}
	got, err := OpenEscrow(pass, blob)
	if err != nil || *got != c {
		t.Fatalf("open = %+v, %v", got, err)
	}
	if _, err := OpenEscrow([]byte("another passphrase, wrong"), blob); !errors.Is(err, ErrEscrowPassphrase) {
		t.Fatalf("wrong passphrase = %v", err)
	}
	// Two escrows of the same key differ (fresh salt and nonce).
	blob2, _ := SealEscrow(pass, c, time.Now())
	var a, b escrowFile
	_ = json.Unmarshal(blob, &a)
	_ = json.Unmarshal(blob2, &b)
	if bytes.Equal(a.Salt, b.Salt) || bytes.Equal(a.Nonce, b.Nonce) || a.KeyID != b.KeyID || a.KeyID != KeyID(testKey) {
		t.Fatal("salt and nonce must be fresh and the key id stable")
	}
}

func TestEscrowRefusesWeakPassphrasesAndTamperedFiles(t *testing.T) {
	cheapKDF(t)
	if _, err := SealEscrow([]byte("short"), EscrowContents{MasterKey: testKey}, time.Now()); err == nil || !strings.Contains(err.Error(), "at least 12") {
		t.Fatalf("short passphrase = %v", err)
	}
	if _, err := SealEscrow([]byte("a long enough passphrase"), EscrowContents{}, time.Now()); err == nil {
		t.Fatal("an escrow without a key was sealed")
	}
	pass := []byte("a long enough passphrase")
	blob, _ := SealEscrow(pass, EscrowContents{MasterKey: testKey}, time.Now())

	var f escrowFile
	_ = json.Unmarshal(blob, &f)
	for name, mut := range map[string]func(*escrowFile){
		"huge memory":     func(f *escrowFile) { f.MemoryKiB = 1 << 30 },
		"huge time":       func(f *escrowFile) { f.Time = 1000 },
		"zero threads":    func(f *escrowFile) { f.Threads = 0 },
		"other kdf":       func(f *escrowFile) { f.KDF = "pbkdf2" },
		"flipped data":    func(f *escrowFile) { f.Ciphertext[0] ^= 1 },
		"different kdf t": func(f *escrowFile) { f.Time++ },
		"other key id":    func(f *escrowFile) { f.KeyID = "0000000000000000" },
	} {
		g := f
		g.Ciphertext = append([]byte(nil), f.Ciphertext...)
		mut(&g)
		b, _ := json.Marshal(g)
		if _, err := OpenEscrow(pass, b); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	if _, err := OpenEscrow(pass, []byte("not json")); err == nil {
		t.Error("garbage opened")
	}
}

func TestEscrowInTheBackend(t *testing.T) {
	cheapKDF(t)
	e := newTestEnv(t)
	ctx := context.Background()
	if info, err := KeyEscrowInfo(ctx, e.store); info != nil || err != nil {
		t.Fatalf("before = %v, %v", info, err)
	}
	if _, err := GetKeyEscrow(ctx, e.store, []byte("a long enough passphrase")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get before = %v", err)
	}
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	pass := []byte("a long enough passphrase")
	if err := PutKeyEscrow(ctx, e.store, pass, EscrowContents{MasterKey: testKey, ConfigTOML: "x = 1\n"}, now); err != nil {
		t.Fatal(err)
	}
	info, err := KeyEscrowInfo(ctx, e.store)
	if err != nil || info == nil || info.KeyID != KeyID(testKey) || !info.Created.Equal(now) {
		t.Fatalf("info = %+v, %v", info, err)
	}
	got, err := GetKeyEscrow(ctx, e.store, pass)
	if err != nil || got.MasterKey != testKey || got.ConfigTOML != "x = 1\n" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	// The escrow is no project's tree: prune and the restore-as-new check ignore it.
	if refs, _ := e.store.ListDirs(ctx, ""); len(refs) != 1 || validRef(refs[0]) == nil {
		t.Fatalf("top level = %v", refs)
	}
}
