package nodeupgrade

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func moveNames(ms []ServiceMove) string {
	var s []string
	for _, m := range ms {
		s = append(s, m.Service)
	}
	return strings.Join(s, ",")
}

// The plan lists every service whose release changes, in the order they are rolled, and
// separates the node's own (the system project and the shared services) from the projects'.
func TestPlanDiff(t *testing.T) {
	n := testNode()
	p := BuildPlan(n, newInfo(), PlanOptions{Canary: 1, Batch: 5})
	if p.Refusal != "" {
		t.Fatal(p.Refusal)
	}
	if !p.BinaryChange || p.From != "v1.0.0" || p.To != "v1.1.0" {
		t.Fatalf("binary: %+v", p)
	}
	if got := moveNames(p.System); got != "gotrue" {
		t.Fatalf("system moves = %s (postgres does not change; postgrest has no system unit)", got)
	}
	if got := moveNames(p.Shared); got != "supavisor,realtime,storage,studio" && got != "realtime,storage,studio" {
		t.Fatalf("shared moves = %s, want the changed ones in roll order", got)
	}
	if p.Shared[0].Service != "realtime" || p.Shared[len(p.Shared)-1].Service != "studio" {
		t.Fatalf("shared order = %s", moveNames(p.Shared))
	}
	if strings.Join(p.Upgrade, ",") != "aaaaaaaaaaaaaaaaaaaa,bbbbbbbbbbbbbbbbbbbb" || len(p.Skipped) != 1 || p.Skipped[0].Ref != "cccccccccccccccccccc" || !strings.Contains(p.Skipped[0].Why, "paused") {
		t.Fatalf("projects: upgrade %v, skipped %+v", p.Upgrade, p.Skipped)
	}
	if p.ProjectTarget["gotrue"] != authNew || p.ProjectTarget["postgrest"] != restNew {
		t.Fatalf("project target = %v", p.ProjectTarget)
	}
	if _, ok := p.ProjectTarget["postgres"]; ok {
		t.Fatalf("PostgreSQL is in the project target without --include-postgres: %v", p.ProjectTarget)
	}
	if p.Empty() {
		t.Fatal("a plan with moves is empty")
	}
}

func TestPlanPostgresIsOptIn(t *testing.T) {
	n := testNode()
	to := newInfo()
	to.Pins["postgres"] = pgNew
	p := BuildPlan(n, to, PlanOptions{})
	if _, ok := p.ProjectTarget["postgres"]; ok || p.HeldBack != 2 || p.HeldTo != pgNew {
		t.Fatalf("without the flag: target %v, held back %d (%s)", p.ProjectTarget, p.HeldBack, p.HeldTo)
	}
	// The system cluster follows the node whatever the flag says.
	if !strings.Contains(moveNames(p.System), "postgres") {
		t.Fatalf("system moves = %s", moveNames(p.System))
	}
	var out bytes.Buffer
	p.Render(&out)
	for _, want := range []string{"pass --include-postgres", "system PostgreSQL cluster", "registry"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, out.String())
		}
	}
	p = BuildPlan(n, to, PlanOptions{IncludePostgres: true})
	if p.ProjectTarget["postgres"] != pgNew || p.HeldBack != 0 {
		t.Fatalf("with the flag: target %v, held back %d", p.ProjectTarget, p.HeldBack)
	}
	out.Reset()
	p.Render(&out)
	if !strings.Contains(out.String(), "PostgreSQL") || strings.Contains(out.String(), "pass --include-postgres") {
		t.Fatalf("plan with the flag:\n%s", out.String())
	}
}

func TestPlanRefusals(t *testing.T) {
	n := testNode()
	// A release that is older than the installed one.
	to := newInfo()
	to.Version = "v0.9.0"
	if p := BuildPlan(n, to, PlanOptions{}); !strings.Contains(p.Refusal, "older than the installed") || !strings.Contains(p.Refusal, "supavise rollback") {
		t.Fatalf("older release: %q", p.Refusal)
	}
	// A new Postgres major version has no upgrade path.
	to = newInfo()
	to.Pins["postgres"] = "postgres-18.1.0-r0"
	if p := BuildPlan(n, to, PlanOptions{}); !strings.Contains(p.Refusal, "17 to 18") {
		t.Fatalf("postgres major: %q", p.Refusal)
	}
}

