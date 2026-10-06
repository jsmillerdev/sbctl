package secrets

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestSealOpenAndKeyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "etc", "master.key")
	s, err := LoadOrCreate(p)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode: %v %v", fi.Mode(), err)
	}
	ct, err := s.Seal([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := LoadOrCreate(p)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := s2.Open(ct)
	if err != nil || string(pt) != "hello" {
		t.Fatalf("open: %q %v", pt, err)
	}
	ct[len(ct)-1] ^= 1
	if _, err := s2.Open(ct); err != ErrCorrupt {
		t.Fatalf("tamper: %v", err)
	}
}

func TestKeyFormats(t *testing.T) {
	if r := NewRef(); !ValidRef(r) || r == "system" {
		t.Fatal(r)
	}
	if ValidRef("system") || ValidRef("ABCDEFGHIJKLMNOPQRST") {
		t.Fatal("bad ref accepted")
	}
	if !regexp.MustCompile(`^sbp_[0-9a-f]{40}$`).MatchString(NewPAT()) {
		t.Fatal(NewPAT())
	}
	if !regexp.MustCompile(`^sb_publishable_[1-9A-HJ-NP-Za-km-z]+$`).MatchString(NewPublishableKey()) {
		t.Fatal(NewPublishableKey())
	}
	if len(NewJWTSecret()) != 40 || len(NewPGSodiumRootKey()) != 64 {
		t.Fatal("lengths")
	}
	if Base58([]byte{0, 0, 1}) != "112" {
		t.Fatal(Base58([]byte{0, 0, 1}))
	}
}

func TestProjectKeys(t *testing.T) {
	ref := NewRef()
	k, err := NewProjectKeys(ref, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	claims, err := ParseHS256(k.ServiceRoleKey, k.JWTSecret)
	if err != nil || claims["role"] != RoleServiceRole || claims["ref"] != ref || claims["iss"] != "supabase" {
		t.Fatalf("%v %v", claims, err)
	}
	if _, err := ParseHS256(k.AnonKey, "wrong"); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if got := KeysFromMap(k.Map()); *got != *k {
		t.Fatal("map round trip")
	}
}
