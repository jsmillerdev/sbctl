package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/supavise/supavise/internal/artifacts"
)

// upstream says where the releases of a slim-services artifact come from. The slim-services tag
// is "<name>-<upstream version>-r<packaging revision>"; the upstream version is the tag of a
// release (or just a git tag) in Repo.
var upstream = map[string]string{
	"postgres":     "supabase/postgres",
	"auth":         "supabase/auth",
	"postgrest":    "PostgREST/postgrest",
	"pooler":       "supabase/supavisor",
	"realtime":     "supabase/realtime",
	"storage":      "supabase/storage",
	"pgmeta":       "supabase/postgres-meta",
	"imgproxy":     "imgproxy/imgproxy",
	"edge-runtime": "supabase/edge-runtime",
}

const (
	slimRepo   = "supabase/slim-services"
	studioRepo = "supabase/supabase"
	cliRepo    = "supabase/cli"
)

var revisionRe = regexp.MustCompile(`-r\d+$`)

// UpstreamVersion extracts the upstream version from a slim-services tag: "auth-v2.195.0-r1"
// gives "v2.195.0".
func UpstreamVersion(name, tag string) string {
	v := strings.TrimPrefix(tag, name+"-")
	return revisionRe.ReplaceAllString(v, "")
}

// Change is one pin that differs between two versions.yaml files.
type Change struct {
	Name     string // artifact name, "studio" or "cli"
	Old, New string // the pin; empty when the service was added or removed
}

