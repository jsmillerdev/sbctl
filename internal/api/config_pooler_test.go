package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/OWNER/sbctl/internal/projectconfig"
	"github.com/OWNER/sbctl/internal/registry"
)

var errPoolerDown = errors.New("supavisor: connection refused")

// The pooler config is saved per project, applied to the project's Supavisor tenant, and read
// back by the CLI's routes (v1, v2) and by Studio's (platform) alike.
func TestPoolerConfigSaveApplyAndReadBack(t *testing.T) {
	f := newFixture(t)
	const v1 = "/v1/projects/" + testRef + "/config/database/pooler"
	const sup = "/platform/projects/" + testRef + "/config/supavisor"
	const pgb = "/platform/projects/" + testRef + "/config/pgbouncer"
	const v2 = "/v2/projects/" + testRef + "/config"
	read := func(path string) map[string]any {
		t.Helper()
		rec := f.mustDo("GET", path, nil, 200)
		if arr, ok := decodeBody(t, rec).([]any); ok {
			return arr[0].(map[string]any)
		}
		return bodyMap(t, rec)
	}

	// Defaults are what the tenant runs without a saved setting.
	for _, path := range []string{v1, sup, pgb} {
		if got := read(path); got["default_pool_size"] != float64(15) || got["max_client_conn"] != float64(1000) {
			t.Fatalf("%s defaults: %v", path, got)
		}
	}

	// The CLI's call: pool size and mode.
	rec := f.mustDo("PATCH", v1, map[string]any{"default_pool_size": 25, "pool_mode": "transaction"}, 200)
	validateAgainstSpec(t, "PATCH /v1/projects/{ref}/config/database/pooler", rec.Body.Bytes())
	if got := bodyMap(t, rec); got["default_pool_size"] != float64(25) || got["pool_mode"] != "transaction" {
		t.Fatalf("v1 PATCH response: %v", got)
	}
	if strings.Join(f.mgr.applied, ",") != testRef+" pooler" {
		t.Fatalf("the tenant was not told: %v", f.mgr.applied)
	}
	for _, path := range []string{v1, sup, pgb} {
		if got := read(path); got["default_pool_size"] != float64(25) || got["max_client_conn"] != float64(1000) {
			t.Fatalf("%s after the save: %v", path, got)
		}
	}

	// Studio's call: the pool size and what it was shown for ignore_startup_parameters. The
	// response is the dashboard's, and the limit of the client is saved too.
	shown := read(pgb)
	rec = f.mustDo("PATCH", pgb, map[string]any{"default_pool_size": 40, "max_client_conn": 300, "ignore_startup_parameters": shown["ignore_startup_parameters"]}, 200)
	validateAgainstSpec(t, "PATCH /platform/projects/{ref}/config/pgbouncer", rec.Body.Bytes())
	got := bodyMap(t, rec)
	if got["default_pool_size"] != float64(40) || got["max_client_conn"] != float64(300) || got["pgbouncer_enabled"] != true || got["pgbouncer_status"] != "ENABLED" {
		t.Fatalf("platform PATCH response: %v", got)
	}
	if got := read(v1); got["default_pool_size"] != float64(40) || got["max_client_conn"] != float64(300) {
		t.Fatalf("v1 after the dashboard save: %v", got)
	}
	pooler := bodyMap(t, f.mustDo("GET", v2, nil, 200))["data"].(map[string]any)["attributes"].(map[string]any)["pooler"].(map[string]any)
	if pooler["default_pool_size"] != float64(40) || pooler["max_client_conn"] != float64(300) || pooler["pool_mode"] != "transaction" {
		t.Fatalf("v2 config pooler: %v", pooler)
	}

	// Studio sends null for an emptied field and an empty ignore_startup_parameters when it
	// was never shown one: both return to the defaults without error.
	f.mustDo("PATCH", pgb, map[string]any{"default_pool_size": nil, "max_client_conn": nil, "ignore_startup_parameters": ""}, 200)
	if got := read(v1); got["default_pool_size"] != float64(15) || got["max_client_conn"] != float64(1000) {
		t.Fatalf("after a reset: %v", got)
	}
	// A save that changes nothing does not touch the tenant.
	n := len(f.mgr.applied)
	f.mustDo("PATCH", v1, map[string]any{"default_pool_size": 15}, 200)
	f.mustDo("PATCH", v1, map[string]any{"default_pool_size": 15}, 200)
	if len(f.mgr.applied) != n+1 {
		t.Fatalf("applies: %v", f.mgr.applied[n:])
	}
}