func TestPlanSkipsWhatCannotMove(t *testing.T) {
	n := testNode()
	n.Projects = append(n.Projects,
		Project{Ref: "dddddddddddddddddddd", Name: "d", Status: "ACTIVE_UNHEALTHY", Versions: map[string]string{"gotrue": authOld, "postgrest": restOld, "postgres": pgOld}},
		// Newer than the release pins: a project is never moved to an older release.
		Project{Ref: "eeeeeeeeeeeeeeeeeeee", Name: "e", Status: "ACTIVE_HEALTHY", Versions: map[string]string{"gotrue": "auth-v2.300.0-r0", "postgrest": restNew, "postgres": pgOld}},
		// Already on the release.
		Project{Ref: "ffffffffffffffffffff", Name: "f", Status: "ACTIVE_HEALTHY", Versions: map[string]string{"gotrue": authNew, "postgrest": restNew, "postgres": pgOld}},
		// Records nothing: it runs the node's pins, which are the old ones.
		Project{Ref: "gggggggggggggggggggg", Name: "g", Status: "ACTIVE_HEALTHY", Versions: map[string]string{}},
	)
	p := BuildPlan(n, newInfo(), PlanOptions{})
	skipped := map[string]string{}
	for _, s := range p.Skipped {
		skipped[s.Ref[:1]] = s.Why
	}
	if !strings.Contains(skipped["d"], "ACTIVE_UNHEALTHY") || !strings.Contains(skipped["e"], "newer than") || !strings.Contains(skipped["c"], "paused") || len(skipped) != 3 {
		t.Fatalf("skipped = %v", skipped)
	}
	if p.Current != 1 {
		t.Fatalf("already there = %d, want 1", p.Current)
	}
	if got := strings.Join(p.Upgrade, ","); !strings.Contains(got, "gggg") || strings.Contains(got, "ffff") {
		t.Fatalf("upgrade = %s (a project that records nothing runs the node's old pins and moves)", got)
	}
}

func TestPlanEmptyWhenNothingDiffers(t *testing.T) {
	n := testNode()
	n.Version, n.Pins = "v1.1.0", newPins()
	for i := range n.Projects {
		n.Projects[i].Versions = map[string]string{"gotrue": authNew, "postgrest": restNew, "postgres": pgOld}
	}
	p := BuildPlan(n, newInfo(), PlanOptions{})
	if !p.Empty() || p.BinaryChange {
		t.Fatalf("plan = %+v", p)
	}
	var out bytes.Buffer
	p.Render(&out)
	if !strings.Contains(out.String(), "nothing to change") {
		t.Fatalf("plan:\n%s", out.String())
	}
	// The same binary with services behind it is a plan: the node converges on its release.
	n.Pins = oldPins()
	if p := BuildPlan(n, newInfo(), PlanOptions{}); p.Empty() || p.BinaryChange {
		t.Fatalf("converge plan: %+v", p)
	}
}

func TestPlanRegistrySchemaNote(t *testing.T) {
	n := testNode()
	to := newInfo()
	to.RegistrySchema = schemaV2
	p := BuildPlan(n, to, PlanOptions{})
	if p.SchemaFrom != schemaV1 || p.SchemaTo != schemaV2 {
		t.Fatalf("schema %s -> %s", p.SchemaFrom, p.SchemaTo)
	}
	var out bytes.Buffer
	p.Render(&out)
	if !strings.Contains(out.String(), "registry schema moves") || !strings.Contains(out.String(), "pre-upgrade backup of the system project") {
		t.Fatalf("plan:\n%s", out.String())
	}
}

// The impact text names what each release actually restarts and nothing else.
func TestPlanSaysWhatRestarts(t *testing.T) {
	n := testNode()
	to := newInfo()
	// Only GoTrue and PostgREST move: no shared service restarts, no pooler or websocket impact.
	to.Pins = oldPins()
	to.Pins["gotrue"], to.Pins["postgrest"] = authNew, restNew
	n.Pins["gotrue"] = authNew // the node already runs it; projects do not
	p := BuildPlan(n, to, PlanOptions{Canary: 1, Batch: 5})
	var out bytes.Buffer
	p.Render(&out)
	s := out.String()
	if strings.Contains(s, "Supavisor") || strings.Contains(s, "websocket") {
		t.Fatalf("a release that moves neither restarts them:\n%s", s)
	}
	if !strings.Contains(s, "GoTrue and PostgREST of 2 project(s), 1 canary first, then 5 at a time") || !strings.Contains(s, "a few seconds") {
		t.Fatalf("plan:\n%s", s)
	}
	// Moving Supavisor and Realtime says what drops.
	to = newInfo()
	to.Pins["supavisor"] = "pooler-v2.10.0-r0"
	out.Reset()
	BuildPlan(n, to, PlanOptions{}).Render(&out)
	for _, want := range []string{"every pooled connection", "every Realtime websocket", "Storage", "Studio"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, out.String())
		}
	}
}

