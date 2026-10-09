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
		bin := fake(strings.ReplaceAll(c.name, " ", "-"), c.body)
		m, err := manifest("-binary", bin)
		if err != nil || m.Host == nil || m.Host.ConvergeRevision != c.want {
			t.Errorf("binary that %s: host %+v err %v, want revision %d", c.name, m.Host, err, c.want)
		}
		// Anything but a revision it reported is worth a warning on stderr.
		if why := probe(bin).Why; (why == "") != (c.name == "reports") {
			t.Errorf("binary that %s: warning %q", c.name, why)
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

// A release names its peer floor and whether its PostgreSQL keeps the WAL format; the binary's own
// word about its revisions is checked against the template's, and a release can insist on the
// converge revision.
func TestManifestPeerWALAndStrictFields(t *testing.T) {
	dir := t.TempDir()
	vf := filepath.Join(dir, "versions.yaml")
	if err := os.WriteFile(vf, []byte(newYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	out := filepath.Join(dir, "m.json")
	manifest := func(extra ...string) (*selfupdate.Manifest, error) {
		os.Remove(out)
		args := append([]string{"-version", "v1.1.0", "-min-upgrade-from", "v1.0.0", "-versions", vf, "-out", out}, extra...)
		if err := cmdManifest(args); err != nil {
			return nil, err
		}
		return selfupdate.ParseManifest(mustRead(t, out))
	}

	m, err := manifest()
	if err != nil || m.MinPeerFrom != "" || !m.WALCompatible() || strings.Contains(string(mustRead(t, out)), "wal_compat") {
		t.Errorf("without the flags the fields are left out: %+v %v\n%s", m, err, mustRead(t, out))
	}
	m, err = manifest("-min-peer-from", "v1.0.0", "-wal-compat=false")
	if err != nil || m.MinPeerFrom != "v1.0.0" || m.WALCompatible() || !strings.Contains(string(mustRead(t, out)), `"wal_compat": false`) {
		t.Errorf("with the flags: %+v %v\n%s", m, err, mustRead(t, out))
	}
	if m, err = manifest("-wal-compat=true"); err != nil || strings.Contains(string(mustRead(t, out)), "wal_compat") {
		t.Errorf("wal_compat true is the absent field: %+v %v", m, err)
	}
	if _, err := manifest("-min-peer-from", "v2.0.0"); err == nil {
		t.Error("a peer floor above the release was accepted")
	}

	// -require-converge turns the warning into a refusal and writes nothing.
	for name, body := range map[string]string{
		"predates":     `echo '{"version":"v1.1.0","pins":{}}'`,
		"does-not-run": `exit 1`,
	} {
		b := bin(name, body)
		if _, err := manifest("-binary", b); err != nil {
			t.Errorf("%s: without -require-converge a warning is enough: %v", name, err)
		}
		if _, err := manifest("-binary", b, "-require-converge"); err == nil || !strings.Contains(err.Error(), "converge_revision") && !strings.Contains(err.Error(), "did not run") {
			t.Errorf("%s: -require-converge: %v", name, err)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%s: a refused manifest was written", name)
		}
	}
	good := bin("good", `echo '{"version":"v1.1.0","converge_revision":4,"infra_revision":`+strconv.Itoa(infra.Current)+`}'`)
	tpl := filepath.Join("..", "cloudformation", "supavise.yaml")
	if m, err := manifest("-binary", good, "-require-converge", "-template", tpl); err != nil || m.Host.ConvergeRevision != 4 || m.AWS.StackRevision != infra.Current {
		t.Errorf("a binary that reports both: %+v %v", m, err)
	}
	// A binary of another stack revision than the template is not of the release.
	stale := bin("stale", `echo '{"version":"v1.1.0","converge_revision":4,"infra_revision":`+strconv.Itoa(infra.Current-1)+`}'`)
	if _, err := manifest("-binary", stale, "-template", tpl); err == nil || !strings.Contains(err.Error(), "not of one release") {
		t.Errorf("a binary at another stack revision: %v", err)
	}
	// A binary that says nothing about the stack revision leaves the template's word standing.
	if _, err := manifest("-binary", bin("nostack", `echo '{"version":"v1.1.0","converge_revision":4}'`), "-template", tpl); err != nil {
		t.Errorf("a binary without infra_revision: %v", err)
	}
}

// The binary is run with a minimal environment: the release job's holds the signing key.
func TestProbeRunsTheBinaryWithAMinimalEnvironment(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	script := "#!/bin/sh\nenv > " + envFile + "\necho '{\"converge_revision\":1}'\n"
	bin := filepath.Join(dir, "supavise")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUPAVISE_SIGNING_KEY", "not-for-the-probe")
	t.Setenv("SOME_OTHER_SECRET", "x")
	if p := probe(bin); p.Why != "" || p.Converge != 1 {
		t.Fatalf("probe: %+v", p)
	}
	got := string(mustRead(t, envFile))
	for _, bad := range []string{"SUPAVISE_SIGNING_KEY", "SOME_OTHER_SECRET", "not-for-the-probe"} {
		if strings.Contains(got, bad) {
			t.Errorf("the probe saw %s:\n%s", bad, got)
		}
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

// The manifest's studio is the Studio build, the tag and the patch set, which is what the release's
// binary reports; the patch set of the Studio archives (studio/PATCHSET) and of the pin file must
// agree, and a binary that pins another build stops the release.
func TestManifestStudioIsTheBuild(t *testing.T) {
	dir := t.TempDir()
	withPatchset := strings.Replace(newYAML, "  tag: 2026.10.12-sha-abcdef1\n", "  tag: 2026.10.12-sha-abcdef1\n  patchset: 3\n", 1)
	vf := filepath.Join(dir, "versions.yaml")
	if err := os.WriteFile(vf, []byte(withPatchset), 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	out := filepath.Join(dir, "m.json")
	manifest := func(extra ...string) (*selfupdate.Manifest, error) {
		os.Remove(out)
		if err := cmdManifest(append([]string{"-version", "v1.1.0", "-min-upgrade-from", "v1.0.0", "-versions", vf, "-out", out}, extra...)); err != nil {
			return nil, err
		}
		return selfupdate.ParseManifest(mustRead(t, out))
	}
	const build = "2026.10.12-sha-abcdef1-p3"
	if m, err := manifest("-patchset", write("PATCHSET", "3\n", 0o644)); err != nil || m.Studio != build {
		t.Fatalf("manifest: %+v, %v", m, err)
	}
	if _, err := manifest("-patchset", write("PATCHSET-2", "2\n", 0o644)); err == nil || !strings.Contains(err.Error(), "change them together") {
		t.Fatalf("a patch set that differs: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a refused manifest was written")
	}
	good := write("good", `#!/bin/sh
echo '{"version":"v1.1.0","converge_revision":1,"pins":{"studio":"`+build+`"}}'`, 0o755)
	if m, err := manifest("-binary", good); err != nil || m.Studio != build {
		t.Fatalf("a binary of the release: %+v, %v", m, err)
	}
	tagOnly := write("tag-only", `#!/bin/sh
echo '{"version":"v1.1.0","converge_revision":1,"pins":{"studio":"2026.10.12-sha-abcdef1"}}'`, 0o755)
	if _, err := manifest("-binary", tagOnly); err == nil || !strings.Contains(err.Error(), "not of one release") {
		t.Fatalf("a binary that pins another build: %v", err)
	}
	// The repository's own files agree.
	repo, err := readVersions(filepath.Join("..", "..", "internal", "versions", "versions.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkPatchset(filepath.Join("..", "..", "studio", "PATCHSET"), repo); err != nil {
		t.Fatal(err)
	}
}

// A release that changes only the patch set lists Studio as moved, linked to its upstream commit
// and without a comparison of a commit with itself.
func TestNotesListAPatchsetOnlyStudioChange(t *testing.T) {
	old := parse(t, oldYAML)
	neu := parse(t, strings.Replace(oldYAML, "  tag: 2026.10.05-sha-94b8b06\n", "  tag: 2026.10.05-sha-94b8b06\n  patchset: 3\n", 1))
	d := Diff(old, neu)
	if len(d) != 1 || d[0] != (Change{"studio", "2026.10.05-sha-94b8b06", "2026.10.05-sha-94b8b06-p3"}) {
		t.Fatalf("diff = %+v", d)
	}
	table := ServiceTable(d)
	if !strings.Contains(table, "[2026.10.05-sha-94b8b06-p3](https://github.com/supabase/supabase/commit/94b8b06)") || strings.Contains(table, "compare") {
		t.Fatalf("table:\n%s", table)
	}
}
