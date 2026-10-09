package nodeupgrade

import (
	"bytes"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/artifacts"
	"github.com/supavise/supavise/internal/config"
)

// pinFile is a versions.yaml that pins Studio's tag with the given patch set line ("" for none, as
// up to v0.2.0).
func pinFile(t *testing.T, patchset string) *artifacts.Versions {
	t.Helper()
	y := "artifacts:\n  postgres: postgres-17.11.0.004-r1\n  auth: auth-v2.195.0-r1\n  postgrest: postgrest-v16.4-r0\n  pooler: pooler-v2.9.13-r1\n" +
		"studio:\n  tag: 2026.10.05-sha-94b8b06\n" + patchset
	v, err := artifacts.ParseVersions([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// v0.1.3 and v0.2.0 pin the same upstream Studio tag; v0.2.0's build has one patch more. A node's
// Studio pin is the directory its unit runs (the tag alone, on a node a release before the patch
// set rendered), the new binary's is the build: the plan moves Studio, so the upgrade fetches the
// release's build and waits for Studio to run it.
func TestPatchsetOnlyChangeMovesStudio(t *testing.T) {
	before, after := pinFile(t, ""), pinFile(t, "  patchset: 3\n")
	n := testNode()
	n.Version, n.Pins = "v0.2.0", PinsOf(before)
	to := &Info{Version: "v0.2.1", Platform: "linux-amd64", Pins: PinsOf(after), RegistryMigrations: migsV1}
	p := BuildPlan(n, to, PlanOptions{Canary: 1, Batch: 5})
	if p.Refusal != "" {
		t.Fatal(p.Refusal)
	}
	if got := moveNames(p.Shared); got != config.SvcStudio || len(p.System) != 0 {
		t.Fatalf("shared moves %q, system moves %q: want Studio alone", got, moveNames(p.System))
	}
	if m := p.Shared[0]; m.From != "2026.10.05-sha-94b8b06" || m.To != "2026.10.05-sha-94b8b06-p3" {
		t.Fatalf("move = %+v", m)
	}
	var out bytes.Buffer
	p.Render(&out)
	if !strings.Contains(out.String(), "Studio, the dashboard") {
		t.Errorf("the plan does not say Studio restarts:\n%s", out.String())
	}

	// A node that runs the build already: nothing moves.
	n.Pins = PinsOf(after)
	if p := BuildPlan(n, to, PlanOptions{Canary: 1, Batch: 5}); len(p.Shared) != 0 {
		t.Fatalf("same build: shared moves %s", moveNames(p.Shared))
	}
	// The next patch set moves it again.
	next := pinFile(t, "  patchset: 4\n")
	to.Pins = PinsOf(next)
	if p := BuildPlan(n, to, PlanOptions{Canary: 1, Batch: 5}); moveNames(p.Shared) != config.SvcStudio || p.Shared[0].To != "2026.10.05-sha-94b8b06-p4" {
		t.Fatalf("p3 to p4: %+v", p.Shared)
	}
	// A node without a dashboard has no Studio unit to move.
	delete(n.Pins, config.SvcStudio)
	if p := BuildPlan(n, to, PlanOptions{Canary: 1, Batch: 5}); len(p.Shared) != 0 {
		t.Fatalf("no dashboard: shared moves %s", moveNames(p.Shared))
	}
}

// The signed manifest's studio is written from the same versions.yaml as the binary's Studio pin, so
// they agree for a release with a patch set and for one without, and a manifest that names only
// the tag is not the manifest of a binary that pins the build.
func TestCheckManifestPinsComparesTheStudioBuild(t *testing.T) {
	after, before := pinFile(t, "  patchset: 3\n"), pinFile(t, "")
	info := &Info{Version: "v0.2.1", Pins: PinsOf(after)}
	if err := CheckManifestPins(info, after.Artifacts, after.StudioBuild()); err != nil {
		t.Fatalf("the release tool's manifest: %v", err)
	}
	if err := CheckManifestPins(info, after.Artifacts, after.Studio.Tag); err == nil {
		t.Fatal("a manifest that names the tag alone was taken for a binary that pins the build")
	}
	old := &Info{Version: "v0.2.0", Pins: PinsOf(before)}
	if err := CheckManifestPins(old, before.Artifacts, before.StudioBuild()); err != nil || before.StudioBuild() != "2026.10.05-sha-94b8b06" {
		t.Fatalf("a release before the patch set: %v", err)
	}
}
