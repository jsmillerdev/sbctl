package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jsmillerdev/supavise/internal/artifacts"
	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/fleet"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// tagArts is an artifact store whose pins a test can move, like a node update does.
type tagArts struct {
	mu       sync.Mutex
	pins     map[string]string
	fetched  []string
	fetchErr error
	// dirs maps "<svc> <tag>" to a real directory (a fake artifact); hidden names the ones DirFor
	// cannot find until FetchTag has fetched them.
	dirs   map[string]string
	hidden map[string]bool
}

func newTagArts() *tagArts {
	return &tagArts{pins: map[string]string{
		config.SvcPostgres: "postgres-17.1.0-r1", config.SvcGoTrue: "auth-v2.100.0-r1", config.SvcPostgREST: "postgrest-v12.0-r0"}}
}

func (a *tagArts) pin(svc, tag string) { a.mu.Lock(); a.pins[svc] = tag; a.mu.Unlock() }
func (a *tagArts) Tag(svc string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pins[svc], nil
}
func (a *tagArts) Dir(svc string) (string, error) {
	t, _ := a.Tag(svc)
	return a.DirFor(svc, t)
}
func (a *tagArts) DirFor(svc, tag string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hidden[svc+" "+tag] {
		return "", errors.New("not fetched")
	}
	if d := a.dirs[svc+" "+tag]; d != "" {
		return d, nil
	}
	return "/art/" + svc + "/" + tag, nil
}
func (a *tagArts) FetchTag(_ context.Context, svc, tag string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fetched = append(a.fetched, svc+" "+tag)
	delete(a.hidden, svc+" "+tag)
	if d := a.dirs[svc+" "+tag]; d != "" {
		return d, a.fetchErr
	}
	return "/art/" + svc + "/" + tag, a.fetchErr
}

// upPlane is a fake data plane that cannot run one release: any project whose versions name
// bad fails to start, or starts and fails its health check (healthBad).
type upPlane struct {
	*fakePlane
	mu        sync.Mutex
	bad       string
	healthBad bool
	// failRollback makes every start after the first failure fail as well: the old release does
	// not come back either.
	failRollback bool
	failedOnce   bool
	steps        []string
	// exts are the extensions the project's databases have installed; extCalls counts listings;
	// verifyBad is a release whose extension code does not load (VerifyExtensions fails on it).
	exts      []InstalledExtension
	extCalls  int
	verifyBad string
	// pgHealthy adds a healthy PostgreSQL entry to Health: the cluster runs whatever the API units do.
	pgHealthy bool
}

func (u *upPlane) Extensions(context.Context, *registry.Project) ([]InstalledExtension, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.extCalls++
	return u.exts, nil
}

func (u *upPlane) VerifyExtensions(_ context.Context, p *registry.Project) error {
	u.step("VerifyExtensions", p)
	if u.verifyBad != "" && u.uses(p, u.verifyBad) {
		return errors.New("extension code does not load on the new release: wrappers in postgres: wrappers_handler: could not find function")
	}
	return nil
}

func (u *upPlane) uses(p *registry.Project, tag string) bool {
	for _, t := range p.Versions {
		if t == tag && tag != "" {
			return true
		}
	}
	return false
}

func (u *upPlane) step(op string, p *registry.Project) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.steps = append(u.steps, fmt.Sprintf("%s %s", op, ShortVersion(config.SvcGoTrue, p.Versions[config.SvcGoTrue])))
}

func (u *upPlane) start(op string, p *registry.Project, f func() error) error {
	u.step(op, p)
	if u.uses(p, u.bad) && !u.healthBad {
		u.failedOnce = true
		return errors.New("gotrue did not become ready")
	}
	if u.failRollback && u.failedOnce {
		return errors.New("the old release does not start either")
	}
	return f()
}

func (u *upPlane) Start(ctx context.Context, p *registry.Project, k *secrets.ProjectKeys) error {
	return u.start("Start", p, func() error { return u.fakePlane.Start(ctx, p, k) })
}
func (u *upPlane) StartDatabase(ctx context.Context, p *registry.Project, k *secrets.ProjectKeys) error {
	return u.start("StartDatabase", p, func() error { return u.fakePlane.StartDatabase(ctx, p, k) })
}
func (u *upPlane) Reconfigure(ctx context.Context, p *registry.Project, k *secrets.ProjectKeys) error {
	return u.start("Reconfigure", p, func() error { return u.fakePlane.Reconfigure(ctx, p, k) })
}
func (u *upPlane) Stop(ctx context.Context, ref string) error {
	u.mu.Lock()
	u.steps = append(u.steps, "Stop")
	u.mu.Unlock()
	return u.fakePlane.Stop(ctx, ref)
}
func (u *upPlane) Health(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys) []ServiceHealth {
	ok := !(u.healthBad && u.uses(p, u.bad))
	hs := []ServiceHealth{{Name: config.SvcGoTrue, Healthy: ok, Status: "x", Error: "GoTrue /health: status 500"}}
	if u.pgHealthy {
		hs = append(hs, ServiceHealth{Name: config.SvcPostgres, Healthy: true, Status: "ACTIVE_HEALTHY"})
	}
	return hs
}

func (u *upPlane) log() string { u.mu.Lock(); defer u.mu.Unlock(); return strings.Join(u.steps, ", ") }

