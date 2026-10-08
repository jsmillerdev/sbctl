package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/projectconfig"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// node gives the harness a machine: memory in GB, cores, and the overcommit ratio.
func (h *harness) node(memGB int64, cpus int, overcommit float64) {
	h.cfg.Compute.Overcommit = overcommit
	h.e.SetNode(func() NodeResources { return NodeResources{MemoryBytes: memGB * gib, CPUs: cpus} })
}

func (h *harness) events(t *testing.T, ref string) string {
	t.Helper()
	evs, err := h.reg.ListEvents(context.Background(), ref, 100)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	return strings.Join(kinds, " ")
}

func (h *harness) project(t *testing.T, ref string) *registry.Project {
	t.Helper()
	p, err := h.reg.GetProject(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResizeRestartsOnlyThatProject(t *testing.T) {
	h := newHarness(t)
	h.node(16, 4, 3)
	a := h.create(t)
	b, err := h.e.Create(context.Background(), CreateRequest{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	h.plane.calls, h.plane.started = nil, nil
	h.tenant.ensured = nil

	p, err := h.e.Resize(context.Background(), a.Ref, "small")
	if err != nil {
		t.Fatal(err)
	}
	if p.Class != "small" || p.Limits != (config.Limits{MemoryMax: "2G", CPUQuota: "100%"}) || p.Status != registry.StatusActiveHealthy {
		t.Fatalf("after resize: class %q limits %+v status %s", p.Class, p.Limits, p.Status)
	}
	if got := strings.Join(h.plane.calls, ","); got != "Stop "+a.Ref+",Start "+a.Ref {
		t.Fatalf("plane calls = %s; only the resized project may restart", got)
	}
	if len(h.plane.started) != 1 || h.plane.started[0].Class != "small" || h.plane.started[0].Limits.MemoryMax != "2G" {
		t.Fatalf("started with %+v", h.plane.started)
	}
	if len(h.tenant.ensured) != 1 || h.tenant.ensured[0].Ref != a.Ref || h.tenant.ensured[0].PoolSize != 35 || h.tenant.ensured[0].MaxClients != 400 {
		t.Fatalf("pooler tenant = %+v", h.tenant.ensured)
	}
	if got := h.project(t, b.Ref); got.Class != "micro" {
		t.Fatalf("the other project changed: %+v", got)
	}
	if ev := h.events(t, a.Ref); !strings.Contains(ev, EventResizeStarted) || !strings.Contains(ev, EventResized) {
		t.Fatalf("events = %s", ev)
	}
}

func TestResizeToTheSameSizeDoesNothing(t *testing.T) {
	h := newHarness(t)
	p := h.create(t)
	h.plane.calls = nil
	run, err := h.e.BeginResize(context.Background(), p.Ref, "Micro")
	if err != nil {
		t.Fatal(err)
	}
	if run.Changed() {
		t.Fatal("same size reported as a change")
	}
	if err := run.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.plane.calls) != 0 {
		t.Fatalf("plane calls = %v", h.plane.calls)
	}
}

func TestResizeShowsResizingWhileItRuns(t *testing.T) {
	h := newHarness(t)
	p := h.create(t)
	run, err := h.e.BeginResize(context.Background(), p.Ref, "medium")
	if err != nil {
		t.Fatal(err)
	}
	if got := h.project(t, p.Ref); got.Status != registry.StatusResizing || got.Class != "medium" {
		t.Fatalf("while resizing: %s %s", got.Status, got.Class)
	}
	// Any other operation on the project is refused, and the lock is not waited for.
	if _, err := h.e.BeginResize(context.Background(), p.Ref, "large"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second resize: %v", err)
	}
	if err := run.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.project(t, p.Ref); got.Status != registry.StatusActiveHealthy {
		t.Fatalf("after: %s", got.Status)
	}
	// The lock is free again.
	if _, err := h.e.Resize(context.Background(), p.Ref, "large"); err != nil {
		t.Fatal(err)
	}
}

func TestResizeCloseWithoutRunUndoesTheRecord(t *testing.T) {
	h := newHarness(t)
	p := h.create(t)
	run, err := h.e.BeginResize(context.Background(), p.Ref, "large")
	if err != nil {
		t.Fatal(err)
	}
	run.Close()
	got := h.project(t, p.Ref)
	if got.Class != "micro" || got.Status != registry.StatusActiveHealthy || got.Limits.MemoryMax != "1G" {
		t.Fatalf("after Close: %+v", got)
	}
}

func TestResizeRefusedWhileAnotherOperationRuns(t *testing.T) {
	h := newHarness(t)
	p := h.create(t)
	unlock, err := h.e.lock(context.Background(), p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = h.e.BeginResize(context.Background(), p.Ref, "small")
	if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "another operation") {
		t.Fatalf("err = %v", err)
	}
	if len(h.plane.started) > 1 || h.project(t, p.Ref).Class != "micro" {
		t.Fatal("a refused resize changed the project")
	}
}

func TestResizeRefusedInTheWrongState(t *testing.T) {
	h := newHarness(t)
	p := h.create(t)
	for _, st := range []registry.Status{registry.StatusComingUp, registry.StatusRestarting, registry.StatusResizing, registry.StatusRestoring, registry.StatusGoingDown, registry.StatusInitFailed} {
		if err := h.reg.SetProjectStatus(context.Background(), p.Ref, st); err != nil {
			t.Fatal(err)
		}
		if _, err := h.e.BeginResize(context.Background(), p.Ref, "small"); !errors.Is(err, ErrInvalidState) {
			t.Errorf("%s: err = %v", st, err)
		}
	}
	if _, err := h.e.BeginResize(context.Background(), config.SystemRef, "small"); !errors.Is(err, ErrInvalidState) {
		t.Errorf("system: %v", err)
	}
	if _, err := h.e.BeginResize(context.Background(), p.Ref, "gigantic"); err == nil || !strings.Contains(err.Error(), "unknown project size") {
		t.Errorf("unknown size: %v", err)
	}
	if _, err := h.e.BeginResize(context.Background(), p.Ref, ""); err == nil {
		t.Error("an empty size was accepted")
	}
}

func TestResizeOfAPausedProjectChangesTheRecordOnly(t *testing.T) {
	h := newHarness(t)
	p := h.create(t)
	if err := h.e.Pause(context.Background(), p.Ref); err != nil {
		t.Fatal(err)
	}
	h.plane.calls, h.plane.started = nil, nil
	got, err := h.e.Resize(context.Background(), p.Ref, "large")
	if err != nil {
		t.Fatal(err)
	}
	if got.Class != "large" || got.Limits.MemoryMax != "8G" || got.Status != registry.StatusInactive {
		t.Fatalf("paused resize: %+v", got)
	}
	if len(h.plane.calls) != 0 {
		t.Fatalf("a paused project was touched: %v", h.plane.calls)
	}
	// Resume renders from the record.
	if err := h.e.Resume(context.Background(), p.Ref); err != nil {
		t.Fatal(err)
	}
	if n := len(h.plane.started); n != 1 || h.plane.started[0].Class != "large" {
		t.Fatalf("resume started %+v", h.plane.started)
	}
}

// A start that fails on the new size puts the old one back: record, units and tenant.
func TestResizeFailureRestoresThePreviousSize(t *testing.T) {
	h := newHarness(t)
	p := h.create(t)
	h.plane.calls, h.plane.started = nil, nil
	h.tenant.ensured = nil
	h.plane.failOnce["Start"] = errors.New("postgres did not answer")

	_, err := h.e.Resize(context.Background(), p.Ref, "large")
	if err == nil || !strings.Contains(err.Error(), "back on Micro") || !strings.Contains(err.Error(), "postgres did not answer") {
		t.Fatalf("err = %v", err)
	}
	got := h.project(t, p.Ref)
	if got.Class != "micro" || got.Limits.MemoryMax != "1G" || got.Status != registry.StatusActiveHealthy {
		t.Fatalf("after failure: class %q limits %+v status %s", got.Class, got.Limits, got.Status)
	}
	if len(h.plane.started) != 2 || h.plane.started[0].Class != "large" || h.plane.started[1].Class != "micro" || h.plane.started[1].Limits.MemoryMax != "1G" {
		t.Fatalf("starts = %+v", h.plane.started)
	}
	if n := len(h.tenant.ensured); n != 1 || h.tenant.ensured[0].PoolSize != 20 {
		t.Fatalf("the tenant must be set back to Micro's pool: %+v", h.tenant.ensured)
	}
	if ev := h.events(t, p.Ref); !strings.Contains(ev, EventResizeFailed) || strings.Contains(ev, EventResized+" ") {
		t.Fatalf("events = %s", ev)
	}
	// Not stuck: the project can be resized again.
	if _, err := h.e.Resize(context.Background(), p.Ref, "small"); err != nil {
		t.Fatal(err)
	}
}

func TestResizeFailureOfThePoolerTenantRollsBack(t *testing.T) {
	h := newHarness(t)
	p := h.create(t)
	h.tenant.err = errors.New("supavisor is down")
	_, err := h.e.Resize(context.Background(), p.Ref, "small")
	if err == nil || !strings.Contains(err.Error(), "pooler tenant") {
		t.Fatalf("err = %v", err)
	}
	if got := h.project(t, p.Ref); got.Class != "micro" || got.Status != registry.StatusActiveHealthy {
		t.Fatalf("after failure: %+v", got)
	}
}

// If the old size does not come back either, the project is flagged, not left RESIZING.
func TestResizeFailureThatCannotRollBack(t *testing.T) {
	h := newHarness(t)
	p := h.create(t)
	h.plane.failOn["Start"] = errors.New("boom")
	_, err := h.e.Resize(context.Background(), p.Ref, "small")
	if err == nil || !strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("err = %v", err)
	}
	got := h.project(t, p.Ref)
	if got.Status != registry.StatusActiveUnhealthy || got.Class != "micro" {
		t.Fatalf("after: %s %s", got.Status, got.Class)
	}
}

func TestResizeRefusedWhenTheNodeCannotHonourIt(t *testing.T) {
	h := newHarness(t)
	h.node(8, 2, 1.5) // a budget of 12 GB
	a := h.create(t)  // Micro: 1 GB
	_, err := h.e.Resize(context.Background(), a.Ref, "xlarge")
	var ce *CapacityError
	if !errors.As(err, &ce) || !strings.Contains(ce.Message, "4 vCPUs") || !strings.Contains(ce.Message, "2 cores") {
		t.Fatalf("xlarge on 2 cores: %v", err)
	}
	got := h.project(t, a.Ref)
	if got.Class != "micro" || got.Status != registry.StatusActiveHealthy {
		t.Fatalf("a refused resize changed the project: %+v", got)
	}
	// Large fits when the room is there: 8 GB + nothing else.
	if _, err := h.e.Resize(context.Background(), a.Ref, "large"); err != nil {
		t.Fatal(err)
	}
	// Another project's Large no longer fits: 8 + 8 > 12.
	b := h.create(t)
	_, err = h.e.Resize(context.Background(), b.Ref, "large")
	if !errors.As(err, &ce) || !strings.Contains(ce.Message, "memory cap of 8 GB") || !strings.Contains(ce.Message, "12 GB") {
		t.Fatalf("second Large: %v", err)
	}
	// Shrinking always works, even when the node is over its budget.
	h.node(4, 2, 1)
	if _, err := h.e.Resize(context.Background(), a.Ref, "medium"); err != nil {
		t.Fatalf("shrinking an over-committed node: %v", err)
	}
}

func TestCreateWithAnExplicitSizeIsCheckedAgainstTheNode(t *testing.T) {
	h := newHarness(t)
	h.node(4, 2, 1) // a budget of 4 GB
	if _, err := h.e.Create(context.Background(), CreateRequest{Name: "a", Class: "large"}); err == nil {
		t.Fatal("Large on a 4 GB node was created")
	} else if _, ok := IsCapacity(err); !ok {
		t.Fatalf("err = %v", err)
	}
	ps, _ := h.reg.ListProjects(context.Background())
	if len(ps) != 0 {
		t.Fatalf("a refused create left %d rows", len(ps))
	}
	if _, err := h.e.Create(context.Background(), CreateRequest{Name: "b", Class: "medium"}); err != nil {
		t.Fatal(err)
	}
	// The node is full now; an explicit Nano is refused too, a create without a size is not
	// checked (the default is what the operator gets).
	if _, err := h.e.Create(context.Background(), CreateRequest{Name: "c", Class: "nano"}); err == nil {
		t.Fatal("create past the budget")
	}
	if _, err := h.e.Create(context.Background(), CreateRequest{Name: "d"}); err != nil {
		t.Fatalf("create without a size: %v", err)
	}
}

func TestOffersMarkWhatDoesNotFit(t *testing.T) {
	h := newHarness(t)
	h.node(8, 4, 2) // 16 GB
	p := h.create(t)
	if _, err := h.e.Create(context.Background(), CreateRequest{Name: "big", Class: "large"}); err != nil {
		t.Fatal(err)
	}
	offers, err := h.e.Offers(context.Background(), p.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != len(hostedTable) {
		t.Fatalf("%d offers", len(offers))
	}
	for _, o := range offers {
		switch o.Class.Name {
		case "micro":
			if !o.Current || !o.Fits {
				t.Errorf("current size: %+v", o)
			}
		case "nano", "small", "medium", "large":
			if !o.Fits {
				t.Errorf("%s should fit: %s", o.Class.Name, o.Reason)
			}
		case "xlarge":
			if o.Fits || !strings.Contains(o.Reason, "4 vCPUs") && !strings.Contains(o.Reason, "memory cap of 16 GB") {
				t.Errorf("xlarge: %+v", o)
			}
		default:
			if o.Fits || o.Reason == "" {
				t.Errorf("%s must be unavailable with a reason: %+v", o.Class.Name, o)
			}
		}
	}
	// Without a node nothing is refused.
	h2 := newHarness(t)
	q := h2.create(t)
	all, _ := h2.e.Offers(context.Background(), q.Ref)
	for _, o := range all {
		if !o.Fits {
			t.Errorf("%s refused without a node", o.Class.Name)
		}
	}
}

func TestPausedAndFailedProjectsDoNotHoldNodeMemory(t *testing.T) {
	h := newHarness(t)
	h.node(4, 4, 1)
	a, err := h.e.Create(context.Background(), CreateRequest{Name: "a", Class: "large"}) // 8 GB on a 4 GB node: refused
	if err == nil {
		t.Fatalf("created %v", a)
	}
	b, err := h.e.Create(context.Background(), CreateRequest{Name: "b", Class: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.e.Pause(context.Background(), b.Ref); err != nil {
		t.Fatal(err)
	}
	cp, _, _ := h.e.Capacity(context.Background(), "")
	if cp.CommittedBytes != 0 || cp.Projects != 0 {
		t.Fatalf("a paused project holds %d bytes", cp.CommittedBytes)
	}
}

func TestRecoverBringsAResizeBackOnTheRecordedSize(t *testing.T) {
	h := newHarness(t)
	p := h.create(t)
	run, err := h.e.BeginResize(context.Background(), p.Ref, "small")
	if err != nil {
		t.Fatal(err)
	}
	_ = run // the daemon dies here: the lock dies with it
	h2 := &harness{e: NewEngine(h.cfg, h.reg, h.sec, fakeArts{}, h.plane, Options{Fleet: nil}), reg: h.reg, plane: h.plane}
	rs := h2.e.Recover(context.Background())
	if len(rs) != 1 || rs[0].Ref != p.Ref || rs[0].To != registry.StatusInactive || !rs[0].Resume {
		t.Fatalf("recovered = %+v", rs)
	}
	h.plane.started = nil
	if errs := h2.e.ResumeRecovered(context.Background(), rs); len(errs) != 0 {
		t.Fatal(errs)
	}
	got := h.project(t, p.Ref)
	if got.Status != registry.StatusActiveHealthy || got.Class != "small" {
		t.Fatalf("after recovery: %s %s", got.Status, got.Class)
	}
	if len(h.plane.started) != 1 || h.plane.started[0].Limits.MemoryMax != "2G" {
		t.Fatalf("started = %+v", h.plane.started)
	}
}

// The default overcommit agrees with the sizing table of docs/guide.md: a node filled with the
// idle projects the guide promises is never refused.
func TestDefaultOvercommitCoversTheGuidesNodeSizes(t *testing.T) {
	cfg := config.Default()
	for _, tc := range []struct {
		memGB  int64
		micros int // the guide's "idle projects that fit", at the 1 GB cap of a Micro
	}{{8, 35}, {16, 90}} {
		budget := int64(float64(tc.memGB*gib) * cfg.Compute.OvercommitRatio())
		if budget < int64(tc.micros)*gib {
			t.Errorf("%d GiB node: budget %d GB for %d Micro projects", tc.memGB, budget/gib, tc.micros)
		}
	}
}

// A branch and a restore as a new project name their size, so the node judges them like a create
// with --size; a plain create is not judged.
func TestBranchAndRestoreAsCreatesAreCheckedAgainstTheNode(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.node(1, 2, 1) // a budget of 1 GB: one Micro
	parent := h.create(t)
	info := &registry.BranchInfo{ID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", ParentRef: parent.Ref, Name: "feature", State: registry.BranchCreatingProject}
	_, err := h.e.Create(ctx, CreateRequest{Name: "feature", Class: "micro", Branch: info})
	if _, ok := IsCapacity(err); !ok {
		t.Fatalf("a branch past the budget: %v", err)
	}
	// restoreAsNew passes the original's size and limits.
	_, err = h.e.Create(ctx, CreateRequest{Name: "copy", Class: parent.Class})
	if _, ok := IsCapacity(err); !ok {
		t.Fatalf("a restore as new past the budget: %v", err)
	}
	if ps, _ := h.reg.ListProjects(ctx); len(ps) != 1 {
		t.Fatalf("refused creates left %d rows", len(ps))
	}
	if _, err := h.e.Create(ctx, CreateRequest{Name: "plain"}); err != nil {
		t.Fatalf("a create without a size is not judged: %v", err)
	}
}

// Pausing a project frees its room for a resize, and resuming it must not put the node over.
func TestResumeIsCheckedAgainstTheNode(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.node(8, 4, 1) // 8 GB
	a := h.create(t)
	b, err := h.e.Create(ctx, CreateRequest{Name: "b", Class: "large"}) // 8 GB: does not fit beside a
	if _, ok := IsCapacity(err); !ok {
		t.Fatalf("b = %v, %v", b, err)
	}
	b, err = h.e.Create(ctx, CreateRequest{Name: "b", Class: "medium"}) // 4 GB
	if err != nil {
		t.Fatal(err)
	}
	if err := h.e.Pause(ctx, b.Ref); err != nil {
		t.Fatal(err)
	}
	// With b paused, a can grow into b's room: 7 GB of 8.
	if _, err := h.e.Resize(ctx, a.Ref, "large"); err != nil {
		t.Fatal(err)
	}
	h.plane.calls = nil
	err = h.e.Resume(ctx, b.Ref)
	ce, ok := IsCapacity(err)
	if !ok || !strings.Contains(ce.Message, "Medium") {
		t.Fatalf("resume over the budget: %v", err)
	}
	if got := h.project(t, b.Ref); got.Status != registry.StatusInactive || len(h.plane.calls) != 0 {
		t.Fatalf("a refused resume touched the project: %s %v", got.Status, h.plane.calls)
	}
	// Shrinking a makes room, and the resume goes through.
	if _, err := h.e.Resize(ctx, a.Ref, "small"); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Resume(ctx, b.Ref); err != nil {
		t.Fatalf("resume with room: %v", err)
	}
}

// While the rollback restarts the old size the project stays RESIZING, and only then is it healthy.
func TestResizeRollbackStaysResizingUntilTheOldUnitsAreBack(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.create(t)
	var seen []registry.Status
	h.plane.onStart = func(started registry.Project) {
		seen = append(seen, h.project(t, p.Ref).Status)
		if started.Class == "micro" {
			if rec := h.project(t, p.Ref); rec.Class != "micro" {
				t.Errorf("the record still says %s while the old units start", rec.Class)
			}
		}
	}
	h.plane.failOnce["Start"] = errors.New("postgres did not answer")
	if _, err := h.e.Resize(ctx, p.Ref, "large"); err == nil {
		t.Fatal("no error")
	}
	if len(seen) != 2 || seen[0] != registry.StatusResizing || seen[1] != registry.StatusResizing {
		t.Fatalf("statuses at the two starts = %v", seen)
	}
	if got := h.project(t, p.Ref); got.Status != registry.StatusActiveHealthy || got.Class != "micro" {
		t.Fatalf("after: %+v", got)
	}
}

// settingsCheck is a Settings that holds saved Postgres settings and judges them as the real
// validator does for the two rules the tests need.
type settingsCheck struct {
	fakeSettings
	savedSharedBuffers int64
	got                []projectconfig.CrossContext
}

func (s *settingsCheck) CheckSaved(_ context.Context, _ string, svc projectconfig.Service, cx projectconfig.CrossContext) error {
	s.got = append(s.got, cx)
	if svc == projectconfig.Postgres && cx.MemoryLimit > 0 && float64(s.savedSharedBuffers) > float64(cx.MemoryLimit)*0.4 {
		return &projectconfig.ValidationError{Msg: "shared_buffers 3GB is more than 40% of the project's memory limit"}
	}
	return nil
}

func TestDownsizeIsRefusedWhenSavedSettingsDoNotFit(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	st := &settingsCheck{savedSharedBuffers: 3 << 30}
	h.e.opts.Settings = st
	p := h.create(t)
	if _, err := h.e.Resize(ctx, p.Ref, "large"); err != nil {
		t.Fatalf("Large holds 3GB of shared buffers: %v", err)
	}
	h.plane.calls = nil
	_, err := h.e.Resize(ctx, p.Ref, "micro")
	se, ok := IsSettings(err)
	if !ok || !strings.Contains(se.Message, "shared_buffers 3GB") || !strings.Contains(se.Message, "Micro") {
		t.Fatalf("downsize with 3GB shared_buffers: %v", err)
	}
	if got := h.project(t, p.Ref); got.Class != "large" || got.Status != registry.StatusActiveHealthy || len(h.plane.calls) != 0 {
		t.Fatalf("a refused resize touched the project: %+v %v", got, h.plane.calls)
	}
	last := st.got[len(st.got)-1]
	if last.MemoryLimit != 1<<30 {
		t.Fatalf("settings were judged against %d bytes, not the target's cap", last.MemoryLimit)
	}
	// Once the setting fits, the downsize goes through.
	st.savedSharedBuffers = 256 << 20
	if _, err := h.e.Resize(ctx, p.Ref, "micro"); err != nil {
		t.Fatal(err)
	}
}
