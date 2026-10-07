package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/internal/backup"
	"github.com/jsmillerdev/supavise/internal/config"
)

const keysTestKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// keysNode writes a config and master key into a temp dir and returns the config path.
func keysNode(t *testing.T) (cfgPath, keyPath, dir string) {
	t.Helper()
	dir = t.TempDir()
	keyPath = filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, []byte(keysTestKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(dir, "config.toml")
	body := "state_dir = " + `"` + filepath.Join(dir, "state") + `"` + "\nkey_path = " + `"` + keyPath + `"` + "\n\n[backup]\nbackend = " + `"file://` + filepath.Join(dir, "backups") + `"` + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, keyPath, dir
}

func writePass(t *testing.T, dir, pass string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, "pass")
	if err := os.WriteFile(p, []byte(pass+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExportKeyPrintsTheKeyAndTheConfig(t *testing.T) {
	cfg, _, _ := keysNode(t)
	out, err := runRoot(t, "--config", cfg, "system", "export-key")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"master_key = " + keysTestKey, "key_id = ", "state_dir =", "WARNING", "offline"} {
		if !strings.Contains(out, want) {
			t.Errorf("export-key output lacks %q:\n%s", want, out)
		}
	}
	out, err = runRoot(t, "--config", cfg, "system", "export-key", "--key-only")
	if err != nil || strings.TrimSpace(out) != keysTestKey {
		t.Fatalf("--key-only = %q, %v", out, err)
	}
}

func TestExportKeyNeedsTheKey(t *testing.T) {
	cfg, key, _ := keysNode(t)
	os.Remove(key)
	if _, err := runRoot(t, "--config", cfg, "system", "export-key"); err == nil || !strings.Contains(err.Error(), "cannot read the master key") {
		t.Fatalf("err = %v", err)
	}
}

func TestEscrowAndRestoreKeyRoundTrip(t *testing.T) {
	cfg, key, dir := keysNode(t)
	pass := writePass(t, dir, "correct horse battery staple", 0o600)

	// Before an escrow exists, `backups status` says the key is not backed up.
	out, err := runRoot(t, "--config", cfg, "backups")
	if err != nil || !strings.Contains(out, "NOT backed up") || !strings.Contains(out, "export-key") {
		t.Fatalf("status before = %q, %v", out, err)
	}
	if out, err = runRoot(t, "--config", cfg, "system", "escrow-key", "--passphrase-file", pass); err != nil || !strings.Contains(out, "key id") || !strings.Contains(out, "WARNING") {
		t.Fatalf("escrow-key = %q, %v (a file backend must warn that the copy shares the disk)", out, err)
	}
	if out, err = runRoot(t, "--config", cfg, "backups", "status"); err != nil || strings.Contains(out, "NOT backed up") || !strings.Contains(out, "encrypted copy in the backend") || !strings.Contains(out, "this node's key") {
		t.Fatalf("status after = %q, %v", out, err)
	}
	// The key is not in the backend in the clear.
	filepath.WalkDir(filepath.Join(dir, "backups"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), keysTestKey) {
				t.Errorf("%s holds the master key in the clear", p)
			}
		}
		return nil
	})

	// A node that lost its key gets it back, and the config too.
	os.Remove(key)
	cfgOut := filepath.Join(dir, "restored-config.toml")
	if out, err = runRoot(t, "--config", cfg, "system", "restore-key", "--passphrase-file", pass, "--config-out", cfgOut); err != nil {
		t.Fatalf("restore-key = %q, %v", out, err)
	}
	if b, _ := os.ReadFile(key); strings.TrimSpace(string(b)) != keysTestKey {
		t.Fatalf("restored key = %q", b)
	}
	if fi, _ := os.Stat(key); fi.Mode().Perm() != 0o600 {
		t.Errorf("restored key mode = %v", fi.Mode())
	}
	if b, _ := os.ReadFile(cfgOut); !strings.Contains(string(b), "state_dir") {
		t.Fatalf("restored config = %q", b)
	}
	// Running it again on a node that has the same key changes nothing.
	if out, err = runRoot(t, "--config", cfg, "system", "restore-key", "--passphrase-file", pass, "--config-out", ""); err != nil || !strings.Contains(out, "already holds this key") {
		t.Fatalf("second restore-key = %q, %v", out, err)
	}
	// A different key is never replaced without --force.
	other := strings.Repeat("ab", 32)
	os.WriteFile(key, []byte(other+"\n"), 0o600)
	if _, err = runRoot(t, "--config", cfg, "system", "restore-key", "--passphrase-file", pass, "--config-out", ""); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("restore-key over another key = %v", err)
	}
	if b, _ := os.ReadFile(key); strings.TrimSpace(string(b)) != other {
		t.Fatal("the node's key was replaced without --force")
	}
	if _, err = runRoot(t, "--config", cfg, "system", "restore-key", "--passphrase-file", pass, "--config-out", "", "--force"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(key); strings.TrimSpace(string(b)) != keysTestKey {
		t.Fatal("--force did not restore the key")
	}
	// The wrong passphrase opens nothing.
	bad := filepath.Join(dir, "bad")
	os.WriteFile(bad, []byte("another long passphrase\n"), 0o600)
	if _, err = runRoot(t, "--config", cfg, "system", "restore-key", "--passphrase-file", bad, "--config-out", "", "--force"); err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("wrong passphrase = %v", err)
	}
}

