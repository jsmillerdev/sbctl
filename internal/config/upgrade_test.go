package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpgradeDefaultsAndValidation(t *testing.T) {
	var u Upgrade
	if u.Canary() != 1 || u.Batch() != 5 || u.Keep() != 3 {
		t.Fatalf("defaults = %d %d %d", u.Canary(), u.Batch(), u.Keep())
	}
	u = Upgrade{CanaryProjects: -1, BatchSize: 7, KeepReleases: 4}
	if u.Canary() != 0 || u.Batch() != 7 || u.Keep() != 4 {
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

// `supavise upgrade` hands the dashboard build of the new release to the artifact fetch through the
// environment: the signed list names its URL and checksum, the config file is not rewritten.
func TestStudioArtifactComesFromTheEnvironment(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[studio]\nartifact_url = \"https://old.example/studio.tar.zst\"\nartifact_sha256 = \"aa\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUPAVISE_STUDIO_ARTIFACT_URL", "https://new.example/studio.tar.zst")
	t.Setenv("SUPAVISE_STUDIO_ARTIFACT_SHA256", "bb")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Studio.ArtifactURL != "https://new.example/studio.tar.zst" || c.Studio.ArtifactSHA256 != "bb" {
		t.Fatalf("studio = %+v", c.Studio)
	}
}
