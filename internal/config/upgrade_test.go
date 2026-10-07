package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpgradeDefaultsAndValidation(t *testing.T) {
	var u Upgrade
	if u.Canary() != 1 || u.Batch() != 3 || u.Keep() != 2 {
		t.Fatalf("defaults = %d %d %d", u.Canary(), u.Batch(), u.Keep())
	}
	u = Upgrade{CanaryProjects: -1, BatchSize: 5, KeepReleases: 4}
	if u.Canary() != 0 || u.Batch() != 5 || u.Keep() != 4 {
		t.Fatalf("explicit = %d %d %d", u.Canary(), u.Batch(), u.Keep())
	}
	for _, bad := range []Upgrade{{CanaryProjects: -2}, {BatchSize: -1}, {KeepReleases: -1}} {
		if bad.validate() == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestUpgradeSectionLoads(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[upgrade]\ncanary_projects = 2\nbatch_size = 4\nkeep_releases = 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUPAVISE_UPGRADE_BATCH_SIZE", "6")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upgrade.CanaryProjects != 2 || c.Upgrade.BatchSize != 6 || c.Upgrade.KeepReleases != 3 {
		t.Fatalf("upgrade = %+v (the environment overrides the file)", c.Upgrade)
	}
}