type upBackup struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (b *upBackup) BaseBackup(_ context.Context, ref string) (*registry.Backup, error) {
	return nil, errors.New("an upgrade must ask for UpgradeBackup")
}
func (b *upBackup) UpgradeBackup(_ context.Context, ref string) (*registry.Backup, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, ref)
	if b.err != nil {
		return nil, b.err
	}
	return &registry.Backup{ID: 42, Ref: ref, Location: "file:///backups/42"}, nil
}

type upHarness struct {
	e      *Engine
	reg    *registry.Memory
	arts   *tagArts
	plane  *upPlane
	backup *upBackup
	cfg    *config.Config
	ref    string
}

// newUpHarness creates one project on the old pins, then moves the node's GoTrue and PostgREST
// pins to newer releases: the state of a node after an update, before anyone upgrades a project.
func newUpHarness(t *testing.T) *upHarness {
	t.Helper()
	cfg := config.Default()
	cfg.Domain = "example.test"
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	h := &upHarness{reg: registry.NewMemory(), arts: newTagArts(), backup: &upBackup{}, cfg: cfg}
	h.plane = &upPlane{fakePlane: newFakePlane()}
	h.e = NewEngine(cfg, h.reg, sec, h.arts, h.plane, Options{Fleet: fleet.Fleet{&fakeTenant{}}, Backup: h.backup})
	p, err := h.e.Create(context.Background(), CreateRequest{Name: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	h.ref = p.Ref
	h.arts.pin(config.SvcGoTrue, "auth-v2.195.0-r1")
	h.arts.pin(config.SvcPostgREST, "postgrest-v16.4-r0")
	h.plane.steps = nil
	return h
}

func (h *upHarness) project(t *testing.T) *registry.Project {
	t.Helper()
	p, err := h.reg.GetProject(context.Background(), h.ref)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (h *upHarness) latest(t *testing.T) *registry.Upgrade {
	t.Helper()
	u, err := h.reg.LatestUpgrade(context.Background(), h.ref)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (h *upHarness) events(t *testing.T) string {
	t.Helper()
	evs, _ := h.reg.ListEvents(context.Background(), h.ref, 50)
	var kinds []string
	for i := len(evs) - 1; i >= 0; i-- {
		kinds = append(kinds, evs[i].Kind)
	}
	return strings.Join(kinds, ",")
}

const (
	oldAuth = "auth-v2.100.0-r1"
	newAuth = "auth-v2.195.0-r1"
	oldPG   = "postgres-17.1.0-r1"
)

// A project keeps the versions it was created with when the node's pins move, and says so.
func TestProjectKeepsItsVersionsWhenThePinsMove(t *testing.T) {
	h := newUpHarness(t)
	p := h.project(t)
	if p.Versions[config.SvcGoTrue] != oldAuth {
		t.Fatalf("versions = %v", p.Versions)
	}
	el, err := h.e.UpgradeEligibility(context.Background(), h.ref)
	if err != nil {
		t.Fatal(err)
	}
	if !el.Eligible || el.UpToDate || el.PostgresRestart || len(el.Changes) != 2 || el.Current[config.SvcGoTrue] != oldAuth || el.Latest[config.SvcGoTrue] != newAuth {
		t.Fatalf("eligibility = %+v", el)
	}
	if el.Changes[0].Service != config.SvcGoTrue || el.Changes[1].Service != config.SvcPostgREST {
		t.Fatalf("changes = %+v", el.Changes)
	}
	if el.DowntimeHours != downtimeAPIHours || len(el.Blockers) != 0 {
		t.Fatalf("downtime/blockers = %v %v", el.DowntimeHours, el.Blockers)
	}
}

func TestUpgradeAPIUnitsOnly(t *testing.T) {
	h := newUpHarness(t)
	ctx := context.Background()
	up, err := h.e.UpgradeProject(ctx, h.ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := h.project(t)
	if p.Status != registry.StatusActiveHealthy || p.Versions[config.SvcGoTrue] != newAuth || p.Versions[config.SvcPostgREST] != "postgrest-v16.4-r0" || p.Versions[config.SvcPostgres] != oldPG {
		t.Fatalf("project = %s %v", p.Status, p.Versions)
	}
	// The cluster is not stopped: only the API units restart, from the target versions.
	if got := h.plane.log(); got != "Reconfigure v2.195.0-r1" {
		t.Fatalf("plane steps = %s", got)
	}
	if want := []string{"gotrue " + newAuth, "postgrest postgrest-v16.4-r0"}; strings.Join(h.arts.fetched, "|") != strings.Join(want, "|") {
		t.Fatalf("fetched = %v, want %v (only what changes)", h.arts.fetched, want)
	}
	if len(h.backup.calls) != 1 {
		t.Fatalf("backups = %v", h.backup.calls)
	}
	st := h.latest(t)
	if st.TrackingID != up.TrackingID || st.Status != registry.UpgradeDone || st.Progress != ProgressCompleted || st.BackupID != 42 || st.Error != "" ||
		st.To[config.SvcGoTrue] != newAuth || st.From[config.SvcGoTrue] != oldAuth {
		t.Fatalf("status row = %+v", st)
	}
	if got := h.events(t); got != "project.created,project.upgrade_started,project.upgrade_backup_done,project.upgrade_succeeded" {
		t.Fatalf("events = %s", got)
	}
	// Nothing left to do now.
	if _, err := h.e.UpgradeProject(ctx, h.ref, nil); !errors.Is(err, ErrUpgradeNotNeeded) {
		t.Fatalf("second upgrade: %v", err)
	}
	if el, _ := h.e.UpgradeEligibility(ctx, h.ref); el.Eligible || !el.UpToDate {
		t.Fatalf("eligibility after the upgrade = %+v", el)
	}
}

func TestUpgradeRestartsPostgresWhenItsReleaseChanges(t *testing.T) {
	h := newUpHarness(t)
	h.arts.pin(config.SvcPostgres, "postgres-17.2.0-r1")
	el, _ := h.e.UpgradeEligibility(context.Background(), h.ref)
	if !el.Eligible || !el.PostgresRestart || el.DowntimeHours != downtimePostgresHours {
		t.Fatalf("eligibility = %+v", el)
	}
	if _, err := h.e.UpgradeProject(context.Background(), h.ref, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := h.plane.log(), "Stop, StartDatabase v2.195.0-r1, Start v2.195.0-r1, VerifyExtensions v2.195.0-r1"; got != want {
		t.Fatalf("plane steps = %s, want %s", got, want)
	}
	if p := h.project(t); p.Versions[config.SvcPostgres] != "postgres-17.2.0-r1" || p.Status != registry.StatusActiveHealthy {
		t.Fatalf("project = %s %v", p.Status, p.Versions)
	}
}

// While the upgrade runs the project is UPGRADING, and nothing else may start on it.
func TestUpgradeStatusAndGuardsWhileRunning(t *testing.T) {
	h := newUpHarness(t)
	ctx := context.Background()
	run, err := h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{TargetVersion: "17"})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.project(t).Status; got != registry.StatusUpgrading {
		t.Fatalf("status = %s", got)
	}
	if st := h.latest(t); st.Status != registry.UpgradeRunning || st.Progress != ProgressRequested || st.TargetVersion != "17" || st.TrackingID != run.Upgrade().TrackingID {
		t.Fatalf("status row = %+v", st)
	}
	for name, err := range map[string]error{
		"pause":   h.e.Pause(ctx, h.ref),
		"resume":  h.e.Resume(ctx, h.ref),
		"delete":  h.e.Delete(ctx, h.ref),
		"upgrade": func() error { _, err := h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{}); return err }(),
	} {
		if !errors.Is(err, ErrInvalidState) {
			t.Errorf("%s while upgrading: %v, want ErrInvalidState", name, err)
		}
	}
	if _, err := h.e.RotateKeys(ctx, h.ref); !errors.Is(err, ErrInvalidState) {
		t.Errorf("rotate keys while upgrading: %v", err)
	}
	if el, _ := h.e.UpgradeEligibility(ctx, h.ref); el.Eligible || len(el.Blockers) == 0 || el.Blockers[0].Type != BlockerStatus {
		t.Errorf("eligibility while upgrading = %+v", el)
	}
	if err := run.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.project(t).Status; got != registry.StatusActiveHealthy {
		t.Fatalf("status after = %s", got)
	}
}

// Fail closed: a failed backup, or artifacts that cannot be had, stop the upgrade before any
// service is touched, and the project goes back to ACTIVE_HEALTHY on its own versions.
func TestUpgradeFailsClosedBeforeTouchingAnything(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(h *upHarness)
		code string
	}{
		{"backup", func(h *upHarness) { h.backup.err = errors.New("the backend refused the upload") }, UpgradeErrBackup},
		{"artifacts", func(h *upHarness) { h.arts.fetchErr = errors.New("404 from the release server") }, UpgradeErrArtifacts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUpHarness(t)
			tc.set(h)
			_, err := h.e.UpgradeProject(context.Background(), h.ref, nil)
			if err == nil || !strings.Contains(err.Error(), "before any service was touched") {
				t.Fatalf("err = %v", err)
			}
			if got := h.plane.log(); got != "" {
				t.Fatalf("services were touched: %s", got)
			}
			p := h.project(t)
			if p.Status != registry.StatusActiveHealthy || p.Versions[config.SvcGoTrue] != oldAuth {
				t.Fatalf("project = %s %v", p.Status, p.Versions)
			}
			st := h.latest(t)
			if st.Status != registry.UpgradeFailed || st.Error != tc.code || !strings.Contains(st.Detail, "nothing was changed") {
				t.Fatalf("status row = %+v", st)
			}
			if !strings.HasSuffix(h.events(t), "project.upgrade_failed") {
				t.Fatalf("events = %s", h.events(t))
			}
			// The project can be upgraded once the cause is gone.
			h.backup.err, h.arts.fetchErr = nil, nil
			if _, err := h.e.UpgradeProject(context.Background(), h.ref, nil); err != nil {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}

func TestUpgradeRollsBackWhenTheNewReleaseDoesNotStart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		healthBad bool
		code      string
	}{
		{"start", false, UpgradeErrStart},
		{"health", true, UpgradeErrHealth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUpHarness(t)
			h.plane.bad, h.plane.healthBad = newAuth, tc.healthBad
			_, err := h.e.UpgradeProject(context.Background(), h.ref, nil)
			if err == nil || !strings.Contains(err.Error(), "previous versions are running again") || !strings.Contains(err.Error(), "42") {
				t.Fatalf("err = %v", err)
			}
			p := h.project(t)
			if p.Status != registry.StatusActiveHealthy || p.Versions[config.SvcGoTrue] != oldAuth || p.Versions[config.SvcPostgREST] != "postgrest-v12.0-r0" {
				t.Fatalf("project = %s %v", p.Status, p.Versions)
			}
			// The target was tried, then the previous versions were started again.
			if got, want := h.plane.log(), "Reconfigure v2.195.0-r1, Reconfigure v2.100.0-r1"; got != want {
				t.Fatalf("plane steps = %s, want %s", got, want)
			}
			st := h.latest(t)
			if st.Status != registry.UpgradeFailed || st.Error != tc.code || st.BackupID != 42 || !strings.Contains(st.Detail, "rolled back") {
				t.Fatalf("status row = %+v", st)
			}
			if !strings.HasSuffix(h.events(t), "project.upgrade_backup_done,project.upgrade_failed") {
				t.Fatalf("events = %s", h.events(t))
			}
		})
	}
}

func TestUpgradeRollbackAfterAPostgresChange(t *testing.T) {
	h := newUpHarness(t)
	h.arts.pin(config.SvcPostgres, "postgres-17.2.0-r1")
	h.plane.bad = "postgres-17.2.0-r1"
	if _, err := h.e.UpgradeProject(context.Background(), h.ref, nil); err == nil {
		t.Fatal("upgrade onto a release that does not start succeeded")
	}
	if got, want := h.plane.log(), "Stop, StartDatabase v2.195.0-r1, Stop, StartDatabase v2.100.0-r1, Start v2.100.0-r1"; got != want {
		t.Fatalf("plane steps = %s, want %s", got, want)
	}
	if p := h.project(t); p.Versions[config.SvcPostgres] != oldPG || p.Status != registry.StatusActiveHealthy {
		t.Fatalf("project = %s %v", p.Status, p.Versions)
	}
}

func TestUpgradeRollbackFailureLeavesTheProjectUnhealthy(t *testing.T) {
	h := newUpHarness(t)
	h.plane.bad, h.plane.failRollback = newAuth, true
	h.plane.healthBad = false
	_, err := h.e.UpgradeProject(context.Background(), h.ref, nil)
	if err == nil || !strings.Contains(err.Error(), "rollback to the previous versions failed too") || !strings.Contains(err.Error(), "backup 42") {
		t.Fatalf("err = %v", err)
	}
	// The registry still names the previous versions; StartActive starts them on the next boot.
	p := h.project(t)
	if p.Status != registry.StatusActiveUnhealthy || p.Versions[config.SvcGoTrue] != oldAuth {
		t.Fatalf("project = %s %v", p.Status, p.Versions)
	}
	if st := h.latest(t); st.Status != registry.UpgradeFailed || !strings.Contains(st.Detail, "the rollback failed too") {
		t.Fatalf("status row = %+v", st)
	}
}

// A project that recorded no versions runs the node's pins; if an upgrade to another release
// fails, the project must be left recording what it ran, or it would float with the pins.
func TestRollbackRecordsTheVersionsOfAProjectThatHadNone(t *testing.T) {
	h := newUpHarness(t)
	ctx := context.Background()
	p := h.project(t)
	p.Versions = nil
	if err := h.reg.UpdateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	h.plane.bad = "auth-v2.300.0-r0"
	if _, err := h.e.UpgradeProject(ctx, h.ref, map[string]string{config.SvcGoTrue: h.plane.bad}); err == nil {
		t.Fatal("expected the upgrade to fail")
	}
	got := h.project(t)
	if got.Versions[config.SvcGoTrue] != newAuth || got.Versions[config.SvcPostgres] != oldPG || got.Status != registry.StatusActiveHealthy {
		t.Fatalf("project = %s %v, want the pins it ran, recorded", got.Status, got.Versions)
	}
	if got := h.plane.log(); got != "Reconfigure v2.300.0-r0, Reconfigure v2.195.0-r1" {
		t.Fatalf("plane steps = %s", got)
	}
}

func TestUpgradeRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("another major version", func(t *testing.T) {
		h := newUpHarness(t)
		h.arts.pin(config.SvcPostgres, "postgres-18.0.0-r1")
		el, _ := h.e.UpgradeEligibility(ctx, h.ref)
		if el.Eligible || el.CurrentMajor != 17 || el.TargetMajor != 18 || len(el.Blockers) == 0 || el.Blockers[0].Type != BlockerNoUpgradePath {
			t.Fatalf("eligibility = %+v", el)
		}
		if _, err := h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{}); !errors.Is(err, ErrUpgradeUnsupported) || !strings.Contains(err.Error(), "major") {
			t.Fatalf("err = %v", err)
		}
		if got := h.project(t).Status; got != registry.StatusActiveHealthy {
			t.Fatalf("a refused upgrade changed the status to %s", got)
		}
		if _, err := h.reg.LatestUpgrade(ctx, h.ref); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("a refused upgrade left a status row: %v", err)
		}
	})
	t.Run("paused", func(t *testing.T) {
		h := newUpHarness(t)
		if err := h.e.Pause(ctx, h.ref); err != nil {
			t.Fatal(err)
		}
		el, _ := h.e.UpgradeEligibility(ctx, h.ref)
		if el.Eligible || el.Blockers[0].Type != BlockerHibernating {
			t.Fatalf("eligibility = %+v", el)
		}
		if _, err := h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{}); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unhealthy", func(t *testing.T) {
		h := newUpHarness(t)
		_ = h.reg.SetProjectStatus(ctx, h.ref, registry.StatusActiveUnhealthy)
		if _, err := h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{}); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no backup service", func(t *testing.T) {
		h := newUpHarness(t)
		h.e.opts.Backup = nil
		if _, err := h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{}); !errors.Is(err, ErrNoBackupEngine) {
			t.Fatalf("err = %v", err)
		}
		if el, _ := h.e.UpgradeEligibility(ctx, h.ref); el.Eligible || el.Blockers[0].Type != BlockerNoBackup {
			t.Fatalf("eligibility = %+v", el)
		}
	})
	t.Run("system project and unknown service", func(t *testing.T) {
		h := newUpHarness(t)
		if _, err := h.e.BeginUpgrade(ctx, config.SystemRef, UpgradeRequest{}); err == nil {
			t.Fatal("the system project was upgraded")
		}
		if _, err := h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{Target: map[string]string{"realtime": "realtime-v1"}}); !errors.Is(err, ErrUpgradeUnsupported) {
			t.Fatalf("err = %v", err)
		}
		if _, err := h.e.BeginUpgrade(ctx, "nosuchprojectxxxxxxx", UpgradeRequest{}); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
}

