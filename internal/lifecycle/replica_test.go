package lifecycle

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover/fenced"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/units"
)

const testRef = "abcdefghijklmnopqrst"

func testReplicaID() string { return registry.ReplicaIdentifier(testRef, "us-east-1", "abc123") }

// replicaSup is a Supervisor that remembers which units run and what was done to them.
type replicaSup struct {
	mu      sync.Mutex
	log     []string
	running map[string]bool
}

func newReplicaSup() *replicaSup { return &replicaSup{running: map[string]bool{}} }

func (s *replicaSup) rec(op, unit string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, op+" "+unit)
}
func (s *replicaSup) Render(_ context.Context, sp units.Spec) error {
	s.rec("render", sp.Unit())
	return nil
}
func (s *replicaSup) Start(_ context.Context, u string) error {
	s.rec("start", u)
	s.mu.Lock()
	s.running[u] = true
	s.mu.Unlock()
	return nil
}
func (s *replicaSup) Stop(_ context.Context, u string) error {
	s.rec("stop", u)
	s.mu.Lock()
	s.running[u] = false
	s.mu.Unlock()
	return nil
}
func (s *replicaSup) Remove(_ context.Context, u string) error {
	s.rec("remove", u)
	s.mu.Lock()
	s.running[u] = false
	s.mu.Unlock()
	return nil
}
func (s *replicaSup) Status(_ context.Context, u string) (units.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[u] {
		return units.Status{Unit: u, State: units.StateActive, SubState: "running", MainPID: 4242, Since: time.Now().Add(-time.Hour)}, nil
	}
	return units.Status{Unit: u, State: units.StateInactive, SubState: "dead"}, nil
}
func (s *replicaSup) ops() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.log, "\n")
}

// fakeSQL is a ClusterSQL over a table of clusters by port.
type fakeSQL struct {
	mu         sync.Mutex
	log        []string
	status     map[int]ClusterStatus // a port that is not here does not answer
	replayed   []bool                // answers of ReplayedTo, in order; the last one repeats
	onPromote  func(a ClusterAddr)
	promoteErr error
	retry      time.Duration
}

func (f *fakeSQL) rec(s string) { f.mu.Lock(); f.log = append(f.log, s); f.mu.Unlock() }
func (f *fakeSQL) calls() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.log, ",")
}
func (f *fakeSQL) Ping(_ context.Context, a ClusterAddr) error { return nil }
func (f *fakeSQL) Status(_ context.Context, a ClusterAddr) (ClusterStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.status[a.Port]; ok {
		return st, nil
	}
	return ClusterStatus{}, errors.New("connection refused")
}
func (f *fakeSQL) ReplayedTo(_ context.Context, a ClusterAddr, lsn string) (bool, error) {
	f.rec("replayed-to " + lsn)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.replayed) == 0 {
		return true, nil
	}
	ok := f.replayed[0]
	if len(f.replayed) > 1 {
		f.replayed = f.replayed[1:]
	}
	return ok, nil
}
func (f *fakeSQL) ReplayLSN(context.Context, ClusterAddr) (string, error) {
	f.rec("replay-lsn")
	return "0/3000000", nil
}
func (f *fakeSQL) RetryInterval(context.Context, ClusterAddr) (time.Duration, error) {
	return f.retry, nil
}
func (f *fakeSQL) Promote(_ context.Context, a ClusterAddr, _ time.Duration) error {
	f.rec("promote")
	if f.promoteErr != nil {
		return f.promoteErr
	}
	f.mu.Lock()
	st := f.status[a.Port]
	st.InRecovery = false
	f.status[a.Port] = st
	f.mu.Unlock()
	if f.onPromote != nil {
		f.onPromote(a)
	}
	return nil
}
func (f *fakeSQL) Checkpoint(context.Context, ClusterAddr) error { f.rec("checkpoint"); return nil }
func (f *fakeSQL) AlterSystem(_ context.Context, a ClusterAddr, name, value string) error {
	f.rec(fmt.Sprintf("alter-system %s=%q on %d", name, value, a.Port))
	return nil
}

// roundTripper answers every request with a status.
type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okHTTP() *http.Client {
	return &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	})}
}

type replicaFixture struct {
	pl  *PostgresPlane
	cfg *config.Config
	sup *replicaSup
	sql *fakeSQL
	t   ReplicaTarget
	rp  pgPaths
	cp  pgPaths
}

func newReplicaFixture(t *testing.T) *replicaFixture {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = shortTempDir(t)
	cfg.Domain = "example.test"
	cfg.BinPath = "/usr/local/bin/supavise"
	cfg.Backup.WALRelay = "on"
	sup := newReplicaSup()
	fsql := &fakeSQL{status: map[int]ClusterStatus{}, retry: time.Millisecond}
	pl := NewPostgresPlane(cfg, sup, fakeArts{}, registry.NewMemory(), PlaneOptions{
		ClusterSQL: fsql, HTTPClient: okHTTP(),
		RestoreCommandFor: func(ref string) string { return "'/usr/local/bin/supavise' wal fetch --ref " + ref + " %f %p" },
	})
	p := testProject(cfg, testRef, 7)
	f := &replicaFixture{pl: pl, cfg: cfg, sup: sup, sql: fsql, t: ReplicaTarget{Identifier: testReplicaID(), Project: p, Keys: goldenKeys()}}
	f.rp, f.cp = pl.replicaPaths(p), pl.paths(p)
	return f
}

