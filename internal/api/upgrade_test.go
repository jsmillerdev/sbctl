package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/members"
	"github.com/jsmillerdev/supavise/internal/notice"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// apiArts is an artifact store whose pins a test moves, the way a node update does.
type apiArts struct {
	mu   sync.Mutex
	pins map[string]string
}

func (a *apiArts) pin(svc, tag string) { a.mu.Lock(); a.pins[svc] = tag; a.mu.Unlock() }
func (a *apiArts) Tag(svc string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pins[svc], nil
}
func (a *apiArts) Dir(svc string) (string, error) {
	t, _ := a.Tag(svc)
	return "/art/" + svc + "/" + t, nil
}
func (a *apiArts) DirFor(svc, tag string) (string, error) { return "/art/" + svc + "/" + tag, nil }

// apiPlane is a data plane that starts anything at once.
type apiPlane struct {
	mu    sync.Mutex
	calls []string
	gate  chan struct{} // when set, Start and Reconfigure wait for it
}

func (p *apiPlane) note(c string) { p.mu.Lock(); p.calls = append(p.calls, c); p.mu.Unlock() }
func (p *apiPlane) Create(context.Context, *registry.Project, *secrets.ProjectKeys, lifecycle.DataSeeder) error {
	return nil
}
func (p *apiPlane) Delete(context.Context, string) error { return nil }
func (p *apiPlane) Snapshot(context.Context, string) (*registry.Backup, error) {
	return nil, lifecycle.ErrNoSnapshot
}
func (p *apiPlane) Route(context.Context, string) (lifecycle.Upstreams, error) {
	return lifecycle.Upstreams{}, nil
}
func (p *apiPlane) Usage(context.Context, string) (lifecycle.Usage, error) {
	return lifecycle.Usage{}, nil
}
func (p *apiPlane) Start(_ context.Context, pr *registry.Project, _ *secrets.ProjectKeys) error {
	if p.gate != nil {
		<-p.gate
	}
	p.note("Start " + pr.Versions[config.SvcGoTrue])
	return nil
}
func (p *apiPlane) StartDatabase(_ context.Context, pr *registry.Project, _ *secrets.ProjectKeys) error {
	p.note("StartDatabase")
	return nil
}
func (p *apiPlane) Stop(context.Context, string) error { p.note("Stop"); return nil }
func (p *apiPlane) Reconfigure(_ context.Context, pr *registry.Project, _ *secrets.ProjectKeys) error {
	if p.gate != nil {
		<-p.gate
	}
	p.note("Reconfigure " + pr.Versions[config.SvcGoTrue])
	return nil
}
func (p *apiPlane) Health(context.Context, *registry.Project, *secrets.ProjectKeys) []lifecycle.ServiceHealth {
	return []lifecycle.ServiceHealth{{Name: config.SvcPostgres, Healthy: true, Status: "ACTIVE_HEALTHY"}}
}

type apiBackup struct{ err error }

func (b apiBackup) BaseBackup(_ context.Context, ref string) (*registry.Backup, error) {
	return &registry.Backup{ID: 7, Ref: ref}, b.err
}

// upgradeManager is the fixture's manager with the real lifecycle Engine behind the upgrade
// capability, over the same registry, so the routes run the real eligibility and upgrade code.
type upgradeManager struct {
	*fakeManager
	eng *lifecycle.Engine
}

func (u upgradeManager) NodeVersions() (map[string]string, error) { return u.eng.NodeVersions() }
func (u upgradeManager) EffectiveVersions(p *registry.Project) (map[string]string, error) {
	return u.eng.EffectiveVersions(p)
}
func (u upgradeManager) UpgradeEligibility(ctx context.Context, ref string) (*lifecycle.UpgradeEligibility, error) {
	return u.eng.UpgradeEligibility(ctx, ref)
}
func (u upgradeManager) BeginUpgrade(ctx context.Context, ref string, req lifecycle.UpgradeRequest) (lifecycle.UpgradeRun, error) {
	return u.eng.BeginUpgrade(ctx, ref, req)
}

// Pause is the Engine's, which refuses a project that is upgrading.
func (u upgradeManager) Pause(ctx context.Context, ref string) error { return u.eng.Pause(ctx, ref) }

