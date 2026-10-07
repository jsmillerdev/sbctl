package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/backup"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// fakeBackupSource answers RestoreWindow from a canned window. Like the real service, a window of a
// project that is not running ends with the archive (idleLatest, when set) and not at now.
type fakeBackupSource struct {
	mu         sync.Mutex
	window     backup.RestoreWindow
	idleLatest time.Time
	err        error
	running    []bool
}

func (f *fakeBackupSource) RestoreWindow(_ context.Context, _ string, running bool) (backup.RestoreWindow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = append(f.running, running)
	w := f.window
	if !running && !f.idleLatest.IsZero() {
		w.Latest = f.idleLatest
	}
	return w, f.err
}

// backupFixture is a fixture whose project has two base backups listed in the registry and in
// the fake backup source: a 48 hours old one that ended 0.5 seconds into a second, and a
// 24 hours old one.
type backupFixture struct {
	*fixture
	src      *fakeBackupSource
	now      time.Time
	old, mid backup.Manifest
	oldID    int64
	midID    int64
}

func newBackupFixture(t *testing.T) *backupFixture {
	t.Helper()
	f := newFixture(t)
	now := time.Now().UTC().Truncate(time.Second).Add(123 * time.Millisecond)
	mk := func(id string, stop time.Time) backup.Manifest {
		return backup.Manifest{ID: id, Ref: testRef, Timeline: 1, StartTime: stop.Add(-time.Minute), StopTime: stop}
	}
	b := &backupFixture{fixture: f, now: now,
		old: mk("20261004T120000Z-aaaaaa", now.Add(-48*time.Hour).Truncate(time.Second).Add(500*time.Millisecond)),
		mid: mk("20261005T120000Z-bbbbbb", now.Add(-24*time.Hour)),
	}
	b.src = &fakeBackupSource{window: backup.RestoreWindow{Backups: []backup.Manifest{b.old, b.mid}, Earliest: b.old.StopTime, Latest: now}}
	f.srv.backups = b.src
	ctx := context.Background()
	add := func(m backup.Manifest, status registry.BackupStatus) int64 {
		row := &registry.Backup{Ref: testRef, Kind: "base", Status: status, StartedAt: m.StartTime,
			Location: "file:///var/lib/supavise/backups/" + testRef + "/base/" + m.ID}
		if err := f.reg.CreateBackup(ctx, row); err != nil {
			t.Fatal(err)
		}
		return row.ID
	}
	b.oldID = add(b.old, registry.BackupCompleted)
	b.midID = add(b.mid, registry.BackupCompleted)
	// Neither a failed attempt nor one still running is a backup to restore.
	add(mkFailed(now), registry.BackupFailed)
	add(mkFailed(now.Add(time.Minute)), registry.BackupRunning)
	return b
}

func mkFailed(at time.Time) backup.Manifest {
	return backup.Manifest{ID: "20261006T030000Z-" + at.Format("0405"), Ref: testRef, StartTime: at, StopTime: at}
}

func (b *backupFixture) post(path string, body any) *httptest.ResponseRecorder {
	b.t.Helper()
	return b.do("POST", path, body)
}

func (b *backupFixture) status() registry.Status {
	b.t.Helper()
	p, err := b.reg.GetProject(context.Background(), testRef)
	if err != nil {
		b.t.Fatal(err)
	}
	return p.Status
}

// waitRestore returns the next finished restore of the fake manager.
func (b *backupFixture) waitRestore() restoreRecord {
	b.t.Helper()
	select {
	case r := <-b.mgr.restores:
		return r
	case <-time.After(10 * time.Second):
		b.t.Fatal("the restore did not run")
		return restoreRecord{}
	}
}