// seeded makes the data directory look like a standby that was seeded.
func (f *replicaFixture) seeded(t *testing.T, signal bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.rp.Data, "global"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"PG_VERSION": "17\n", "postmaster.opts": "postgres\n",
		"postgresql.auto.conf": "# Do not edit this file manually!\nwork_mem = '8MB'\nprimary_conninfo = 'host=x'\nrestore_command = 'cp'\nrecovery_target_timeline = 'latest'\n"} {
		if err := os.WriteFile(filepath.Join(f.rp.Data, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if signal {
		if err := os.WriteFile(filepath.Join(f.rp.Data, "standby.signal"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *replicaFixture) autoConf(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.rp.Data, "postgresql.auto.conf"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReplicaPostgresSpec(t *testing.T) {
	f := newReplicaFixture(t)
	f.seeded(t, true)
	ctx := context.Background()
	primary, err := f.pl.postgresSpec(ctx, f.t.Project, f.t.Keys)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := f.pl.replicaPostgresSpec(ctx, f.t)
	if err != nil {
		t.Fatal(err)
	}
	// The replica port is the replica base plus three per sequence; the primary's is the project base's.
	if f.rp.Port != f.cfg.ReplicaBase()+3*7 || f.cp.Port != f.cfg.Ports.ProjectBase+3*7 {
		t.Fatalf("ports: replica %d, primary %d", f.rp.Port, f.cp.Port)
	}
	if spec.Unit() != primary.Unit() || spec.WorkDir != primary.WorkDir {
		t.Fatalf("a replica keeps the unit of a primary: %s vs %s", spec.Unit(), primary.Unit())
	}
	args := strings.Join(spec.Exec, " ")
	for _, want := range []string{
		"bin/supabase-postgres-start -p " + strconv.Itoa(f.rp.Port) + " ",
		"-c hot_standby=on", "-c hot_standby_feedback=on", "-c archive_mode=on", "-c archive_command=",
		"-c unix_socket_directories=" + f.rp.Sock, "-c wal_level=logical",
		"-c cron.use_background_workers=on", "-c cron.max_running_jobs=8", "-c max_connections=60", "-c max_wal_senders=5",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	// Every setting of the primary is on the standby's command line too (a standby needs at least the
	// primary's limits), and the only setting that differs is the port.
	for i, a := range primary.Exec {
		if i == 2 { // the port
			continue
		}
		found := false
		for _, b := range spec.Exec {
			if a == b {
				found = true
			}
		}
		if !found {
			t.Errorf("the primary's %q is not on the standby's command line", a)
		}
	}
	if _, ok := spec.Env["POSTGRES_PASSWORD"]; ok {
		t.Fatal("a standby's environment holds the bootstrap password")
	}
	if spec.Env["PGDATA"] != f.rp.Data {
		t.Fatalf("env = %v", spec.Env)
	}
	// A directory that is not seeded is refused instead of being initialized as a new cluster.
	if err := os.RemoveAll(f.rp.Data); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pl.replicaPostgresSpec(ctx, f.t); err == nil || !strings.Contains(err.Error(), "no seeded data directory") {
		t.Fatalf("an unseeded directory: %v", err)
	}
}

func TestReplicaRESTSpec(t *testing.T) {
	f := newReplicaFixture(t)
	f.pl.opts.Settings = &fakeSettings{rest: map[string]string{"PGRST_DB_SCHEMAS": "public,extra", "PGRST_DB_MAX_ROWS": "25"}}
	spec, err := f.pl.replicaRESTSpec(context.Background(), f.t)
	if err != nil {
		t.Fatal(err)
	}
	rp := f.cfg.ReplicaPorts(testRef, 7)
	k := f.t.Keys
	want := fmt.Sprintf("postgres://authenticator:%s@127.0.0.1:%d,127.0.0.1:%d/postgres?sslmode=disable&application_name=postgrest-replica&target_session_attrs=read-only",
		k.AuthenticatorPassword, rp.Postgres, f.cfg.PortsFor(testRef, 7).Postgres)
	env := spec.Env
	if env["PGRST_DB_URI"] != want {
		t.Fatalf("PGRST_DB_URI = %s\nwant %s", env["PGRST_DB_URI"], want)
	}
	if env["PGRST_SERVER_PORT"] != strconv.Itoa(rp.PostgREST) || rp.PostgREST != rp.Postgres+2 {
		t.Fatalf("server port %s, replica ports %+v", env["PGRST_SERVER_PORT"], rp)
	}
	if env["PGRST_OPENAPI_SERVER_PROXY_URI"] != "https://"+testReplicaID()+".api.example.test/rest/v1" {
		t.Fatalf("openapi = %s", env["PGRST_OPENAPI_SERVER_PROXY_URI"])
	}
	// The project's own secrets and saved settings: the same apikey works on every database.
	if env["PGRST_JWT_SECRET"] != k.JWTSecret || env["PGRST_APP_SETTINGS_JWT_SECRET"] != k.JWTSecret ||
		env["PGRST_DB_SCHEMAS"] != "public,extra" || env["PGRST_DB_MAX_ROWS"] != "25" || env["PGRST_DB_ANON_ROLE"] != "anon" {
		t.Fatalf("env = %v", env)
	}
	if spec.Unit() != "supavise-postgrest@"+testRef+".service" {
		t.Fatalf("unit = %s", spec.Unit())
	}
	// The URL must parse as libpq's multi-host form: two hosts, one database.
	if !strings.Contains(env["PGRST_DB_URI"], "@127.0.0.1:") || strings.Count(env["PGRST_DB_URI"], "127.0.0.1:") != 2 {
		t.Fatalf("hosts in %s", env["PGRST_DB_URI"])
	}
}

func TestReplicaTargetRefusals(t *testing.T) {
	f := newReplicaFixture(t)
	other := f.t
	other.Identifier = registry.ReplicaIdentifier("zzzzzzzzzzzzzzzzzzzz", "us-east-1", "abc123")
	if err := other.check(f.cfg); err == nil {
		t.Fatal("an identifier of another project was accepted")
	}
	bad := f.t
	bad.Identifier = "not-an-identifier"
	if err := bad.check(f.cfg); err == nil {
		t.Fatal("a malformed identifier was accepted")
	}
	high := f.t
	p := *f.t.Project
	p.Seq = f.cfg.MaxReplicaSeq() + 1
	high.Project = &p
	if err := high.check(f.cfg); !errors.Is(err, ErrReplicaSeq) {
		t.Fatalf("a sequence above MaxReplicaSeq: %v", err)
	}
	p.Seq = f.cfg.MaxReplicaSeq()
	if err := high.check(f.cfg); err != nil {
		t.Fatalf("the largest sequence: %v", err)
	}
	// The system cluster's standby has no sequence.
	sys := ReplicaTarget{Identifier: registry.ReplicaIdentifier("system", "us-east-1", "abc123"), Project: &registry.Project{Ref: config.SystemRef}, Keys: goldenKeys()}
	if err := sys.check(f.cfg); err != nil {
		t.Fatalf("system standby: %v", err)
	}
	if got := f.pl.replicaPaths(sys.Project).Port; got != f.cfg.ReplicaBase() {
		t.Fatalf("system standby port = %d, want the replica base", got)
	}
}

func TestCreateReplicaSeedsAndStarts(t *testing.T) {
	f := newReplicaFixture(t)
	var plans []ReplicaSeedPlan
	var stages []ReplicaStage
	seeder := func(_ context.Context, plan ReplicaSeedPlan) error {
		plans = append(plans, plan)
		if entries, _ := os.ReadDir(plan.DataDir); len(entries) != 0 {
			t.Errorf("the seeder got a directory that is not empty: %v", entries)
		}
		if err := os.MkdirAll(plan.DataDir, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(plan.DataDir, "PG_VERSION"), []byte("17\n"), 0o600)
	}
	err := f.pl.CreateReplica(context.Background(), f.t, ReplicaCreateOptions{Seeder: seeder, BackupID: "20261001T000000Z", Progress: func(s ReplicaStage) { stages = append(stages, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %+v", plans)
	}
	plan := plans[0]
	if plan.Ref != testRef || plan.Identifier != testReplicaID() || plan.DataDir != f.rp.Data || plan.BackupID != "20261001T000000Z" ||
		plan.PrimaryPort != f.cp.Port || plan.ReplicationPassword != f.t.Keys.ReplicationPassword {
		t.Fatalf("plan = %+v", plan)
	}
	if got := fmt.Sprint(stages); got != "[launched seeding seeded started]" {
		t.Fatalf("stages = %s", got)
	}
	// The launcher's invariants, pg_hba.conf and the root key are in place, the unit runs, GoTrue was never touched.
	if _, err := os.Stat(filepath.Join(f.rp.Data, "postmaster.opts")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.rp.HBA, f.rp.RootKey} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", path, fi, err)
		}
	}
	if _, err := os.Stat(f.cfg.Paths().ProjectService(testRef, config.SvcGoTrue)); err == nil {
		t.Fatal("a replica got a GoTrue directory")
	}
	ops := f.sup.ops()
	if !strings.Contains(ops, "start supavise-postgres@"+testRef+".service") || strings.Contains(ops, "gotrue") || strings.Contains(ops, "postgrest") {
		t.Fatalf("ops:\n%s", ops)
	}

	// A second create over the cluster is refused and leaves it alone.
	if err := f.pl.CreateReplica(context.Background(), f.t, ReplicaCreateOptions{Seeder: seeder}); !errors.Is(err, ErrClusterExists) {
		t.Fatalf("create over a cluster: %v", err)
	}
}

func TestCreateReplicaWithoutUpstreamAndFailure(t *testing.T) {
	f := newReplicaFixture(t)
	var got ReplicaSeedPlan
	ok := func(_ context.Context, plan ReplicaSeedPlan) error {
		got = plan
		_ = os.MkdirAll(plan.DataDir, 0o700)
		return os.WriteFile(filepath.Join(plan.DataDir, "PG_VERSION"), []byte("17\n"), 0o600)
	}
	if err := f.pl.CreateReplica(context.Background(), f.t, ReplicaCreateOptions{Seeder: ok, NoUpstream: true}); err != nil {
		t.Fatal(err)
	}
	if got.PrimaryPort != 0 || got.ReplicationPassword != "" {
		t.Fatalf("an archive-only standby got an upstream: %+v", got)
	}

	// A seeder that fails leaves no partial directory behind, so the call can be repeated.
	f2 := newReplicaFixture(t)
	boom := func(_ context.Context, plan ReplicaSeedPlan) error {
		_ = os.MkdirAll(plan.DataDir, 0o700)
		_ = os.WriteFile(filepath.Join(plan.DataDir, "half"), []byte("x"), 0o600)
		return errors.New("download: connection reset")
	}
	err := f2.pl.CreateReplica(context.Background(), f2.t, ReplicaCreateOptions{Seeder: boom})
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("seeder failure: %v", err)
	}
	if _, err := os.Stat(f2.rp.Data); !os.IsNotExist(err) {
		t.Fatalf("the partial data directory stayed: %v", err)
	}
	if strings.Contains(f2.sup.ops(), "start ") {
		t.Fatalf("a unit started after a failed seed:\n%s", f2.sup.ops())
	}
}

// fastReplicaPoll makes the waits of PromoteReplica look at the standby every millisecond for the
// length of the test.
func fastReplicaPoll(t *testing.T) {
	t.Helper()
	was := replicaPoll
	replicaPoll = time.Millisecond
	t.Cleanup(func() { replicaPoll = was })
}

func TestPromoteReplicaStateMachine(t *testing.T) {
	fastReplicaPoll(t)
	f := newReplicaFixture(t)
	f.seeded(t, true)
	f.sql.status[f.rp.Port] = ClusterStatus{InRecovery: true, ReceiverStatus: "streaming", ReceiveLSN: "0/3000100", ReplayLSN: "0/3000100"}
	f.sql.replayed = []bool{false, false, true}
	f.sup.running["supavise-postgres@"+testRef+".service"] = true
	f.sup.running["supavise-postgrest@"+testRef+".service"] = true

	err := f.pl.PromoteReplica(context.Background(), f.t, PromoteOptions{Epoch: 5, WaitLSN: "0/3000100", DrainArchive: true})
	if err != nil {
		t.Fatal(err)
	}
	// promote.ok holds the epoch, written before the promotion.
	b, err := os.ReadFile(f.cfg.Paths().PromoteOK(testRef))
	if err != nil || string(b) != "5\n" {
		t.Fatalf("promote.ok = %q, %v", b, err)
	}
	if fi, _ := os.Stat(f.cfg.Paths().PromoteOK(testRef)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("promote.ok mode %v", fi.Mode())
	}
	calls := f.sql.calls()
	if !strings.Contains(calls, "replayed-to 0/3000100,replayed-to 0/3000100,replayed-to 0/3000100") ||
		strings.Index(calls, "replay-lsn") > strings.Index(calls, "promote") || !strings.HasSuffix(calls, "promote,checkpoint") {
		t.Fatalf("sql calls = %s", calls)
	}
	// The recovery settings are gone, the rest of postgresql.auto.conf stays, standby.signal is gone.
	conf := f.autoConf(t)
	if strings.Contains(conf, "primary_conninfo") || strings.Contains(conf, "restore_command") || strings.Contains(conf, "recovery_target") || !strings.Contains(conf, "work_mem = '8MB'") {
		t.Fatalf("auto.conf:\n%s", conf)
	}
	if fileExists(filepath.Join(f.rp.Data, "standby.signal")) {
		t.Fatal("standby.signal is still there")
	}
	// PostgREST and the cluster stopped, then the cluster started again from the primary's spec.
	ops := f.sup.ops()
	iStopRest := strings.Index(ops, "stop supavise-postgrest@"+testRef)
	iStopPG := strings.Index(ops, "stop supavise-postgres@"+testRef)
	iStart := strings.LastIndex(ops, "start supavise-postgres@"+testRef)
	if !(iStopRest >= 0 && iStopRest < iStopPG && iStopPG < iStart) {
		t.Fatalf("ops:\n%s", ops)
	}
	if strings.Contains(ops, "gotrue") {
		t.Fatalf("promotion touched GoTrue:\n%s", ops)
	}

	// A repeated request after the cluster runs as a primary on its canonical port changes nothing.
	f.sql.status[f.cp.Port] = ClusterStatus{}
	before := f.sup.ops()
	if err := f.pl.PromoteReplica(context.Background(), f.t, PromoteOptions{Epoch: 5}); err != nil {
		t.Fatal(err)
	}
	if f.sup.ops() != before {
		t.Fatalf("a repeated promotion touched the units:\n%s", f.sup.ops())
	}
}

// A fence record that appears while the promotion runs does not come back as ErrFenced: pg_promote
// ran, so the error is not the refusal that changed nothing.
func TestPromoteReplicaFencedAfterThePromotionIsNotARefusal(t *testing.T) {
	fastReplicaPoll(t)
	f := newReplicaFixture(t)
	f.seeded(t, true)
	f.sql.status[f.rp.Port] = ClusterStatus{InRecovery: true}
	f.sql.onPromote = func(ClusterAddr) {
		if err := fenced.WriteProject(f.cfg.Paths(), fenced.Record{Epoch: 9, Leader: "n2", Ref: testRef, Reason: "project failover of " + testRef}); err != nil {
			t.Error(err)
		}
	}
	err := f.pl.PromoteReplica(context.Background(), f.t, PromoteOptions{Epoch: 2})
	if err == nil || errors.Is(err, ErrFenced) || !strings.Contains(err.Error(), "promoted") {
		t.Fatalf("a promotion fenced after pg_promote = %v", err)
	}
	if !strings.Contains(f.sql.calls(), "promote") {
		t.Fatalf("sql calls = %s", f.sql.calls())
	}
}

func TestPromoteReplicaRefusalsAndRecovery(t *testing.T) {
	fastReplicaPoll(t)
	ctx := context.Background()

	// No epoch.
	f := newReplicaFixture(t)
	if err := f.pl.PromoteReplica(ctx, f.t, PromoteOptions{}); err == nil {
		t.Fatal("a promotion without an epoch")
	}
	// Nothing here: not a standby.
	if err := f.pl.PromoteReplica(ctx, f.t, PromoteOptions{Epoch: 2}); !errors.Is(err, ErrNotStandby) {
		t.Fatalf("no cluster: %v", err)
	}

	// Replay never reaches the position: the promotion is not made.
	f = newReplicaFixture(t)
	f.seeded(t, true)
	f.sql.status[f.rp.Port] = ClusterStatus{InRecovery: true}
	f.sql.replayed = []bool{false}
	err := f.pl.PromoteReplica(ctx, f.t, PromoteOptions{Epoch: 2, WaitLSN: "0/9000000", Timeout: 20 * time.Millisecond})
	if !errors.Is(err, ErrReplayBehind) || strings.Contains(f.sql.calls(), "promote") {
		t.Fatalf("replay behind: %v (calls %s)", err, f.sql.calls())
	}
	if fileExists(f.cfg.Paths().PromoteOK(testRef)) {
		t.Fatal("promote.ok was written for a node that is still a standby behind the old primary")
	}

	// pg_promote fails: the error comes back and the cluster keeps running as a standby.
	f = newReplicaFixture(t)
	f.seeded(t, true)
	f.sql.status[f.rp.Port] = ClusterStatus{InRecovery: true}
	f.sql.promoteErr = errors.New("pg_promote: the promotion did not finish")
	if err := f.pl.PromoteReplica(ctx, f.t, PromoteOptions{Epoch: 2}); err == nil || strings.Contains(f.sup.ops(), "stop") {
		t.Fatalf("pg_promote failure: %v\n%s", err, f.sup.ops())
	}
	if fileExists(f.cfg.Paths().PromoteOK(testRef)) {
		t.Fatal("promote.ok stayed on a standby whose promotion did not start")
	}

	// A standby that is down is started to be promoted.
	f = newReplicaFixture(t)
	f.seeded(t, true)
	started := false
	// The cluster answers once its unit was started.
	f.pl.opts.ClusterSQL = &startedSQL{fakeSQL: f.sql, sup: f.sup, port: f.rp.Port, started: &started}
	if err := f.pl.PromoteReplica(ctx, f.t, PromoteOptions{Epoch: 3}); err != nil {
		t.Fatal(err)
	}
	if !started || !strings.Contains(f.sup.ops(), "render supavise-postgres@"+testRef) {
		t.Fatalf("a down standby was not started:\n%s", f.sup.ops())
	}

	// Promoted by an earlier try that died before the restart: no second pg_promote.
	f = newReplicaFixture(t)
	f.seeded(t, false)
	f.sql.status[f.rp.Port] = ClusterStatus{InRecovery: false}
	if err := f.pl.PromoteReplica(ctx, f.t, PromoteOptions{Epoch: 4}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.sql.calls(), "promote") || !strings.Contains(f.sup.ops(), "start supavise-postgres@"+testRef) {
		t.Fatalf("restart after an earlier promotion: calls %s\n%s", f.sql.calls(), f.sup.ops())
	}
}

// startedSQL answers on port only after the unit was started.
type startedSQL struct {
	*fakeSQL
	sup     *replicaSup
	port    int
	started *bool
}

func (s *startedSQL) Status(ctx context.Context, a ClusterAddr) (ClusterStatus, error) {
	if a.Port == s.port {
		s.sup.mu.Lock()
		up := s.sup.running["supavise-postgres@"+testRef+".service"]
		s.sup.mu.Unlock()
		if !up {
			return ClusterStatus{}, errors.New("connection refused")
		}
		*s.started = true
		s.fakeSQL.mu.Lock()
		if _, ok := s.fakeSQL.status[a.Port]; !ok {
			s.fakeSQL.status[a.Port] = ClusterStatus{InRecovery: true}
		}
		s.fakeSQL.mu.Unlock()
	}
	return s.fakeSQL.Status(ctx, a)
}

// writeControl writes a pg_control file with the layout Postgres 17 uses for its first fields.
func writeControl(t *testing.T, dataDir string, state uint32, lsn uint64, big bool) {
	t.Helper()
	b := make([]byte, 8192)
	put32 := func(off int, v uint32) {
		if big {
			b[off], b[off+1], b[off+2], b[off+3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
		} else {
			b[off], b[off+1], b[off+2], b[off+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
		}
	}
	put32(8, 1700)
	put32(12, 202406281)
	put32(16, state)
	if big {
		put32(32, uint32(lsn>>32))
		put32(36, uint32(lsn))
	} else {
		put32(32, uint32(lsn))
		put32(36, uint32(lsn>>32))
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "global"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "global", "pg_control"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadControl(t *testing.T) {
	dir := t.TempDir()
	writeControl(t, dir, 1, 0x0000000A_03000060, false)
	ci, err := ReadControl(dir)
	if err != nil || ci.State != "shut down" || ci.Checkpoint != "A/3000060" || !ci.ShutDown() {
		t.Fatalf("little endian: %+v %v", ci, err)
	}
	writeControl(t, dir, 6, 0x0000000000000028, true)
	if ci, err = ReadControl(dir); err != nil || ci.State != "in production" || ci.Checkpoint != "0/28" || ci.ShutDown() {
		t.Fatalf("big endian: %+v %v", ci, err)
	}
	writeControl(t, dir, 99, 1, false)
	if _, err = ReadControl(dir); err == nil {
		t.Fatal("an unknown state was accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "global", "pg_control"), make([]byte, 100), 0o600); err == nil {
		if _, err := ReadControl(dir); err == nil {
			t.Fatal("a zero file was accepted")
		}
	}
	if _, err := ReadControl(t.TempDir()); err == nil {
		t.Fatal("no file")
	}
}

func TestDemoteToReplica(t *testing.T) {
	ctx := context.Background()
	f := newReplicaFixture(t)
	// The old primary: cleanly shut down, its data in the same directory, promote.ok from its time.
	f.seeded(t, false)
	writeControl(t, f.cp.Data, 1, 0x3000060, false)
	if err := f.pl.writePromoteOK(testRef, 2); err != nil {
		t.Fatal(err)
	}
	gotrue := "supavise-gotrue@" + testRef + ".service"
	f.sup.running[gotrue] = true

	if err := f.pl.DemoteToReplica(ctx, f.t); err != nil {
		t.Fatal(err)
	}
	if fileExists(f.cfg.Paths().PromoteOK(testRef)) {
		t.Fatal("promote.ok survived the demotion")
	}
	if !fileExists(filepath.Join(f.rp.Data, "standby.signal")) {
		t.Fatal("no standby.signal")
	}
	conf := f.autoConf(t)
	for _, want := range []string{
		"primary_conninfo = 'host=127.0.0.1 port=" + strconv.Itoa(f.cp.Port) + " user=supabase_replication_admin password=''replication-password'' application_name=''" + testReplicaID() + "'' sslmode=disable'",
		"restore_command = '''/usr/local/bin/supavise'' wal fetch --ref " + testRef + " %f %p'",
		"recovery_target_timeline = 'latest'", "work_mem = '8MB'",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("auto.conf lacks %q:\n%s", want, conf)
		}
	}
	if strings.Count(conf, "primary_conninfo") != 1 || strings.Contains(conf, "host=x") {
		t.Fatalf("the old recovery settings stayed:\n%s", conf)
	}
	ops := f.sup.ops()
	if !strings.Contains(ops, "remove "+gotrue) || !strings.Contains(ops, "start supavise-postgres@"+testRef) || !strings.Contains(ops, "start supavise-postgrest@"+testRef) {
		t.Fatalf("ops:\n%s", ops)
	}
	// The settings reach the cluster as a standby on the replica port.
	spec, err := f.pl.replicaPostgresSpec(ctx, f.t)
	if err != nil || !strings.Contains(strings.Join(spec.Exec, " "), "-p "+strconv.Itoa(f.rp.Port)+" ") {
		t.Fatalf("spec after the demotion: %v", err)
	}

	// A second demotion finds the standby running and starts nothing again.
	f.sql.status[f.rp.Port] = ClusterStatus{InRecovery: true}
	n := strings.Count(f.sup.ops(), "stop ")
	if err := f.pl.DemoteToReplica(ctx, f.t); err != nil {
		t.Fatal(err)
	}
	if strings.Count(f.sup.ops(), "stop ") != n {
		t.Fatalf("a repeated demotion stopped units:\n%s", f.sup.ops())
	}
}

func TestDemoteToReplicaRefusals(t *testing.T) {
	ctx := context.Background()
	// A cluster that crashed cannot follow the new primary.
	f := newReplicaFixture(t)
	f.seeded(t, false)
	writeControl(t, f.cp.Data, 6, 0x3000060, false) // "in production": not shut down
	if err := f.pl.DemoteToReplica(ctx, f.t); !errors.Is(err, ErrNotCleanShutdown) {
		t.Fatalf("crashed cluster: %v", err)
	}
	if fileExists(filepath.Join(f.rp.Data, "standby.signal")) {
		t.Fatal("standby.signal written for a cluster that cannot follow")
	}
	// No restore_command configured.
	g := newReplicaFixture(t)
	g.pl.opts.RestoreCommandFor = nil
	if err := g.pl.DemoteToReplica(ctx, g.t); err == nil || !strings.Contains(err.Error(), "restore_command") {
		t.Fatalf("no restore command: %v", err)
	}
	// No control file at all.
	h := newReplicaFixture(t)
	if err := h.pl.DemoteToReplica(ctx, h.t); err == nil {
		t.Fatal("demotion of a directory with no cluster")
	}
}

func TestRewriteAutoConfKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	in := "# Do not edit this file manually!\n\nwork_mem = '8MB'\n  Primary_Conninfo='x'\n# primary_conninfo = 'commented'\nrecovery_target_time = '2026-01-01'\nshared_buffers = '1GB'\nrestore_command='a b'\n" +
		// The block the backup service's seeder appends to a standby.
		"# --- supavise standby id of ref ---\nrecovery_target_timeline = 'latest'\nhot_standby = on\n# --- supavise archive-only standby id of ref ---\n"
	if err := os.WriteFile(filepath.Join(dir, "postgresql.auto.conf"), []byte(in), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := rewriteAutoConf(dir, []string{"recovery_target_timeline = 'latest'"}, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	want := "# Do not edit this file manually!\n\nwork_mem = '8MB'\n# primary_conninfo = 'commented'\nshared_buffers = '1GB'\nrecovery_target_timeline = 'latest'\n"
	if string(b) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b, want)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "postgresql.auto.conf")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// A missing file is created.
	empty := t.TempDir()
	if err := rewriteAutoConf(empty, []string{"a = 1"}, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(empty, "postgresql.auto.conf")); string(b) != "a = 1\n" {
		t.Fatalf("created file = %q", b)
	}
}

// The seeder's block is one unit: its header, its recovery settings and, when asked, the archive settings
// that follow the header. An archive_command of the primary's own that stands elsewhere in the file stays
// in any case.
func TestClearStandbyBlockDropsTheArchiveLinesOfTheBlockOnRequest(t *testing.T) {
	in := "archive_mode = on\narchive_command = 'primary'\nwork_mem = '8MB'\n" +
		"\n# --- supavise standby id of ref (backup b1) ---\narchive_mode = on\narchive_command = 'seeded'\nrestore_command = 'r'\n" +
		"recovery_target_timeline = 'latest'\nhot_standby = on\nprimary_conninfo = 'c'\nmax_connections = '60'\n"
	for _, tc := range []struct {
		archive bool
		want    string
	}{
		{false, "archive_mode = on\narchive_command = 'primary'\nwork_mem = '8MB'\n\narchive_mode = on\narchive_command = 'seeded'\nmax_connections = '60'\n"},
		{true, "archive_mode = on\narchive_command = 'primary'\nwork_mem = '8MB'\n\nmax_connections = '60'\n"},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "postgresql.auto.conf"), []byte(in), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ClearStandbyBlock(dir, tc.archive); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf")); string(b) != tc.want {
			t.Errorf("archive=%v:\n%s\nwant:\n%s", tc.archive, b, tc.want)
		}
	}
}

func TestObserveReplica(t *testing.T) {
	f := newReplicaFixture(t)
	ctx := context.Background()
	// Not answering anywhere: absent.
	if obs := f.pl.ObserveReplica(ctx, f.t); obs.Role != ReplicaRoleAbsent || obs.PostgresUp {
		t.Fatalf("absent: %+v", obs)
	}
	// Streaming and caught up: lag 0 and PostgREST ready.
	f.sql.status[f.rp.Port] = ClusterStatus{InRecovery: true, ReceiverStatus: "streaming", ReceiveLSN: "0/5", ReplayLSN: "0/5"}
	obs := f.pl.ObserveReplica(ctx, f.t)
	if obs.Role != ReplicaRoleReplica || !obs.InRecovery || !obs.PostgRESTReady || obs.LagSeconds == nil || *obs.LagSeconds != 0 || obs.ReplayLSN != "0/5" {
		t.Fatalf("caught up: %+v", obs)
	}
	// Behind: the age of the last replayed commit.
	age := 12.5
	f.sql.status[f.rp.Port] = ClusterStatus{InRecovery: true, ReceiverStatus: "streaming", ReceiveLSN: "0/9", ReplayLSN: "0/5", ReplayAgeSeconds: &age}
	if obs = f.pl.ObserveReplica(ctx, f.t); obs.LagSeconds == nil || *obs.LagSeconds != 12.5 {
		t.Fatalf("behind: %+v", obs)
	}
	// No receiver and nothing replayed: unknown.
	f.sql.status[f.rp.Port] = ClusterStatus{InRecovery: true}
	if obs = f.pl.ObserveReplica(ctx, f.t); obs.LagSeconds != nil || obs.ReceiverStatus != "" {
		t.Fatalf("no receiver: %+v", obs)
	}
	// Promoted and restarted on the canonical port: a primary.
	delete(f.sql.status, f.rp.Port)
	f.sql.status[f.cp.Port] = ClusterStatus{}
	if obs = f.pl.ObserveReplica(ctx, f.t); obs.Role != ReplicaRolePrimary || obs.InRecovery || obs.PostgRESTReady {
		t.Fatalf("promoted: %+v", obs)
	}
	// The health of a replica checks the cluster and PostgREST only.
	f.sup.running["supavise-postgres@"+testRef+".service"] = true
	f.sup.running["supavise-postgrest@"+testRef+".service"] = true
	hs := f.pl.ReplicaHealth(ctx, f.t)
	if len(hs) != 2 || hs[0].Name != config.SvcPostgres || hs[1].Name != config.SvcPostgREST || !allHealthy(hs) {
		t.Fatalf("health = %+v", hs)
	}
}

func TestStopAndRemoveReplica(t *testing.T) {
	f := newReplicaFixture(t)
	f.seeded(t, true)
	if err := f.pl.StopReplica(context.Background(), testRef); err != nil {
		t.Fatal(err)
	}
	if ops := f.sup.ops(); ops != "stop supavise-postgrest@"+testRef+".service\nstop supavise-postgres@"+testRef+".service" {
		t.Fatalf("stop order:\n%s", ops)
	}
	if err := f.pl.RemoveReplica(context.Background(), testRef); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.cfg.Paths().Project(testRef)); !os.IsNotExist(err) {
		t.Fatalf("the project directory stayed: %v", err)
	}
}

func TestReloadSchemaSignalsOnlyARunningPostgREST(t *testing.T) {
	f := newReplicaFixture(t)
	unit := "supavise-postgrest@" + testRef + ".service"
	// Not running: nothing to signal, no error.
	if err := f.pl.ReloadSchema(context.Background(), testRef); err != nil {
		t.Fatal(err)
	}
	// Running: SIGUSR1 goes to a real child process that records it.
	if !signalsAvailable() {
		t.Skip("no sleep(1)")
	}
	pid, got, stop := startSignalProbe(t)
	defer stop()
	f.sup.running[unit] = true
	f.pl.sup = pidSup{replicaSup: f.sup, pid: pid}
	if err := f.pl.ReloadSchema(context.Background(), testRef); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the process did not get SIGUSR1")
	}
}

func TestRunSchemaReloadTicks(t *testing.T) {
	f := newReplicaFixture(t)
	var mu sync.Mutex
	var asked int
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.pl.RunSchemaReload(ctx, 5*time.Millisecond, func(context.Context) ([]string, error) {
			mu.Lock()
			asked++
			mu.Unlock()
			return []string{testRef}, nil
		})
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := asked
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reload timer did not tick")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func signalsAvailable() bool {
	_, err := exec.LookPath("sh")
	return err == nil
}

// startSignalProbe starts a shell that prints "got" and exits when it receives SIGUSR1.
func startSignalProbe(t *testing.T) (pid int, got <-chan struct{}, stop func()) {
	t.Helper()
	cmd := exec.Command("sh", "-c", `trap 'echo got; exit 0' USR1; echo ready; while :; do sleep 0.05; done`)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(out)
	if line, _ := r.ReadString('\n'); strings.TrimSpace(line) != "ready" {
		t.Fatalf("probe said %q", line)
	}
	ch := make(chan struct{})
	go func() {
		if line, _ := r.ReadString('\n'); strings.TrimSpace(line) == "got" {
			close(ch)
		}
	}()
	return cmd.Process.Pid, ch, func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
}

// pidSup is a replicaSup whose units report a given main process.
type pidSup struct {
	*replicaSup
	pid   int
	since time.Time // when the unit became active, if not an hour ago
}

func (p pidSup) Status(ctx context.Context, u string) (units.Status, error) {
	st, err := p.replicaSup.Status(ctx, u)
	st.MainPID = p.pid
	if !p.since.IsZero() {
		st.Since = p.since
	}
	return st, err
}

func TestSetWALKeepSizeAltersTheCanonicalCluster(t *testing.T) {
	f := newReplicaFixture(t)
	if err := f.pl.SetWALKeepSize(context.Background(), f.t.Project, "2GB"); err != nil {
		t.Fatal(err)
	}
	if err := f.pl.SetWALKeepSize(context.Background(), f.t.Project, ""); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("alter-system wal_keep_size=\"2GB\" on %d,alter-system wal_keep_size=\"\" on %d", f.cp.Port, f.cp.Port)
	if got := f.sql.calls(); got != want {
		t.Fatalf("calls = %s\nwant    %s", got, want)
	}
}

// TestReplicaUnitsGolden pins what a replica's Postgres and PostgREST units render to, with the
// primary's golden test (TestPrimaryUnitsMatchV011) beside it: a change to either shows in a diff
// that someone has to read. SUPAVISE_UPDATE_GOLDEN=1 rewrites testdata/replica.
func TestReplicaUnitsGolden(t *testing.T) {
	f := newReplicaFixture(t)
	f.seeded(t, true)
	f.t.Keys = goldenKeys()
	f.pl.opts.Settings = &fakeSettings{
		rest: map[string]string{"PGRST_DB_SCHEMAS": "public,extra"},
		pg:   []string{"max_connections=80", "statement_timeout=30s"},
	}
	ctx := context.Background()
	pg, err := f.pl.replicaPostgresSpec(ctx, f.t)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := f.pl.replicaRESTSpec(ctx, f.t)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, spec := range []units.Spec{pg, rest} {
		js, err := json.MarshalIndent(spec, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		env, err := units.FormatEnv(spec.Env)
		if err != nil {
			t.Fatal(err)
		}
		run, err := units.FormatRun(spec)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "### %s\n--- spec\n%s\n--- env\n%s--- run\n%s\n", spec.Unit(), js, env, run)
	}
	got := strings.ReplaceAll(b.String(), f.cfg.StateDir, "/STATE")
	path := filepath.Join("testdata", "replica", "units.golden")
	if os.Getenv("SUPAVISE_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("the replica's units changed:\n%s", firstDifference(string(want), got))
	}
}

// pg-meta and the parameterized-query path of the Management API connect to the replica's port on
// loopback with a password, as postgres and as supavise_read_only, as they do to the primary's: the
// standby's pg_hba.conf is the primary's, which takes a SCRAM login from any role on 127.0.0.1 and
// ::1 and trusts no TCP connection.
func TestReplicaHBATakesPasswordLoginsOnLoopback(t *testing.T) {
	f := newReplicaFixture(t)
	if err := f.pl.prepareAt(f.t.Project, f.t.Keys, f.rp, true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.rp.HBA)
	if err != nil {
		t.Fatal(err)
	}
	var rules []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			rules = append(rules, strings.Join(strings.Fields(line), " "))
		}
	}
	has := func(want string) bool {
		for _, r := range rules {
			if r == want {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"host all all 127.0.0.1/32 scram-sha-256", "host all all ::1/128 scram-sha-256"} {
		if !has(want) {
			t.Errorf("the replica's pg_hba.conf lacks %q:\n%s", want, b)
		}
	}
	for _, r := range rules {
		if strings.HasPrefix(r, "host") && strings.HasSuffix(r, " trust") {
			t.Errorf("the replica trusts a TCP connection: %q", r)
		}
	}
	if fi, err := os.Stat(f.rp.HBA); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("pg_hba.conf = %v, %v", fi, err)
	}
	// It is the file of the primary: one source for both.
	p, err := os.ReadFile(f.cp.HBA)
	if err != nil || string(p) != string(b) {
		t.Errorf("the primary's pg_hba.conf differs: %v", err)
	}
}

// A directory that carries the seeder's marker was cut off while it was filled: it holds a backup_label
// and no standby.signal, and a unit started on it would be a primary. Nothing starts one.
func TestNoUnitStartsOnADirectoryASeedLeftUnfinished(t *testing.T) {
	ctx := context.Background()
	f := newReplicaFixture(t)
	f.seeded(t, true)
	if err := os.WriteFile(filepath.Join(f.rp.Data, SeedMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"StartReplicaDatabase": func() error { return f.pl.StartReplicaDatabase(ctx, f.t) },
		"StartReplica":         func() error { return f.pl.StartReplica(ctx, f.t) },
		"DemoteToReplica":      func() error { return f.pl.DemoteToReplica(ctx, f.t) },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "did not finish") {
			t.Errorf("%s on an unfinished seed = %v", name, err)
		}
	}
	if ops := f.sup.ops(); ops != "" {
		t.Fatalf("the supervisor was asked: %s", ops)
	}
	if err := os.Remove(filepath.Join(f.rp.Data, SeedMarker)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pl.replicaPostgresSpec(ctx, f.t); err != nil {
		t.Fatalf("a finished seed: %v", err)
	}
}

// A promotion that died after it wrote promote.ok and before pg_promote ran leaves a standby with the
// file; the standby's next start removes it, because the relay would trust it for the epoch it names.
// A cluster that is already a primary keeps its file.
func TestAStandbyThatStartsDoesNotKeepPromoteOK(t *testing.T) {
	ctx := context.Background()
	f := newReplicaFixture(t)
	f.seeded(t, true)
	if err := f.pl.writePromoteOK(testRef, 4); err != nil {
		t.Fatal(err)
	}
	if err := f.pl.StartReplicaDatabase(ctx, f.t); err != nil {
		t.Fatal(err)
	}
	if fileExists(f.cfg.Paths().PromoteOK(testRef)) {
		t.Fatal("a standby started with a promote.ok beside it")
	}

	g := newReplicaFixture(t)
	g.seeded(t, false) // promoted: no standby.signal
	if err := g.pl.writePromoteOK(testRef, 4); err != nil {
		t.Fatal(err)
	}
	if err := g.pl.StartReplicaDatabase(ctx, g.t); err != nil {
		t.Fatal(err)
	}
	if !fileExists(g.cfg.Paths().PromoteOK(testRef)) {
		t.Fatal("the promote.ok of a promoted cluster went")
	}
}