const (
	oldAuth = "auth-v2.100.0-r1"
	newAuth = "auth-v2.195.0-r1"
	oldPG   = "postgres-17.1.0.001-r1"
	newPG   = "postgres-17.11.0.004-r1"
)

type upgradeFixture struct {
	*fixture
	arts  *apiArts
	plane *apiPlane
	eng   *lifecycle.Engine
}

// newUpgradeFixture is a node whose pins moved after testRef started: the project runs the old
// GoTrue, PostgREST and Postgres, and the node pins newer ones.
func newUpgradeFixture(t *testing.T) *upgradeFixture {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	arts := &apiArts{pins: map[string]string{config.SvcPostgres: newPG, config.SvcGoTrue: newAuth, config.SvcPostgREST: "postgrest-v16.4-r0"}}
	plane := &apiPlane{}
	eng := lifecycle.NewEngine(f.cfg, f.reg, sec, arts, plane, lifecycle.Options{Backup: apiBackup{}})
	p, err := f.reg.GetProject(ctx, testRef)
	if err != nil {
		t.Fatal(err)
	}
	p.Versions = map[string]string{config.SvcPostgres: oldPG, config.SvcGoTrue: oldAuth, config.SvcPostgREST: "postgrest-v12.0-r0"}
	if err := f.reg.UpdateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	// The Engine reads the project's credentials from the registry.
	for name, val := range f.mgr.keys[testRef].Map() {
		sealed, err := sec.Seal([]byte(val))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.reg.PutSecret(ctx, testRef, name, sealed); err != nil {
			t.Fatal(err)
		}
	}
	f.srv.mgr = upgradeManager{f.mgr, eng}
	return &upgradeFixture{fixture: f, arts: arts, plane: plane, eng: eng}
}

func (f *upgradeFixture) waitStatus(t *testing.T, want registry.Status) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if p, _ := f.reg.GetProject(context.Background(), testRef); p != nil && p.Status == want {
			return
		}
	}
	p, _ := f.reg.GetProject(context.Background(), testRef)
	t.Fatalf("project status = %s, want %s", p.Status, want)
}

func upJSON(t *testing.T, f *fixture, method, path string, in any, want int, spec string) map[string]any {
	t.Helper()
	rec := f.do(method, path, in)
	if rec.Code != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, want, rec.Body.String())
	}
	if spec != "" {
		validateAgainstSpec(t, spec, rec.Body.Bytes())
	}
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return m
}

func TestUpgradeEligibilityAndServiceVersions(t *testing.T) {
	f := newUpgradeFixture(t)
	const p = "/v1/projects/" + testRef

	m := upJSON(t, f.fixture, "GET", p+"/upgrade/eligibility", nil, 200, "GET /v1/projects/{ref}/upgrade/eligibility")
	if m["eligible"] != true || m["current_app_version"] != "supabase-"+oldPG || m["latest_app_version"] != "supabase-"+newPG || m["current_app_version_release_channel"] != "ga" {
		t.Fatalf("eligibility = %v", m)
	}
	ts, _ := m["target_upgrade_versions"].([]any)
	if len(ts) != 1 {
		t.Fatalf("targets = %v", m["target_upgrade_versions"])
	}
	tv := ts[0].(map[string]any)
	if tv["postgres_version"] != "17" || tv["release_channel"] != "ga" || tv["app_version"] != "supabase-"+newPG {
		t.Fatalf("target = %v (Studio posts postgres_version as target_version)", tv)
	}
	// Studio reads every array without a null check: each is present and empty.
	for _, k := range []string{"legacy_auth_custom_roles", "objects_to_be_dropped", "unsupported_extensions", "user_defined_objects_in_internal_schemas", "validation_errors", "warnings", "potential_breaking_changes"} {
		if a, ok := m[k].([]any); !ok || len(a) != 0 {
			t.Errorf("%s = %#v, want []", k, m[k])
		}
	}
	if h, _ := m["duration_estimate_hours"].(float64); h <= 0 {
		t.Errorf("duration_estimate_hours = %v", m["duration_estimate_hours"])
	}

	sv := upJSON(t, f.fixture, "GET", "/platform/projects/"+testRef+"/service-versions", nil, 200, "GET /platform/projects/{ref}/service-versions")
	if sv["gotrue"] != "v2.100.0-r1" || sv["postgrest"] != "v12.0-r0" || sv["supabase-postgres"] != "17.1.0.001-r1" {
		t.Fatalf("service versions = %v", sv)
	}

	st := upJSON(t, f.fixture, "GET", p+"/upgrade/status", nil, 200, "GET /v1/projects/{ref}/upgrade/status")
	if v, ok := st["databaseUpgradeStatus"]; !ok || v != nil {
		t.Fatalf("status of a project never upgraded = %v, want null", st)
	}
}