func TestBackupListWithoutABackupService(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct{ key, path string }{
		{"GET /platform/database/{ref}/backups", "/platform/database/" + testRef + "/backups"},
		{"GET /v1/projects/{ref}/database/backups", "/v1/projects/" + testRef + "/database/backups"},
	} {
		rec := f.do("GET", tc.path, nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", tc.key, rec.Code, rec.Body)
		}
		validateAgainstSpec(t, tc.key, rec.Body.Bytes())
		m := decodeBody(t, rec).(map[string]any)
		if m["pitr_enabled"] != false || m["walg_enabled"] != false || len(m["backups"].([]any)) != 0 {
			t.Errorf("%s: %v", tc.key, m)
		}
	}
	// Nothing can be restored, and the error says why.
	for _, tc := range []struct{ path, body string }{
		{"/platform/database/" + testRef + "/backups/restore", `{"id":1}`},
		{"/platform/database/" + testRef + "/backups/pitr", `{"recovery_time_target_unix":1760000000}`},
		{"/v1/projects/" + testRef + "/database/backups/restore-pitr", `{"recovery_time_target_unix":1760000000}`},
	} {
		if rec := f.do("POST", tc.path, tc.body); rec.Code != 503 || !strings.Contains(rec.Body.String(), "not set up") {
			t.Errorf("POST %s: %d %s", tc.path, rec.Code, rec.Body)
		}
	}
	if st, _ := f.reg.GetProject(context.Background(), testRef); st.Status != registry.StatusActiveHealthy {
		t.Errorf("a refused restore changed the status to %s", st.Status)
	}
}

func TestPlatformBackupList(t *testing.T) {
	b := newBackupFixture(t)
	rec := b.do("GET", "/platform/database/"+testRef+"/backups", nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /platform/database/{ref}/backups", rec.Body.Bytes())
	m := decodeBody(t, rec).(map[string]any)
	if m["pitr_enabled"] != true || m["walg_enabled"] != true || m["region"] == "" {
		t.Fatalf("flags: %v", m)
	}
	list := m["backups"].([]any)
	if len(list) != 2 {
		t.Fatalf("backups = %v, want the two completed ones", list)
	}
	first := list[0].(map[string]any)
	if first["id"] != float64(b.oldID) || first["isPhysicalBackup"] != true || first["status"] != "COMPLETED" ||
		first["inserted_at"] != ts(b.old.StopTime) || first["project_id"] != float64(b.project.Seq) {
		t.Errorf("first backup = %v", first)
	}
	if list[1].(map[string]any)["id"] != float64(b.midID) {
		t.Errorf("second backup = %v", list[1])
	}
	// The picker is offered whole seconds inside the window: the earliest rounds up, the latest down.
	pd := m["physicalBackupData"].(map[string]any)
	if got, want := pd["earliestPhysicalBackupDateUnix"], float64(b.old.StopTime.Truncate(time.Second).Add(time.Second).Unix()); got != want {
		t.Errorf("earliest = %v, want %v", got, want)
	}
	if got, want := pd["latestPhysicalBackupDateUnix"], float64(b.now.Unix()); got != want {
		t.Errorf("latest = %v, want %v", got, want)
	}
	if len(b.src.running) != 1 || !b.src.running[0] {
		t.Errorf("the window of an active project is asked for with running=%v", b.src.running)
	}

	// A paused project is not running: its window ends with the archive.
	if err := b.reg.SetProjectStatus(context.Background(), testRef, registry.StatusInactive); err != nil {
		t.Fatal(err)
	}
	b.do("GET", "/platform/database/"+testRef+"/backups", nil)
	if b.src.running[1] {
		t.Error("a paused project was reported as running")
	}
}

func TestV1BackupList(t *testing.T) {
	b := newBackupFixture(t)
	rec := b.do("GET", "/v1/projects/"+testRef+"/database/backups", nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, "GET /v1/projects/{ref}/database/backups", rec.Body.Bytes())
	m := decodeBody(t, rec).(map[string]any)
	list := m["backups"].([]any)
	if len(list) != 2 || m["pitr_enabled"] != true {
		t.Fatalf("%v", m)
	}
	if e := list[0].(map[string]any); e["id"] != float64(b.oldID) || e["is_physical_backup"] != true || e["status"] != "COMPLETED" || e["inserted_at"] != ts(b.old.StopTime) {
		t.Errorf("backup = %v", e)
	}
	pd := m["physical_backup_data"].(map[string]any)
	if pd["latest_physical_backup_date_unix"] != float64(b.now.Unix()) || pd["earliest_physical_backup_date_unix"] == nil {
		t.Errorf("physical_backup_data = %v", pd)
	}
}

func TestBackupListWithoutAnyBackupYet(t *testing.T) {
	b := newBackupFixture(t)
	b.src.window = backup.RestoreWindow{}
	rec := b.do("GET", "/platform/database/"+testRef+"/backups", nil)
	validateAgainstSpec(t, "GET /platform/database/{ref}/backups", rec.Body.Bytes())
	m := decodeBody(t, rec).(map[string]any)
	// Archiving is on, so PITR is enabled; Studio shows its "No backups yet" state while the
	// picker's span is absent.
	if m["pitr_enabled"] != true || len(m["backups"].([]any)) != 0 || len(m["physicalBackupData"].(map[string]any)) != 0 {
		t.Fatalf("%v", m)
	}
	// And a point-in-time restore has nothing to start from.
	rec = b.post("/platform/database/"+testRef+"/backups/pitr", map[string]any{"recovery_time_target_unix": b.now.Unix()})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "no backup") {
		t.Fatalf("pitr with no backup: %d %s", rec.Code, rec.Body)
	}
}

