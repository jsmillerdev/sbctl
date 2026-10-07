package health

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/backup"
	"github.com/jsmillerdev/supavise/internal/config"
)

func escrowConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Backup.Backend = "file://" + t.TempDir()
	cfg.KeyPath = filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(cfg.KeyPath, []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestEscrowCheckSeesTheNodesKey(t *testing.T) {
	cfg := escrowConfig(t)
	ctx := context.Background()
	check := EscrowCheck(cfg, 0, false)

	e, err := check(ctx)
	if err != nil || e.Covered || !strings.Contains(e.Detail, "not in the backups") {
		t.Fatalf("a node whose key is not backed up: %+v %v", e, err)
	}
	st, err := backup.OpenStore(ctx, cfg.Backup)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(cfg.KeyPath)
	if _, err := backup.PutKeyEscrow(ctx, st, []byte("a long enough passphrase"), backup.EscrowContents{MasterKey: string(key)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	e, err = check(ctx)
	if err != nil || !e.Covered {
		t.Fatalf("a node whose key is backed up: %+v %v", e, err)
	}

	// A copy of some other node's key does not cover this one.
	other := escrowConfig(t)
	other.Backup.Backend = cfg.Backup.Backend
	if err := os.WriteFile(other.KeyPath, []byte("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if e, err := EscrowCheck(other, 0, false)(ctx); err != nil || e.Covered {
		t.Errorf("another node's key was taken for ours: %+v %v", e, err)
	}
}

func TestEscrowCheckInTheBackgroundNeverWaits(t *testing.T) {
	cfg := escrowConfig(t)
	check := EscrowCheck(cfg, time.Hour, true)
	if _, err := check(context.Background()); err == nil || err.Error() != "not checked yet" {
		t.Fatalf("the first answer must not wait for the backend: %v", err)
	}
	var e *Escrow
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		if e, err = check(context.Background()); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e == nil || e.Covered {
		t.Errorf("the refreshed answer: %+v", e)
	}
	// The report says "not checked yet" without making it an advisory.
	d := &Deps{Cfg: cfg, Escrow: func(context.Context) (*Escrow, error) { return nil, errEscrowPending }}
	if c := d.checkEscrow(context.Background()); c == nil || c.State != OK || c.Detail != "not checked yet" {
		t.Errorf("%+v", c)
	}
}

func TestProbes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.TLS.Mode = "off"
	cfg.Listen.Admin, cfg.Listen.HTTP, cfg.Listen.HTTPS = ln.Addr().String(), ln.Addr().String(), "127.0.0.1:1"
	ctx := context.Background()
	if err := ProbeAdmin(cfg)(ctx); err != nil {
		t.Errorf("a listening admin port: %v", err)
	}
	if err := ProbeEdge(cfg)(ctx); err != nil {
		t.Errorf("TLS off probes the HTTP listener: %v", err)
	}
	cfg.TLS.Mode = "auto"
	if err := ProbeEdge(cfg)(ctx); err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Errorf("with TLS the HTTPS listener is probed (nothing is there): %v", err)
	}
	ln.Close()
	if err := ProbeAdmin(cfg)(ctx); err == nil {
		t.Error("a closed port answered")
	}
	// ":443" means every interface; the probe goes to loopback.
	cfg.Listen.Admin = ":1"
	if err := ProbeAdmin(cfg)(ctx); err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("%v", err)
	}
	cfg.Listen.Admin = "nonsense"
	if err := ProbeAdmin(cfg)(ctx); err == nil {
		t.Error("a bad address")
	}
}
