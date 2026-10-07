package app

import (
	"context"
	"strings"
	"testing"

	"github.com/OWNER/sbctl/internal/backup"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

func TestLifecycleOptionsWireTheArchiveCommand(t *testing.T) {
	cfg := config.Default()
	cfg.BinPath = "/opt/my sbctl/bin/sbctl"
	cfg.Backup.ArchiveTimeoutSeconds = 120
	oo := LifecycleOptions(cfg, Options{ConfigPath: "/etc/sbctl/other.toml"})
	const ref = "abcdefghijklmnopqrst"
	got := oo.ArchiveCommandFor(ref)
	if want := backup.ArchiveCommand(cfg.BinPath, ref, "/etc/sbctl/other.toml"); got != want {
		t.Fatalf("archive_command = %q, want the backup package's %q", got, want)
	}
	// A path with a space is quoted, % is doubled, and the config file the daemon loaded is passed on.
	if !strings.Contains(got, "'/opt/my sbctl/bin/sbctl'") || !strings.Contains(got, "--config /etc/sbctl/other.toml") || !strings.HasSuffix(got, "%p") {
		t.Errorf("archive_command = %q", got)
	}
	if oo.ArchiveTimeout != 120 {
		t.Errorf("archive timeout %d, want the configured 120", oo.ArchiveTimeout)
	}
	if oo.BackupFactory == nil {
		t.Error("the backup service is not wired into the lifecycle")
	}
	if def := LifecycleOptions(config.Default(), Options{}); def.ArchiveTimeout != backup.DefaultArchiveTimeout {
		t.Errorf("default archive timeout %d, want %d", def.ArchiveTimeout, backup.DefaultArchiveTimeout)
	}
	// The default config path is not repeated in archive_command.
	if c := LifecycleOptions(config.Default(), Options{ConfigPath: config.DefaultPath}).ArchiveCommandFor(ref); strings.Contains(c, "--config") {
		t.Errorf("default config path passed on: %q", c)
	}
}

func TestPGMetaCryptoKey(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMemory()
	if err := reg.CreateProject(ctx, &registry.Project{Ref: config.SystemRef, Name: "system", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	// Without a configured key the registry's sealed one is created once and reused.
	k1, err := PGMetaCryptoKey(ctx, cfg, reg, sec)
	if err != nil || len(k1) < 16 {
		t.Fatalf("key %q, %v", k1, err)
	}
	if k2, _ := PGMetaCryptoKey(ctx, cfg, reg, sec); k2 != k1 {
		t.Fatal("the key changed between calls")
	}
	// A configured key wins, and is what sb-pgmeta must run with.
	cfg.API.PGMetaCryptoKey = "configured-key-0123456789"
	if k, _ := PGMetaCryptoKey(ctx, cfg, reg, sec); k != "configured-key-0123456789" {
		t.Fatalf("configured key ignored: %q", k)
	}
}