func TestBackupListWhenTheStoreFails(t *testing.T) {
	b := newBackupFixture(t)
	b.src.err = errors.New("s3: AccessDenied for bucket private-bucket-name")
	for _, path := range []string{"/platform/database/" + testRef + "/backups", "/v1/projects/" + testRef + "/database/backups"} {
		rec := b.do("GET", path, nil)
		if rec.Code != 502 || strings.Contains(rec.Body.String(), "private-bucket-name") {
			t.Errorf("GET %s: %d %s (the storage error must stay in the log)", path, rec.Code, rec.Body)
		}
	}
	rec := b.post("/platform/database/"+testRef+"/backups/pitr", map[string]any{"recovery_time_target_unix": b.now.Unix()})
	if rec.Code != 502 || b.status() != registry.StatusActiveHealthy {
		t.Errorf("a restore while the store fails: %d %s, status %s", rec.Code, rec.Body, b.status())
	}
}

func TestRestoreFromAListedBackup(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"platform", "/platform/database/" + testRef + "/backups/restore"},
		{"platform physical", "/platform/database/" + testRef + "/backups/restore-physical"},
		{"v1", "/v1/projects/" + testRef + "/database/backups/restore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBackupFixture(t)
			b.mgr.restores = make(chan restoreRecord, 4)
			b.mgr.gate = make(chan struct{})
			rec := b.post(tc.path, map[string]any{"id": b.midID})
			if rec.Code != 201 {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			// The project is RESTORING for as long as the restore runs, and another restore is refused.
			if got := b.status(); got != registry.StatusRestoring {
				t.Fatalf("status during the restore = %s", got)
			}
			if rec := b.do("GET", "/platform/projects/"+testRef+"/status", nil); !strings.Contains(rec.Body.String(), "RESTORING") {
				t.Errorf("the status route says %s", rec.Body)
			}
			if rec := b.post(tc.path, map[string]any{"id": b.oldID}); rec.Code != 409 || !strings.Contains(rec.Body.String(), "RESTORING") {
				t.Errorf("a second restore: %d %s", rec.Code, rec.Body)
			}
			close(b.mgr.gate)
			got := b.waitRestore()
			if got.ref != testRef || got.req.BackupID != b.mid.ID || !got.req.Target.IsZero() {
				t.Fatalf("restore request = %+v", got)
			}
			waitStatus(t, b, registry.StatusActiveHealthy)
		})
	}
}

func waitStatus(t *testing.T, b *backupFixture, want registry.Status) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if b.status() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("status = %s, want %s", b.status(), want)
}

