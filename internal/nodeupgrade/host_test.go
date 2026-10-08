package nodeupgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/infra"
	"github.com/supavise/supavise/internal/selfupdate"
)

func titles() []string { return []string{"Install the units", "Create the directories"} }

// newInfoWithHost is the release that has a host layer at revision 2 and needs stack revision 2.
func newInfoWithHost() *Info {
	i := newInfo()
	i.HostChanges, i.ConvergeRevision, i.InfraRevision = titles(), 2, 2
	return i
}

// A node that has not converged (a v0.1.x node, with no marker) is behind a release that has a host
// layer, and the plan says what the layer does.
func TestPlanHostLayer(t *testing.T) {
	n := testNode()
	n.ConvergeKnown, n.ConvergeRevision = true, 0
	p := BuildPlan(n, newInfoWithHost(), PlanOptions{Canary: 1, Batch: 5})
	if !p.HostPending || p.HostFrom != 0 || p.HostTo != 2 || len(p.HostChanges) != 2 {
		t.Fatalf("host: %+v", p)
	}
	var out bytes.Buffer
	p.Render(&out)
	for _, want := range []string{"Host (converge revision 0 -> 2, restarts no project):", "  - Install the units",
		"the host layer (`supavise system converge`, revision 0 -> 2) runs right after the binary is swapped"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, out.String())
		}
	}

	// A node at the release's revision, or one whose marker could not be read, claims no host work.
	n.ConvergeRevision = 2
	if p := BuildPlan(n, newInfoWithHost(), PlanOptions{}); p.HostPending {
		t.Error("a converged node has host work")
	}
	n.ConvergeKnown, n.ConvergeRevision = false, 0
	if p := BuildPlan(n, newInfoWithHost(), PlanOptions{}); p.HostPending {
		t.Error("an unreadable marker was taken for revision 0")
	}
	// A release that has no host layer (a release from before it) asks nothing.
	n.ConvergeKnown = true
	if p := BuildPlan(n, newInfo(), PlanOptions{}); p.HostPending {
		t.Error("a release without a host layer has host work")
	}
}

// The plan lists the registry migrations the release adds, by name and in full.
func TestPlanListsMigrations(t *testing.T) {
	n := testNode()
	to := newInfo()
	to.RegistryMigrations = append(append([]string{}, migsV1...), "1300_cluster.sql", "1301_replicas.sql", "1302_a.sql", "1303_b.sql", "1304_c.sql", "1305_d.sql", "1306_e.sql")
	p := BuildPlan(n, to, PlanOptions{})
	var out bytes.Buffer
	p.Render(&out)
	mustContain(t, out.String(), "Registry migrations (forward only): 1300_cluster.sql, 1301_replicas.sql, 1302_a.sql, 1303_b.sql, 1304_c.sql, 1305_d.sql, 1306_e.sql")
	mustContain(t, out.String(), "migrations only go forward")
}

// A plan whose only work is the host layer or the stack is not empty.
func TestPlanEmptyWhileAHostOrStackRevisionIsPending(t *testing.T) {
	n := testNode()
	n.Version, n.Pins = "v1.1.0", newPins()
	for i := range n.Projects {
		if n.Projects[i].Ref != "system" {
			n.Projects[i].Versions = map[string]string{"gotrue": authNew, "postgrest": restNew, "postgres": pgOld}
		}
	}
	to := newInfo()
	if p := BuildPlan(n, to, PlanOptions{}); !p.Empty() {
		t.Fatalf("a current node has work: %+v", p)
	}

	n.ConvergeKnown, n.ConvergeRevision = true, 1
	to.ConvergeRevision, to.HostChanges = 2, titles()
	p := BuildPlan(n, to, PlanOptions{})
	if p.Empty() || !p.NodeChanges() || !p.HostPending || p.BinaryChange {
		t.Fatalf("pending converge revision: empty %v, node changes %v", p.Empty(), p.NodeChanges())
	}
	var plan bytes.Buffer
	p.Render(&plan)
	mustContain(t, plan.String(), "Projects restarted: none expected")
	mustContain(t, plan.String(), "the host layer (`supavise system converge`, revision 1 -> 2) runs after the backups")

	n.ConvergeRevision = 2
	n.Infra = &infra.Report{Platform: "aws", Stack: "supavise", Have: 1, Need: 2, Fix: "sudo -E supavise upgrade --aws",
		Missing: []infra.Missing{{Capability: "peer-rule", Title: "Peer rule in the security group", Why: "needed to add a second server"}}}
	to.InfraRevision = 2
	p = BuildPlan(n, to, PlanOptions{})
	if p.Empty() || p.NodeChanges() || !p.StackPending() {
		t.Fatalf("pending stack revision: empty %v, node changes %v, stack %+v", p.Empty(), p.NodeChanges(), p.Stack)
	}
	var out bytes.Buffer
	p.Render(&out)
	for _, want := range []string{"Infrastructure  AWS stack \"supavise\" is at revision 1; this release needs 2", "missing  Peer rule in the security group", "Fix: sudo -E supavise upgrade --aws"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "nothing to change") {
		t.Errorf("a plan with a stack to update says there is nothing to change:\n%s", out.String())
	}
	if strings.Contains(out.String(), "base backup") {
		t.Errorf("a plan that only updates the stack promises backups:\n%s", out.String())
	}

	// A stack at the revision, and a host that is not on a stack, have nothing to update.
	n.Infra.Have = 2
	if p := BuildPlan(n, to, PlanOptions{}); !p.Empty() {
		t.Error("a current stack is pending")
	}
	n.Infra = &infra.Report{Have: 1, Need: 2}
	if p := BuildPlan(n, to, PlanOptions{}); !p.Empty() {
		t.Error("a host that is not on AWS has a stack to update")
	}
	// When the release needs more than the installed one asked for, the plan still names the gap.
	n.Infra = &infra.Report{Platform: "aws", Have: 2, Need: 2}
	to.InfraRevision = 3
	p = BuildPlan(n, to, PlanOptions{})
	out.Reset()
	p.Render(&out)
	mustContain(t, out.String(), "AWS stack is at revision 2; this release needs 3")
	mustContain(t, out.String(), "Fix: sudo -E supavise upgrade --aws")
}

