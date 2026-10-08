package api

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/registry"
)

const connHeader = "X-Connection-Encrypted"

// connString is a string of the shape GET databases lists, for the database called identifier.
func connString(user, identifier string) string {
	return "postgresql://" + user + ":[YOUR-PASSWORD]@db." + identifier + ".api.example.test:5432/postgres"
}

// dsnPort is the port of the connection string pg-meta was given for the last request.
func dsnPort(t *testing.T, f *fixture) (port, dsn string) {
	t.Helper()
	dsn = f.meta.last().DSN
	i := strings.Index(dsn, "@127.0.0.1:")
	if i < 0 {
		t.Fatalf("dsn = %q", dsn)
	}
	rest := dsn[i+len("@127.0.0.1:"):]
	return rest[:strings.IndexAny(rest, "/?")], dsn
}

// x-connection-encrypted chooses between the project's databases and carries nothing else: the
// connection is built from the project's own credentials, at the replica's port for a replica.
func TestPGMetaHeaderSelectsTheDatabase(t *testing.T) {
	rf := newReplicaFixture(t)
	id := rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	other := rf.addHealthyReplica(t, rf.addNode(t, "us", "us-east-1", "", registry.NodeActive), "us-east-1", "uvwxyz")
	seq := rf.projectRow(t).Seq
	replicaPort := strconv.Itoa(rf.cfg.ReplicaPorts(testRef, seq).Postgres)
	const tables = "/platform/pg-meta/" + testRef + "/tables"

	call := func(header string) (port, dsn string) {
		t.Helper()
		var hdr []string
		if header != "" {
			hdr = []string{connHeader, header}
		}
		if rec := rf.do("GET", tables, nil, hdr...); rec.Code != 200 {
			t.Fatalf("header %q: %d %s", header, rec.Code, rec.Body)
		}
		return dsnPort(t, rf.fixture)
	}

	// The fake manager's string for the primary is on port 1; a replica's is at its replica port.
	for _, h := range []string{"", connString("postgres", testRef), connString("postgres", testRef), "garbage", "postgresql://u:p@example.org:5432/postgres", "postgresql://u:[x]@db.nowhere.example.org:5432/postgres"} {
		if port, dsn := call(h); port != "1" || !strings.HasPrefix(dsn, "postgres://postgres:pw@") {
			t.Errorf("header %q: dsn %s", h, dsn)
		}
	}
	port, dsn := call(connString("postgres", id))
	if port != replicaPort || !strings.HasPrefix(dsn, "postgres://postgres:pw@127.0.0.1:") {
		t.Errorf("replica: port %s (want %s), dsn %s", port, replicaPort, dsn)
	}
	// Nothing the header carries reaches the connection: its user and password are not used.
	port, dsn = call("postgresql://attacker:hunter2@db." + id + ".api.example.test:5432/postgres?options=-c%20role=supabase_admin")
	if port != replicaPort || strings.Contains(dsn, "attacker") || strings.Contains(dsn, "hunter2") || strings.Contains(dsn, "supabase_admin") {
		t.Errorf("header credentials: %s", dsn)
	}
	// Case in the host does not matter to a name resolver, nor to the selection.
	if port, _ := call("postgresql://postgres:[YOUR-PASSWORD]@DB." + strings.ToUpper(id) + ".API.EXAMPLE.TEST:5432/postgres"); port != replicaPort {
		t.Errorf("upper case host: port %s", port)
	}

	// The read-only string chooses the database like the read-write one and nothing more: the role
	// stays the caller's on a replica as on the primary, so Studio's reports read pg_stat_statements
	// as postgres does and no role has to reach the replica with the WAL first.
	port, dsn = call(connString(readOnlyUser, id))
	if port != replicaPort || !strings.HasPrefix(dsn, "postgres://postgres:pw@") {
		t.Errorf("read-only string of a replica: port %s dsn %s", port, dsn)
	}
	if port, dsn := call(connString(readOnlyUser, testRef)); port != "1" || !strings.HasPrefix(dsn, "postgres://postgres:pw@") {
		t.Errorf("read-only string of the primary: port %s dsn %s", port, dsn)
	}

	// The requests were authorized by {ref}: a database that is not the project's is refused.
	for _, h := range []string{
		connString("postgres", "bcdefghijklmnopqrstu"),                                  // another project
		connString("postgres", "bcdefghijklmnopqrstu-rr-eu-west-1-abcdef"),              // another project's replica
		connString("postgres", testRef+"-rr-eu-west-1-zzzzzz"),                          // not a replica
		connString("postgres", "system"),                                                // the system project
		"postgresql://postgres:[YOUR-PASSWORD]@db.x.y.z.api.example.test:5432/postgres", // not an identifier
	} {
		rec := rf.do("GET", tables, nil, connHeader, h)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "does not belong to this project") {
			t.Errorf("header %q: %d %s", h, rec.Code, rec.Body)
		}
	}
	// The identifier of a replica of this project on another node is fine for this project's caller.
	if port, _ := call(connString("postgres", other)); port != replicaPort {
		t.Errorf("second replica: port %s", port)
	}
}