func TestRestoreFromABackupRefusals(t *testing.T) {
	b := newBackupFixture(t)
	b.mgr.restores = make(chan restoreRecord, 4)
	path := "/platform/database/" + testRef + "/backups/restore"
	// An unknown id, a missing id and a fractional id.
	for _, tc := range []struct {
		body any
		code int
	}{
		{map[string]any{"id": 987654}, 404},
		{map[string]any{}, 400},
		{map[string]any{"id": 1.5}, 400},
		{`{"id":`, 400},
	} {
		if rec := b.post(path, tc.body); rec.Code != tc.code {
			t.Errorf("POST %v: %d %s, want %d", tc.body, rec.Code, rec.Body, tc.code)
		}
	}
	// A backup row whose manifest left the window (pruned, or on a timeline that is not the
	// current history) is refused with the reason, not restored into a failure.
	b.src.window.Backups = b.src.window.Backups[1:]
	if rec := b.post(path, map[string]any{"id": b.oldID}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "cannot be restored") {
		t.Errorf("a backup outside the window: %d %s", rec.Code, rec.Body)
	}
	// A project that is not active.
	if err := b.reg.SetProjectStatus(context.Background(), testRef, registry.StatusInactive); err != nil {
		t.Fatal(err)
	}
	if rec := b.post(path, map[string]any{"id": b.midID}); rec.Code != 409 {
		t.Errorf("restore of a paused project: %d %s", rec.Code, rec.Body)
	}
	// A project that does not exist.
	if rec := b.post("/platform/database/zzzzzzzzzzzzzzzzzzzz/backups/restore", map[string]any{"id": b.midID}); rec.Code != 404 {
		t.Errorf("restore of a missing project: %d", rec.Code)
	}
	select {
	case r := <-b.mgr.restores:
		t.Fatalf("a refused request ran a restore: %+v", r)
	default:
	}
}

// A backup id is a registry row id, and rows of other projects are numbered in the same
// sequence: naming another project's backup must not restore it into this one, even when
// the backup service happens to know the manifest.
func TestRestoreRefusesAnotherProjectsBackup(t *testing.T) {
	b := newBackupFixture(t)
	b.mgr.restores = make(chan restoreRecord, 4)
	ctx := context.Background()
	const otherRef = "zyxwvutsrqponmlkjihg"
	if err := b.reg.CreateProject(ctx, &registry.Project{Ref: otherRef, Name: "other", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	foreign := backup.Manifest{ID: "20261003T120000Z-cccccc", Ref: otherRef, Timeline: 1, StartTime: b.now.Add(-72 * time.Hour), StopTime: b.now.Add(-72*time.Hour + time.Minute)}
	row := &registry.Backup{Ref: otherRef, Kind: "base", Status: registry.BackupCompleted, StartedAt: foreign.StartTime,
		Location: "file:///var/lib/supavise/backups/" + otherRef + "/base/" + foreign.ID}
	if err := b.reg.CreateBackup(ctx, row); err != nil {
		t.Fatal(err)
	}
	// The fake source ignores the ref, so only the ref-scoped row lookup stands in the way.
	b.src.window.Backups = append(b.src.window.Backups, foreign)
	for _, path := range []string{
		"/platform/database/" + testRef + "/backups/restore",
		"/platform/database/" + testRef + "/backups/restore-physical",
		"/v1/projects/" + testRef + "/database/backups/restore",
	} {
		if rec := b.post(path, map[string]any{"id": row.ID}); rec.Code != 404 {
			t.Errorf("POST %s with another project's backup: %d %s, want 404", path, rec.Code, rec.Body)
		}
	}
	if got := b.status(); got != registry.StatusActiveHealthy {
		t.Errorf("status after the refusals = %s", got)
	}
	select {
	case r := <-b.mgr.restores:
		t.Fatalf("a refused request ran a restore: %+v", r)
	default:
	}
}

func TestRestorePhysicalToATimeFromABackup(t *testing.T) {
	b := newBackupFixture(t)
	b.mgr.restores = make(chan restoreRecord, 4)
	path := "/platform/database/" + testRef + "/backups/restore-physical"
	target := b.mid.StopTime.Add(3 * time.Hour).Truncate(time.Second)
	rec := b.post(path, map[string]any{"id": b.midID, "recovery_time_target": target.Format(time.RFC3339)})
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got := b.waitRestore()
	if got.req.BackupID != b.mid.ID || !got.req.Target.Equal(target) {
		t.Fatalf("request = %+v, want backup %s to %s", got.req, b.mid.ID, target)
	}
	waitStatus(t, b, registry.StatusActiveHealthy)
	// Unix seconds are read too (this one is long before the backup ended).
	if rec := b.post(path, map[string]any{"id": b.midID, "recovery_time_target": "1000000000"}); rec.Code != 400 {
		t.Errorf("a target before the backup ended: %d %s", rec.Code, rec.Body)
	}
	// Before the backup ended, after the window, or not a time at all.
	for _, bad := range []string{b.mid.StopTime.Add(-time.Hour).Format(time.RFC3339), b.now.Add(time.Hour).Format(time.RFC3339), "yesterday"} {
		if rec := b.post(path, map[string]any{"id": b.midID, "recovery_time_target": bad}); rec.Code != 400 {
			t.Errorf("target %q: %d %s", bad, rec.Code, rec.Body)
		}
	}
}

func TestPointInTimeRestore(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"platform", "/platform/database/" + testRef + "/backups/pitr"},
		{"v1", "/v1/projects/" + testRef + "/database/backups/restore-pitr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBackupFixture(t)
			b.mgr.restores = make(chan restoreRecord, 4)
			b.mgr.gate = make(chan struct{})
			target := b.now.Add(-6 * time.Hour).Truncate(time.Second)
			rec := b.post(tc.path, map[string]any{"recovery_time_target_unix": target.Unix()})
			if rec.Code != 201 {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if got := b.status(); got != registry.StatusRestoring {
				t.Fatalf("status during the restore = %s", got)
			}
			close(b.mgr.gate)
			got := b.waitRestore()
			if got.ref != testRef || !got.req.Target.Equal(target) || got.req.BackupID != "" {
				t.Fatalf("request = %+v, want a restore to %s", got, target)
			}
			waitStatus(t, b, registry.StatusActiveHealthy)
		})
	}
}