func TestBackupRefs(t *testing.T) {
	got := BackupRefs(testNode())
	if strings.Join(got, ",") != "system,aaaaaaaaaaaaaaaaaaaa,bbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("refs = %v (the system project first, then running projects; a paused one is not backed up)", got)
	}
}

func TestGates(t *testing.T) {
	plan := func(n *Node) *Plan { return BuildPlan(n, newInfo(), PlanOptions{}) }
	opts := GateOptions{Now: t0}
	n := testNode()
	if g := CheckGates(n, plan(n), opts); len(g.Refusals)+len(g.Warnings) != 0 {
		t.Fatalf("a healthy node: %+v", g)
	}

	// Down is refused always; degraded only for an unattended run.
	n = testNode()
	n.Verdict, n.Summary = VerdictDown, "down: the system cluster is not running"
	if g := CheckGates(n, plan(n), opts); len(g.Refusals) != 1 || !strings.Contains(g.Refusals[0], "down") {
		t.Fatalf("down: %+v", g)
	}
	n = testNode()
	n.Verdict, n.Summary = VerdictDegraded, "degraded: 1 project not answering: abc"
	if g := CheckGates(n, plan(n), opts); len(g.Refusals) != 0 || len(g.Warnings) != 1 {
		t.Fatalf("degraded, attended: %+v", g)
	}
	if g := CheckGates(n, plan(n), GateOptions{Now: t0, Unattended: true}); len(g.Refusals) != 1 || !strings.Contains(g.Refusals[0], "unattended") {
		t.Fatalf("degraded, unattended: %+v", g)
	}
	n.Verdict = VerdictUnknown
	if g := CheckGates(n, plan(n), GateOptions{Now: t0, Unattended: true}); len(g.Refusals) != 1 {
		t.Fatalf("unknown, unattended: %+v", g)
	}

	// Backups: an unattended upgrade needs every running project (and the registry's) backed up
	// within 24 hours; a paused project is not asked.
	n = testNode()
	n.Projects[2].LastBackup = t0.Add(-30 * time.Hour)
	n.Projects[1].LastBackup = time.Time{}
	g := CheckGates(n, plan(n), GateOptions{Now: t0, Unattended: true})
	if len(g.Refusals) != 1 || !strings.Contains(g.Refusals[0], "2 project(s) have no backup newer than 24h0m0s") || strings.Contains(g.Refusals[0], "cccc") {
		t.Fatalf("stale backups: %+v", g)
	}
	if g := CheckGates(n, plan(n), opts); len(g.Refusals) != 0 {
		t.Fatalf("an attended upgrade takes fresh backups anyway: %+v", g)
	}

	// Key escrow: a warning to a person, a refusal to a timer.
	n = testNode()
	n.Escrow = Escrow{Known: true, Covered: false}
	if g := CheckGates(n, plan(n), opts); len(g.Refusals) != 0 || len(g.Warnings) != 1 || !strings.Contains(g.Warnings[0], "master key") {
		t.Fatalf("escrow, attended: %+v", g)
	}
	if g := CheckGates(n, plan(n), GateOptions{Now: t0, Unattended: true}); len(g.Refusals) != 1 || !strings.Contains(g.Refusals[0], "master key") {
		t.Fatalf("escrow, unattended: %+v", g)
	}
	n.Escrow = Escrow{}
	if g := CheckGates(n, plan(n), GateOptions{Now: t0, Unattended: true}); len(g.Refusals) != 0 || len(g.Warnings) != 1 {
		t.Fatalf("escrow unknown is a warning even unattended: %+v", g)
	}

	// Disk: refused whoever asks.
	n = testNode()
	n.DiskFree = 1 << 30
	if g := CheckGates(n, plan(n), opts); len(g.Refusals) != 1 || !strings.Contains(g.Refusals[0], "free") {
		t.Fatalf("disk: %+v", g)
	}
	n = testNode()
	n.LocalBackups = true
	n.Projects[1].DiskBytes, n.Projects[2].DiskBytes = 120<<30, 120<<30
	if g := CheckGates(n, plan(n), opts); len(g.Refusals) != 1 {
		t.Fatalf("a local backup copy of 240 GiB (about 120 GiB compressed) does not fit in 100 GiB: %+v", g)
	}
}
