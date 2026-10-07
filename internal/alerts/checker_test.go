package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/health"
	"github.com/jsmillerdev/supavise/internal/notice"
)

const (
	refA = "aaaaaaaaaaaaaaaaaaaa"
	refB = "bbbbbbbbbbbbbbbbbbbb"
)

type rig struct {
	t       *testing.T
	cfg     *config.Config
	sink    *sink
	clk     *clock
	report  *health.Report
	err     error
	checker *Checker
}

func newRig(t *testing.T) *rig {
	t.Helper()
	s := newSink(t)
	cfg := testCfg(t, config.AlertWebhook{URL: s.srv.URL})
	cfg.Alerts.UnhealthyAfterSeconds = 180
	r := &rig{t: t, cfg: cfg, sink: s, clk: newClock()}
	r.report = healthyReport()
	r.checker = r.newChecker()
	return r
}

func (r *rig) newChecker() *Checker {
	return &Checker{
		Notifier: New(r.cfg, Options{Now: r.clk.now}),
		Report:   func(context.Context) (*health.Report, error) { return r.report, r.err },
		Cfg:      r.cfg, Now: r.clk.now,
	}
}

func healthyReport() *health.Report {
	r := &health.Report{
		Components: []health.Component{{Name: "daemon", State: health.OK, Critical: true}, {Name: "disk", State: health.OK}},
		Projects: []health.ProjectResult{
			{Ref: refA, Name: "alpha", State: health.OK, Probed: true, Services: []health.ServiceResult{{Name: "postgres", OK: true}}},
			{Ref: refB, Name: "beta", State: health.OK, Probed: true, Services: []health.ServiceResult{{Name: "postgres", OK: true}}},
		},
	}
	r.Finish()
	return r
}

// cycle runs one check and advances the clock by a minute.
func (r *rig) cycle() {
	r.checker.Once(context.Background())
	r.clk.add(time.Minute)
}

func (r *rig) setComponent(name string, st health.State, detail string) {
	for i := range r.report.Components {
		if r.report.Components[i].Name == name {
			r.report.Components[i].State, r.report.Components[i].Detail = st, detail
			return
		}
	}
	r.report.Components = append(r.report.Components, health.Component{Name: name, State: st, Detail: detail})
}

func (r *rig) sent() []webhookBody {
	r.sink.mu.Lock()
	defer r.sink.mu.Unlock()
	var out []webhookBody
	for _, b := range r.sink.bodies {
		var w webhookBody
		_ = json.Unmarshal(b, &w)
		out = append(out, w)
	}
	return out
}

func TestHealthyNodeRaisesNothing(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 10; i++ {
		r.cycle()
	}
	if n := r.sink.count(); n != 0 {
		t.Errorf("%d alerts from a healthy node", n)
	}
}

func TestProjectUnhealthyIsDebouncedRaisedOnceAndResolved(t *testing.T) {
	r := newRig(t)
	r.report.Projects[0].Services = []health.ServiceResult{{Name: "postgres", OK: true}, {Name: "postgrest", OK: false, Error: "unit is inactive/dead"}}
	// 180 s debounce at one cycle a minute: the fourth cycle is the first that is old enough.
	for i := 0; i < 3; i++ {
		r.cycle()
		if r.sink.count() != 0 {
			t.Fatalf("raised after %d cycle(s), before the debounce", i+1)
		}
	}
	r.cycle()
	got := r.sent()
	if len(got) != 1 || got[0].Kind != KindProjectUnhealthy || got[0].Ref != refA || got[0].Severity != SeverityCritical ||
		!strings.Contains(got[0].Detail, "postgrest") || !strings.Contains(got[0].Title, "alpha") {
		t.Fatalf("sent %+v", got)
	}
	for i := 0; i < 20; i++ { // still down: no repeat inside the interval
		r.cycle()
	}
	if r.sink.count() != 1 {
		t.Errorf("%d alerts for one standing problem", r.sink.count())
	}
	// It heals.
	r.report.Projects[0].Services = []health.ServiceResult{{Name: "postgres", OK: true}}
	r.cycle()
	got = r.sent()
	if len(got) != 2 || !got[1].Resolved || got[1].Ref != refA || got[1].Kind != KindProjectUnhealthy {
		t.Fatalf("recovery: %+v", got)
	}
	r.cycle()
	if r.sink.count() != 2 {
		t.Error("the recovery repeats")
	}
}

func TestAProblemThatClearsBeforeTheDebounceNeverSendsAnything(t *testing.T) {
	r := newRig(t)
	bad := []health.ServiceResult{{Name: "postgres", OK: false, Error: "restarting"}}
	good := []health.ServiceResult{{Name: "postgres", OK: true}}
	for i := 0; i < 10; i++ {
		r.report.Projects[0].Services = bad
		r.cycle()
		r.cycle()
		r.report.Projects[0].Services = good
		r.cycle()
	}
	if n := r.sink.count(); n != 0 {
		t.Errorf("a flapping project that never stayed down for 3 minutes raised %d alerts", n)
	}
}