// A replica that is not up yet cannot be queried; the answer says why.
func TestPGMetaHeaderOfAReplicaThatIsNotReady(t *testing.T) {
	rf := newReplicaFixture(t)
	id := rf.addReplica(t, euNode, "eu-west-1", "abcdef", "INIT_READ_REPLICA", "4_downloaded_base_backup", "")
	before := len(rf.meta.Requests)
	rec := rf.do("GET", "/platform/pg-meta/"+testRef+"/tables", nil, connHeader, connString("postgres", id))
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "is not ready (status INIT_READ_REPLICA)") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(rf.meta.Requests) != before {
		t.Fatal("pg-meta was called")
	}
	// An unhealthy replica still answers.
	if err := rf.reg.SetReplicaStatus(t.Context(), id, "ACTIVE_UNHEALTHY", "6_completed_read_replica_setup", ""); err != nil {
		t.Fatal(err)
	}
	if rec := rf.do("GET", "/platform/pg-meta/"+testRef+"/tables", nil, connHeader, connString("postgres", id)); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// A caller who may not write SQL runs as the read-only role on a replica too, whichever string
// Studio sent.
func TestPGMetaReplicaSelectionKeepsTheCallersRole(t *testing.T) {
	rf := newRolesFixture(t)
	f := rf.fixture
	f.addNode(t, "eu", "eu-west-1", "", registry.NodeActive)
	f.srv.replicas = &fakeReplicas{reg: f.reg}
	id := f.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	replicaPort := strconv.Itoa(f.cfg.ReplicaPorts(testRef, f.projectRow(t).Seq).Postgres)
	for _, user := range []string{"postgres", readOnlyUser} {
		rec := rf.doAs(rf.tokens["ro"], "GET", "/platform/pg-meta/"+testRef+"/tables", nil, connHeader, connString(user, id))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", user, rec.Code, rec.Body)
		}
		if port, dsn := dsnPort(t, f); port != replicaPort || !strings.HasPrefix(dsn, "postgres://supavise_read_only:") {
			t.Fatalf("%s: dsn %s", user, dsn)
		}
	}
}

// The host of the header is read from its authority alone: an "@" in the query or the path does
// not move it.
func TestConnHeaderHost(t *testing.T) {
	for _, c := range []struct {
		in, host string
		ok       bool
	}{
		{"postgresql://postgres:[YOUR-PASSWORD]@db.abc.api.example.test:5432/postgres", "db.abc.api.example.test", true},
		{"postgres://u@db.abc.api.example.test/postgres", "db.abc.api.example.test", true},
		{"postgresql://u:p@db.abc.api.example.test:5432/postgres?options=a@b", "db.abc.api.example.test", true},
		{"postgresql://u:p@db.abc.api.example.test/postgres#x@y", "db.abc.api.example.test", true},
		{"postgresql://db.abc.api.example.test/postgres?options=a@b", "", false},
		{"postgresql://u@:5432/postgres", "", false},
		{"mysql://u@db.abc.api.example.test", "", false},
		{"", "", false},
	} {
		if host, ok := connHeaderHost(c.in); host != c.host || ok != c.ok {
			t.Errorf("connHeaderHost(%q) = %q, %v; want %q, %v", c.in, host, ok, c.host, c.ok)
		}
	}
}

