package failover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover/fenced"
)

func dataDir(t *testing.T, cfg *config.Config, ref string) string {
	t.Helper()
	d := cfg.Paths().PostgresData(ref)
	must(t, os.MkdirAll(d, 0o700))
	must(t, os.WriteFile(filepath.Join(d, "PG_VERSION"), []byte("17\n"), 0o600))
	return d
}

func TestSetAsideKeepsTheOldDataAndRecordsTheLostTail(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	data := dataDir(t, cfg, refA)
	must(t, fenced.WriteProject(cfg.Paths(), fenced.Record{Epoch: 2, Ref: refA, Reason: "failed over"}))
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	d, err := SetAsideDiverged(cfg, refA, 2, "0/5000060", "0/4000000", now)
	if err != nil {
		t.Fatal(err)
	}
	want := data + ".diverged-2"
	if d.Path != want || d.LostBytes != 0x1000060 || d.Ref != refA || !d.At.Equal(now) {
		t.Fatalf("diverged: %+v", d)
	}
	if _, err := os.Stat(data); err == nil {
		t.Fatal("the data directory is still where the standby will be built")
	}
	if b, err := os.ReadFile(filepath.Join(want, "PG_VERSION")); err != nil || string(b) != "17\n" {
		t.Fatalf("the data was not kept: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(want, divergedMarker)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("marker: %v %v", fi, err)
	}
	if r, _ := fenced.Project(cfg.Paths(), refA); r != nil {
		t.Fatal("the project's fence record outlives its set-aside data")
	}
	// A second set-aside at the same epoch does not overwrite the first.
	dataDir(t, cfg, refA)
	d2, err := SetAsideDiverged(cfg, refA, 2, "", "", now)
	if err != nil || d2.Path != want+"-2" {
		t.Fatalf("second: %+v, %v", d2, err)
	}
	// A project with nothing on this node has nothing to set aside.
	if d, err := SetAsideDiverged(cfg, refB, 2, "", "", now); err != nil || d.Path != "" {
		t.Fatalf("empty: %+v, %v", d, err)
	}
	// A tail that cannot be measured is left out, not guessed.
	dataDir(t, cfg, refB)
	if d, _ := SetAsideDiverged(cfg, refB, 3, "0/1000000", "", now); d.LostBytes != 0 {
		t.Fatalf("lost bytes from one position: %d", d.LostBytes)
	}
}

func TestSweepRemovesOnlyWhatIsPastKeepDiverged(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	old := dataDir(t, cfg, refA)
	if _, err := SetAsideDiverged(cfg, refA, 2, "", "", now.Add(-4*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	dataDir(t, cfg, refB)
	if _, err := SetAsideDiverged(cfg, refB, 2, "", "", now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	live := dataDir(t, cfg, refC) // a running cluster's data: never touched
	// A directory set aside by hand has no marker; its age is its modification time, and it is not
	// guessed at when that is unreadable.
	byHand := cfg.Paths().PostgresData("system") + ".diverged-1"
	must(t, os.MkdirAll(byHand, 0o700))
	must(t, os.Chtimes(byHand, now.Add(-10*24*time.Hour), now.Add(-10*24*time.Hour)))

	all, err := ListDiverged(cfg)
	if err != nil || len(all) != 3 {
		t.Fatalf("list: %+v, %v", all, err)
	}
	removed, err := SweepDiverged(cfg, 3*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %v", removed)
	}
	if _, err := os.Stat(old + ".diverged-2"); err == nil {
		t.Fatal("the old one is still there")
	}
	if _, err := os.Stat(byHand); err == nil {
		t.Fatal("the old one set aside by hand is still there")
	}
	if _, err := os.Stat(cfg.Paths().PostgresData(refB) + ".diverged-2"); err != nil {
		t.Fatalf("the young one went: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live data went: %v", err)
	}
	for _, r := range removed {
		if !strings.Contains(r, ".diverged-") {
			t.Fatalf("removed %s", r)
		}
	}
}
