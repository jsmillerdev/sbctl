package api

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Studio decides whether to enable "Add read replica" in the browser, from the answers of six
// calls. This file ports that decision, and the helpers it reads, to Go and evaluates it against
// the handlers of this package, so a change to a handler that would grey the button out (or turn
// on one that the API then refuses) fails here.
//
// Ported from the pinned Studio, github.com/supabase/supabase at 94b8b06eb294cf6b217c68d30357566cc8f146d9
// (studio.tag 2026.10.05-sha-94b8b06 in internal/versions/versions.yaml), under apps/studio:
//
//	components/interfaces/Settings/Infrastructure/ReadReplicas/ReadReplicaForm/useCheckEligibilityDeployReplica.ts
//	data/read-replicas/replicas-query.ts          (READ_REPLICA_COMPUTE_CAPS, getMaxReplicas)
//	hooks/misc/useCheckEntitlements.ts            (hasAccess of an entitlement)
//
// The calls, as the hook makes them: GET /platform/projects/{ref} (cloud_provider,
// is_physical_backups_enabled, dbVersion, high_availability), GET /platform/organizations (plan,
// usage_billing_enabled), GET /platform/organizations/{slug}/entitlements, GET
// /platform/stripe/invoices/overdue (only off the higher plans), GET
// /platform/projects/{ref}/billing/addons (the compute_instance add-on) and GET
// /platform/projects/{ref}/databases. When Studio is bumped, diff those files against this port.

// studioCaps is READ_REPLICA_COMPUTE_CAPS; a size not listed has studioDefaultCap replicas.
var studioCaps = map[string]int{"ci_pico": 0, "ci_nano": 0, "ci_micro": 0, "ci_small": 4, "ci_medium": 4, "ci_large": 4}

const studioDefaultCap = 5 // READ_REPLICAS_MAX_COUNT

// studioMaxReplicas is getMaxReplicas: the cap of a compute add-on, "" being no add-on, which
// JavaScript looks up as the key "undefined".
func studioMaxReplicas(addon string) int {
	if addon == "" {
		addon = "undefined"
	}
	if n, ok := studioCaps[addon]; ok {
		return n
	}
	return studioDefaultCap
}

// studioPgMajor is the hook's parsedPgVersion: the major version after "supabase-postgres-", and
// ok false (undefined in Studio) for any other shape, a bare "17.11.0.004" included.
func studioPgMajor(dbVersion string) (major int, ok bool) {
	parts := strings.Split(dbVersion, "supabase-postgres-")
	if len(parts) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.SplitN(parts[1], ".", 2)[0])
	return n, err == nil
}

// studioEligibility is what useCheckEligibilityDeployReplica returns.
type studioEligibility struct {
	can                      bool
	hasOverdueInvoices       bool
	isAWSProvider            bool
	isAwsK8s                 bool
	isPgVersionBelow15       bool
	isBelowSmallCompute      bool
	isWalgNotEnabled         bool
	isProWithSpendCapEnabled bool
	isReachedMaxReplicas     bool
	isHighAvailability       bool
	maxNumberOfReplicas      int
}