// An explicit target moves a project to a release the node does not pin.
func TestUpgradeToAnExplicitTarget(t *testing.T) {
	h := newUpHarness(t)
	target := map[string]string{config.SvcGoTrue: "auth-v2.150.0-r0"}
	if _, err := h.e.UpgradeProject(context.Background(), h.ref, target); err != nil {
		t.Fatal(err)
	}
	p := h.project(t)
	if p.Versions[config.SvcGoTrue] != "auth-v2.150.0-r0" || p.Versions[config.SvcPostgREST] != "postgrest-v12.0-r0" {
		t.Fatalf("versions = %v: only the named service changes", p.Versions)
	}
	if got := strings.Join(h.arts.fetched, "|"); got != "gotrue auth-v2.150.0-r0" {
		t.Fatalf("fetched = %s", got)
	}
}

// daemonOf is a second Engine over the harness's registry and plane: the daemon, next to the
// CLI process whose Engine (h.e) runs an upgrade.
func (h *upHarness) daemonOf() *Engine {
	return NewEngine(h.cfg, h.reg, h.e.sec, h.arts, h.plane, Options{Fleet: fleet.Fleet{&fakeTenant{}}, Backup: h.backup, Timers: h.e.opts.Timers})
}

// setProgress moves the project's running upgrade row to progress, as the runner's saves do.
func (h *upHarness) setProgress(t *testing.T, progress string) {
	t.Helper()
	u := h.latest(t)
	u.Progress = progress
	if err := h.reg.PutUpgrade(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

// A process that stopped in the middle of an upgrade: Recover stops the units, ends the
// upgrade, and lets StartActive start the project on the recorded versions.
func TestRecoverAnInterruptedUpgrade(t *testing.T) {
	h := newUpHarness(t)
	ctx := context.Background()
	run, err := h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	daemon := h.daemonOf()
	// The runner is alive (another process holds the project's upgrade): the daemon's Recover
	// must not stop units under its backup.
	if rec := daemon.Recover(ctx); len(rec) != 0 || h.project(t).Status != registry.StatusUpgrading || h.plane.has("Stop "+h.ref) {
		t.Fatalf("Recover touched an upgrade that is running: %+v, calls %v", rec, h.plane.calls)
	}
	if rec := daemon.SettleUpgrades(ctx); len(rec) != 0 || h.project(t).Status != registry.StatusUpgrading {
		t.Fatalf("SettleUpgrades touched an upgrade that is running: %+v", rec)
	}
	h.setProgress(t, ProgressStopping) // it had reached the units
	run.(*upgradeRun).release()        // the process that ran it is gone
	h.e.upgrading.Delete(h.ref)
	rec := daemon.Recover(ctx)
	if len(rec) != 1 || rec[0].From != registry.StatusUpgrading || rec[0].To != registry.StatusActiveUnhealthy {
		t.Fatalf("recovered = %+v", rec)
	}
	if !h.plane.has("Stop " + h.ref) {
		t.Fatalf("units were not stopped: %v", h.plane.calls)
	}
	st := h.latest(t)
	if st.TrackingID != run.Upgrade().TrackingID || st.Status != registry.UpgradeFailed || !strings.Contains(st.Detail, "process that ran the upgrade stopped") {
		t.Fatalf("status row = %+v", st)
	}
	if errs := daemon.StartActive(ctx); len(errs) != 0 {
		t.Fatalf("StartActive: %v", errs)
	}
	p := h.project(t)
	if p.Status != registry.StatusActiveHealthy || p.Versions[config.SvcGoTrue] != oldAuth {
		t.Fatalf("project = %s %v", p.Status, p.Versions)
	}
}

// An upgrade that dies before it touches a unit (the artifact fetch, the base backup) leaves a
// serving project: Recover and SettleUpgrades close the upgrade and do not stop or start anything.
func TestInterruptedUpgradeBeforeAnyUnitIsTouchedKeepsTheProjectServing(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ progress, code string }{
		{ProgressRequested, UpgradeErrArtifacts},
		{ProgressStarted, UpgradeErrArtifacts},
		{ProgressArtifactsReady, UpgradeErrBackup}, // the base backup runs here
		{ProgressBackupDone, UpgradeErrStart},
	} {
		for _, via := range []string{"Recover", "SettleUpgrades"} {
			t.Run(via+" "+tc.progress, func(t *testing.T) {
				h := newUpHarness(t)
				run, err := h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{})
				if err != nil {
					t.Fatal(err)
				}
				h.setProgress(t, tc.progress)
				run.(*upgradeRun).release()
				h.e.upgrading.Delete(h.ref)
				h.plane.steps = nil
				daemon := h.daemonOf()
				var rec []Recovered
				if via == "Recover" {
					rec = daemon.Recover(ctx)
				} else {
					rec = daemon.SettleUpgrades(ctx)
				}
				if len(rec) != 1 || rec[0].To != registry.StatusActiveHealthy {
					t.Fatalf("recovered = %+v", rec)
				}
				if got := h.plane.log(); got != "" {
					t.Fatalf("a project that was serving was touched: %s", got)
				}
				if p := h.project(t); p.Status != registry.StatusActiveHealthy || p.Versions[config.SvcGoTrue] != oldAuth {
					t.Fatalf("project = %s %v", p.Status, p.Versions)
				}
				st := h.latest(t)
				if st.Status != registry.UpgradeFailed || st.Error != tc.code || !strings.Contains(st.Detail, "before any service was touched") {
					t.Fatalf("status row = %+v", st)
				}
				if !strings.HasSuffix(h.events(t), "project.upgrade_failed,project.recovered") {
					t.Fatalf("events = %s", h.events(t))
				}
				if _, err := daemon.UpgradeProject(ctx, h.ref, nil); err != nil {
					t.Fatalf("upgrade after the settle: %v", err)
				}
			})
		}
	}
}