// release-info: the fields the plan reads, with the keys the old drivers ignore.
func TestOwnInfoCarriesTheHostAndStackRevisions(t *testing.T) {
	i, err := OwnInfo("v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if i.ConvergeRevision != hostsetup.Revision || i.InfraRevision != infra.Revision || strings.Join(i.HostChanges, "|") != strings.Join(hostsetup.Titles(), "|") || len(i.HostChanges) == 0 {
		t.Fatalf("info = %+v", i)
	}
	b, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "platform", "pins", "registry_schema", "registry_migrations", "host_changes", "converge_revision", "infra_revision"} {
		if _, ok := m[key]; !ok {
			t.Errorf("release-info JSON has no %q: %s", key, b)
		}
	}
	// What an older binary printed still parses, with the new fields at zero.
	old, err := ParseInfo([]byte(`{"version":"v0.1.1","platform":"linux-amd64","pins":{"postgres":"postgres-17.1.0-r1"},"registry_schema":"1250_compute_sizes.sql","registry_migrations":["0001_init.sql"]}`))
	if err != nil || old.ConvergeRevision != 0 || old.InfraRevision != 0 || len(old.HostChanges) != 0 {
		t.Errorf("old info = %+v, %v", old, err)
	}
	// And an old driver's parse of the new output ignores what it does not know.
	back, err := ParseInfo(b)
	if err != nil || back.ConvergeRevision != i.ConvergeRevision {
		t.Errorf("round trip: %+v, %v", back, err)
	}
}

func TestCheckManifestRevisions(t *testing.T) {
	info := &Info{Version: "v1.2.0", ConvergeRevision: 2, InfraRevision: 3}
	ok := &selfupdate.Manifest{Host: &selfupdate.ManifestHost{ConvergeRevision: 2}, AWS: &selfupdate.ManifestAWS{StackRevision: 3}}
	if err := CheckManifestRevisions(info, ok); err != nil {
		t.Errorf("matching: %v", err)
	}
	if err := CheckManifestRevisions(info, &selfupdate.Manifest{}); err != nil {
		t.Errorf("an older manifest: %v", err)
	}
	if err := CheckManifestRevisions(info, &selfupdate.Manifest{Host: &selfupdate.ManifestHost{ConvergeRevision: 1}}); err == nil || !strings.Contains(err.Error(), "converge revision 1") {
		t.Errorf("host mismatch: %v", err)
	}
	if err := CheckManifestRevisions(info, &selfupdate.Manifest{AWS: &selfupdate.ManifestAWS{StackRevision: 2}}); err == nil || !strings.Contains(err.Error(), "stack revision 2") {
		t.Errorf("stack mismatch: %v", err)
	}
}

