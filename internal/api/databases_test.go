package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	plat "github.com/supavise/supavise/internal/api/gen/platform"
	v1 "github.com/supavise/supavise/internal/api/gen/v1"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
)

const (
	dbsPath      = "/platform/projects/" + testRef + "/databases"
	statusesPath = "/platform/projects/" + testRef + "/databases-statuses"
	lbPath       = "/platform/projects/" + testRef + "/load-balancers"
)

func list(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not a list of objects: %v: %s", err, rec.Body)
	}
	return out
}

func idsOf(rows []map[string]any) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r["identifier"].(string))
	}
	return out
}

// With no replica the list is the primary alone, in the shape Studio needs of it.
func TestDatabasesOfAProjectWithoutReplicas(t *testing.T) {
	rf := newReplicaFixture(t)
	rf.run(t, []step{{key: "GET /platform/projects/{ref}/databases", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
		rows := list(t, rec)
		if len(rows) != 1 {
			t.Fatalf("rows = %v", rows)
		}
		want := map[string]any{
			"identifier": testRef, "status": "ACTIVE_HEALTHY", "size": "small", "cloud_provider": "AWS", "db_port": float64(5432),
			"db_name": "postgres", "db_user": "postgres", "db_host": "db." + testRef + ".api.example.test",
			"restUrl":          "https://" + testRef + ".api.example.test/rest/v1/",
			"connectionString": "postgresql://postgres:[YOUR-PASSWORD]@db." + testRef + ".api.example.test:5432/postgres",
		}
		for k, v := range want {
			if rows[0][k] != v {
				t.Errorf("%s = %v, want %v", k, rows[0][k], v)
			}
		}
		if ro, _ := rows[0]["connection_string_read_only"].(string); !strings.Contains(ro, "@db."+testRef+".api.") || strings.Contains(ro, ":pw@") {
			t.Errorf("connection_string_read_only = %q", ro)
		}
	}}})
}

func TestDatabasesListThePrimaryFirstThenEachReplica(t *testing.T) {
	rf := newReplicaFixture(t)
	us := rf.addNode(t, "us", "us-east-1", "us.pooler.example.test", "active")
	first := rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	second := rf.addReplica(t, us, "us-east-1", "uvwxyz", "INIT_READ_REPLICA", "4_downloaded_base_backup", "")

	rf.run(t, []step{{key: "GET /platform/projects/{ref}/databases", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
		rows := list(t, rec)
		if got := idsOf(rows); strings.Join(got, ",") != testRef+","+first+","+second {
			t.Fatalf("identifiers = %v", got)
		}
		r := rows[1]
		want := map[string]any{
			"status": "ACTIVE_HEALTHY", "region": "eu-west-1", "cloud_provider": "AWS", "size": "small", "db_name": "postgres",
			"db_user": "postgres", "db_port": float64(5432), "db_host": "db." + first + ".api.example.test",
			"restUrl":          "https://" + first + ".api.example.test/rest/v1/",
			"connectionString": "postgresql://postgres:[YOUR-PASSWORD]@db." + first + ".api.example.test:5432/postgres",
		}
		for k, v := range want {
			if r[k] != v {
				t.Errorf("replica %s = %v, want %v", k, r[k], v)
			}
		}
		// Studio's reports have no fallback to the primary's string for a replica.
		if ro, _ := r["connection_string_read_only"].(string); !strings.Contains(ro, "@db."+first+".api.") {
			t.Errorf("replica connection_string_read_only = %q", ro)
		}
		if rows[2]["status"] != "INIT_READ_REPLICA" || rows[2]["region"] != "us-east-1" {
			t.Errorf("second replica = %v", rows[2])
		}
		for _, row := range rows {
			if _, err := time.Parse(time.RFC3339, row["inserted_at"].(string)); err != nil {
				t.Errorf("inserted_at %q: %v", row["inserted_at"], err)
			}
		}
	}}})
}

