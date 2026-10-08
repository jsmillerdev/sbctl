package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/artifacts"
	"github.com/supavise/supavise/internal/infra"
	"github.com/supavise/supavise/internal/selfupdate"
)

const oldYAML = `# Pinned upstream releases.
artifacts:
  postgres: postgres-17.11.0.004-r1
  auth: auth-v2.194.0-r1
  postgrest: postgrest-v16.4-r0
  pooler: pooler-v2.9.13-r1
  storage: storage-v1.79.36-r0
  imgproxy: imgproxy-v3.26.0-r0
studio:
  # Upstream Studio git ref. The slim studio artifact of the same upstream version is
  # studio-2026.10.05-sha-94b8b06-r0.
  tag: 2026.10.05-sha-94b8b06
cli:
  version_tested: "2.120.0"
`

const newYAML = `# Pinned upstream releases.
artifacts:
  postgres: postgres-17.11.0.004-r1
  auth: auth-v2.195.0-r1
  postgrest: postgrest-v16.4-r0
  pooler: pooler-v2.9.14-r0
  storage: storage-v1.79.36-r0
  realtime: realtime-v2.140.10-r0
studio:
  tag: 2026.10.12-sha-abcdef1
cli:
  version_tested: "2.121.0"
`

func parse(t *testing.T, s string) *artifacts.Versions {
	t.Helper()
	v, err := artifacts.ParseVersions([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDiff(t *testing.T) {
	got := Diff(parse(t, oldYAML), parse(t, newYAML))
	want := []Change{
		{"auth", "auth-v2.194.0-r1", "auth-v2.195.0-r1"},
		{"imgproxy", "imgproxy-v3.26.0-r0", ""},
		{"pooler", "pooler-v2.9.13-r1", "pooler-v2.9.14-r0"},
		{"realtime", "", "realtime-v2.140.10-r0"},
		{"studio", "2026.10.05-sha-94b8b06", "2026.10.12-sha-abcdef1"},
		{"cli", "2.120.0", "2.121.0"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	if d := Diff(parse(t, oldYAML), parse(t, oldYAML)); len(d) != 0 {
		t.Errorf("identical files differ: %+v", d)
	}
	if d := Diff(nil, parse(t, newYAML)); len(d) != 8 {
		t.Errorf("a first release lists every pin as added: %d", len(d))
	}
}

func TestUpstreamVersion(t *testing.T) {
	for _, c := range []struct{ name, tag, want string }{
		{"auth", "auth-v2.195.0-r1", "v2.195.0"},
		{"postgres", "postgres-17.11.0.004-r1", "17.11.0.004"},
		{"edge-runtime", "edge-runtime-v1.77.4-r0", "v1.77.4"},
		{"postgrest", "postgrest-v16.4-r0", "v16.4"},
	} {
		if got := UpstreamVersion(c.name, c.tag); got != c.want {
			t.Errorf("UpstreamVersion(%s, %s) = %q, want %q", c.name, c.tag, got, c.want)
		}
	}
}

func TestServiceTableLinksEachUpstreamRelease(t *testing.T) {
	table := ServiceTable(Diff(parse(t, oldYAML), parse(t, newYAML)))
	for _, want := range []string{
		"| auth | `auth-v2.194.0-r1` | `auth-v2.195.0-r1` | [v2.195.0](https://github.com/supabase/auth/releases/tag/v2.195.0)",
		"https://github.com/supabase/auth/compare/v2.194.0...v2.195.0",
		"https://github.com/supabase/slim-services/releases/tag/auth-v2.195.0-r1",
		"[v2.9.14](https://github.com/supabase/supavisor/releases/tag/v2.9.14)",
		"| realtime | not included |",
		"| imgproxy | `imgproxy-v3.26.0-r0` | removed | removed |",
		"Studio (dashboard build)",
		"https://github.com/supabase/supabase/compare/94b8b06...abcdef1",
		"[v2.121.0](https://github.com/supabase/cli/releases/tag/v2.121.0)",
	} {
		if !strings.Contains(table, want) {
			t.Errorf("table lacks %q:\n%s", want, table)
		}
	}
	// Services that did not change are not listed.
	for _, not := range []string{"| postgres |", "| postgrest |", "| storage |"} {
		if strings.Contains(table, not) {
			t.Errorf("unchanged service listed: %s", not)
		}
	}
	if got := ServiceTable(nil); !strings.Contains(got, "No Supabase service version changed") {
		t.Errorf("empty table: %q", got)
	}
}

func TestRenderNotes(t *testing.T) {
	log := ParseLog("1111111111111111111111111111111111111111\tupgrade: roll back on failure\n\n2222222222222222222222222222222222222222\tupdate: maintenance window\nbroken line\n")
	if len(log) != 2 {
		t.Fatalf("ParseLog: %+v", log)
	}
	notes := RenderNotes(NotesInput{Tag: "v1.1.0", PrevTag: "v1.0.0", Repo: "o/r", MinUpgradeFrom: "v1.0.0",
		Old: parse(t, oldYAML), New: parse(t, newYAML), Commits: log})
	for _, want := range []string{
		"Supavise v1.1.0",
		"Upgrades straight from v1.0.0 or later",
		"## Supabase service versions",
		"Compared with v1.0.0:",
		"| auth |",
		"## Supavise changes since v1.0.0",
		"- upgrade: roll back on failure ([1111111](https://github.com/o/r/commit/1111111111111111111111111111111111111111))",
		"https://github.com/o/r/compare/v1.0.0...v1.1.0",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	// Supavise's own changes come after the service table? The order is fixed: services, then changes.
	if strings.Index(notes, "Supabase service versions") > strings.Index(notes, "Supavise changes") {
		t.Error("the service table comes first")
	}

	first := RenderNotes(NotesInput{Tag: "v1.0.0", Repo: "o/r", New: parse(t, newYAML)})
	if !strings.Contains(first, "first release") || strings.Contains(first, "Full changelog") || strings.Contains(first, "since") {
		t.Errorf("first release notes:\n%s", first)
	}
	same := RenderNotes(NotesInput{Tag: "v1.0.1", PrevTag: "v1.0.0", Repo: "o/r", Old: parse(t, newYAML), New: parse(t, newYAML)})
	if !strings.Contains(same, "No Supabase service version changed") || !strings.Contains(same, "No changes to list") {
		t.Errorf("a release with no changes:\n%s", same)
	}

	var many []Commit
	for i := 0; i < maxCommits+5; i++ {
		many = append(many, Commit{"abcdef0123456789", "change"})
	}
	if got := RenderNotes(NotesInput{Tag: "v2.0.0", PrevTag: "v1.0.0", Repo: "o/r", New: parse(t, newYAML), Old: parse(t, newYAML), Commits: many}); !strings.Contains(got, "- and 5 more") {
		t.Errorf("a long change list is cut:\n%s", got)
	}
}

func TestBump(t *testing.T) {
	out, err := Bump([]byte(oldYAML), "auth", "auth-v2.196.0-r0")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(oldYAML, "auth-v2.194.0-r1", "auth-v2.196.0-r0", 1)
	if string(out) != want {
		t.Errorf("bump changed more than the pin:\n%s", out)
	}
	// postgrest and postgres are different services: bumping one leaves the other alone.
	out, err = Bump([]byte(oldYAML), "postgres", "postgres-17.11.0.005-r0")
	if err != nil || !strings.Contains(string(out), "postgres: postgres-17.11.0.005-r0") || !strings.Contains(string(out), "postgrest: postgrest-v16.4-r0") {
		t.Fatalf("%v\n%s", err, out)
	}

	out, err = Bump([]byte(oldYAML), "studio", "2026.10.12-sha-abcdef1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "tag: 2026.10.12-sha-abcdef1") || !strings.Contains(string(out), "studio-2026.10.12-sha-abcdef1-r0") || strings.Contains(string(out), "94b8b06") {
		t.Errorf("studio bump:\n%s", out)
	}
	if v := parse(t, string(out)); v.Studio.Tag != "2026.10.12-sha-abcdef1" || v.CLI.VersionTested != "2.120.0" {
		t.Errorf("bumped file parses to %+v", v)
	}

	// Same pin: nothing changes.
	if out, err := Bump([]byte(oldYAML), "auth", "auth-v2.194.0-r1"); err != nil || string(out) != oldYAML {
		t.Errorf("same pin: %v", err)
	}
	for name, c := range map[string][2]string{
		"another service's tag": {"auth", "storage-v1.80.0-r0"},
		"not a tag":             {"auth", "v2.196.0"},
		"unknown service":       {"nosuch", "nosuch-v1.0.0-r0"},
		"bad service":           {"Auth; rm", "auth-v1.0.0-r0"},
		"bad studio":            {"studio", "latest"},
	} {
		if _, err := Bump([]byte(oldYAML), c[0], c[1]); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, pre := range []string{"auth-v2.198.0-rc.1-r0", "auth-v2.198.0-beta.2-r0", "auth-v3.0.0-alpha.1-r1"} {
		if _, err := Bump([]byte(oldYAML), "auth", pre); err != errPrerelease {
			t.Errorf("%s: %v, want errPrerelease", pre, err)
		}
	}
}

func TestManifestFromTheRepositoryPins(t *testing.T) {
	dir := t.TempDir()
	vf := filepath.Join(dir, "versions.yaml")
	if err := os.WriteFile(vf, []byte(newYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "supavise-release.json")
	if err := cmdManifest([]string{"-version", "v1.1.0", "-min-upgrade-from", "v1.0.0", "-versions", vf, "-out", out}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	m, err := selfupdate.ParseManifest(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "v1.1.0" || m.MinUpgradeFrom != "v1.0.0" || m.Artifacts["auth"] != "auth-v2.195.0-r1" || m.Studio != "2026.10.12-sha-abcdef1" {
		t.Errorf("%+v", m)
	}
	// A manifest that the binary would refuse is never written.
	if err := cmdManifest([]string{"-version", "latest", "-min-upgrade-from", "v1.0.0", "-versions", vf, "-out", filepath.Join(dir, "bad.json")}); err == nil {
		t.Error("a bad version produced a manifest")
	}
	if _, err := os.Stat(filepath.Join(dir, "bad.json")); err == nil {
		t.Error("a refused manifest was written anyway")
	}
}

// The manifest names the host converge revision of the built binary and the stack revision and
// SHA-256 of the template, and says nothing about them when it is not given either.
func TestManifestHostAndAWSFields(t *testing.T) {
	dir := t.TempDir()
	vf := filepath.Join(dir, "versions.yaml")
	if err := os.WriteFile(vf, []byte(newYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	manifest := func(extra ...string) (*selfupdate.Manifest, error) {
		out := filepath.Join(dir, "m.json")
		os.Remove(out)
		args := append([]string{"-version", "v1.1.0", "-min-upgrade-from", "v1.0.0", "-versions", vf, "-out", out}, extra...)
		if err := cmdManifest(args); err != nil {
			return nil, err
		}
		return selfupdate.ParseManifest(mustRead(t, out))
	}

	m, err := manifest()
	if err != nil || m.Host != nil || m.AWS != nil {
		t.Fatalf("without the flags: %+v %v", m, err)
	}

	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"reports", `echo '{"version":"v1.1.0","converge_revision":4,"pins":{"auth":"x"}}'`, 4},
		{"predates the field", `echo '{"version":"v1.1.0","pins":{"auth":"x"}}'`, 0},
		{"does not run", `exit 1`, 0},
		{"prints no JSON", `echo converged`, 0},
		{"reports a bad number", `echo '{"converge_revision":"four"}'`, 0},
	} {
		m, err := manifest("-binary", fake(strings.ReplaceAll(c.name, " ", "-"), c.body))
		if err != nil || m.Host == nil || m.Host.ConvergeRevision != c.want {
			t.Errorf("binary that %s: host %+v err %v, want revision %d", c.name, m.Host, err, c.want)
		}
	}
	// A path that does not exist is the same as one that cannot run: 0, and a warning.
	if m, err := manifest("-binary", filepath.Join(dir, "absent")); err != nil || m.Host == nil || m.Host.ConvergeRevision != 0 {
		t.Errorf("absent binary: %+v %v", m, err)
	}

	tpl := filepath.Join("..", "cloudformation", "supavise.yaml")
	m, err = manifest("-template", tpl)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(mustRead(t, tpl))
	if m.AWS == nil || m.AWS.StackRevision != infra.Current || m.AWS.TemplateAsset != "supavise.yaml" || m.AWS.TemplateSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("aws: %+v", m.AWS)
	}
	// A template at another revision than the release's code, or one with no revision, is refused,
	// and no manifest is written.
	revision := regexp.MustCompile(`(InfraRevision:\n    Description: .*\n    Value: )"\d+"`)
	for name, body := range map[string]string{
		"old":     revision.ReplaceAllString(string(mustRead(t, tpl)), `${1}"`+strconv.Itoa(infra.Current-1)+`"`),
		"missing": strings.Replace(string(mustRead(t, tpl)), "InfraRevision:", "InfraRevisionX:", 1),
	} {
		p := filepath.Join(dir, name+".yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := manifest("-template", p); err == nil {
			t.Errorf("a %s template produced a manifest", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "m.json")); err == nil {
		t.Error("a refused manifest was written anyway")
	}
}

// The repository's own pin file is something the notes can be made from.
func TestRepositoryVersionsFileParses(t *testing.T) {
	v, err := readVersions(filepath.Join("..", "..", "internal", "versions", "versions.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for name := range v.Artifacts {
		if _, ok := upstream[name]; !ok {
			t.Errorf("artifact %q has no upstream repository in notes.go: its release notes would not be linked", name)
		}
	}
	if _, err := Bump(mustRead(t, filepath.Join("..", "..", "internal", "versions", "versions.yaml")), "studio", "2099.01.01-sha-0123456"); err != nil {
		t.Errorf("the real versions.yaml cannot be bumped: %v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