// A CLI upgrade that dies while the daemon runs: the periodic settle starts the project on its
// previous versions, and a second upgrade of it is accepted afterwards.
func TestSettleUpgradesStartsAProjectWhoseRunnerDied(t *testing.T) {
	h := newUpHarness(t)
	ctx := context.Background()
	run, err := h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Another process cannot start a second upgrade of the same project while this one lives.
	if _, err := h.daemonOf().BeginUpgrade(ctx, h.ref, UpgradeRequest{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("a second runner: %v", err)
	}
	h.setProgress(t, ProgressServices)
	run.(*upgradeRun).release()
	h.e.upgrading.Delete(h.ref)
	h.plane.steps = nil
	daemon := h.daemonOf()
	rec := daemon.SettleUpgrades(ctx)
	if len(rec) != 1 || rec[0].Ref != h.ref || rec[0].To != registry.StatusActiveUnhealthy {
		t.Fatalf("settled = %+v", rec)
	}
	if !strings.HasPrefix(h.plane.log(), "Stop, Start ") {
		t.Fatalf("a project whose upgrade had touched its units was not stopped and started: %s", h.plane.log())
	}
	if p := h.project(t); p.Status != registry.StatusActiveHealthy || p.Versions[config.SvcGoTrue] != oldAuth {
		t.Fatalf("project = %s %v", p.Status, p.Versions)
	}
	if st := h.latest(t); st.Status != registry.UpgradeFailed {
		t.Fatalf("status row = %+v", st)
	}
	if _, err := daemon.UpgradeProject(ctx, h.ref, nil); err != nil {
		t.Fatalf("upgrade after the settle: %v", err)
	}
}

// A process that died after recording the new versions but before the final progress: the
// record is closed as done, or as failed when the project never moved.
func TestSettleUpgradeRowsClosesWhatNoRunnerFinished(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		moved    bool
		want     registry.UpgradeStatus
		contains string
	}{
		{"versions recorded", true, registry.UpgradeDone, ""},
		{"versions not recorded", false, registry.UpgradeFailed, "previous versions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUpHarness(t)
			run, err := h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{})
			if err != nil {
				t.Fatal(err)
			}
			p := h.project(t)
			p.Status = registry.StatusActiveHealthy
			if tc.moved {
				p.Versions = mergeVersions(p.Versions, run.Upgrade().To)
			}
			if err := h.reg.UpdateProject(ctx, p); err != nil {
				t.Fatal(err)
			}
			run.(*upgradeRun).release()
			h.e.upgrading.Delete(h.ref)
			h.daemonOf().SettleUpgrades(ctx)
			st := h.latest(t)
			if st.Status != tc.want || !strings.Contains(st.Detail, tc.contains) {
				t.Fatalf("status row = %+v", st)
			}
			if tc.moved && st.Progress != ProgressCompleted {
				t.Fatalf("progress = %s", st.Progress)
			}
		})
	}
}