// A project status the database schemas have no word for is reported by the closest one they have.
func TestDatabaseStatusOfAPausedProject(t *testing.T) {
	rf := newReplicaFixture(t)
	for status, want := range map[string]string{"INACTIVE": "UNKNOWN", "PAUSING": "GOING_DOWN", "UPGRADING": "RESTARTING", "RESIZING": "RESIZING", "RESTORE_FAILED": "UNKNOWN"} {
		p := rf.projectRow(t)
		p.Status = registry.Status(status)
		_ = rf.reg.UpdateProject(t.Context(), p)
		for _, path := range []string{dbsPath, statusesPath} {
			rec := rf.do("GET", path, nil)
			validateAgainstSpec(t, map[string]string{dbsPath: "GET /platform/projects/{ref}/databases", statusesPath: "GET /platform/projects/{ref}/databases-statuses"}[path], rec.Body.Bytes())
			if got := list(t, rec)[0]["status"]; got != want {
				t.Errorf("%s of a %s project: %v, want %s", path, status, got, want)
			}
		}
	}
}

// Studio refetches until databases-statuses has as many items as databases: the primary is one.
func TestDatabasesStatusesIncludeThePrimary(t *testing.T) {
	rf := newReplicaFixture(t)
	rf.run(t, []step{{key: "GET /platform/projects/{ref}/databases-statuses", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
		rows := list(t, rec)
		if len(rows) != 1 || rows[0]["identifier"] != testRef || rows[0]["status"] != "ACTIVE_HEALTHY" {
			t.Fatalf("rows = %v", rows)
		}
		if _, ok := rows[0]["replicaInitializationStatus"]; ok {
			t.Fatalf("the primary has no initialization status: %v", rows[0])
		}
	}}})

	us := rf.addNode(t, "us", "us-east-1", "", "active")
	ap := rf.addNode(t, "ap", "ap-south-1", "", "active")
	sa := rf.addNode(t, "sa", "sa-east-1", "", "active")
	up := rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	going := rf.addReplica(t, us, "us-east-1", "uvwxyz", "INIT_READ_REPLICA", "4_downloaded_base_backup", "")
	failed := rf.addReplica(t, ap, "ap-south-1", "qrstuv", "INIT_READ_REPLICA_FAILED", "3_initiated_read_replica_setup", "3_download_base_backup_failed")
	bare := rf.addReplica(t, sa, "sa-east-1", "mnopqr", "INIT_READ_REPLICA", "0_requested", "")
	rf.svc.unreported = map[string]bool{bare: true}
	rf.svc.statuses[going] = replicas.Status{Identifier: going, Status: "INIT_READ_REPLICA", Init: &replicas.InitStatus{
		Status: "in_progress", Progress: "4_downloaded_base_backup", BaseBackupDownloadEstimateSeconds: 120, WALArchiveReplayEstimateSeconds: 30}}
	rf.svc.statuses[failed] = replicas.Status{Identifier: failed, Status: "INIT_READ_REPLICA_FAILED", Init: &replicas.InitStatus{
		Status: "failed", Error: "3_download_base_backup_failed"}}
	rf.svc.statuses[up] = replicas.Status{Identifier: up, Status: "ACTIVE_HEALTHY", Init: &replicas.InitStatus{Status: "completed"}}

	dbs := list(t, rf.do("GET", dbsPath, nil))
	rf.run(t, []step{{key: "GET /platform/projects/{ref}/databases-statuses", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
		rows := list(t, rec)
		if len(rows) != len(dbs) {
			t.Fatalf("%d statuses for %d databases", len(rows), len(dbs))
		}
		for i, r := range rows {
			if r["identifier"] != dbs[i]["identifier"] {
				t.Fatalf("order differs at %d: %v vs %v", i, r["identifier"], dbs[i]["identifier"])
			}
		}
		init := func(i int) map[string]any { m, _ := rows[i]["replicaInitializationStatus"].(map[string]any); return m }
		if got := init(1); got["status"] != "completed" {
			t.Errorf("up: %v", got)
		}
		got := init(2)
		est, _ := got["estimations"].(map[string]any)
		if got["status"] != "in_progress" || got["progress"] != "4_downloaded_base_backup" || est["baseBackupDownloadEstimateSeconds"] != float64(120) || est["walArchiveReplayEstimateSeconds"] != float64(30) {
			t.Errorf("going: %v", got)
		}
		if got := init(3); got["status"] != "failed" || got["error"] != "3_download_base_backup_failed" {
			t.Errorf("failed: %v", got)
		}
		// The controller has not reported on the last one yet: the row's own step stands in.
		if got := init(4); got["status"] != "in_progress" || got["progress"] != "0_requested" {
			t.Errorf("bare: %v (%s)", got, bare)
		}
	}}})
}