// What the shared Supavisor cannot do is refused with the usual error body, never accepted and
// dropped. Nothing is saved or applied by a refused request.
func TestPoolerConfigRefusesWhatSupavisorCannotHonor(t *testing.T) {
	f := newFixture(t)
	const v1 = "/v1/projects/" + testRef + "/config/database/pooler"
	const pgb = "/platform/projects/" + testRef + "/config/pgbouncer"
	for _, tc := range []struct {
		path string
		body map[string]any
		msg  string
	}{
		{v1, map[string]any{"pool_mode": "session"}, "pool_mode"},
		{v1, map[string]any{"pool_mode": "session", "default_pool_size": 30}, "pool_mode"},
		{pgb, map[string]any{"pool_mode": "statement"}, "pool_mode"},
		{pgb, map[string]any{"server_idle_timeout": 60}, "server_idle_timeout"},
		{pgb, map[string]any{"server_lifetime": 3600}, "server_lifetime"},
		{pgb, map[string]any{"query_wait_timeout": 30}, "query_wait_timeout"},
		{pgb, map[string]any{"reserve_pool_size": 3}, "reserve_pool_size"},
		{pgb, map[string]any{"ignore_startup_parameters": "options"}, "ignore_startup_parameters"},
		{pgb, map[string]any{"pgbouncer_enabled": false}, "pgbouncer_enabled"},
		// out of range for the setting itself
		{v1, map[string]any{"default_pool_size": 0}, "between"},
		{v1, map[string]any{"default_pool_size": 3001}, "between 1 and 3000"},
		{pgb, map[string]any{"default_pool_size": 4951}, "between"},
		{pgb, map[string]any{"max_client_conn": 0}, "between"},
		{pgb, map[string]any{"max_client_conn": 54001}, "between"},
		{pgb, map[string]any{"default_pool_size": "many"}, "number"},
	} {
		rec := f.mustDo("PATCH", tc.path, tc.body, 400)
		msg, _ := bodyMap(t, rec)["message"].(string)
		if !strings.Contains(msg, tc.msg) {
			t.Errorf("PATCH %s %v: message %q does not name %q", tc.path, tc.body, msg, tc.msg)
		}
	}
	if len(f.mgr.applied) != 0 {
		t.Fatalf("a refused request reached the tenant: %v", f.mgr.applied)
	}
	// The same values that were echoed back are fine, and the pool size of a mixed body is
	// not saved when another field of it is refused.
	got := bodyMap(t, f.mustDo("GET", pgb, nil, 200))
	if got["default_pool_size"] != float64(15) {
		t.Fatalf("a refused request saved something: %v", got)
	}
	f.mustDo("PATCH", v1, map[string]any{"pool_mode": "transaction", "default_pool_size": nil}, 200)
	f.mustDo("PATCH", pgb, map[string]any{"pgbouncer_enabled": true, "server_idle_timeout": nil, "pool_mode": "transaction"}, 200)
}

// A failed apply puts the previous settings back, so the saved value and the running tenant agree.
func TestPoolerConfigRollsBackWhenTheTenantRefuses(t *testing.T) {
	f := newFixture(t)
	const v1 = "/v1/projects/" + testRef + "/config/database/pooler"
	f.mustDo("PATCH", v1, map[string]any{"default_pool_size": 20}, 200)
	f.mgr.applyHook = func(svc projectconfig.Service) error {
		if svc == projectconfig.Pooler && len(f.mgr.applied) == 2 {
			return errPoolerDown
		}
		return nil
	}
	rec := f.do("PATCH", v1, map[string]any{"default_pool_size": 99})
	if rec.Code < 500 {
		t.Fatalf("a refused apply answered %d %s", rec.Code, rec.Body)
	}
	got := bodyMap(t, f.mustDo("GET", "/platform/projects/"+testRef+"/config/pgbouncer", nil, 200))
	if got["default_pool_size"] != float64(20) {
		t.Fatalf("the pool size after a failed apply: %v", got["default_pool_size"])
	}
}

// A paused project takes the settings and applies them when it resumes; a project that is
// still being created refuses them.
func TestPoolerConfigOnAProjectThatIsNotRunning(t *testing.T) {
	f := newFixture(t)
	const v1 = "/v1/projects/" + testRef + "/config/database/pooler"
	f.project.Status = registry.StatusComingUp
	if err := f.reg.UpdateProject(t.Context(), f.project); err != nil {
		t.Fatal(err)
	}
	f.mustDo("PATCH", v1, map[string]any{"default_pool_size": 30}, 503)
	f.project.Status = registry.StatusInactive
	if err := f.reg.UpdateProject(t.Context(), f.project); err != nil {
		t.Fatal(err)
	}
	f.mustDo("PATCH", v1, map[string]any{"default_pool_size": 30}, 200)
	f.mustDo("PATCH", "/v1/projects/zzzzzzzzzzzzzzzzzzzz/config/database/pooler", map[string]any{"default_pool_size": 30}, 404)
}