// Rendering: a project runs the artifact of its own recorded version, the node's pin otherwise,
// and the system project always the pin.
func TestPlaneRendersFromTheProjectsVersions(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.Domain = "example.test"
	cfg.BinPath = "/usr/local/bin/supavise"
	arts := newTagArts()
	pl := NewPostgresPlane(cfg, nil, arts, registry.NewMemory(), PlaneOptions{})
	ctx := context.Background()
	p := testProject(cfg, "abcdefghijklmnopqrst", 2)
	keys := testKeys(t, p.Ref)

	// No recorded versions: the pins.
	specs, err := pl.apiSpecs(ctx, p, keys)
	if err != nil || specs[0].ArtifactDir != "/art/gotrue/auth-v2.100.0-r1" || specs[1].ArtifactDir != "/art/postgrest/postgrest-v12.0-r0" {
		t.Fatalf("specs = %+v, %v", specs, err)
	}
	// The node moves on; the project keeps what it recorded.
	p.Versions = map[string]string{config.SvcPostgres: oldPG, config.SvcGoTrue: oldAuth, config.SvcPostgREST: "postgrest-v12.0-r0"}
	arts.pin(config.SvcGoTrue, newAuth)
	arts.pin(config.SvcPostgres, "postgres-17.2.0-r1")
	specs, err = pl.apiSpecs(ctx, p, keys)
	if err != nil || specs[0].ArtifactDir != "/art/gotrue/"+oldAuth {
		t.Fatalf("specs = %+v, %v", specs, err)
	}
	pg, err := pl.postgresSpec(ctx, p, keys)
	if err != nil || pg.ArtifactDir != "/art/postgres/"+oldPG {
		t.Fatalf("postgres spec dir = %s, %v", pg.ArtifactDir, err)
	}
	// Moved to the pins, it follows them.
	p.Versions[config.SvcGoTrue] = newAuth
	if specs, _ = pl.apiSpecs(ctx, p, keys); specs[0].ArtifactDir != "/art/gotrue/"+newAuth {
		t.Fatalf("specs = %+v", specs)
	}
	// The system project belongs to the node: it runs the pins whatever it recorded.
	sys := testProject(cfg, config.SystemRef, 0)
	sys.Versions = map[string]string{config.SvcGoTrue: oldAuth}
	if specs, err = pl.apiSpecs(ctx, sys, keys); err != nil || specs[0].ArtifactDir != "/art/gotrue/"+newAuth {
		t.Fatalf("system specs = %+v, %v", specs, err)
	}
}