func TestPointInTimeRestoreOutsideTheWindow(t *testing.T) {
	b := newBackupFixture(t)
	b.mgr.restores = make(chan restoreRecord, 4)
	path := "/v1/projects/" + testRef + "/database/backups/restore-pitr"
	earliest := b.old.StopTime.Truncate(time.Second).Add(time.Second)
	// The window's edges are inside; one second beyond either is refused with the window in the message.
	if rec := b.post(path, map[string]any{"recovery_time_target_unix": earliest.Unix()}); rec.Code != 201 {
		t.Fatalf("the earliest second: %d %s", rec.Code, rec.Body)
	}
	b.waitRestore()
	waitStatus(t, b, registry.StatusActiveHealthy)
	for name, unix := range map[string]int64{
		"before the oldest backup": earliest.Unix() - 1,
		"in the future":            b.now.Unix() + 1,
		"zero":                     0,
	} {
		rec := b.post(path, map[string]any{"recovery_time_target_unix": unix})
		if rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
			continue
		}
		if name != "zero" && (!strings.Contains(rec.Body.String(), ts(earliest)) || !strings.Contains(rec.Body.String(), ts(b.now.Truncate(time.Second)))) {
			t.Errorf("%s: the message does not name the window: %s", name, rec.Body)
		}
	}
	for _, body := range []any{map[string]any{}, map[string]any{"recovery_time_target_unix": 1.5}, map[string]any{"recovery_time_target_unix": "tomorrow"}} {
		if rec := b.post(path, body); rec.Code != 400 {
			t.Errorf("body %v: %d %s", body, rec.Code, rec.Body)
		}
	}
	if b.status() != registry.StatusActiveHealthy {
		t.Fatalf("a refused request left the project %s", b.status())
	}
	select {
	case r := <-b.mgr.restores:
		t.Fatalf("a refused request ran a restore: %+v", r)
	default:
	}
}