// fakeResolver answers ReplicasOf from a canned list, so a test shows which code asks the placement
// Resolver and not the registry.
type fakeResolver struct {
	placement.Resolver
	byRef map[string][]registry.Replica
	asked []string
}

func (r *fakeResolver) ReplicasOf(_ context.Context, ref string) ([]registry.Replica, error) {
	r.asked = append(r.asked, ref)
	return r.byRef[ref], nil
}

// The replicas a project has come from the placement Resolver: the pg-meta selector and the cap on
// adding one ask it.
func TestReplicasComeFromThePlacementResolver(t *testing.T) {
	rf := newReplicaFixture(t)
	known := registry.ReplicaIdentifier(testRef, "eu-west-1", "abcdef")
	res := &fakeResolver{byRef: map[string][]registry.Replica{testRef: {{Identifier: known, Ref: testRef, NodeID: euNode, Status: "ACTIVE_HEALTHY"}}}}
	rf.srv.placement = res
	// The registry has no such row, the Resolver does.
	if rs, _ := rf.reg.ListReplicas(t.Context(), testRef); len(rs) != 0 {
		t.Fatalf("registry rows: %v", rs)
	}

	if rec := rf.do("GET", "/platform/pg-meta/"+testRef+"/tables", nil, connHeader, connString("postgres", known)); rec.Code != 200 {
		t.Fatalf("pg-meta: %d %s", rec.Code, rec.Body)
	}
	if port, _ := dsnPort(t, rf.fixture); port != strconv.Itoa(rf.cfg.ReplicaPorts(testRef, rf.projectRow(t).Seq).Postgres) {
		t.Fatalf("port %s", port)
	}
	if rec := rf.do("GET", "/platform/pg-meta/"+testRef+"/tables", nil, connHeader, connString("postgres", registry.ReplicaIdentifier(testRef, "eu-west-1", "zzzzzz"))); rec.Code != 403 {
		t.Fatalf("an identifier the Resolver does not know: %d %s", rec.Code, rec.Body)
	}

	res.byRef[testRef] = make([]registry.Replica, 4) // a Small project's cap
	if rec := rf.do("POST", setupPath, map[string]any{"read_replica_region": "eu-west-1"}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "maximum of 4") {
		t.Fatalf("cap: %d %s", rec.Code, rec.Body)
	}
	if len(res.asked) == 0 {
		t.Fatal("the Resolver was never asked")
	}
}

// replicaReads records the reads of the replicas table that go to the registry.
type replicaReads struct {
	registry.Registry
	refs []string
}

func (r *replicaReads) ListReplicas(ctx context.Context, ref string) ([]registry.Replica, error) {
	r.refs = append(r.refs, ref)
	return r.Registry.ListReplicas(ctx, ref)
}

// The organization's project list reads the replicas of its whole page in one query, and a node
// without a replica controller reads none.
func TestOrganizationProjectsReadReplicasOnce(t *testing.T) {
	rf := newReplicaFixture(t)
	rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	rf.mgr.addProject(t, "bcdefghijklmnopqrstu", "Second project", rf.org.ID, registry.StatusActiveHealthy)
	reads := &replicaReads{Registry: rf.reg}
	rf.srv.reg = reads
	res := &fakeResolver{}
	rf.srv.placement = res

	rec := rf.do("GET", "/platform/organizations/default/projects", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "READ_REPLICA") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(reads.refs) != 1 || reads.refs[0] != "" || len(res.asked) != 0 {
		t.Fatalf("replica reads %q, Resolver asked %q", reads.refs, res.asked)
	}

	reads.refs = nil
	rf.srv.replicas = nil
	rec = rf.do("GET", "/platform/organizations/default/projects", nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "READ_REPLICA") || len(reads.refs) != 0 || len(res.asked) != 0 {
		t.Fatalf("without a controller: %d, replica reads %q, Resolver asked %q: %s", rec.Code, reads.refs, res.asked, rec.Body)
	}
}
