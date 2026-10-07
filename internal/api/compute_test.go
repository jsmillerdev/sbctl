package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/diskquota"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/registry"
)

func (f *fixture) body(t *testing.T, method, path string, in any, wantStatus int) map[string]any {
	t.Helper()
	rec := f.do(method, path, in)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s: %d %s (want %d)", method, path, rec.Code, rec.Body, wantStatus)
	}
	if rec.Body.Len() == 0 {
		return nil
	}
	m, _ := decodeBody(t, rec).(map[string]any)
	return m
}

func (f *fixture) projectRow(t *testing.T) *registry.Project {
	t.Helper()
	p, err := f.reg.GetProject(t.Context(), testRef)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const (
	platAddons = "/platform/projects/" + testRef + "/billing/addons"
	v1Addons   = "/v1/projects/" + testRef + "/billing/addons"
)

func variantIDs(t *testing.T, resp map[string]any, key string) []string {
	t.Helper()
	var ids []string
	for _, a := range resp["available_addons"].([]any) {
		am := a.(map[string]any)
		if am["type"] != "compute_instance" {
			continue
		}
		for _, v := range am["variants"].([]any) {
			vm := v.(map[string]any)
			id, _ := vm[key].(string)
			ids = append(ids, id)
		}
	}
	return ids
}

// Studio's Compute and Disk page reads the sizes from available_addons: the ones the node can give
// the project, with prices of 0, and the project's own size is always among them.
func TestAddonsListTheSizesTheNodeCanOffer(t *testing.T) {
	f := newFixture(t)
	f.mgr.unfit = map[string]string{"xlarge": "no room", "2xlarge": "no room", "4xlarge": "no room", "8xlarge": "no room", "12xlarge": "no room", "16xlarge": "no room"}

	plat := f.body(t, "GET", platAddons, nil, 200)
	validateAgainstSpec(t, "GET /platform/projects/{ref}/billing/addons", asJSON(t, plat))
	ids := variantIDs(t, plat, "identifier")
	if strings.Join(ids, ",") != "ci_micro,ci_small,ci_medium,ci_large" {
		t.Fatalf("offered = %v (Nano is Studio's own card, the unfit sizes are left out)", ids)
	}
	sel := plat["selected_addons"].([]any)[0].(map[string]any)
	v := sel["variant"].(map[string]any)
	if sel["type"] != "compute_instance" || v["identifier"] != "ci_micro" || v["name"] != "Micro" || v["price"] != float64(0) {
		t.Fatalf("selected = %v", sel)
	}
	meta := v["meta"].(map[string]any)
	if meta["memory_gb"] != float64(1) || meta["cpu_cores"] != float64(1) || meta["connections_direct"] != float64(60) || meta["connections_pooler"] != float64(200) {
		t.Fatalf("meta = %v", meta)
	}

	v1 := f.body(t, "GET", v1Addons, nil, 200)
	validateAgainstSpec(t, "GET /v1/projects/{ref}/billing/addons", asJSON(t, v1))
	if got := strings.Join(variantIDs(t, v1, "id"), ","); got != "ci_micro,ci_small,ci_medium,ci_large" {
		t.Fatalf("v1 offered = %s", got)
	}

	// A project on a size the node could no longer give still lists it.
	p := f.projectRow(t)
	p.Class = "4xlarge"
	_ = f.reg.UpdateProject(t.Context(), p)
	plat = f.body(t, "GET", platAddons, nil, 200)
	if got := strings.Join(variantIDs(t, plat, "identifier"), ","); !strings.Contains(got, "ci_4xlarge") {
		t.Fatalf("the current size is missing: %s", got)
	}

	// Nano is the absence of an add-on.
	p.Class = "nano"
	_ = f.reg.UpdateProject(t.Context(), p)
	plat = f.body(t, "GET", platAddons, nil, 200)
	for _, a := range plat["selected_addons"].([]any) {
		if a.(map[string]any)["type"] == "compute_instance" {
			t.Fatalf("a Nano project lists a compute add-on: %v", a)
		}
	}
}

func asJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestChangingTheSizeShowsResizingUntilTheProjectIsBack(t *testing.T) {
	f := newFixture(t)
	f.mgr.resizeGate = make(chan struct{})

	f.body(t, "POST", platAddons, map[string]any{"addon_type": "compute_instance", "addon_variant": "ci_small"}, http.StatusCreated)
	p := f.projectRow(t)
	if p.Status != registry.StatusResizing || p.Class != "small" {
		t.Fatalf("while resizing: %s %s", p.Status, p.Class)
	}
	// What Studio polls.
	if st := f.body(t, "GET", "/platform/projects/"+testRef+"/status", nil, 200); st["status"] != "RESIZING" {
		t.Fatalf("status = %v", st)
	}
	if d := f.body(t, "GET", "/platform/projects/"+testRef, nil, 200); d["infra_compute_size"] != "small" || d["status"] != "RESIZING" {
		t.Fatalf("detail = %v", d)
	}
	// Another change while it runs is refused, not queued.
	f.mgr.mu.Lock()
	f.mgr.resizeErr = lifecycle.ErrInvalidState
	f.mgr.mu.Unlock()
	rec := f.do("POST", platAddons, map[string]any{"addon_type": "compute_instance", "addon_variant": "ci_large"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second change: %d %s", rec.Code, rec.Body)
	}
	f.mgr.mu.Lock()
	f.mgr.resizeErr = nil
	f.mgr.mu.Unlock()

	close(f.mgr.resizeGate)
	waitFor(t, "the project to be healthy again", func() bool { return f.projectRow(t).Status == registry.StatusActiveHealthy })
	list := f.body(t, "GET", "/platform/projects", nil, 200)
	if row := list["projects"].([]any)[0].(map[string]any); row["infra_compute_size"] != "small" {
		t.Fatalf("list row = %v", row)
	}
	validateAgainstSpec(t, "GET /platform/projects", asJSON(t, list))
}

func TestChangingTheSizeOnV1AndByRemovingTheAddon(t *testing.T) {
	f := newFixture(t)
	f.body(t, "PATCH", v1Addons, map[string]any{"addon_type": "compute_instance", "addon_variant": "ci_medium"}, http.StatusOK)
	waitFor(t, "medium", func() bool {
		p := f.projectRow(t)
		return p.Class == "medium" && p.Status == registry.StatusActiveHealthy
	})
	if got := f.projectRow(t).Limits; got.MemoryMax != "4G" || got.CPUQuota != "200%" {
		t.Fatalf("limits = %+v", got)
	}
	// Taking the compute add-on away returns the project to Nano.
	f.body(t, "DELETE", platAddons+"/ci_medium", nil, http.StatusOK)
	waitFor(t, "nano", func() bool {
		p := f.projectRow(t)
		return p.Class == "nano" && p.Status == registry.StatusActiveHealthy
	})
	// The same on /v1: add a Micro, then take the compute add-on away again.
	f.body(t, "POST", platAddons, map[string]any{"addon_type": "compute_instance", "addon_variant": "ci_micro"}, http.StatusCreated)
	waitFor(t, "micro for v1", func() bool {
		return f.projectRow(t).Class == "micro" && f.projectRow(t).Status == registry.StatusActiveHealthy
	})
	f.body(t, "DELETE", v1Addons+"/ci_micro", nil, http.StatusOK)
	waitFor(t, "nano by v1", func() bool {
		return f.projectRow(t).Class == "nano" && f.projectRow(t).Status == registry.StatusActiveHealthy
	})
	// Studio sends ci_nano from its own Nano card.
	f.body(t, "POST", platAddons, map[string]any{"addon_type": "compute_instance", "addon_variant": "ci_micro"}, http.StatusCreated)
	waitFor(t, "micro", func() bool {
		return f.projectRow(t).Class == "micro" && f.projectRow(t).Status == registry.StatusActiveHealthy
	})
	f.body(t, "POST", platAddons, map[string]any{"addon_type": "compute_instance", "addon_variant": "ci_nano"}, http.StatusCreated)
	waitFor(t, "nano again", func() bool {
		return f.projectRow(t).Class == "nano" && f.projectRow(t).Status == registry.StatusActiveHealthy
	})
	// The size already held changes nothing.
	f.mgr.mu.Lock()
	n := len(f.mgr.resizes)
	f.mgr.mu.Unlock()
	f.body(t, "POST", platAddons, map[string]any{"addon_type": "compute_instance", "addon_variant": "ci_nano"}, http.StatusCreated)
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	if len(f.mgr.resizes) != n {
		t.Fatal("the same size ran a resize")
	}
}

func TestChangingTheSizeIsRefusedWithAClearMessage(t *testing.T) {
	f := newFixture(t)
	f.mgr.unfit = map[string]string{"large": "this node cannot run a Large project: it needs a memory cap of 8 GB"}

	for _, tc := range []struct {
		name string
		body map[string]any
		want int
		msg  string
	}{
		{"does not fit", map[string]any{"addon_type": "compute_instance", "addon_variant": "ci_large"}, 400, "This node cannot run a Large project"},
		{"unknown variant", map[string]any{"addon_type": "compute_instance", "addon_variant": "ci_24xlarge"}, 400, "not a compute size this node offers"},
		{"no variant", map[string]any{"addon_type": "compute_instance"}, 400, "addon_variant is required"},
	} {
		rec := f.do("POST", platAddons, tc.body)
		if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.msg) {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
	}
	if p := f.projectRow(t); sizeOf(p).Name != "micro" || p.Status != registry.StatusActiveHealthy {
		t.Fatalf("a refused change touched the project: %+v", p)
	}
	// Other add-ons are not sold: the call succeeds and changes nothing.
	f.body(t, "POST", platAddons, map[string]any{"addon_type": "ipv4", "addon_variant": "ipv4_default"}, http.StatusCreated)
	// A project that is not running cannot be resized.
	_ = f.reg.SetProjectStatus(t.Context(), testRef, registry.StatusComingUp)
	if rec := f.do("POST", platAddons, map[string]any{"addon_type": "compute_instance", "addon_variant": "ci_small"}); rec.Code != http.StatusConflict {
		t.Fatalf("resize of a project that is coming up: %d %s", rec.Code, rec.Body)
	}
}

func TestCreateWithADesiredInstanceSize(t *testing.T) {
	f := newFixture(t)
	f.body(t, "POST", "/platform/projects", map[string]any{"name": "sized", "desired_instance_size": "small"}, http.StatusCreated)
	if len(f.mgr.created) == 0 || f.mgr.created[len(f.mgr.created)-1].Class != "small" {
		t.Fatalf("created = %+v", f.mgr.created)
	}
	rec := f.do("POST", "/platform/projects", map[string]any{"name": "huge", "desired_instance_size": "24xlarge_high_memory"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "not a compute size") {
		t.Fatalf("unknown size: %d %s", rec.Code, rec.Body)
	}
	f.mgr.createFn = func(lifecycle.CreateRequest) (*registry.Project, error) {
		return nil, &lifecycle.CapacityError{Message: "this node cannot run a XL project: no room"}
	}
	rec = f.do("POST", "/platform/projects", map[string]any{"name": "big", "desired_instance_size": "xlarge"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "This node cannot run a XL project") {
		t.Fatalf("capacity refusal: %d %s", rec.Code, rec.Body)
	}
}

// ---- disk -------------------------------------------------------------------------------------

func TestDiskReportsTheVolumeWhenNoSizeIsEnforced(t *testing.T) {
	f := newFixture(t)
	f.srv.disk = &fakeDisk{vol: diskquota.Volume{Mount: "/var/lib/supavise", FSType: "ext4", TotalBytes: 80 << 30, FreeBytes: 50 << 30}, used: 3 << 30}

	d := f.body(t, "GET", "/platform/projects/"+testRef+"/disk", nil, 200)
	validateAgainstSpec(t, "GET /platform/projects/{ref}/disk", asJSON(t, d))
	a := d["attributes"].(map[string]any)
	if a["size_gb"] != float64(80) || a["type"] != "gp3" || a["iops"] != float64(3000) {
		t.Fatalf("attributes = %v", a)
	}
	v1 := f.body(t, "GET", "/v1/projects/"+testRef+"/config/disk", nil, 200)
	validateAgainstSpec(t, "GET /v1/projects/{ref}/config/disk", asJSON(t, v1))

	u := f.body(t, "GET", "/platform/projects/"+testRef+"/disk/util", nil, 200)
	validateAgainstSpec(t, "GET /platform/projects/{ref}/disk/util", asJSON(t, u))
	m := u["metrics"].(map[string]any)
	if m["fs_used_bytes"] != float64(3<<30) || m["fs_size_bytes"] != float64(80<<30) || m["fs_avail_bytes"] != float64(50<<30) {
		t.Fatalf("metrics = %v", m)
	}
	f.body(t, "GET", "/v1/projects/"+testRef+"/config/disk/util", nil, 200)
	for _, p := range []string{"/platform/projects/" + testRef + "/disk/custom-config", "/v1/projects/" + testRef + "/config/disk/autoscale"} {
		c := f.body(t, "GET", p, nil, 200)
		if c["max_size_gb"] != float64(80) || c["growth_percent"] == nil {
			t.Fatalf("%s = %v", p, c)
		}
	}

	// Without prjquota the size is informational and a change is refused, saying how to enable it.
	for _, tc := range []struct{ method, path string }{
		{"POST", "/platform/projects/" + testRef + "/disk"},
		{"POST", "/v1/projects/" + testRef + "/config/disk"},
	} {
		rec := f.do(tc.method, tc.path, map[string]any{"attributes": map[string]any{"type": "gp3", "size_gb": 100, "iops": 3000, "throughput_mbps": 125}})
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "prjquota") || !strings.Contains(rec.Body.String(), "informational") {
			t.Errorf("%s: %d %s", tc.path, rec.Code, rec.Body)
		}
	}
	rec := f.do("POST", "/platform/projects/"+testRef+"/resize", map[string]any{"volume_size_gb": 100})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "prjquota") {
		t.Errorf("resize: %d %s", rec.Code, rec.Body)
	}
	// Saving the size it already has is not a change.
	f.body(t, "POST", "/platform/projects/"+testRef+"/disk", map[string]any{"attributes": map[string]any{"type": "gp3", "size_gb": 80, "iops": 3000, "throughput_mbps": 125}}, http.StatusCreated)
	if rec := f.do("POST", "/platform/projects/"+testRef+"/disk/custom-config", map[string]any{"growth_percent": 10}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "autoscaling is not available") {
		t.Errorf("autoscale: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/platform/projects/"+testRef+"/disk", map[string]any{"attributes": map[string]any{"type": "io2", "size_gb": 80, "iops": 9000}}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "IOPS and throughput") {
		t.Errorf("io2: %d %s", rec.Code, rec.Body)
	}
}

func TestDiskSizeIsEnforcedOnAnXFSProjectQuotaVolume(t *testing.T) {
	f := newFixture(t)
	d := &fakeDisk{vol: diskquota.Volume{Mount: "/var/lib/supavise", FSType: "xfs", Options: []string{"rw", "prjquota"}, TotalBytes: 100 << 30, FreeBytes: 90 << 30}, used: 4 << 30}
	f.srv.disk = d
	path := "/platform/projects/" + testRef + "/disk"
	set := func(gb float64) *httptest.ResponseRecorder {
		return f.do("POST", path, map[string]any{"attributes": map[string]any{"type": "gp3", "size_gb": gb, "iops": 3000, "throughput_mbps": 125}})
	}

	if r := set(20); r.Code != http.StatusCreated {
		t.Fatalf("set 20: %d %s", r.Code, r.Body)
	}
	if got := strings.Join(d.setCall, ","); got != testRef+"=20" {
		t.Fatalf("set calls = %s", got)
	}
	a := f.body(t, "GET", path, nil, 200)["attributes"].(map[string]any)
	if a["size_gb"] != float64(20) {
		t.Fatalf("size after = %v", a)
	}
	m := f.body(t, "GET", path+"/util", nil, 200)["metrics"].(map[string]any)
	if m["fs_size_bytes"] != float64(20<<30) || m["fs_used_bytes"] != float64(4<<30) || m["fs_avail_bytes"] != float64(16<<30) {
		t.Fatalf("metrics under a quota = %v", m)
	}
	for _, tc := range []struct {
		gb  float64
		msg string
	}{{4, "plus 20%"}, {200, "holds 100 GB"}, {0, "at least 1"}, {2.5, "whole number"}} {
		if r := set(tc.gb); r.Code != 400 || !strings.Contains(r.Body.String(), tc.msg) {
			t.Errorf("size %v: %d %s", tc.gb, r.Code, r.Body)
		}
	}
	if rec := f.do("POST", "/platform/projects/"+testRef+"/resize", map[string]any{"volume_size_gb": 30}); rec.Code != http.StatusCreated {
		t.Fatalf("resize: %d %s", rec.Code, rec.Body)
	}
	if g, _ := d.Limit(testRef); g != 30 {
		t.Fatalf("limit after resize = %d", g)
	}
}

// The pooler tenant of a project runs with its size's pool and client limit until someone saves others.
func TestPoolerDefaultsFollowTheSize(t *testing.T) {
	f := newFixture(t)
	p := f.projectRow(t)
	p.Class = "small"
	_ = f.reg.UpdateProject(t.Context(), p)
	got := f.body(t, "GET", "/platform/projects/"+testRef+"/config/pgbouncer", nil, 200)
	if got["default_pool_size"] != float64(35) || got["max_client_conn"] != float64(400) {
		t.Fatalf("small: %v", got)
	}
	f.cfg.Fleet.PoolerMaxClientConn = 300 // the node's ceiling holds the default down
	got = f.body(t, "GET", "/platform/projects/"+testRef+"/config/pgbouncer", nil, 200)
	if got["max_client_conn"] != float64(300) {
		t.Fatalf("under a ceiling of 300: %v", got)
	}
}
