package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/backup"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

func TestLifecycleOptionsWireTheArchiveCommand(t *testing.T) {
	cfg := config.Default()
	cfg.BinPath = "/opt/my sbctl/bin/sbctl"
	cfg.Backup.ArchiveTimeoutSeconds = 120
	cfg.Backup.WALRelay = "off" // the relay form is checked below
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
	direct := config.Default()
	direct.Backup.WALRelay = "off"
	if c := LifecycleOptions(direct, Options{ConfigPath: config.DefaultPath}).ArchiveCommandFor(ref); strings.Contains(c, "--config") {
		t.Errorf("default config path passed on: %q", c)
	}
	// Under systemd (and with wal_relay on) clusters archive through the daemon's socket and
	// the command names no config file: the unit cannot read it.
	relay := config.Default()
	relay.Backup.WALRelay = "on"
	rc := LifecycleOptions(relay, Options{ConfigPath: "/etc/sbctl/other.toml"}).ArchiveCommandFor(ref)
	if want := backup.ArchiveCommandRelay(relay.BinPath, ref, relay.Paths().WALSocket(ref)); rc != want || strings.Contains(rc, "--config") {
		t.Errorf("relay archive_command = %q, want %q", rc, want)
	}
	// The engine's hook reaches the plane.
	var got2 string
	if oo := LifecycleOptions(relay, Options{ArchiveReady: func(ref string) { got2 = ref }}); oo.ArchiveReady == nil {
		t.Error("ArchiveReady is not wired")
	} else {
		oo.ArchiveReady(ref)
		if got2 != ref {
			t.Error("ArchiveReady does not call the hook")
		}
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

// At boot the registry may not accept connections yet. Serve keeps trying (bounded), and
// only for that error: anything else, such as a missing master key, fails at once.
func TestOpenNodeWaitsForTheRegistryAndFailsFastOnOtherErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = dir
	cfg.KeyPath = dir + "/master.key"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	start := time.Now()
	if _, err := openNode(context.Background(), cfg, lifecycle.OpenOptions{}, log); err == nil || errors.Is(err, lifecycle.ErrRegistryUnreachable) {
		t.Fatalf("missing master key = %v, want a plain error", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("a configuration error was retried")
	}

	if _, err := secrets.LoadOrCreate(cfg.KeyPath); err != nil {
		t.Fatal(err)
	}
	cfg.Supervisor = config.SupervisorExec
	old := registryWait
	registryWait = 1500 * time.Millisecond
	defer func() { registryWait = old }()
	lo := lifecycle.OpenOptions{Artifacts: dirArts{}}
	start = time.Now()
	_, err := openNode(context.Background(), cfg, lo, log)
	if !errors.Is(err, lifecycle.ErrRegistryUnreachable) {
		t.Fatalf("no registry = %v, want ErrRegistryUnreachable", err)
	}
	if time.Since(start) < time.Second {
		t.Fatalf("gave up after %s, want it to wait for the registry", time.Since(start))
	}
}