func TestRestoreRefusedWhileNotActiveOrWithoutBackends(t *testing.T) {
	b := newBackupFixture(t)
	path := "/platform/database/" + testRef + "/backups/pitr"
	body := map[string]any{"recovery_time_target_unix": b.now.Add(-time.Hour).Unix()}
	// The Engine says the node has no working backup backend.
	b.mgr.beginErr = lifecycle.ErrNoRestorer
	if rec := b.post(path, body); rec.Code != 503 {
		t.Errorf("no restorer: %d %s", rec.Code, rec.Body)
	}
	// The Engine refuses a project that went out of state between the check and the call.
	b.mgr.beginErr = fmt.Errorf("%w: cannot restore %s while it is RESTORING", lifecycle.ErrInvalidState, testRef)
	if rec := b.post(path, body); rec.Code != 409 {
		t.Errorf("the Engine refuses: %d %s", rec.Code, rec.Body)
	}
	// A disk without room for the restored copy is a 409 that says so, and the project is untouched.
	b.mgr.beginErr = fmt.Errorf("%w: the disk has 1.0 GiB free and restoring project %s needs about 9.0 GiB", lifecycle.ErrInsufficientDisk, testRef)
	if rec := b.post(path, body); rec.Code != 409 || !strings.Contains(rec.Body.String(), "free") || b.status() != registry.StatusActiveHealthy {
		t.Errorf("not enough disk: %d %s, status %s", rec.Code, rec.Body, b.status())
	}
	b.mgr.beginErr = nil
	// While the project is not running, its time is not judged against a window that ends with the
	// archive: the answer is 409 (a second restore during RESTORING), not a 400 about the time.
	b.src.idleLatest = b.old.StopTime.Add(time.Hour)
	for _, st := range []registry.Status{registry.StatusInactive, registry.StatusRestoring, registry.StatusComingUp} {
		if err := b.reg.SetProjectStatus(context.Background(), testRef, st); err != nil {
			t.Fatal(err)
		}
		if rec := b.post(path, body); rec.Code != 409 {
			t.Errorf("restore while %s: %d %s", st, rec.Code, rec.Body)
		}
		if b.status() != st {
			t.Errorf("a refused restore moved %s to %s", st, b.status())
		}
	}
}

// The restore itself has no deadline: one that expired halfway would also expire the rollback
// that stops and starts the project.
func TestRestoreRunsWithoutADeadline(t *testing.T) {
	b := newBackupFixture(t)
	b.mgr.restores = make(chan restoreRecord, 4)
	rec := b.post("/platform/database/"+testRef+"/backups/pitr", map[string]any{"recovery_time_target_unix": b.now.Add(-time.Hour).Unix()})
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := b.waitRestore(); got.deadline {
		t.Error("the restore's context carries a deadline")
	}
}

func TestRestoreFailureLeavesTheProjectUnhealthy(t *testing.T) {
	b := newBackupFixture(t)
	b.mgr.restores = make(chan restoreRecord, 4)
	b.mgr.restoreErr = errors.New("recovery ended before configured recovery target was reached")
	rec := b.post("/platform/database/"+testRef+"/backups/pitr", map[string]any{"recovery_time_target_unix": b.now.Add(-time.Hour).Unix()})
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	b.waitRestore()
	waitStatus(t, b, registry.StatusActiveUnhealthy)
}

func TestDrainWaitsForARestore(t *testing.T) {
	b := newBackupFixture(t)
	b.mgr.restores = make(chan restoreRecord, 4)
	b.mgr.gate = make(chan struct{})
	if rec := b.post("/platform/database/"+testRef+"/backups/pitr", map[string]any{"recovery_time_target_unix": b.now.Add(-time.Hour).Unix()}); rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := b.srv.Drain(short); err == nil {
		t.Fatal("Drain returned while a restore was running")
	}
	close(b.mgr.gate)
	b.waitRestore()
	long, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if err := b.srv.Drain(long); err != nil {
		t.Fatalf("Drain after the restore: %v", err)
	}
	// A draining server refuses a new restore before anything moves.
	waitStatus(t, b, registry.StatusActiveHealthy)
	rec := b.post("/platform/database/"+testRef+"/backups/pitr", map[string]any{"recovery_time_target_unix": b.now.Add(-time.Hour).Unix()})
	if rec.Code != http.StatusServiceUnavailable || b.status() != registry.StatusActiveHealthy {
		t.Fatalf("restore while draining: %d %s, status %s", rec.Code, rec.Body, b.status())
	}
}