// Studio renders the upgrade alert, the validation errors and the warnings of the Service
// versions section only when project_settings:database_upgrades is enabled, and its upgrade
// dialog prints a "right-sized" disk note unless the disk call returns the plan's included size.
func TestStudioCanOfferTheUpgrade(t *testing.T) {
	f := newUpgradeFixture(t)
	rec := f.do("GET", "/platform/profile", nil)
	if rec.Code != 200 {
		t.Fatalf("profile = %d", rec.Code)
	}
	var prof struct {
		Disabled []string `json:"disabled_features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &prof); err != nil {
		t.Fatal(err)
	}
	for _, d := range prof.Disabled {
		if d == "project_settings:database_upgrades" {
			t.Fatalf("disabled_features hides the upgrade section: %v", prof.Disabled)
		}
	}
}

func TestUpgradeThroughTheAPI(t *testing.T) {
	f := newUpgradeFixture(t)
	const p = "/v1/projects/" + testRef
	f.plane.gate = make(chan struct{})

	m := upJSON(t, f.fixture, "POST", p+"/upgrade", map[string]any{"target_version": "17", "release_channel": "ga"}, 201, "POST /v1/projects/{ref}/upgrade")
	id, _ := m["tracking_id"].(string)
	if len(id) != 36 {
		t.Fatalf("tracking_id = %q", id)
	}
	// The upgrade runs in the background: the project is UPGRADING and the status says so.
	f.waitStatus(t, registry.StatusUpgrading)
	proj := upJSON(t, f.fixture, "GET", "/platform/projects/"+testRef, nil, 200, "")
	if proj["status"] != "UPGRADING" {
		t.Fatalf("project status = %v", proj["status"])
	}
	var running map[string]any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		st := upJSON(t, f.fixture, "GET", p+"/upgrade/status?tracking_id="+id, nil, 200, "GET /v1/projects/{ref}/upgrade/status")
		running, _ = st["databaseUpgradeStatus"].(map[string]any)
		if running != nil && running["progress"] == lifecycle.ProgressServices {
			break
		}
	}
	if running == nil || running["status"] != float64(0) || running["progress"] != lifecycle.ProgressServices || running["target_version"] != "17.11.0.004-r1" || running["error"] != nil {
		t.Fatalf("running status = %v", running)
	}
	// A second upgrade, a pause and a delete are refused while one runs.
	upJSON(t, f.fixture, "POST", p+"/upgrade", map[string]any{"target_version": "17"}, 409, "")
	upJSON(t, f.fixture, "POST", p+"/pause", nil, 409, "")

	close(f.plane.gate)
	f.waitStatus(t, registry.StatusActiveHealthy)
	var done map[string]any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		st := upJSON(t, f.fixture, "GET", p+"/upgrade/status", nil, 200, "GET /v1/projects/{ref}/upgrade/status")
		done, _ = st["databaseUpgradeStatus"].(map[string]any)
		if done != nil && done["status"] == float64(1) {
			break
		}
	}
	if done == nil || done["status"] != float64(1) || done["progress"] != lifecycle.ProgressCompleted {
		t.Fatalf("final status = %v", done)
	}
	sv := upJSON(t, f.fixture, "GET", "/platform/projects/"+testRef+"/service-versions", nil, 200, "")
	if sv["gotrue"] != "v2.195.0-r1" || sv["supabase-postgres"] != "17.11.0.004-r1" {
		t.Fatalf("service versions after = %v", sv)
	}
	// Up to date: not eligible, no target, and another upgrade is refused with a message.
	m = upJSON(t, f.fixture, "GET", p+"/upgrade/eligibility", nil, 200, "GET /v1/projects/{ref}/upgrade/eligibility")
	if m["eligible"] != false || m["current_app_version"] != m["latest_app_version"] || len(m["target_upgrade_versions"].([]any)) != 0 {
		t.Fatalf("eligibility after = %v", m)
	}
	rec := f.do("POST", p+"/upgrade", map[string]any{"target_version": "17"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "already runs") {
		t.Fatalf("second upgrade = %d %s", rec.Code, rec.Body.String())
	}
}

// While `supavise upgrade` moves the node, a project upgrade from Studio or the API waits: the
// daemon restarts under it, and the rollout would take it for its own move and revert it on a
// failure.
func TestProjectUpgradeIsRefusedWhileTheNodeUpgrades(t *testing.T) {
	f := newUpgradeFixture(t)
	f.cfg.StateDir = t.TempDir()
	const p = "/v1/projects/" + testRef
	marker := notice.Upgrade{Phase: "projects", From: "v1.0.0", To: "v1.1.0", StartedAt: time.Now(), PID: os.Getpid()}
	if err := notice.WriteUpgrade(f.cfg.Paths(), marker); err != nil {
		t.Fatal(err)
	}
	rec := f.do("POST", p+"/upgrade", map[string]any{"target_version": "17"})
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "being upgraded") {
		t.Fatalf("during the node upgrade = %d %s", rec.Code, rec.Body.String())
	}
	if pr, err := f.reg.GetProject(context.Background(), testRef); err != nil || pr.Status != registry.StatusActiveHealthy {
		t.Fatalf("project = %v, %v", pr, err)
	}
	// Once the marker says it is over, the same request starts the upgrade.
	marker.Phase = "done"
	if err := notice.WriteUpgrade(f.cfg.Paths(), marker); err != nil {
		t.Fatal(err)
	}
	f.plane.gate = make(chan struct{})
	upJSON(t, f.fixture, "POST", p+"/upgrade", map[string]any{"target_version": "17"}, 201, "")
	f.waitStatus(t, registry.StatusUpgrading)
	close(f.plane.gate)
	f.waitStatus(t, registry.StatusActiveHealthy)
}

func TestUpgradeRequestValidation(t *testing.T) {
	f := newUpgradeFixture(t)
	const p = "/v1/projects/" + testRef
	for name, tc := range map[string]struct {
		in   any
		want int
		msg  string
	}{
		"no target":     {map[string]any{}, 400, "target_version is required"},
		"another major": {map[string]any{"target_version": "18"}, 400, "not available"},
		"older major":   {map[string]any{"target_version": "15"}, 400, "not available"},
		"another build": {map[string]any{"target_version": "17.1.0.001-r1"}, 400, "not available"},
		"other channel": {map[string]any{"target_version": "17", "release_channel": "beta"}, 400, "ga channel only"},
		"not json":      {"nope", 400, ""},
	} {
		rec := f.do("POST", p+"/upgrade", tc.in)
		if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.msg) {
			t.Errorf("%s: %d %s, want %d %q", name, rec.Code, rec.Body.String(), tc.want, tc.msg)
		}
	}
	if got, _ := f.reg.GetProject(context.Background(), testRef); got.Status != registry.StatusActiveHealthy {
		t.Fatalf("refused requests changed the status to %s", got.Status)
	}
	// The app version and the version without a revision name the node's target too.
	for _, v := range []string{"supabase-" + newPG, "17.11.0.004-r1", "17.11.0.004", newPG} {
		if err := upgradeTarget(f.eng, v); err != nil {
			t.Errorf("target %q refused: %v", v, err)
		}
	}
	// Unknown projects and the system project do not exist for these routes.
	upJSON(t, f.fixture, "GET", "/v1/projects/zzzzzzzzzzzzzzzzzzzz/upgrade/eligibility", nil, 404, "")
	upJSON(t, f.fixture, "POST", "/v1/projects/system/upgrade", map[string]any{"target_version": "17"}, 404, "")
}

func TestUpgradeAcrossMajorVersionsIsRefused(t *testing.T) {
	f := newUpgradeFixture(t)
	f.arts.pin(config.SvcPostgres, "postgres-18.0.0.001-r1")
	const p = "/v1/projects/" + testRef
	m := upJSON(t, f.fixture, "GET", p+"/upgrade/eligibility", nil, 200, "GET /v1/projects/{ref}/upgrade/eligibility")
	if m["eligible"] != false || len(m["target_upgrade_versions"].([]any)) != 0 || len(m["validation_errors"].([]any)) != 0 {
		t.Fatalf("eligibility = %v", m)
	}
	rec := f.do("POST", p+"/upgrade", map[string]any{"target_version": "18"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "major") {
		t.Fatalf("cross-major upgrade = %d %s", rec.Code, rec.Body.String())
	}
}

// A node whose pins went back (a rolled-back binary) does not offer a project that runs newer
// releases an upgrade, and the upgrade route refuses it.
func TestUpgradeToOlderPinsIsRefused(t *testing.T) {
	f := newUpgradeFixture(t)
	f.arts.pin(config.SvcPostgres, "postgres-17.0.5.001-r1")
	f.arts.pin(config.SvcGoTrue, "auth-v2.50.0-r1")
	const p = "/v1/projects/" + testRef
	m := upJSON(t, f.fixture, "GET", p+"/upgrade/eligibility", nil, 200, "GET /v1/projects/{ref}/upgrade/eligibility")
	if m["eligible"] != false || len(m["target_upgrade_versions"].([]any)) != 0 {
		t.Fatalf("eligibility = %v", m)
	}
	if m["current_app_version"] != "supabase-"+oldPG || m["latest_app_version"] != "supabase-"+oldPG {
		t.Fatalf("a project ahead of the node has nothing newer to offer: current %v, latest %v", m["current_app_version"], m["latest_app_version"])
	}
	rec := f.do("POST", p+"/upgrade", map[string]any{"target_version": "17"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "newer than") {
		t.Fatalf("downgrade = %d %s", rec.Code, rec.Body.String())
	}
	st := upJSON(t, f.fixture, "GET", p+"/upgrade/status", nil, 200, "GET /v1/projects/{ref}/upgrade/status")
	if st["databaseUpgradeStatus"] != nil {
		t.Fatalf("a refused upgrade left a status: %v", st)
	}
	if pr, _ := f.reg.GetProject(context.Background(), testRef); pr.Status != registry.StatusActiveHealthy || pr.Versions[config.SvcGoTrue] != oldAuth {
		t.Fatalf("project = %s %v", pr.Status, pr.Versions)
	}
}

func TestUpgradePausedProjectAndNoBackupService(t *testing.T) {
	f := newUpgradeFixture(t)
	const p = "/v1/projects/" + testRef
	_ = f.reg.SetProjectStatus(context.Background(), testRef, registry.StatusInactive)
	m := upJSON(t, f.fixture, "GET", p+"/upgrade/eligibility", nil, 200, "GET /v1/projects/{ref}/upgrade/eligibility")
	ve, _ := m["validation_errors"].([]any)
	if m["eligible"] != false || len(ve) != 1 || ve[0].(map[string]any)["type"] != "project_hibernating" {
		t.Fatalf("paused eligibility = %v", m)
	}
	upJSON(t, f.fixture, "POST", p+"/upgrade", map[string]any{"target_version": "17"}, 409, "")
	_ = f.reg.SetProjectStatus(context.Background(), testRef, registry.StatusActiveHealthy)

	// A node without a backup service does not upgrade.
	sec, _ := secrets.New(make([]byte, 32))
	f.srv.mgr = upgradeManager{f.mgr, lifecycle.NewEngine(f.cfg, f.reg, sec, f.arts, f.plane, lifecycle.Options{})}
	rec := f.do("POST", p+"/upgrade", map[string]any{"target_version": "17"})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "backup") {
		t.Fatalf("no backup service = %d %s", rec.Code, rec.Body.String())
	}
}

func TestUpgradeFailureIsReportedToStudio(t *testing.T) {
	f := newUpgradeFixture(t)
	const p = "/v1/projects/" + testRef
	sec, _ := secrets.New(make([]byte, 32))
	f.srv.mgr = upgradeManager{f.mgr, lifecycle.NewEngine(f.cfg, f.reg, sec, f.arts, f.plane, lifecycle.Options{Backup: apiBackup{err: errors.New("the backend refused the upload")}})}
	upJSON(t, f.fixture, "POST", p+"/upgrade", map[string]any{"target_version": "17"}, 201, "")
	f.waitStatus(t, registry.StatusActiveHealthy)
	var st map[string]any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		m := upJSON(t, f.fixture, "GET", p+"/upgrade/status", nil, 200, "GET /v1/projects/{ref}/upgrade/status")
		st, _ = m["databaseUpgradeStatus"].(map[string]any)
		if st != nil && st["status"] == float64(2) {
			break
		}
	}
	if st == nil || st["status"] != float64(2) || st["error"] != lifecycle.UpgradeErrBackup {
		t.Fatalf("status of a failed upgrade = %v", st)
	}
	// No API route of a node without the capability answers with a stale stub.
	f.srv.mgr = f.mgr
	upJSON(t, f.fixture, "GET", p+"/upgrade/eligibility", nil, 503, "")
}

// The roles of the upgrade routes: reading is for every member, upgrading for Owners and
// Administrators.
func TestUpgradeRoutesAuthorization(t *testing.T) {
	rf := newRolesFixture(t)
	up := newUpgradeFixtureOn(t, rf.fixture)
	_ = up
	const p = "/v1/projects/" + testRef
	for role, want := range map[string][3]int{
		// eligibility, status, POST upgrade (a body that is refused before anything starts
		// is 400; an allowed role gets past the permission check)
		"owner":    {200, 200, 400},
		"admin":    {200, 200, 400},
		"dev":      {200, 200, 403},
		"ro":       {200, 200, 403},
		"scoped":   {200, 200, 403},
		"stranger": {403, 403, 403},
	} {
		got := [3]int{
			rf.as(role, "GET", p+"/upgrade/eligibility", nil).Code,
			rf.as(role, "GET", p+"/upgrade/status", nil).Code,
			rf.as(role, "POST", p+"/upgrade", map[string]any{"target_version": "99"}).Code,
		}
		if got != want {
			t.Errorf("%s: eligibility, status, upgrade = %v, want %v", role, got, want)
		}
		if c := rf.as(role, "GET", "/platform/projects/"+testRef+"/service-versions", nil).Code; (c == 200) != (want[0] == 200) {
			t.Errorf("%s: service-versions = %d", role, c)
		}
	}
	_ = members.RoleOwner
}

// newUpgradeFixtureOn gives an existing fixture the upgrade capability.
func newUpgradeFixtureOn(t *testing.T, f *fixture) *upgradeFixture {
	t.Helper()
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	arts := &apiArts{pins: map[string]string{config.SvcPostgres: newPG, config.SvcGoTrue: newAuth, config.SvcPostgREST: "postgrest-v16.4-r0"}}
	plane := &apiPlane{}
	eng := lifecycle.NewEngine(f.cfg, f.reg, sec, arts, plane, lifecycle.Options{Backup: apiBackup{}})
	f.srv.mgr = upgradeManager{f.mgr, eng}
	return &upgradeFixture{fixture: f, arts: arts, plane: plane, eng: eng}
}

// An extension the new Postgres release cannot serve reaches Studio as hosted's
// unsupported_extension validation error, and the deprecated list names it too.
func TestEligibilityNamesUnsupportedExtensions(t *testing.T) {
	el := &lifecycle.UpgradeEligibility{
		Ref: testRef, Current: map[string]string{"postgres": oldPG}, Latest: map[string]string{"postgres": newPG}, Target: map[string]string{"postgres": newPG},
		CurrentMajor: 17, TargetMajor: 17,
		Blockers: []lifecycle.UpgradeBlocker{{Type: lifecycle.BlockerExtension, Extension: "wrappers", Message: "extension wrappers 0.5.0 (postgres): gone"}},
	}
	body := eligibilityBody(el)
	if body["eligible"] != false {
		t.Fatalf("eligible = %v", body["eligible"])
	}
	ve, _ := body["validation_errors"].([]any)
	if len(ve) != 1 || ve[0].(map[string]any)["type"] != "unsupported_extension" || ve[0].(map[string]any)["extension_name"] != "wrappers" {
		t.Fatalf("validation_errors = %v", body["validation_errors"])
	}
	if ue, _ := body["unsupported_extensions"].([]any); len(ue) != 1 || ue[0] != "wrappers" {
		t.Fatalf("unsupported_extensions = %v", body["unsupported_extensions"])
	}
}