// studioCanDeployReplica makes the hook's calls against the fixture's server and applies its rules.
func studioCanDeployReplica(t *testing.T, f *fixture) studioEligibility {
	t.Helper()
	get := func(path string) any {
		t.Helper()
		rec := f.do("GET", path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
		return decodeBody(t, rec)
	}
	str := func(v any) string { s, _ := v.(string); return s }
	obj := func(v any) map[string]any { m, _ := v.(map[string]any); return m }

	project := obj(get("/platform/projects/" + testRef))
	var org map[string]any
	for _, o := range get("/platform/organizations").([]any) {
		if obj(o)["id"] == project["organization_id"] {
			org = obj(o)
		}
	}
	if org == nil {
		t.Fatalf("no organization %v in the organization list", project["organization_id"])
	}
	slug := str(org["slug"])
	hasReadReplicaAccess := false
	for _, e := range obj(get("/platform/organizations/" + slug + "/entitlements"))["entitlements"].([]any) {
		if str(obj(obj(e)["feature"])["key"]) == "instances.read_replicas" {
			hasReadReplicaAccess, _ = obj(e)["hasAccess"].(bool)
		}
	}

	var e studioEligibility
	e.isAWSProvider = project["cloud_provider"] == "AWS"
	e.isAwsK8s = project["cloud_provider"] == "AWS_K8S"
	isWalgEnabled, _ := project["is_physical_backups_enabled"].(bool)
	e.isWalgNotEnabled = !isWalgEnabled
	plan := str(obj(org["plan"])["id"])
	isNotOnHigherPlan := !slices.Contains([]string{"team", "enterprise", "platform"}, plan)
	usageBilling, _ := org["usage_billing_enabled"].(bool)
	e.isProWithSpendCapEnabled = plan == "pro" && !usageBilling
	e.isHighAvailability, _ = project["high_availability"].(bool)
	if isNotOnHigherPlan {
		if overdue, ok := get("/platform/stripe/invoices/overdue").([]any); ok {
			for _, o := range overdue {
				if obj(o)["organization_id"] == project["organization_id"] {
					e.hasOverdueInvoices = true
				}
			}
		}
	}

	addon := ""
	for _, a := range obj(get("/platform/projects/" + testRef + "/billing/addons"))["selected_addons"].([]any) {
		if str(obj(a)["type"]) == "compute_instance" {
			addon = str(obj(obj(a)["variant"])["identifier"])
			break
		}
	}
	e.isBelowSmallCompute = addon == "" || studioCaps[addon] == 0 && hasKey(studioCaps, addon)
	e.maxNumberOfReplicas = studioMaxReplicas(addon)
	replicas := 0
	for _, d := range get("/platform/projects/" + testRef + "/databases").([]any) {
		if str(obj(d)["identifier"]) != testRef {
			replicas++
		}
	}
	e.isReachedMaxReplicas = replicas >= e.maxNumberOfReplicas
	major, known := studioPgMajor(str(project["dbVersion"]))
	e.isPgVersionBelow15 = !known || major < 15

	e.can = !e.isReachedMaxReplicas && known && major >= 15 && e.isAWSProvider && hasReadReplicaAccess && isWalgEnabled &&
		!e.hasOverdueInvoices && !e.isAwsK8s && !e.isProWithSpendCapEnabled && !e.isBelowSmallCompute && !e.isHighAvailability
	return e
}

func hasKey(m map[string]int, k string) bool { _, ok := m[k]; return ok }

// fillReplicas gives the fixture's project n healthy replicas, each on a node of its own.
func (f *fixture) fillReplicas(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		region := fmt.Sprintf("x%d", i)
		node := f.addNode(t, "fill-"+region, region, "", "active")
		f.addHealthyReplica(t, node, region, fmt.Sprintf("f%05d", i))
	}
}

