package notice

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

var t0 = time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)

func paths(t *testing.T) config.Paths {
	t.Helper()
	return config.Paths{Root: t.TempDir()}
}

func writeUpgrade(t *testing.T, p config.Paths, u any) {
	t.Helper()
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if raw, ok := u.(string); ok {
		b = []byte(raw)
	}
	if err := os.MkdirAll(filepath.Join(p.Root, "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Root, "system", "upgrade.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceValidation(t *testing.T) {
	ok := Maintenance{Message: "database work", StartsAt: t0, EndsAt: t0.Add(time.Hour)}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]Maintenance{
		"no message":     {StartsAt: t0, EndsAt: t0.Add(time.Hour)},
		"blank message":  {Message: "  ", StartsAt: t0, EndsAt: t0.Add(time.Hour)},
		"long message":   {Message: strings.Repeat("x", 501), StartsAt: t0, EndsAt: t0.Add(time.Hour)},
		"no start":       {Message: "m", EndsAt: t0},
		"ends too early": {Message: "m", StartsAt: t0, EndsAt: t0.Add(-time.Hour)},
		"zero length":    {Message: "m", StartsAt: t0, EndsAt: t0},
		"negative lead":  {Message: "m", StartsAt: t0, EndsAt: t0.Add(time.Hour), LeadSeconds: -1},
		"month long":     {Message: "m", StartsAt: t0, EndsAt: t0.Add(720 * time.Hour)},
	} {
		if m.Validate() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestLongMaintenanceNeedsPermissionAndIsCapped(t *testing.T) {
	long := Maintenance{Message: "m", StartsAt: t0, EndsAt: t0.Add(48 * time.Hour)}
	if long.Validate() == nil {
		t.Fatal("a 48 hour window was accepted without permission")
	}
	if err := (Maintenance{Message: "m", StartsAt: t0, EndsAt: t0.Add(MaxWindow)}).Validate(); err != nil {
		t.Errorf("a window of exactly %s: %v", MaxWindow, err)
	}
	long.Extended = true
	if err := long.Validate(); err != nil {
		t.Fatalf("with permission: %v", err)
	}
	if !long.Quiet(t0.Add(40 * time.Hour)) {
		t.Error("an extended window is quiet for all of it")
	}
	// A file edited by hand to a month, without permission: quiet for MaxWindow, no longer.
	edited := Maintenance{Message: "m", StartsAt: t0, EndsAt: t0.Add(720 * time.Hour)}
	if !edited.Quiet(t0.Add(time.Hour)) || edited.Quiet(t0.Add(MaxWindow+time.Minute)) {
		t.Error("an unextended window must stop being quiet after MaxWindow")
	}
	if edited.Quiet(t0.Add(-time.Hour)) {
		t.Error("quiet before the window opens")
	}
}

func TestMaintenanceWindows(t *testing.T) {
	m := Maintenance{Message: "m", StartsAt: t0, EndsAt: t0.Add(2 * time.Hour)}
	cases := []struct {
		at                 time.Time
		lead               int
		active, inProgress bool
	}{
		{t0.Add(-time.Hour), 0, false, false},
		{t0, 0, true, true},
		{t0.Add(time.Hour), 0, true, true},
		{t0.Add(2 * time.Hour), 0, false, false}, // the end is exclusive
		{t0.Add(-time.Hour), 86400, true, false}, // a banner ahead of the window
		{t0.Add(-25 * time.Hour), 86400, false, false},
	}
	for _, c := range cases {
		m.LeadSeconds = c.lead
		if m.Active(c.at) != c.active || m.InProgress(c.at) != c.inProgress {
			t.Errorf("at %s lead %d: active %v inProgress %v", c.at.Format(time.RFC3339), c.lead, m.Active(c.at), m.InProgress(c.at))
		}
	}
}

func TestMaintenanceFileRoundTrip(t *testing.T) {
	p := paths(t)
	if m, err := ReadMaintenance(p); m != nil || err != nil {
		t.Fatalf("nothing announced: %v %v", m, err)
	}
	if had, err := ClearMaintenance(p); had || err != nil {
		t.Fatalf("clearing nothing: %v %v", had, err)
	}
	if _, err := WriteMaintenance(p, Maintenance{Message: ""}, t0); err == nil {
		t.Fatal("an invalid announcement was written")
	}
	m, err := WriteMaintenance(p, Maintenance{Message: "kernel update", StartsAt: t0.Add(time.Hour), EndsAt: t0.Add(3 * time.Hour)}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID == "" || !m.AnnouncedAt.Equal(t0) {
		t.Errorf("%+v", m)
	}
	got, err := ReadMaintenance(p)
	if err != nil || got == nil || got.Message != "kernel update" || got.ID != m.ID {
		t.Fatalf("%+v %v", got, err)
	}
	fi, err := os.Stat(filepath.Join(p.Root, "system", "maintenance.json"))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("the proxy must be able to read the file: %v %v", fi, err)
	}
	// A new announcement replaces the old and has a new ID, so a dismissed banner comes back.
	m2, _ := WriteMaintenance(p, Maintenance{Message: "again", StartsAt: t0.Add(time.Hour), EndsAt: t0.Add(2 * time.Hour)}, t0.Add(time.Second))
	if m2.ID == m.ID {
		t.Error("two announcements share an ID")
	}
	if had, err := ClearMaintenance(p); !had || err != nil {
		t.Errorf("clear: %v %v", had, err)
	}
	if m, _ := ReadMaintenance(p); m != nil {
		t.Error("still announced after clear")
	}
}

func TestUpgradeRunning(t *testing.T) {
	cases := []struct {
		name string
		u    Upgrade
		want bool
	}{
		{"rolling out", Upgrade{Phase: "rollout", From: "v1", To: "v2", StartedAt: t0}, true},
		{"fetching", Upgrade{Phase: "fetch", StartedAt: t0.Add(-time.Hour)}, true},
		{"no start time and no file date is not believed", Upgrade{Phase: "backup"}, false},
		{"no start time, file touched a minute ago", Upgrade{Phase: "backup", Heartbeat: t0}, true},
		{"no start time, file untouched for hours", Upgrade{Phase: "backup", Heartbeat: t0.Add(-3 * time.Hour)}, false},
		{"old start, touched at every phase", Upgrade{Phase: "rollout", StartedAt: t0.Add(-5 * time.Hour), Heartbeat: t0}, true},
		{"done", Upgrade{Phase: "done", StartedAt: t0}, false},
		{"failed", Upgrade{Phase: "failed", StartedAt: t0}, false},
		{"rolled back, any case", Upgrade{Phase: "Rolled_Back", StartedAt: t0}, false},
		{"empty phase", Upgrade{StartedAt: t0}, false},
		{"left behind by a dead process", Upgrade{Phase: "rollout", StartedAt: t0.Add(-3 * time.Hour)}, false},
		{"just inside the limit", Upgrade{Phase: "rollout", StartedAt: t0.Add(-100 * time.Minute)}, true},
	}
	for _, c := range cases {
		if got := c.u.Running(t0.Add(time.Minute)); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestReadUpgradeDatesTheMarkerByTheFile(t *testing.T) {
	p := paths(t)
	writeUpgrade(t, p, map[string]any{"phase": "rollout", "from": "v1.0.0", "to": "v1.1.0"})
	mtime := t0.Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(p.Root, "system", "upgrade.json"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if _, ok := UpgradeRunning(p, t0); !ok {
		t.Error("a marker without started_at, touched a minute ago, is running")
	}
	// A crashed upgrade leaves the marker; hours later it is no longer believed.
	if _, ok := UpgradeRunning(p, t0.Add(3*time.Hour)); ok {
		t.Error("a marker untouched for hours still counts as running")
	}
}

func TestReadUpgradeIsTolerant(t *testing.T) {
	p := paths(t)
	if ReadUpgrade(p) != nil {
		t.Error("no file")
	}
	writeUpgrade(t, p, "{not json")
	if ReadUpgrade(p) != nil {
		t.Error("garbage must read as no upgrade")
	}
	if _, ok := UpgradeRunning(p, t0); ok {
		t.Error("garbage must not show a banner")
	}
	writeUpgrade(t, p, map[string]any{"phase": "rollout", "from": "v1.0.0", "to": "v1.1.0", "started_at": t0, "extra": "fields are fine"})
	if u, ok := UpgradeRunning(p, t0.Add(time.Minute)); !ok || u.To != "v1.1.0" {
		t.Errorf("%+v %v", u, ok)
	}
}

func decode(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out struct {
		Incidents []map[string]any `json:"incidents"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return out.Incidents
}

func TestBannerIsEmptyWhenThereIsNothingToSay(t *testing.T) {
	p := paths(t)
	if got := string(IncidentsJSON(p, t0)); got != `{"incidents":[]}`+"\n" {
		t.Errorf("%q", got)
	}
}

// Studio reads id, show_banner and metadata.{affected_regions, affects_project_creation, force}
// of each incident (apps/studio/data/platform/incident-banner-query.ts).
func TestBannerShapeMatchesWhatStudioReads(t *testing.T) {
	p := paths(t)
	if _, err := WriteMaintenance(p, Maintenance{Message: "Storage maintenance", StartsAt: t0, EndsAt: t0.Add(time.Hour)}, t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	inc := decode(t, IncidentsJSON(p, t0.Add(time.Minute)))
	if len(inc) != 1 {
		t.Fatalf("%v", inc)
	}
	i := inc[0]
	if id, _ := i["id"].(string); !strings.HasPrefix(id, "maintenance-") {
		t.Errorf("id %v", i["id"])
	}
	if i["show_banner"] != "force" {
		t.Errorf("show_banner %v: users without projects would not see it", i["show_banner"])
	}
	md, _ := i["metadata"].(map[string]any)
	if md["force"] != true || md["affects_project_creation"] != false {
		t.Errorf("metadata %v", md)
	}
	if v, present := md["affected_regions"]; !present || v != nil {
		t.Errorf("affected_regions must be present and null (no region restriction): %v", md)
	}
	if i["kind"] != "maintenance" || i["title"] != "Maintenance in progress" || i["message"] != "Storage maintenance" {
		t.Errorf("our fields: %v", i)
	}
}

func TestBannerShowsMaintenanceOnlyWhileItsBannerIsActive(t *testing.T) {
	p := paths(t)
	if _, err := WriteMaintenance(p, Maintenance{Message: "later", StartsAt: t0.Add(48 * time.Hour), EndsAt: t0.Add(50 * time.Hour)}, t0); err != nil {
		t.Fatal(err)
	}
	if got := decode(t, IncidentsJSON(p, t0)); len(got) != 0 {
		t.Errorf("a window two days away shows a banner by default: %v", got)
	}
	// With a notice lead the banner comes early, titled as scheduled.
	if _, err := WriteMaintenance(p, Maintenance{Message: "later", StartsAt: t0.Add(48 * time.Hour), EndsAt: t0.Add(50 * time.Hour), LeadSeconds: 24 * 3600}, t0); err != nil {
		t.Fatal(err)
	}
	if got := decode(t, IncidentsJSON(p, t0)); len(got) != 0 {
		t.Errorf("too early: %v", got)
	}
	got := decode(t, IncidentsJSON(p, t0.Add(30*time.Hour)))
	if len(got) != 1 || got[0]["title"] != "Scheduled maintenance" {
		t.Errorf("%v", got)
	}
	if got := decode(t, IncidentsJSON(p, t0.Add(51*time.Hour))); len(got) != 0 {
		t.Errorf("a finished window still shows: %v", got)
	}
}

func TestBannerShowsARunningUpgradeAndBoth(t *testing.T) {
	p := paths(t)
	writeUpgrade(t, p, Upgrade{Phase: "rollout", From: "v1.0.0", To: "v1.1.0", StartedAt: t0})
	got := decode(t, IncidentsJSON(p, t0.Add(time.Minute)))
	if len(got) != 1 || got[0]["kind"] != "upgrade" || got[0]["title"] != "Upgrade in progress" || !strings.Contains(got[0]["message"].(string), "v1.1.0") {
		t.Fatalf("%v", got)
	}
	idA := got[0]["id"]
	if _, err := WriteMaintenance(p, Maintenance{Message: "m", StartsAt: t0, EndsAt: t0.Add(time.Hour)}, t0); err != nil {
		t.Fatal(err)
	}
	both := decode(t, IncidentsJSON(p, t0.Add(time.Minute)))
	if len(both) != 2 || both[0]["id"] == both[1]["id"] {
		t.Errorf("%v", both)
	}
	// A second upgrade is a new banner for a user who dismissed the first.
	writeUpgrade(t, p, Upgrade{Phase: "rollout", From: "v1.1.0", To: "v1.2.0", StartedAt: t0.Add(24 * time.Hour)})
	if again := decode(t, IncidentsJSON(p, t0.Add(24*time.Hour+time.Minute))); again[0]["id"] == idA {
		t.Error("two upgrades share a banner ID")
	}
	writeUpgrade(t, p, Upgrade{Phase: "done", From: "v1.1.0", To: "v1.2.0", StartedAt: t0.Add(24 * time.Hour)})
	for _, i := range decode(t, IncidentsJSON(p, t0.Add(24*time.Hour+time.Minute))) {
		if i["kind"] == "upgrade" {
			t.Error("a finished upgrade still shows")
		}
	}
}

// The dashboard's users cannot act on a release; the operator hears about it from status and
// the alerts.
func TestBannerNeverMentionsAnAvailableUpdate(t *testing.T) {
	p := paths(t)
	if err := os.MkdirAll(filepath.Join(p.Root, "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Root, "system", "update.json"), []byte(`{"latest":"v9.0.0","available":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := string(IncidentsJSON(p, t0)); got != `{"incidents":[]}`+"\n" {
		t.Errorf("an available update reached the dashboard: %s", got)
	}
}

// Studio would draw an announced window or a running upgrade as "We are investigating a
// technical issue" with a link to Supabase's status page, so the served answer stays empty.
func TestServedBannerStaysEmptyWhileStudioCannotShowOurWords(t *testing.T) {
	p := paths(t)
	if _, err := WriteMaintenance(p, Maintenance{Message: "m", StartsAt: t0, EndsAt: t0.Add(time.Hour)}, t0); err != nil {
		t.Fatal(err)
	}
	writeUpgrade(t, p, Upgrade{Phase: "rollout", From: "v1.0.0", To: "v1.1.0", StartedAt: t0})
	if len(Banner(p, t0.Add(time.Minute))) != 2 {
		t.Fatal("the notices themselves are still computed")
	}
	if got := string(BannerJSON()); got != `{"incidents":[]}`+"\n" {
		t.Errorf("%q", got)
	}
}

// The upgrade writes the marker at each phase and leaves a finished phase when it ends; the
// banner and `supavise status` read the same file.
func TestWriteUpgradeRoundTrip(t *testing.T) {
	p := paths(t)
	if had, err := ClearUpgrade(p); err != nil || had {
		t.Fatalf("clearing nothing: %v %v", had, err)
	}
	u := Upgrade{Phase: "services", From: "v1.0.0", To: "v1.1.0", StartedAt: t0, PID: 4242, Detail: "realtime"}
	if err := WriteUpgrade(p, u); err != nil {
		t.Fatal(err)
	}
	got := ReadUpgrade(p)
	if got == nil || got.Phase != "services" || got.From != "v1.0.0" || got.To != "v1.1.0" || got.PID != 4242 || got.Detail != "realtime" || !got.StartedAt.Equal(t0) {
		t.Fatalf("read back %+v", got)
	}
	if _, ok := UpgradeRunning(p, t0.Add(time.Minute)); !ok {
		t.Fatal("a marker written a minute ago is not running")
	}
	u.Phase = "done"
	if err := WriteUpgrade(p, u); err != nil {
		t.Fatal(err)
	}
	if _, ok := UpgradeRunning(p, t0.Add(time.Minute)); ok {
		t.Fatal("a finished upgrade still counts as running")
	}
	if ReadUpgrade(p) == nil {
		t.Fatal("the finished marker was not kept")
	}
	if had, err := ClearUpgrade(p); err != nil || !had || ReadUpgrade(p) != nil {
		t.Fatalf("ClearUpgrade: %v %v", had, err)
	}
}

// The owner of the marker is set on the temporary file before it is renamed into place, so a
// symlink put at the marker path (the state directory belongs to a user that root does not trust)
// is replaced, not followed: its target is neither written nor re-owned.
func TestWriteUpgradeAsReplacesASymlinkInsteadOfFollowingIt(t *testing.T) {
	p := config.Paths{Root: t.TempDir()}
	dir := filepath.Join(p.Root, "system")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "upgrade.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteUpgradeAs(p, Upgrade{Phase: "services", From: "v1", To: "v2", StartedAt: time.Now()}, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the marker is %v, %v", fi, err)
	}
	if b, _ := os.ReadFile(target); string(b) != "precious" {
		t.Fatalf("the target was written: %q", b)
	}
	if after, _ := os.Stat(target); after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the target was changed")
	}
	if u := ReadUpgrade(p); u == nil || u.Phase != "services" || u.To != "v2" {
		t.Fatalf("marker = %+v", u)
	}
}

func TestUpgradeForwardAndProcess(t *testing.T) {
	if !(Upgrade{Phase: "projects"}).Forward() || (Upgrade{Phase: PhaseRollingBack}).Forward() {
		t.Fatal("Forward")
	}
	if !(Upgrade{PID: os.Getpid()}).ProcessAlive() || !(Upgrade{}).ProcessAlive() {
		t.Fatal("this process is alive, and an unnamed one counts as alive")
	}
	if (Upgrade{PID: 1 << 30}).ProcessAlive() {
		t.Fatal("a process that does not exist is alive")
	}
}
