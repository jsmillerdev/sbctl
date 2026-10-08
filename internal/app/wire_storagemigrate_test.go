package app

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
)

func TestWireStorageMigrateSaysWhenARunStopped(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	dir := cfg.Paths().System(config.SvcStorage)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	w := &Wire{Cfg: cfg, Log: slog.New(slog.NewTextHandler(&logs, nil))}

	// A node that never migrated: nothing happens.
	if err := wireStorageMigrate(context.Background(), w); err != nil || logs.Len() != 0 {
		t.Fatalf("no record: %v, log %q", err, logs.String())
	}

	// A run that stopped and a hold that expired.
	state := `{"id":"1","phase":"verifying","destination":{"bucket":"objects","region":"r"},"credentials":"file","started_at":"2026-10-08T00:00:00Z","updated_at":"2026-10-08T00:00:00Z","error":"boom"}`
	if err := os.WriteFile(filepath.Join(dir, "migrate.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(dir, "write-hold.json"), []byte(`{"pid":1,"since":"`+old+`","until":"`+old+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wireStorageMigrate(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"write hold", "storage migrate --resume", "phase=verifying", "bucket=objects"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "write-hold.json")); !os.IsNotExist(err) {
		t.Errorf("the expired hold is still there: %v", err)
	}

	// A finished run is not mentioned.
	logs.Reset()
	done := strings.Replace(state, `"verifying"`, `"done"`, 1)
	if err := os.WriteFile(filepath.Join(dir, "migrate.json"), []byte(done), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wireStorageMigrate(context.Background(), w); err != nil || logs.Len() != 0 {
		t.Errorf("finished: %v, log %q", err, logs.String())
	}
}