func TestRecoveryAfterADaemonRestart(t *testing.T) {
	r := newRig(t)
	r.report.Projects[0].Services = []health.ServiceResult{{Name: "postgres", OK: false, Error: "down"}}
	for i := 0; i < 4; i++ {
		r.cycle()
	}
	if r.sink.count() != 1 {
		t.Fatalf("%d", r.sink.count())
	}
	// The daemon restarts: a new Checker over the same state, still down, then healed.
	r.checker = r.newChecker()
	for i := 0; i < 5; i++ {
		r.cycle()
	}
	if r.sink.count() != 1 {
		t.Errorf("the restart repeated the alert: %d", r.sink.count())
	}
	r.report.Projects[0].Services = []health.ServiceResult{{Name: "postgres", OK: true}}
	r.cycle()
	if got := r.sent(); len(got) != 2 || !got[1].Resolved {
		t.Errorf("no recovery after a restart: %+v", got)
	}
}

func TestDiskCertificateBackupAndNodeConditions(t *testing.T) {
	r := newRig(t)
	r.setComponent("disk", health.Fail, "3% free")
	r.setComponent("certificates", health.Warn, "the certificate for api.example.com expires in 5d")
	r.setComponent("realtime", health.Fail, "unit is failed")
	r.setComponent("system backup", health.Warn, "backup is stale")
	r.report.Projects[1].Backup = &health.BackupResult{LastFailed: "AccessDenied"}
	for i := 0; i < 4; i++ {
		r.cycle()
	}
	byKind := map[string][]webhookBody{}
	for _, b := range r.sent() {
		byKind[b.Kind] = append(byKind[b.Kind], b)
	}
	if d := byKind[KindDiskLow]; len(d) != 1 || d[0].Severity != SeverityCritical {
		t.Errorf("disk: %+v", d)
	}
	if c := byKind[KindCertificateExpiring]; len(c) != 1 || c[0].Severity != SeverityWarning || !strings.Contains(c[0].Detail, "api.example.com") {
		t.Errorf("certificate: %+v", c)
	}
	if n := byKind[KindNodeUnhealthy]; len(n) != 1 || !strings.Contains(n[0].Title, "realtime") {
		t.Errorf("node: %+v", n)
	}
	bf := byKind[KindBackupFailed]
	if len(bf) != 2 {
		t.Fatalf("backup alerts: %+v", bf)
	}
	refs := map[string]bool{bf[0].Ref: true, bf[1].Ref: true}
	if !refs["system"] || !refs[refB] {
		t.Errorf("backup alerts are for %v", refs)
	}
}

func TestStaleBackupWithoutAnyCompletedOne(t *testing.T) {
	r := newRig(t)
	r.report.Projects[0].Backup = &health.BackupResult{Stale: true, Note: "no completed backup"}
	for i := 0; i < 4; i++ {
		r.cycle()
	}
	got := r.sent()
	if len(got) != 1 || got[0].Kind != KindBackupFailed || !strings.Contains(got[0].Title, "stale") || !strings.Contains(got[0].Detail, "no completed backup") {
		t.Errorf("%+v", got)
	}
}

func TestAnUnreachableSharedServiceIsNotBlamedOnEveryProject(t *testing.T) {
	r := newRig(t)
	for i := range r.report.Projects {
		r.report.Projects[i].Tenants = []health.TenantResult{{Service: "realtime", Error: "connection refused"}}
		r.report.Projects[i].State = health.Warn
	}
	r.setComponent("realtime", health.Fail, "unit is failed")
	for i := 0; i < 4; i++ {
		r.cycle()
	}
	got := r.sent()
	if len(got) != 1 || got[0].Kind != KindNodeUnhealthy {
		t.Errorf("one dead service raised %+v", got)
	}
	// A tenant that is simply missing is the project's problem.
	r = newRig(t)
	r.report.Projects[0].Tenants = []health.TenantResult{{Service: "storage", Present: false}}
	for i := 0; i < 4; i++ {
		r.cycle()
	}
	if got := r.sent(); len(got) != 1 || got[0].Kind != KindProjectUnhealthy || !strings.Contains(got[0].Detail, "storage has no tenant") {
		t.Errorf("%+v", got)
	}
}