func TestStudioEnablesAddReadReplicaOnlyWhereTheAPIAcceptsIt(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, rf *replicaFixture)
		// What the hook is expected to return for the case.
		can        bool
		belowSmall bool
		reachedMax bool
		pgBelow15  bool
		walgNotOn  bool
		max        int
		// The API's answer to POST read-replicas/setup: 204 where Studio enables the button; where
		// Studio disables it for a rule the API enforces too, a 400 containing refuse. The gates
		// only Studio has (apiNotChecked) are not asked.
		apiAccepts    bool
		refuse        string
		apiNotChecked bool
	}{
		{name: "small with backups", setup: func(*testing.T, *replicaFixture) {}, can: true, max: 4, apiAccepts: true},
		{name: "medium", setup: func(t *testing.T, rf *replicaFixture) { rf.setClass(t, "medium") }, can: true, max: 4, apiAccepts: true},
		{name: "xlarge", setup: func(t *testing.T, rf *replicaFixture) { rf.setClass(t, "xlarge") }, can: true, max: 5, apiAccepts: true},
		{name: "micro", setup: func(t *testing.T, rf *replicaFixture) { rf.setClass(t, "micro") }, belowSmall: true, reachedMax: true, max: 0, refuse: "compute size of small or larger"},
		// Micro has a cap of 0, so Studio also counts its (no) replicas as the cap. Nano has no compute
		// add-on at all, which Studio reads as below Small (its cap lookup is then 5).
		{name: "nano", setup: func(t *testing.T, rf *replicaFixture) { rf.setClass(t, "nano") }, belowSmall: true, max: 5, refuse: "compute size of small or larger"},
		{name: "small at its cap", setup: func(t *testing.T, rf *replicaFixture) { rf.fillReplicas(t, 4) }, reachedMax: true, max: 4, refuse: "maximum of 4 read replicas"},
		{name: "small below its cap", setup: func(t *testing.T, rf *replicaFixture) { rf.fillReplicas(t, 3) }, can: true, max: 4, apiAccepts: true},
		{name: "xlarge at its cap", setup: func(t *testing.T, rf *replicaFixture) { rf.setClass(t, "xlarge"); rf.fillReplicas(t, 5) }, reachedMax: true, max: 5, refuse: "maximum of 5 read replicas"},
		{name: "xlarge below its cap", setup: func(t *testing.T, rf *replicaFixture) { rf.setClass(t, "xlarge"); rf.fillReplicas(t, 4) }, can: true, max: 5, apiAccepts: true},
		// Studio alone gates on these two; the API has no opinion.
		{name: "no point-in-time recovery", setup: func(t *testing.T, rf *replicaFixture) { rf.srv.backups = nil }, walgNotOn: true, max: 4, apiNotChecked: true},
		{name: "Postgres 14", setup: func(t *testing.T, rf *replicaFixture) {
			p := rf.projectRow(t)
			p.Versions["postgres"] = "postgres-14.1.0.1-r0"
			_ = rf.reg.UpdateProject(t.Context(), p)
		}, pgBelow15: true, max: 4, apiNotChecked: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rf := newReplicaFixture(t)
			rf.addNode(t, "eu2", "eu-west-2", "", "active")
			tc.setup(t, rf)
			e := studioCanDeployReplica(t, rf.fixture)
			if e.can != tc.can || e.isBelowSmallCompute != tc.belowSmall || e.isReachedMaxReplicas != tc.reachedMax ||
				e.isPgVersionBelow15 != tc.pgBelow15 || e.isWalgNotEnabled != tc.walgNotOn || e.maxNumberOfReplicas != tc.max {
				t.Fatalf("eligibility = %+v, want can=%v belowSmall=%v reachedMax=%v pgBelow15=%v walgNotOn=%v max=%d",
					e, tc.can, tc.belowSmall, tc.reachedMax, tc.pgBelow15, tc.walgNotOn, tc.max)
			}
			// What Studio always sees on this node, whatever the case.
			if !e.isAWSProvider || e.isAwsK8s || e.isHighAvailability || e.hasOverdueInvoices || e.isProWithSpendCapEnabled {
				t.Fatalf("eligibility = %+v: a gate other than the case's is closed", e)
			}
			if tc.apiNotChecked {
				return
			}
			rec := rf.do("POST", setupPath, map[string]any{"read_replica_region": "eu-west-2"})
			switch {
			case tc.apiAccepts && rec.Code != 204:
				t.Fatalf("Studio enables the button and the API answers %d %s", rec.Code, rec.Body)
			case tc.refuse != "" && (rec.Code != 400 || !strings.Contains(rec.Body.String(), tc.refuse)):
				t.Fatalf("Studio disables the button, and the API answers %d %s, want 400 with %q", rec.Code, rec.Body, tc.refuse)
			}
		})
	}

	// The hook counts a bare version (what dbVersion was before) as below 15.
	if _, ok := studioPgMajor("17.11.0.004"); ok {
		t.Fatal("a bare version has a major version for Studio")
	}
	if major, ok := studioPgMajor("supabase-postgres-17.11.0.004"); !ok || major != 17 {
		t.Fatalf("hosted's shape: %d %v", major, ok)
	}
}