// Without a store that finds releases by tag, a project on another version cannot be rendered,
// and the error says so; a release that is not on disk says how to move on.
func TestPlaneRefusesVersionsItCannotFind(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.Domain = "example.test"
	pl := NewPostgresPlane(cfg, nil, fakeArts{}, registry.NewMemory(), PlaneOptions{})
	p := testProject(cfg, "abcdefghijklmnopqrst", 2)
	p.Versions = map[string]string{config.SvcGoTrue: "auth-other"}
	if _, err := pl.apiSpecs(context.Background(), p, testKeys(t, p.Ref)); err == nil || !strings.Contains(err.Error(), "cannot find a release by tag") {
		t.Fatalf("err = %v", err)
	}

	st, err := artifacts.New(cfg, artifacts.WithVersions(&artifacts.Versions{Artifacts: map[string]string{"postgres": "postgres-17.1-r1", "auth": newAuth, "postgrest": "postgrest-v1-r0"}}))
	if err != nil {
		t.Fatal(err)
	}
	pl = NewPostgresPlane(cfg, nil, st, registry.NewMemory(), PlaneOptions{})
	p.Versions = map[string]string{config.SvcGoTrue: oldAuth}
	_, err = pl.apiSpecs(context.Background(), p, testKeys(t, p.Ref))
	if !errors.Is(err, artifacts.ErrNotFetched) || !strings.Contains(err.Error(), "supavise projects upgrade "+p.Ref) {
		t.Fatalf("err = %v", err)
	}
}