func TestProjectAddonsReportPITR(t *testing.T) {
	b := newBackupFixture(t)
	get := func(path, key string) map[string]any {
		t.Helper()
		rec := b.do("GET", path, nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		validateAgainstSpec(t, key, rec.Body.Bytes())
		return decodeBody(t, rec).(map[string]any)
	}
	plat := func() map[string]any {
		return get("/platform/projects/"+testRef+"/billing/addons", "GET /platform/projects/{ref}/billing/addons")
	}
	v1 := func() map[string]any {
		return get("/v1/projects/"+testRef+"/billing/addons", "GET /v1/projects/{ref}/billing/addons")
	}

	for _, tc := range []struct {
		days int
		id   string
	}{{7, "pitr_7"}, {10, "pitr_7"}, {14, "pitr_14"}, {21, "pitr_14"}, {28, "pitr_28"}, {90, "pitr_28"}} {
		b.cfg.Backup.RetentionDays = tc.days
		sel := plat()["selected_addons"].([]any)
		if len(sel) != 1 {
			t.Fatalf("%d days: selected_addons = %v", tc.days, sel)
		}
		a := sel[0].(map[string]any)
		v := a["variant"].(map[string]any)
		if a["type"] != "pitr" || v["identifier"] != tc.id || v["price"] != float64(0) || v["price_description"] != "Included" ||
			v["meta"].(map[string]any)["backup_duration_days"] != float64(tc.days) || !strings.Contains(v["name"].(string), "days") {
			t.Errorf("%d days: %v", tc.days, a)
		}
		a1 := v1()["selected_addons"].([]any)[0].(map[string]any)
		v1v := a1["variant"].(map[string]any)
		if v1v["id"] != tc.id || v1v["price"].(map[string]any)["amount"] != float64(0) || v1v["meta"].(map[string]any)["backup_duration_days"] != float64(tc.days) {
			t.Errorf("%d days (v1): %v", tc.days, a1)
		}
	}
	if len(plat()["available_addons"].([]any)) != 0 {
		t.Error("an add-on is offered for purchase")
	}

	// Without a backup service there is no PITR.
	b.srv.backups = nil
	if len(plat()["selected_addons"].([]any)) != 0 || len(v1()["selected_addons"].([]any)) != 0 {
		t.Error("an add-on is reported on a node without backups")
	}
}

func TestPitrRetentionDays(t *testing.T) {
	f := newFixture(t)
	f.cfg.Backup.RetentionDays = 9
	p := &registry.Project{CreatedAt: time.Now().Add(-99*24*time.Hour - time.Hour)} // 99 days and 1 hour old
	if got := f.srv.pitrRetentionDays(p); got != 9 {
		t.Errorf("retention_days 9: %d", got)
	}
	// With pruning off (0) every backup is kept, so the history reaches back to the project's creation.
	f.cfg.Backup.RetentionDays = 0
	if got := f.srv.pitrRetentionDays(p); got != 100 {
		t.Errorf("pruning off: %d, want 100", got)
	}
	if got := f.srv.pitrRetentionDays(&registry.Project{CreatedAt: time.Now()}); got != 1 {
		t.Errorf("a new project with pruning off: %d, want 1", got)
	}
}

// A backup that ended less than a second ago leaves no whole second to restore to: the backup is
// listed and the span is absent, so the picker is not offered an inverted range.
func TestBackupListInTheSecondAfterTheFirstBackup(t *testing.T) {
	b := newBackupFixture(t)
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	b.old.StopTime = base.Add(600 * time.Millisecond)
	b.src.window = backup.RestoreWindow{Backups: []backup.Manifest{b.old}, Earliest: b.old.StopTime, Latest: base.Add(900 * time.Millisecond)}
	rec := b.do("GET", "/v1/projects/"+testRef+"/database/backups", nil)
	validateAgainstSpec(t, "GET /v1/projects/{ref}/database/backups", rec.Body.Bytes())
	m := decodeBody(t, rec).(map[string]any)
	if len(m["backups"].([]any)) != 1 || len(m["physical_backup_data"].(map[string]any)) != 0 {
		t.Fatalf("%v", m)
	}
	rec = b.post("/v1/projects/"+testRef+"/database/backups/restore-pitr", map[string]any{"recovery_time_target_unix": base.Unix()})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "no backup") {
		t.Fatalf("pitr in that second: %d %s", rec.Code, rec.Body)
	}
}