func TestManyUnhealthyProjectsAreOneAlert(t *testing.T) {
	r := newRig(t)
	r.report.Projects = nil
	for i := 0; i < 12; i++ {
		ref := strings.Repeat(string(rune('a'+i)), 20)
		r.report.Projects = append(r.report.Projects, health.ProjectResult{Ref: ref, State: health.Fail, Probed: true,
			Services: []health.ServiceResult{{Name: "postgres", OK: false, Error: "down"}}})
	}
	for i := 0; i < 4; i++ {
		r.cycle()
	}
	got := r.sent()
	if len(got) != 1 || !strings.Contains(got[0].Title, "Many projects") || !strings.Contains(got[0].Detail, "12 projects") || !strings.Contains(got[0].Detail, "and 2 more") {
		t.Errorf("%+v", got)
	}
}

func TestProjectsAreNotJudgedDuringAnUpgradeOrAMaintenanceWindow(t *testing.T) {
	r := newRig(t)
	r.report.Projects[0].Services = []health.ServiceResult{{Name: "postgres", OK: false, Error: "down"}}
	up := notice.Upgrade{Phase: "rollout", From: "v1", To: "v2", StartedAt: r.clk.now()}
	b, _ := json.Marshal(up)
	if err := os.MkdirAll(filepath.Join(r.cfg.StateDir, "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	upgradeFile := filepath.Join(r.cfg.StateDir, "system", "upgrade.json")
	if err := os.WriteFile(upgradeFile, b, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		r.cycle()
	}
	if r.sink.count() != 0 {
		t.Fatalf("%d alerts during an upgrade", r.sink.count())
	}
	// The upgrade ends; the project is still down: the debounce starts now, not before.
	if err := os.Remove(upgradeFile); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		r.cycle()
	}
	if r.sink.count() != 0 {
		t.Fatalf("raised %d alert(s) before the debounce after the upgrade", r.sink.count())
	}
	r.cycle()
	if r.sink.count() != 1 {
		t.Fatalf("not raised after the debounce: %d", r.sink.count())
	}

	// A maintenance window silences the checker the same way, recoveries included.
	if _, err := notice.WriteMaintenance(r.cfg.Paths(), notice.Maintenance{Message: "m", StartsAt: r.clk.now().Add(-time.Minute), EndsAt: r.clk.now().Add(time.Hour)}, r.clk.now()); err != nil {
		t.Fatal(err)
	}
	r.report.Projects[0].Services = []health.ServiceResult{{Name: "postgres", OK: true}}
	r.cycle()
	if r.sink.count() != 1 {
		t.Errorf("a recovery was sent inside a maintenance window")
	}
	if _, err := notice.ClearMaintenance(r.cfg.Paths()); err != nil {
		t.Fatal(err)
	}
	r.cycle()
	if got := r.sent(); len(got) != 2 || !got[1].Resolved {
		t.Errorf("the recovery after the window: %+v", got)
	}
}

func TestReportErrorRaisesNothing(t *testing.T) {
	r := newRig(t)
	r.err = errors.New("check timed out")
	r.report = nil
	for i := 0; i < 5; i++ {
		r.cycle()
	}
	if r.sink.count() != 0 {
		t.Error("an alert from a failed check")
	}
}

func TestUndeliveredAlertIsRetriedAtTheNextCheck(t *testing.T) {
	r := newRig(t)
	r.sink.setStatus(400)
	r.report.Projects[0].Services = []health.ServiceResult{{Name: "postgres", OK: false, Error: "down"}}
	for i := 0; i < 6; i++ {
		r.cycle()
	}
	attempts := r.sink.count()
	if attempts < 2 {
		t.Fatalf("a failed delivery was not retried: %d attempts", attempts)
	}
	r.sink.setStatus(200)
	r.cycle()
	if got := r.sent(); !strings.Contains(got[len(got)-1].Title, "alpha") || r.sink.count() != attempts+1 {
		t.Errorf("%d attempts, %d after the endpoint healed", attempts, r.sink.count())
	}
}

// ---- update_available ----

func TestUpdateAvailableIsRaisedOncePerVersion(t *testing.T) {
	r := newRig(t)
	installed := "v1.2.3"
	latest := "v1.3.0"
	checks := 0
	r.checker.CheckUpdate = func(ctx context.Context, now time.Time) (*health.UpdateRecord, error) {
		checks++
		api := githubAPI(t, latest, 200)
		return health.CheckUpdate(ctx, r.cfg, installed, health.UpdateSource{Repo: "o/r", APIBase: api.URL}, now)
	}
	r.checker.UpdateInterval = 24 * time.Hour

	r.cycle()
	got := r.sent()
	if len(got) != 1 || got[0].Kind != KindUpdateAvailable || got[0].Severity != SeverityInfo || !strings.Contains(got[0].Title, "v1.3.0") ||
		!strings.Contains(got[0].Detail, "supavise upgrade --check") {
		t.Fatalf("%+v", got)
	}
	// Daily: not again within the interval, and the version is not announced twice.
	for i := 0; i < 30; i++ {
		r.cycle()
	}
	if checks != 1 || r.sink.count() != 1 {
		t.Errorf("%d checks, %d alerts within a day", checks, r.sink.count())
	}
	// A day later the check runs again and finds nothing new: still one alert.
	r.clk.add(25 * time.Hour)
	r.cycle()
	if checks != 2 || r.sink.count() != 1 {
		t.Errorf("%d checks, %d alerts after a day", checks, r.sink.count())
	}
	// A newer release is a new alert.
	latest = "v1.4.0"
	r.clk.add(25 * time.Hour)
	r.cycle()
	if got := r.sent(); len(got) != 2 || !strings.Contains(got[1].Title, "v1.4.0") {
		t.Errorf("%+v", got)
	}
	// After the upgrade nothing is available.
	installed = "v1.4.0"
	r.clk.add(25 * time.Hour)
	r.cycle()
	if r.sink.count() != 2 {
		t.Errorf("an alert for the release that is installed")
	}
}

func TestUpdateCheckFailureDoesNotRaiseAndIsNotRetriedEveryCycle(t *testing.T) {
	r := newRig(t)
	calls := 0
	r.checker.CheckUpdate = func(ctx context.Context, now time.Time) (*health.UpdateRecord, error) {
		calls++
		bad := githubAPI(t, "", 500)
		return health.CheckUpdate(ctx, r.cfg, "v1.0.0", health.UpdateSource{APIBase: bad.URL}, now)
	}
	for i := 0; i < 10; i++ {
		r.cycle()
	}
	if calls != 1 {
		t.Errorf("%d update checks in ten minutes after a failure", calls)
	}
	if r.sink.count() != 0 {
		t.Error("a failed update check raised an alert")
	}
}

func TestUpdateIsNotMarkedNotifiedWhileNoDestinationExists(t *testing.T) {
	r := newRig(t)
	r.cfg.Alerts.Webhooks = nil
	r.checker = r.newChecker()
	api := githubAPI(t, "v2.0.0", 200)
	r.checker.CheckUpdate = func(ctx context.Context, now time.Time) (*health.UpdateRecord, error) {
		return health.CheckUpdate(ctx, r.cfg, "v1.0.0", health.UpdateSource{APIBase: api.URL}, now)
	}
	r.cycle()
	rec, _ := health.ReadUpdate(r.cfg.Paths())
	if rec == nil || rec.NotifiedVersion != "" {
		t.Errorf("marked as told with nobody to tell: %+v", rec)
	}
	// An operator adds a webhook later; the next check announces it.
	r.cfg.Alerts.Webhooks = []config.AlertWebhook{{URL: r.sink.srv.URL}}
	r.checker = r.newChecker()
	r.checker.CheckUpdate = func(context.Context, time.Time) (*health.UpdateRecord, error) { return rec, nil }
	r.cycle()
	if r.sink.count() != 1 {
		t.Errorf("%d alerts", r.sink.count())
	}
}

// githubAPI is a fake GitHub API answering /repos/<repo>/releases/latest.
func githubAPI(t *testing.T, tag string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": []any{}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The disk is most likely to run out while an upgrade takes its fresh backups, and a failed
// backup matters most then; only what the window restarts is held back.
func TestDiskAndBackupsAreStillRaisedDuringAnUpgradeAndNotResolvedEarly(t *testing.T) {
	r := newRig(t)
	r.report.Projects[0].Services = []health.ServiceResult{{Name: "postgres", OK: false, Error: "down"}}
	r.report.Projects[1].Backup = &health.BackupResult{LastFailed: "no space left on device"}
	r.setComponent("disk", health.Warn, "4% free")
	r.setComponent("system backup", health.Warn, "no completed backup")
	b, _ := json.Marshal(notice.Upgrade{Phase: "backup", From: "v1", To: "v2", StartedAt: r.clk.now()})
	if err := os.MkdirAll(filepath.Join(r.cfg.StateDir, "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.cfg.StateDir, "system", "upgrade.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		r.cycle()
	}
	kinds := map[string]int{}
	for _, w := range r.sent() {
		kinds[w.Kind]++
	}
	if kinds[KindDiskLow] != 1 || kinds[KindBackupFailed] != 2 || kinds[KindProjectUnhealthy] != 0 || len(kinds) != 2 {
		t.Errorf("during the upgrade: %v", kinds)
	}
	// The disk recovers while the upgrade still runs: that is told. The project is not resolved or raised.
	r.setComponent("disk", health.OK, "40% free")
	r.cycle()
	got := r.sent()
	if last := got[len(got)-1]; last.Kind != KindDiskLow || !last.Resolved {
		t.Errorf("disk recovery during the upgrade: %+v", last)
	}
}