// Diff lists the pins that differ, artifacts in name order, then Studio, then the CLI. old may be
// nil (a first release): every pin of new is then an addition.
func Diff(old, new *artifacts.Versions) []Change {
	var out []Change
	names := map[string]bool{}
	for n := range new.Artifacts {
		names[n] = true
	}
	if old != nil {
		for n := range old.Artifacts {
			names[n] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	pin := func(v *artifacts.Versions, name string) string {
		if v == nil {
			return ""
		}
		return v.Artifacts[name]
	}
	for _, n := range sorted {
		if o, w := pin(old, n), pin(new, n); o != w {
			out = append(out, Change{n, o, w})
		}
	}
	// Studio is compared as the build, so a release that changes only the patch set lists it.
	var oldStudio, oldCLI string
	if old != nil {
		oldStudio, oldCLI = old.StudioBuild(), old.CLI.VersionTested
	}
	if oldStudio != new.StudioBuild() {
		out = append(out, Change{"studio", oldStudio, new.StudioBuild()})
	}
	if oldCLI != new.CLI.VersionTested {
		out = append(out, Change{"cli", oldCLI, new.CLI.VersionTested})
	}
	return out
}

// shaRe finds the upstream commit in a Studio build name: <date>-sha-<commit>, with -p<N> when the
// build has a patch set.
var shaRe = regexp.MustCompile(`-sha-([0-9a-f]{7,40})(?:-p\d+)?$`)

// Link renders where a pin's release can be read: the upstream release page and, for the
// artifacts, the slim-services release that packages it. It returns "" for an empty pin.
func Link(name, pin string) string {
	if pin == "" {
		return ""
	}
	switch name {
	case "studio":
		if m := shaRe.FindStringSubmatch(pin); m != nil {
			return fmt.Sprintf("[%s](https://github.com/%s/commit/%s)", pin, studioRepo, m[1])
		}
		return "`" + pin + "`"
	case "cli":
		return fmt.Sprintf("[v%s](https://github.com/%s/releases/tag/v%s)", strings.TrimPrefix(pin, "v"), cliRepo, strings.TrimPrefix(pin, "v"))
	}
	repo, ok := upstream[name]
	if !ok {
		return "`" + pin + "`"
	}
	v := UpstreamVersion(name, pin)
	return fmt.Sprintf("[%s](https://github.com/%s/releases/tag/%s) ([packaged as `%s`](https://github.com/%s/releases/tag/%s))", v, repo, v, pin, slimRepo, pin)
}

func label(name string) string {
	switch name {
	case "studio":
		return "Studio (dashboard build)"
	case "cli":
		return "Supabase CLI (tested with)"
	}
	return name
}

// ServiceTable renders the changes as a Markdown table, or says that nothing changed. Each row
// links the new version's release notes upstream.
func ServiceTable(changes []Change) string {
	if len(changes) == 0 {
		return "No Supabase service version changed in this release.\n"
	}
	var b strings.Builder
	b.WriteString("| Service | Before | After | Upstream release notes |\n|---|---|---|---|\n")
	for _, c := range changes {
		before, after := "not included", "removed"
		if c.Old != "" {
			before = "`" + c.Old + "`"
		}
		if c.New != "" {
			after = "`" + c.New + "`"
		}
		notes := Link(c.Name, c.New)
		if c.New == "" {
			notes = "removed"
		}
		if c.Old != "" && c.New != "" {
			if cmp := compareLink(c); cmp != "" {
				notes += "; " + cmp
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", label(c.Name), before, after, notes)
	}
	return b.String()
}

// compareLink links the changes between two pins of one service, where upstream has a tag for each.
func compareLink(c Change) string {
	switch c.Name {
	case "studio":
		o, n := shaRe.FindStringSubmatch(c.Old), shaRe.FindStringSubmatch(c.New)
		if o != nil && n != nil && o[1] != n[1] { // one commit: only our patches changed
			return fmt.Sprintf("[all changes](https://github.com/%s/compare/%s...%s)", studioRepo, o[1], n[1])
		}
	case "cli":
		return ""
	default:
		if repo, ok := upstream[c.Name]; ok {
			return fmt.Sprintf("[all changes](https://github.com/%s/compare/%s...%s)", repo, UpstreamVersion(c.Name, c.Old), UpstreamVersion(c.Name, c.New))
		}
	}
	return ""
}

// Commit is one line of the Supavise change list.
type Commit struct{ Hash, Subject string }

// ParseLog reads `git log --pretty=format:'%H%x09%s'` output.
func ParseLog(s string) []Commit {
	var out []Commit
	for _, line := range strings.Split(s, "\n") {
		h, subj, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || h == "" || strings.TrimSpace(subj) == "" {
			continue
		}
		out = append(out, Commit{h, strings.TrimSpace(subj)})
	}
	return out
}

// NotesInput is what the release notes are made from.
type NotesInput struct {
	Tag, PrevTag, Repo string
	MinUpgradeFrom     string
	Old, New           *artifacts.Versions // Old is nil for a first release
	Commits            []Commit
}

// maxCommits bounds the change list; the compare link covers the rest.
const maxCommits = 100

// RenderNotes renders the release notes: what Supavise changed since the previous tag, then the
// Supabase service versions that moved, each linked to its upstream release.
func RenderNotes(in NotesInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Supavise %s\n\n", in.Tag)
	if in.MinUpgradeFrom != "" {
		fmt.Fprintf(&b, "Upgrades straight from %s or later: `sudo supavise upgrade`. An older node goes through %s first.\n\n", in.MinUpgradeFrom, in.MinUpgradeFrom)
	}

	b.WriteString("## Supabase service versions\n\n")
	switch {
	case in.Old == nil:
		b.WriteString("This is the first release. It installs these Supabase releases:\n\n")
		b.WriteString(ServiceTable(Diff(nil, in.New)))
	default:
		changes := Diff(in.Old, in.New)
		if len(changes) > 0 {
			fmt.Fprintf(&b, "Compared with %s:\n\n", in.PrevTag)
		}
		b.WriteString(ServiceTable(changes))
	}

	b.WriteString("\n## Supavise changes")
	if in.PrevTag != "" {
		fmt.Fprintf(&b, " since %s", in.PrevTag)
	}
	b.WriteString("\n\n")
	if len(in.Commits) == 0 {
		b.WriteString("No changes to list.\n")
	}
	for i, c := range in.Commits {
		if i == maxCommits {
			fmt.Fprintf(&b, "- and %d more\n", len(in.Commits)-maxCommits)
			break
		}
		short := c.Hash
		if len(short) > 7 {
			short = short[:7]
		}
		fmt.Fprintf(&b, "- %s ([%s](https://github.com/%s/commit/%s))\n", c.Subject, short, in.Repo, c.Hash)
	}
	if in.PrevTag != "" {
		fmt.Fprintf(&b, "\n**Full changelog**: https://github.com/%s/compare/%s...%s\n", in.Repo, in.PrevTag, in.Tag)
	}
	return b.String()
}