func TestLoadBalancers(t *testing.T) {
	rf := newReplicaFixture(t)
	empty := func(label string) {
		t.Helper()
		rec := rf.do("GET", lbPath, nil)
		validateAgainstSpec(t, "GET /platform/projects/{ref}/load-balancers", rec.Body.Bytes())
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("%s: %d %s", label, rec.Code, rec.Body)
		}
	}
	empty("no replica")
	rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	empty("a replica, balancer not served")

	rf.srv.lbOn = true
	us := rf.addNode(t, "us", "us-east-1", "", "active")
	down := rf.addReplica(t, us, "us-east-1", "uvwxyz", "INIT_READ_REPLICA", "1_started", "")
	rf.run(t, []step{{key: "GET /platform/projects/{ref}/load-balancers", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
		var lbs []struct {
			Endpoint  string `json:"endpoint"`
			Databases []struct{ Identifier, Type, Status string }
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &lbs); err != nil || len(lbs) != 1 {
			t.Fatalf("balancers = %s", rec.Body)
		}
		lb := lbs[0]
		if lb.Endpoint != "https://"+testRef+"-lb.api.example.test" {
			t.Errorf("endpoint = %q", lb.Endpoint)
		}
		if len(lb.Databases) != 3 {
			t.Fatalf("databases = %+v", lb.Databases)
		}
		if d := lb.Databases[0]; d.Identifier != testRef || d.Type != "PRIMARY" || d.Status != "ACTIVE_HEALTHY" {
			t.Errorf("primary = %+v", d)
		}
		if d := lb.Databases[2]; d.Identifier != down || d.Type != "READ_REPLICA" || d.Status != "INIT_READ_REPLICA" {
			t.Errorf("replica = %+v", d)
		}
	}}})
}

// Both routes list a pooler entry for the primary and one for each replica.
func TestPoolerConfigHasAnEntryPerDatabase(t *testing.T) {
	rf := newReplicaFixture(t)
	first := rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	rf.addNode(t, "us", "us-east-1", "", "active") // a node with no public host falls back to the pooler host
	second := rf.addHealthyReplica(t, "n3", "us-east-1", "uvwxyz")

	for _, key := range []string{"GET /platform/projects/{ref}/config/supavisor", "GET /v1/projects/{ref}/config/database/pooler"} {
		rf.run(t, []step{{key: key, check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			rows := list(t, rec)
			if got := strings.Join(idsOf(rows), ","); got != testRef+","+first+","+second {
				t.Fatalf("identifiers = %s", got)
			}
			for i, kind := range []string{"PRIMARY", "READ_REPLICA", "READ_REPLICA"} {
				if rows[i]["database_type"] != kind || rows[i]["db_name"] != "postgres" || rows[i]["db_port"] != float64(6543) || rows[i]["pool_mode"] != "transaction" {
					t.Errorf("entry %d = %v", i, rows[i])
				}
				if rows[i]["default_pool_size"] != rows[0]["default_pool_size"] || rows[i]["max_client_conn"] != rows[0]["max_client_conn"] {
					t.Errorf("entry %d does not carry the project's pool settings: %v", i, rows[i])
				}
			}
			if rows[0]["db_host"] != "pooler.example.test" || rows[0]["db_user"] != "postgres."+testRef {
				t.Errorf("primary = %v", rows[0])
			}
			if rows[1]["db_host"] != "eu.pooler.example.test" || rows[1]["db_user"] != "postgres."+first ||
				rows[1]["connection_string"] != "postgres://postgres."+first+":[YOUR-PASSWORD]@eu.pooler.example.test:6543/postgres" || rows[1]["connectionString"] != rows[1]["connection_string"] {
				t.Errorf("first replica = %v", rows[1])
			}
			if rows[2]["db_host"] != "pooler.example.test" {
				t.Errorf("second replica = %v", rows[2])
			}
		}}})
	}
}

