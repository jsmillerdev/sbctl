package nodeupgrade

import (
	"bytes"
	"context"
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

// sameReleaseStudioBehind is a node whose installed binary is the release asked for and pins a
// Studio build its unit does not run yet: `supavise self-update` swapped the binary and its
// converge could not fetch the build, so Studio kept running the one before.
func sameReleaseStudioBehind() *fakeHost {
	h := newFakeHost()
	h.tag = "v1.1.0"
	h.node.Version = "v1.1.0"
	h.info.Pins[config.SvcStudio] = "2026.10.05-sha-94b8b06-p3"
	h.node.BinaryInfo = h.info
	h.node.Pins = newPins()
	h.node.Pins[config.SvcStudio] = "2026.10.05-sha-94b8b06"
	for i := range h.node.Projects {
		if h.node.Projects[i].Ref != "system" {
			h.node.Projects[i].Versions = map[string]string{"gotrue": authNew, "postgrest": restNew, "postgres": pgOld}
		}
	}
	return h
}

// The run on the installed release moves Studio: no binary is swapped, so it restarts the daemon,
// which renders Studio from the new build, before it waits for Studio. Without the restart nothing
// moves Studio and the wait fails after its six minutes.
func TestSameReleaseStudioMoveRestartsTheDaemon(t *testing.T) {
	h := sameReleaseStudioBehind()
	p := BuildPlan(h.node, h.node.BinaryInfo, PlanOptions{Canary: 1, Batch: 5})
	if p.BinaryChange || moveNames(p.Shared) != config.SvcStudio || p.Rollout() {
		t.Fatalf("plan: binary change %v, shared %q, rollout %v", p.BinaryChange, moveNames(p.Shared), p.Rollout())
	}
	var plan bytes.Buffer
	p.Render(&plan)
	mustContain(t, plan.String(), "supavise.service, the daemon")
	if err := Run(context.Background(), h, runOpts(h)); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	order := h.order()
	if h.has("install") || h.has("stage") || !h.has("prefetch") {
		t.Fatalf("calls: %s", order)
	}
	if i, j := strings.Index(order, "restart-daemon"), strings.Index(order, "wait studio"); i < 0 || j < i {
		t.Fatalf("the daemon did not restart before the wait: %s", order)
	}

	// A daemon that does not come back cannot be put back by a rollback either: exit 4, no wait.
	h = sameReleaseStudioBehind()
	h.restartErr = errBoom
	err := Run(context.Background(), h, runOpts(h))
	if code(t, err) != ExitNeedsOperator || h.has("wait") {
		t.Fatalf("err = %v, calls %s", err, h.order())
	}

	// A run that moves nothing restarts nothing.
	h = sameReleaseStudioBehind()
	h.node.Pins = newPins()
	h.node.Pins[config.SvcStudio] = h.info.Pins[config.SvcStudio]
	if err := Run(context.Background(), h, runOpts(h)); err != nil || h.has("restart-daemon") {
		t.Fatalf("err = %v, calls %s", err, h.order())
	}
}