// Artifacts that a project runs, or is being upgraded to, are not garbage.
func TestCollectArtifactsKeepsWhatProjectsRun(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.Domain = "example.test"
	v := &artifacts.Versions{Artifacts: map[string]string{"postgres": oldPG, "auth": newAuth, "postgrest": "postgrest-v16.4-r0"}}
	st, err := artifacts.New(cfg, artifacts.WithVersions(v))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"postgres/" + oldPG, "auth/" + newAuth, "auth/" + oldAuth, "auth/auth-v2.50.0-r0", "postgrest/postgrest-v16.4-r0"} {
		if err := os.MkdirAll(filepath.Join(cfg.Paths().Artifacts(), d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	reg := registry.NewMemory()
	sec, _ := secrets.New(make([]byte, 32))
	e := NewEngine(cfg, reg, sec, st, newFakePlane(), Options{})
	ctx := context.Background()
	p := &registry.Project{Ref: "abcdefghijklmnopqrst", Name: "x", Status: registry.StatusActiveHealthy,
		Versions: map[string]string{config.SvcPostgres: oldPG, config.SvcGoTrue: oldAuth, config.SvcPostgREST: "postgrest-v16.4-r0"}}
	if err := reg.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	gone, err := e.CollectArtifacts(ctx, 1, false)
	if err != nil || len(gone) != 1 || gone[0].Tag != "auth-v2.50.0-r0" {
		t.Fatalf("removed = %+v, %v (only the version nobody runs or pins)", gone, err)
	}
	// Once the project moves on and nothing is mid-upgrade, its old version goes.
	p.Versions[config.SvcGoTrue] = newAuth
	_ = reg.UpdateProject(ctx, p)
	gone, err = e.CollectArtifacts(ctx, 1, true)
	if err != nil || len(gone) != 1 || gone[0].Tag != oldAuth {
		t.Fatalf("dry run = %+v, %v", gone, err)
	}
	// An upgrade that is running names its target.
	_ = reg.PutUpgrade(ctx, &registry.Upgrade{TrackingID: "11111111-1111-4111-8111-111111111111", Ref: p.Ref,
		To: map[string]string{config.SvcGoTrue: oldAuth}, Status: registry.UpgradeRunning})
	if gone, err = e.CollectArtifacts(ctx, 1, true); err != nil || len(gone) != 0 {
		t.Fatalf("dry run during an upgrade = %+v, %v", gone, err)
	}
}

// The node's pins can be older than what a project runs (the binary was rolled back, or the
// versions file points at an older release). Such a project is not offered an upgrade, which
// would be a downgrade, and BeginUpgrade refuses it before anything changes.
func TestUpgradeNeverDowngrades(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		pins  map[string]string
		want  string // a fragment of the blocker; Ahead lists this many services
		ahead int
	}{
		{"gotrue only", map[string]string{config.SvcGoTrue: "auth-v2.50.0-r1", config.SvcPostgREST: "postgrest-v12.0-r0"}, "newer than", 1},
		{"older packaging revision", map[string]string{config.SvcGoTrue: "auth-v2.100.0-r0", config.SvcPostgREST: "postgrest-v12.0-r0"}, "newer than", 1},
		{"postgres minor", map[string]string{config.SvcPostgres: "postgres-17.0.5-r1"}, "newer than", 1},
		{"one older and one newer service", map[string]string{config.SvcGoTrue: "auth-v2.50.0-r1"}, "newer than", 1},
		{"tags that cannot be ordered", map[string]string{config.SvcGoTrue: "auth-nightly"}, "cannot tell", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUpHarness(t)
			for svc, tag := range tc.pins {
				h.arts.pin(svc, tag)
			}
			el, err := h.e.UpgradeEligibility(ctx, h.ref)
			if err != nil {
				t.Fatal(err)
			}
			if el.Eligible || len(el.Ahead) != tc.ahead || len(el.Blockers) == 0 || el.Blockers[0].Type != BlockerNoUpgradePath || !strings.Contains(el.Blockers[0].Message, tc.want) {
				t.Fatalf("eligibility = %+v", el)
			}
			if _, err := h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{}); !errors.Is(err, ErrUpgradeUnsupported) {
				t.Fatalf("err = %v", err)
			}
			if _, err := h.e.UpgradeProject(ctx, h.ref, nil); !errors.Is(err, ErrUpgradeUnsupported) {
				t.Fatalf("UpgradeProject err = %v", err)
			}
			if _, err := h.reg.LatestUpgrade(ctx, h.ref); !errors.Is(err, registry.ErrNotFound) {
				t.Fatalf("a refused upgrade left a status row: %v", err)
			}
			p := h.project(t)
			if p.Status != registry.StatusActiveHealthy || p.Versions[config.SvcGoTrue] != oldAuth || p.Versions[config.SvcPostgres] != oldPG {
				t.Fatalf("project = %s %v", p.Status, p.Versions)
			}
			if got := h.plane.log(); got != "" || len(h.backup.calls) != 0 || len(h.arts.fetched) != 0 {
				t.Fatalf("a refused upgrade did work: steps %q, backups %v, fetched %v", got, h.backup.calls, h.arts.fetched)
			}
		})
	}
	t.Run("an explicit older target", func(t *testing.T) {
		h := newUpHarness(t)
		if _, err := h.e.UpgradeProject(ctx, h.ref, map[string]string{config.SvcGoTrue: "auth-v2.50.0-r1"}); !errors.Is(err, ErrUpgradeUnsupported) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCompareTags(t *testing.T) {
	for _, tc := range []struct {
		svc, a, b string
		want      int
		bad       bool
	}{
		{config.SvcGoTrue, "auth-v2.100.0-r1", "auth-v2.195.0-r1", -1, false},
		{config.SvcGoTrue, "auth-v2.195.0-r1", "auth-v2.100.0-r1", 1, false},
		{config.SvcGoTrue, "auth-v2.9.0-r1", "auth-v2.10.0-r1", -1, false}, // numbers, not text
		{config.SvcGoTrue, "auth-v2.100.0-r1", "auth-v2.100.0-r2", -1, false},
		{config.SvcGoTrue, "auth-v2.100.0-r1", "auth-v2.100.0-r1", 0, false},
		{config.SvcPostgREST, "postgrest-v12.0-r5", "postgrest-v12.0.0-r5", 0, false},
		{config.SvcPostgREST, "postgrest-v12.1-r5", "postgrest-v12.1.0-r0", 1, false}, // revision, not a longer version
		{config.SvcPostgres, "postgres-17.1.0.001-r1", "postgres-17.11.0.004-r1", -1, false},
		{config.SvcPostgres, "postgres-17.11.0.004-r1", "postgres-18.0.0.001-r1", -1, false},
		{config.SvcGoTrue, "auth-v2.100.0-rc1", "auth-v2.100.0-r1", 0, true},
		{config.SvcGoTrue, "", "auth-v2.100.0-r1", 0, true},
	} {
		got, err := CompareTags(tc.svc, tc.a, tc.b)
		if (err != nil) != tc.bad || (err == nil && got != tc.want) {
			t.Errorf("CompareTags(%q, %q) = %d, %v; want %d, error %v", tc.a, tc.b, got, err, tc.want, tc.bad)
		}
	}
}

func TestVersionHelpers(t *testing.T) {
	if got := PostgresMajor("postgres-17.11.0.004-r1"); got != 17 {
		t.Fatalf("major = %d", got)
	}
	if PostgresMajor("") != 0 || PostgresMajor("postgres-x") != 0 {
		t.Fatal("a tag without a number has no major")
	}
	if AppVersion("postgres-17.11.0.004-r1") != "supabase-postgres-17.11.0.004-r1" || AppVersion("") != "" {
		t.Fatal("AppVersion")
	}
	if ShortVersion(config.SvcGoTrue, "auth-v2.195.0-r1") != "v2.195.0-r1" || ShortVersion(config.SvcPostgres, "postgres-17.11.0.004-r1") != "17.11.0.004-r1" || ShortVersion(config.SvcPostgREST, "odd") != "odd" {
		t.Fatal("ShortVersion")
	}
	d := DiffVersions(map[string]string{"postgres": "a", "gotrue": "b", "postgrest": "c"}, map[string]string{"gotrue": "b2", "postgrest": "c", "postgres": ""})
	if len(d) != 1 || d[0] != (ServiceChange{Service: "gotrue", From: "b", To: "b2"}) {
		t.Fatalf("diff = %+v", d)
	}
}