// The organization's project list names every database of a project, a replica as READ_REPLICA.
func TestOrganizationProjectsListReplicas(t *testing.T) {
	rf := newReplicaFixture(t)
	id := rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	rf.run(t, []step{{key: "GET /platform/organizations/{slug}/projects", path: "/platform/organizations/default/projects", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
		var out struct {
			Projects []struct {
				Ref       string
				Databases []map[string]any
			}
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		var dbs []map[string]any
		for _, p := range out.Projects {
			if p.Ref == testRef {
				dbs = p.Databases
			}
		}
		if len(dbs) != 2 || dbs[0]["type"] != "PRIMARY" || dbs[0]["identifier"] != testRef {
			t.Fatalf("databases = %v", dbs)
		}
		if dbs[1]["type"] != "READ_REPLICA" || dbs[1]["identifier"] != id || dbs[1]["region"] != "eu-west-1" || dbs[1]["status"] != "ACTIVE_HEALTHY" || dbs[1]["infra_compute_size"] != "small" {
			t.Fatalf("replica = %v", dbs[1])
		}
	}}})
}

// The bodies decode into the generated types with unknown fields refused, as a strict client
// (the Supabase CLI, Studio's generated client) decodes them.
func TestReplicaResponsesDecodeIntoTheGeneratedTypes(t *testing.T) {
	rf := newReplicaFixture(t)
	rf.srv.lbOn = true
	us := rf.addNode(t, "us", "us-east-1", "us.pooler.example.test", "active")
	up := rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	going := rf.addReplica(t, us, "us-east-1", "uvwxyz", "INIT_READ_REPLICA", "5_replayed_wal_archives", "")
	rf.svc.statuses[going] = replicas.Status{Identifier: going, Status: "INIT_READ_REPLICA", Init: &replicas.InitStatus{
		Status: "in_progress", Progress: "5_replayed_wal_archives", BaseBackupDownloadEstimateSeconds: 90}}
	strict := func(path string, into any) {
		t.Helper()
		rec := rf.do("GET", path, nil)
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
		dec := json.NewDecoder(rec.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(into); err != nil {
			t.Fatalf("GET %s: %v: %s", path, err, rec.Body)
		}
	}
	var dbs []plat.DatabaseDetailResponseOutput
	strict(dbsPath, &dbs)
	var sts []plat.DatabaseStatusResponseOutput
	strict(statusesPath, &sts)
	var lbs []plat.LoadBalancerDetailResponseOutput
	strict(lbPath, &lbs)
	var pool []v1.SupavisorConfigResponseOutput
	strict("/platform/projects/"+testRef+"/config/supavisor", &pool)
	var pool1 []v1.SupavisorConfigResponseOutput
	strict("/v1/projects/"+testRef+"/config/database/pooler", &pool1)
	var orgs plat.OrganizationProjectsResponseOutput
	strict("/platform/organizations/default/projects", &orgs)

	if len(dbs) != 3 || dbs[1].Identifier != up || string(dbs[2].Status) != "INIT_READ_REPLICA" || dbs[0].ConnectionStringReadOnly == nil {
		t.Errorf("databases = %+v", dbs)
	}
	if len(sts) != 3 || sts[2].ReplicaInitializationStatus == nil {
		t.Fatalf("statuses = %+v", sts)
	}
	in, err := sts[2].ReplicaInitializationStatus.AsDatabaseStatusResponseOutputReplicaInitializationStatus0()
	if err != nil || in.Status != "in_progress" || in.Progress == nil || string(*in.Progress) != "5_replayed_wal_archives" || in.Estimations == nil || in.Estimations.BaseBackupDownloadEstimateSeconds != 90 {
		t.Errorf("initialization status = %+v (%v)", in, err)
	}
	if len(lbs) != 1 || len(lbs[0].Databases) != 3 || len(pool) != 3 || len(pool1) != 3 {
		t.Errorf("balancers %d, pooler %d and %d", len(lbs), len(pool), len(pool1))
	}
}