// Without a swap, a host layer that is behind is converged after the backups, before anything else.
func TestHostLayerWithoutASwap(t *testing.T) {
	h := newFakeHost()
	h.node.Version, h.node.Pins = "v1.1.0", newPins()
	h.node.ConvergeKnown, h.node.ConvergeRevision = true, 0
	for i := range h.node.Projects {
		if h.node.Projects[i].Ref != "system" {
			h.node.Projects[i].Versions = map[string]string{"gotrue": authNew, "postgrest": restNew, "postgres": pgOld}
		}
	}
	h.info = newInfoWithHost()
	h.info.Version = "v1.1.0"
	if err := Run(context.Background(), h, runOpts(h)); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	order := h.order()
	if !strings.Contains(order, "backup") || strings.Index(order, "backup") > strings.Index(order, "converge") || strings.Contains(order, "install") {
		t.Fatalf("order: %s", order)
	}
	mustContain(t, h.out.String(), "converging the host (revision 0 -> 2)")
	mustContain(t, h.out.String(), "Host (converge revision 0 -> 2")

	// A failure of it fails the upgrade: nothing was moved, the node is as healthy as before (exit 3).
	h = newFakeHost()
	h.node.Version, h.node.Pins, h.node.ConvergeKnown = "v1.1.0", newPins(), true
	h.info = newInfoWithHost()
	h.info.Version = "v1.1.0"
	h.convergeErr = errors.New("ufw is broken")
	for i := range h.node.Projects {
		if h.node.Projects[i].Ref != "system" {
			h.node.Projects[i].Versions = map[string]string{"gotrue": authNew, "postgrest": restNew, "postgres": pgOld}
		}
	}
	err := Run(context.Background(), h, runOpts(h))
	if code(t, err) != ExitRolledBack || !strings.Contains(err.Error(), "converging the host failed: ufw is broken") {
		t.Fatalf("err = %v (exit %d)", err, ExitCode(err))
	}
	if h.has("projects") || h.has("wait") {
		t.Errorf("the upgrade went on after a failed converge: %s", h.order())
	}
}

// --aws: the stack step runs after the gates and before anything is prepared or changed.
func TestStackStepRunsFirst(t *testing.T) {
	h := newFakeHost()
	o := runOpts(h)
	o.AWS, o.StackName, o.StackSets = true, "supavise", []string{"Failover=on"}
	if err := Run(context.Background(), h, o); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	order := h.order()
	if !(strings.Index(order, "confirm") < 0) { // --yes: no confirmation
		t.Errorf("order: %s", order)
	}
	if strings.Index(order, "stack v1.1.0") < 0 || strings.Index(order, "stack v1.1.0") > strings.Index(order, "prefetch") {
		t.Fatalf("the stack step is not before the preparation: %s", order)
	}
	if h.stackOpts.Name != "supavise" || len(h.stackOpts.Sets) != 1 || h.stackOpts.Sets[0] != "Failover=on" {
		t.Errorf("stack options = %+v", h.stackOpts)
	}
	mustContain(t, h.out.String(), "Supavise v1.1.0 is running")
}

func TestStackStepRefusalLeavesTheNodeAlone(t *testing.T) {
	for _, c := range []struct {
		code int
		word string
	}{{2, "was refused"}, {3, "failed"}} {
		h := newFakeHost()
		h.stackErr = &StackError{Code: c.code, Err: errors.New("the change set would replace the instance")}
		o := runOpts(h)
		o.AWS = true
		err := Run(context.Background(), h, o)
		if code(t, err) != ExitRefused || !strings.Contains(err.Error(), "the AWS stack update "+c.word) || !strings.Contains(err.Error(), "the node was not changed") {
			t.Errorf("script exit %d: %v (exit %d)", c.code, err, ExitCode(err))
		}
		for _, call := range []string{"prefetch", "backup", "install"} {
			if h.has(call) {
				t.Errorf("script exit %d: %s ran after a stack failure: %s", c.code, call, h.order())
			}
		}
	}
}

// Without the operator's credentials the host side goes ahead and the command is printed twice: where
// the step would have been, and at the end.
func TestStackStepWithoutCredentials(t *testing.T) {
	h := newFakeHost()
	h.stackCmd = "sudo -E supavise upgrade --aws"
	o := runOpts(h)
	o.AWS = true
	if err := Run(context.Background(), h, o); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if !h.has("install") {
		t.Errorf("the host side did not go ahead: %s", h.order())
	}
	if n := strings.Count(h.out.String(), "sudo -E supavise upgrade --aws"); n != 2 {
		t.Errorf("the command is printed %d times:\n%s", n, h.out)
	}
	mustContain(t, h.out.String(), "so the stack was not updated")
}

