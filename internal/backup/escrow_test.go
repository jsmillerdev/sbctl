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
	if all, err := ListKeyEscrows(ctx, e.store); len(all) != 0 || err != nil {
		t.Fatalf("before = %v, %v", all, err)
	}
	if _, err := GetKeyEscrow(ctx, e.store, []byte("a long enough passphrase"), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get before = %v", err)
	}
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	pass := []byte("a long enough passphrase")
	key, err := PutKeyEscrow(ctx, e.store, pass, EscrowContents{MasterKey: testKey, ConfigTOML: "x = 1\n"}, now)
	if err != nil || key != EscrowKeyFor(KeyID(testKey)) {
		t.Fatalf("put = %q, %v", key, err)
	}
	all, err := ListKeyEscrows(ctx, e.store)
	if err != nil || len(all) != 1 || all[0].KeyID != KeyID(testKey) || !all[0].Created.Equal(now) || all[0].Key != key {
		t.Fatalf("list = %+v, %v", all, err)
	}
	got, err := GetKeyEscrow(ctx, e.store, pass, "")
	if err != nil || got.MasterKey != testKey || got.ConfigTOML != "x = 1\n" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	// The escrow is no project's tree: prune and the restore-as-new check ignore it.
	if refs, _ := e.store.ListDirs(ctx, ""); len(refs) != 1 || validRef(refs[0]) == nil {
		t.Fatalf("top level = %v", refs)
	}
}

// A second node's key (an install on a rebuilt server) gets its own escrow: the first
// key's copy, which the existing backups need, is untouched and still opens.
func TestEscrowsOfDifferentKeysCoexist(t *testing.T) {
	cheapKDF(t)
	e := newTestEnv(t)
	ctx := context.Background()
	oldPass, newPass := []byte("the old node's passphrase"), []byte("the new node's passphrase")
	newKey := strings.Repeat("ab", 32)
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	if _, err := PutKeyEscrow(ctx, e.store, oldPass, EscrowContents{MasterKey: testKey, ConfigTOML: "old = 1\n"}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := PutKeyEscrow(ctx, e.store, newPass, EscrowContents{MasterKey: newKey, ConfigTOML: "new = 1\n"}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	all, err := ListKeyEscrows(ctx, e.store)
	if err != nil || len(all) != 2 || all[0].KeyID != KeyID(testKey) || all[1].KeyID != KeyID(newKey) {
		t.Fatalf("list = %+v, %v", all, err)
	}
	// Without a key id the choice is the operator's.
	var amb *AmbiguousEscrowError
	if _, err := GetKeyEscrow(ctx, e.store, oldPass, ""); !errors.As(err, &amb) || len(amb.Escrows) != 2 {
		t.Fatalf("get without a key id = %v, want an AmbiguousEscrowError", err)
	}
	got, err := GetKeyEscrow(ctx, e.store, oldPass, KeyID(testKey))
	if err != nil || got.MasterKey != testKey || got.ConfigTOML != "old = 1\n" {
		t.Fatalf("old escrow = %+v, %v", got, err)
	}
	if got, err = GetKeyEscrow(ctx, e.store, newPass, KeyID(newKey)); err != nil || got.MasterKey != newKey {
		t.Fatalf("new escrow = %+v, %v", got, err)
	}
	// Writing the same key again replaces only its own copy.
	if _, err := PutKeyEscrow(ctx, e.store, newPass, EscrowContents{MasterKey: newKey, ConfigTOML: "new = 2\n"}, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if all, _ = ListKeyEscrows(ctx, e.store); len(all) != 2 {
		t.Fatalf("after a rewrite: %+v", all)
	}
	if got, err = GetKeyEscrow(ctx, e.store, oldPass, KeyID(testKey)); err != nil || got.ConfigTOML != "old = 1\n" {
		t.Fatalf("old escrow after the rewrite = %+v, %v", got, err)
	}
	if _, err := GetKeyEscrow(ctx, e.store, oldPass, "not-a-key-id"); err == nil {
		t.Error("a malformed key id was accepted")
	}
	if _, err := GetKeyEscrow(ctx, e.store, oldPass, "0123456789abcdef"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown key id = %v", err)
	}
}
