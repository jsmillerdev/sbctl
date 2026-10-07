package artifacts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

// A node keeps old artifacts for two reasons: a project may still run them (a project moves to
// the node's pinned versions only when it is upgraded), and the previous release's binary
// needs them if the operator rolls back. Everything else is garbage, and GC removes it.

// releasesFile records, oldest first, the artifact pins of every release this node has run.
const releasesFile = ".releases.json"

// Release is one entry of the node's release history: the tag of every artifact (by directory
// name under artifacts/, so "auth" and not "gotrue") the node pinned at that time.
type Release struct {
	At   time.Time         `json:"at"`
	Tags map[string]string `json:"tags"`
}

// Pins returns the tag of every artifact the loaded versions.yaml pins, by directory name
// under artifacts/ (Studio is "studio").
func (v *Versions) Pins() map[string]string {
	pins := make(map[string]string, len(v.Artifacts)+1)
	for name, tag := range v.Artifacts {
		pins[name] = tag
	}
	if v.Studio.Tag != "" {
		pins[config.SvcStudio] = v.Studio.Tag
	}
	return pins
}

func (s *Store) releasesPath() string { return filepath.Join(s.cfg.Paths().Artifacts(), releasesFile) }

// Releases returns the node's release history, oldest first. A node that never recorded one
// has none.
func (s *Store) Releases() ([]Release, error) {
	b, err := os.ReadFile(s.releasesPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rs []Release
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("artifacts: %s: %w", s.releasesPath(), err)
	}
	return rs, nil
}

// RecordPins appends the pinned versions to the release history when they differ from the last
// entry. The daemon calls it at every start, so the history lists the releases the node has
// actually run. It reports whether it appended.
func (s *Store) RecordPins() (bool, error) {
	rs, err := s.Releases()
	if err != nil {
		return false, err
	}
	pins := s.versions.Pins()
	if n := len(rs); n > 0 && samePins(rs[n-1].Tags, pins) {
		return false, nil
	}
	rs = append(rs, Release{At: time.Now().UTC(), Tags: pins})
	// The history needs no more than the largest keep_releases anyone sets; a long tail is noise.
	if len(rs) > 20 {
		rs = rs[len(rs)-20:]
	}
	b, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(s.releasesPath()), 0o755); err != nil {
		return false, err
	}
	return true, writeAtomic(s.releasesPath(), append(b, '\n'))
}

func samePins(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Unused is one unpacked artifact that nothing references.
type Unused struct {
	Name string // directory name under artifacts/, for example "auth"
	Tag  string
	Dir  string
}

// KeepSet lists what GC must not remove, as "<name>/<tag>" keys: the pinned versions, those of
// the last keepReleases entries of the release history (the pinned ones count as the newest),
// and extra, the tags the projects run (by service name, as registry.Project.Versions holds
// them).
func (s *Store) KeepSet(keepReleases int, extra []map[string]string) (map[string]bool, error) {
	keep := map[string]bool{}
	add := func(name, tag string) {
		if tag != "" {
			keep[name+"/"+tag] = true
		}
	}
	for name, tag := range s.versions.Pins() {
		add(name, tag)
	}
	rs, err := s.Releases()
	if err != nil {
		return nil, err
	}
	// The pinned versions are the newest release whether or not the daemon recorded them yet.
	if n := len(rs); keepReleases > 1 && n > 0 {
		from := n - (keepReleases - 1)
		if samePins(rs[n-1].Tags, s.versions.Pins()) {
			from = n - keepReleases
		}
		for _, r := range rs[max(from, 0):] {
			for name, tag := range r.Tags {
				add(name, tag)
			}
		}
	}
	for _, versions := range extra {
		for svc, tag := range versions {
			add(config.ArtifactName(svc), tag)
		}
	}
	return keep, nil
}

// FindUnused lists the unpacked artifacts that keep does not name. Directories that start with
// a dot (the archive cache, unpacking in progress) are never listed.
func (s *Store) FindUnused(keep map[string]bool) ([]Unused, error) {
	root := s.cfg.Paths().Artifacts()
	names, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Unused
	for _, n := range names {
		if !n.IsDir() || strings.HasPrefix(n.Name(), ".") {
			continue
		}
		tags, err := os.ReadDir(filepath.Join(root, n.Name()))
		if err != nil {
			return nil, err
		}
		for _, t := range tags {
			if !t.IsDir() || strings.HasPrefix(t.Name(), ".") || keep[n.Name()+"/"+t.Name()] {
				continue
			}
			out = append(out, Unused{Name: n.Name(), Tag: t.Name(), Dir: filepath.Join(root, n.Name(), t.Name())})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// GC removes the artifacts that keep does not name and returns them. With dryRun it only lists
// them. An artifact that a running unit executes cannot be removed from under it by mistake as
// long as keep holds the versions of every project, which is the caller's job (lifecycle.Engine
// builds it); the removal of one directory that fails is reported and does not stop the rest.
func (s *Store) GC(keep map[string]bool, dryRun bool) ([]Unused, error) {
	unused, err := s.FindUnused(keep)
	if err != nil || dryRun {
		return unused, err
	}
	var removed []Unused
	var first error
	for _, u := range unused {
		if err := os.RemoveAll(u.Dir); err != nil {
			if first == nil {
				first = fmt.Errorf("artifacts: remove %s: %w", u.Dir, err)
			}
			continue
		}
		s.log.Info("artifact removed", "service", u.Name, "tag", u.Tag)
		removed = append(removed, u)
	}
	return removed, first
}