// A plan whose only work is the stack: without --aws the run says so and succeeds (and a timer
// raises infra_behind); with --aws it runs the update and nothing else.
func TestStackOnly(t *testing.T) {
	stackOnly := func() *fakeHost {
		h := newFakeHost()
		h.node.Version, h.node.Pins, h.node.ConvergeKnown, h.node.ConvergeRevision = "v1.1.0", newPins(), true, 2
		for i := range h.node.Projects {
			if h.node.Projects[i].Ref != "system" {
				h.node.Projects[i].Versions = map[string]string{"gotrue": authNew, "postgrest": restNew, "postgres": pgOld}
			}
		}
		h.node.Infra = &infra.Report{Platform: "aws", Stack: "supavise", Have: 1, Need: 2}
		h.info = newInfoWithHost()
		h.info.Version = "v1.1.0"
		return h
	}

	h := stackOnly()
	var events []Event
	o := runOpts(h)
	o.Unattended = true
	o.Notify = func(_ context.Context, ev Event) { events = append(events, ev) }
	if err := Run(context.Background(), h, o); err != nil {
		t.Fatal(err)
	}
	if h.has("backup") || h.has("stack") || h.has("confirm") {
		t.Errorf("a stack-only plan touched the node: %s", h.order())
	}
	mustContain(t, h.out.String(), "Infrastructure  AWS stack \"supavise\" is at revision 1; this release needs 2")
	if len(events) != 1 || events[0].Kind != EventInfraBehind || !events[0].Unattended || !strings.Contains(events[0].Cause, "revision 1") {
		t.Errorf("events = %+v", events)
	}
	// An interactive run only prints the plan.
	h = stackOnly()
	events = nil
	o = runOpts(h)
	o.Notify = func(_ context.Context, ev Event) { events = append(events, ev) }
	if err := Run(context.Background(), h, o); err != nil || len(events) != 1 {
		t.Errorf("%v, %v", err, events)
	}

	// With --aws: the update, with no node gates, no confirmation under --yes, no backups.
	h = stackOnly()
	h.node.Verdict = VerdictDown // the stack can be updated even when the node is down
	o = runOpts(h)
	o.AWS = true
	if err := Run(context.Background(), h, o); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if !h.has("stack v1.1.0") || h.has("backup") || h.has("install") {
		t.Errorf("order: %s", h.order())
	}

	// Without credentials the update did not happen, which is a refusal here (nothing else was asked).
	h = stackOnly()
	h.stackCmd = "sudo -E supavise upgrade --aws"
	o = runOpts(h)
	o.AWS = true
	if err := Run(context.Background(), h, o); code(t, err) != ExitRefused || !strings.Contains(err.Error(), "no AWS credentials") {
		t.Errorf("err = %v", err)
	}

	// --plan --aws prints and stops; everything current with --aws still looks at the stack.
	h = stackOnly()
	o = runOpts(h)
	o.AWS, o.Plan = true, true
	if err := Run(context.Background(), h, o); err != nil || h.has("stack") {
		t.Errorf("plan: %v, %s", err, h.order())
	}
	h = stackOnly()
	h.node.Infra.Have = 2
	o = runOpts(h)
	o.AWS = true
	if err := Run(context.Background(), h, o); err != nil || !h.has("stack v1.1.0") {
		t.Errorf("a current stack with --aws: %v, %s", err, h.order())
	}
}

func TestAWSRefusesUnattended(t *testing.T) {
	h := newFakeHost()
	o := runOpts(h)
	o.AWS, o.Unattended = true, true
	err := Run(context.Background(), h, o)
	if code(t, err) != ExitRefused || !strings.Contains(err.Error(), "cannot run with --unattended") {
		t.Errorf("err = %v", err)
	}
	if len(h.calls) != 0 {
		t.Errorf("calls before the refusal: %v", h.calls)
	}
}

// A host layer that fails after the swap is the upgrade's failure: the previous binary is put back
// before any service or project moves, and the cause names the converge.
func TestConvergeFailureAfterTheSwapRollsBack(t *testing.T) {
	h := newFakeHost()
	h.node.ConvergeKnown = true
	h.info = newInfoWithHost()
	h.installErr, h.installSwaps = errors.New("supavise system converge: exit status 1"), true
	var events []Event
	o := runOpts(h)
	o.Notify = func(_ context.Context, ev Event) { events = append(events, ev) }
	err := Run(context.Background(), h, o)
	if code(t, err) != ExitRolledBack || !strings.Contains(err.Error(), "supavise system converge: exit status 1") {
		t.Fatalf("err = %v (exit %d)\n%s", err, ExitCode(err), h.order())
	}
	if !strings.Contains(h.order(), "restore v1.0.0 (from v1.1.0)") || h.has("projects") {
		t.Fatalf("calls: %s", h.order())
	}
	last := events[len(events)-1]
	if last.Kind != EventRolledBack || !strings.Contains(last.Cause, "supavise system converge") {
		t.Errorf("events = %+v", events)
	}
}
