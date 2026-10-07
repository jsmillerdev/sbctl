package config

import "fmt"

// Upgrade is the [upgrade] section: how `supavise upgrade` and `supavise projects upgrade --all`
// roll the projects of a node onto the node's pinned service versions, and how many releases
// (binaries and artifacts) the node keeps for `supavise rollback`. A single project's upgrade
// (Studio, the Management API, `projects upgrade <ref>`) ignores the rollout settings.
type Upgrade struct {
	// CanaryProjects is how many projects `--all` upgrades first, one at a time, before it
	// starts on the rest. A failure in a canary stops the rollout. Zero means 1; -1 means none.
	CanaryProjects int `toml:"canary_projects"`
	// BatchSize is how many projects `--all` upgrades at once after the canaries (each one
	// still takes its own base backup first). Zero means 5.
	BatchSize int `toml:"batch_size"`
	// KeepReleases is how many of the releases this node has run keep their binary and their
	// artifacts on disk (the current one counts), so that `supavise rollback` finds the previous
	// release's. Artifacts a project still uses are never removed. Zero means 3.
	KeepReleases int `toml:"keep_releases"`
}

// Canary returns CanaryProjects with its default applied.
func (u Upgrade) Canary() int {
	switch {
	case u.CanaryProjects < 0:
		return 0
	case u.CanaryProjects == 0:
		return 1
	}
	return u.CanaryProjects
}

// Batch returns BatchSize with its default applied.
func (u Upgrade) Batch() int {
	if u.BatchSize <= 0 {
		return 5
	}
	return u.BatchSize
}

// Keep returns KeepReleases with its default applied.
func (u Upgrade) Keep() int {
	if u.KeepReleases <= 0 {
		return 3
	}
	return u.KeepReleases
}

func (u Upgrade) validate() error {
	if u.CanaryProjects < -1 {
		return fmt.Errorf("config: upgrade.canary_projects must be -1 (none), 0 (the default, 1) or more")
	}
	if u.BatchSize < 0 || u.KeepReleases < 0 {
		return fmt.Errorf("config: upgrade.batch_size and upgrade.keep_releases must not be negative")
	}
	return nil
}