func TestPassphraseFileRules(t *testing.T) {
	dir := t.TempDir()
	if _, err := readPassphrase(writePass(t, dir, "short", 0o600), nil); err == nil || !strings.Contains(err.Error(), "at least") {
		t.Errorf("short passphrase = %v", err)
	}
	if _, err := readPassphrase(writePass(t, dir, "a long enough passphrase", 0o644), nil); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("world-readable passphrase file = %v", err)
	}
	b, err := readPassphrase(writePass(t, dir, "a long enough passphrase", 0o600), nil)
	if err != nil || string(b) != "a long enough passphrase" {
		t.Errorf("passphrase = %q, %v", b, err)
	}
	if b, err = readPassphrase("-", strings.NewReader("from standard input\r\n")); err != nil || string(b) != "from standard input" {
		t.Errorf("stdin passphrase = %q, %v", b, err)
	}
	if _, err := readPassphrase(filepath.Join(dir, "missing"), nil); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestSummaryRemindsAboutTheKeyUntilItIsEscrowed(t *testing.T) {
	var without, with strings.Builder
	cfg := config.Default()
	printSummary(&without, cfg, "203.0.113.7", "tok", false, installOptions{})
	printSummary(&with, cfg, "203.0.113.7", "tok", false, installOptions{KeyEscrowed: true})
	for _, want := range []string{"IMPORTANT", "export-key", "escrow-key"} {
		if !strings.Contains(without.String(), want) {
			t.Errorf("the summary of an install without an escrow lacks %q:\n%s", want, without.String())
		}
	}
	if strings.Contains(with.String(), "master key") {
		t.Errorf("the summary of an install with an escrow still talks about the master key:\n%s", with.String())
	}
}

// A rebuilt node installs with a new key and escrows it into the same bucket. That must not
// destroy the old node's copy, and restore-key must make the operator choose between them.
func TestEscrowOfANewKeyKeepsTheOldNodesCopy(t *testing.T) {
	cfg, key, dir := keysNode(t)
	pass := writePass(t, dir, "correct horse battery staple", 0o600)
	if out, err := runRoot(t, "--config", cfg, "system", "escrow-key", "--passphrase-file", pass); err != nil {
		t.Fatalf("escrow-key = %q, %v", out, err)
	}

	// The new node: another key, the same backend.
	newKey := strings.Repeat("ab", 32)
	if err := os.WriteFile(key, []byte(newKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, "--config", cfg, "system", "escrow-key", "--passphrase-file", pass)
	if err != nil || !strings.Contains(out, "also holds the copy of another master key") || !strings.Contains(out, "--key-id") {
		t.Fatalf("escrow-key of the new key = %q, %v (it must say that the old copy was kept)", out, err)
	}
	if out, err = runRoot(t, "--config", cfg, "backups", "status"); err != nil || !strings.Contains(out, "this node's key") || !strings.Contains(out, "another master key") {
		t.Fatalf("status = %q, %v", out, err)
	}

	// Two copies: restore-key asks which one.
	if _, err = runRoot(t, "--config", cfg, "system", "restore-key", "--passphrase-file", pass, "--config-out", "", "--force"); err == nil || !strings.Contains(err.Error(), "--key-id") && !strings.Contains(err.Error(), "key id") {
		t.Fatalf("restore-key with two copies = %v", err)
	}
	if b, _ := os.ReadFile(key); strings.TrimSpace(string(b)) != newKey {
		t.Fatal("restore-key changed the key although it could not tell which copy to use")
	}
	oldID := backup.KeyID(keysTestKey)
	if out, err = runRoot(t, "--config", cfg, "system", "restore-key", "--passphrase-file", pass, "--config-out", "", "--key-id", oldID, "--force"); err != nil {
		t.Fatalf("restore-key --key-id = %q, %v", out, err)
	}
	if b, _ := os.ReadFile(key); strings.TrimSpace(string(b)) != keysTestKey {
		t.Fatalf("the old key did not come back: %q", b)
	}
}
