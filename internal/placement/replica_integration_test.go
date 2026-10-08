package placement

// An integration test of the replica data plane on real Postgres clusters (the slim-services
// artifacts, exec backend), run by tests/linux/replica-blocks.sh. It needs SUPAVISE_TEST_UNPACKED to
// name a directory with unpacked postgres-17*, auth-* and postgrest-* artifacts for this platform.
// Ports come from 39000-39999.
//
// One machine plays two nodes. Node A is a full node (system cluster, Engine, backup service over a
// file store) with a project; node B has a state directory of its own, a plane over the same
// artifacts and the node agent. B's replica of the project is seeded into B's directory from A's
// base backup and streams from A's primary on its canonical port, which stands for the forwarder a
// real second node has there. The test then switches over: A's primary stops, B's standby is
// promoted onto the canonical port, and A's cluster is demoted into a standby of B.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pelletier/go-toml/v2"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/units"
)

type dirArts map[string]string

func (d dirArts) Dir(svc string) (string, error) {
	if dir, ok := d[svc]; ok {
		return dir, nil
	}
	return "", fmt.Errorf("no artifact for %s", svc)
}
func (d dirArts) Tag(svc string) (string, error) { return filepath.Base(d[svc]), nil }

func freePortBase(t *testing.T, n int) int {
	t.Helper()
	for base := 39000; base+n < 39900; base += n + 3 {
		var ls []net.Listener
		ok := true
		for i := 0; i < n; i++ {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				ok = false
				break
			}
			ls = append(ls, l)
		}
		for _, l := range ls {
			l.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatal("no free port range in 39000-39999")
	return 0
}

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// supaviseBinary builds cmd/supavise once: it is every cluster's archive_command and restore_command.
func supaviseBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "supavise-replica-bin-")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "supavise")
		cmd := exec.Command("go", "build", "-o", builtBin, "./cmd/supavise")
		_, thisFile, _, _ := runtime.Caller(0)
		cmd.Dir = filepath.Join(filepath.Dir(thisFile), "..", "..")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/supavise: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtBin
}

// dumpLogs prints the end of every unit log of a node's directory (the exec backend's).
func dumpLogs(t *testing.T, node, state string) {
	files, _ := filepath.Glob(filepath.Join(state, "logs", "*.log"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if len(b) > 6000 {
			b = b[len(b)-6000:]
		}
		t.Logf("--- %s: %s\n%s", node, filepath.Base(f), b)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if builtBin != "" {
		os.RemoveAll(filepath.Dir(builtBin))
	}
	os.Exit(code)
}

// writeConfig stores cfg as the config file the clusters' archive and restore commands read.
func writeConfig(t *testing.T, cfg *config.Config) string {
	t.Helper()
	b, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(cfg.StateDir, "config.toml")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// testSeeder is the standby builder the test uses: the backup service's SeedReplica, which the
// compiler checks the service implements (backup.ReplicaSeeder).
func testSeeder(svc *backup.Service) lifecycle.ReplicaSeeder { return SeederFrom(svc) }

// rrEnv is the two nodes of the test.
type rrEnv struct {
	t          *testing.T
	cfgA, cfgB *config.Config
	nodeA      *lifecycle.Node
	planeB     *lifecycle.PostgresPlane
	agentA     *NodeAgent
	agentB     *NodeAgent
	project    *registry.Project
	keys       *secrets.ProjectKeys
	svc        *backup.Service
}

func (e *rrEnv) ports() (primary, replica config.ProjectPorts) {
	return e.cfgA.PortsFor(e.project.Ref, e.project.Seq), e.cfgA.ReplicaPorts(e.project.Ref, e.project.Seq)
}

// sql connects to the cluster on port as the project's postgres role over TCP.
func (e *rrEnv) sql(port int) *pgx.Conn {
	e.t.Helper()
	dsn := fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable", e.keys.DBPassword, port)
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		e.t.Fatalf("connect to %d: %v", port, err)
	}
	e.t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func (e *rrEnv) exec(port int, sql string) {
	e.t.Helper()
	if _, err := e.sql(port).Exec(context.Background(), sql); err != nil {
		e.t.Fatalf("port %d: %s: %v", port, sql, err)
	}
}

func (e *rrEnv) count(port int, query string) int {
	e.t.Helper()
	var n int
	if err := e.sql(port).QueryRow(context.Background(), query).Scan(&n); err != nil {
		e.t.Fatalf("port %d: %s: %v", port, query, err)
	}
	return n
}

func (e *rrEnv) text(port int, query string) string {
	e.t.Helper()
	var s string
	if err := e.sql(port).QueryRow(context.Background(), query).Scan(&s); err != nil {
		e.t.Fatalf("port %d: %s: %v", port, query, err)
	}
	return s
}

// admin connects to the primary of node A as supabase_admin, whom pg_hba trusts on the cluster's socket.
func (e *rrEnv) admin() *pgx.Conn {
	e.t.Helper()
	prim, _ := e.ports()
	sock := filepath.Join(e.cfgA.Paths().ProjectService(e.project.Ref, config.SvcPostgres), "sock")
	c, err := pgx.Connect(context.Background(), fmt.Sprintf("host=%s port=%d user=supabase_admin dbname=postgres sslmode=disable", sock, prim.Postgres))
	if err != nil {
		e.t.Fatalf("connect as supabase_admin: %v", err)
	}
	e.t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// pgrstTriggers enables or disables the event triggers that NOTIFY PostgREST of a DDL change and
// returns how many there are.
func (e *rrEnv) pgrstTriggers(enable bool) int {
	e.t.Helper()
	verb := "disable"
	if enable {
		verb = "enable"
	}
	c := e.admin()
	var n int
	if err := c.QueryRow(context.Background(), `select count(*) from pg_event_trigger where evtname like 'pgrst%'`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	if _, err := c.Exec(context.Background(), `do $$ declare t name; begin
		for t in select evtname from pg_event_trigger where evtname like 'pgrst%' loop
			execute format('alter event trigger %I `+verb+`', t);
		end loop; end $$`); err != nil {
		e.t.Fatalf("%s the event triggers: %v", verb, err)
	}
	return n
}

// eventually polls ok until it returns "" or the time is up; the last message is the failure.
func eventually(t *testing.T, within time.Duration, what string, ok func() string) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for {
		if last = ok(); last == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s: %s", what, within, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func httpStatus(url, bearer, method, body string) (int, string) {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("apikey", bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(b)
}

func newReplicaEnv(t *testing.T) *rrEnv {
	t.Helper()
	root := os.Getenv("SUPAVISE_TEST_UNPACKED")
	if root == "" || testing.Short() {
		t.Skip("SUPAVISE_TEST_UNPACKED not set")
	}
	arts := dirArts{}
	for svc, glob := range map[string]string{config.SvcPostgres: "postgres-17*", config.SvcGoTrue: "auth-*", config.SvcPostgREST: "postgrest-*"} {
		m, _ := filepath.Glob(filepath.Join(root, glob))
		if len(m) == 0 {
			t.Skipf("no %s artifact under %s", svc, root)
		}
		arts[svc] = m[len(m)-1]
	}
	mk := func(prefix string) string {
		d, err := os.MkdirTemp("/tmp", prefix)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(d) })
		return d
	}
	stateA, stateB := mk("sbra"), mk("sbrb")
	// A failure is explained by the units' logs, which go with the directories.
	t.Cleanup(func() {
		if t.Failed() {
			dumpLogs(t, "node A", stateA)
			dumpLogs(t, "node B", stateB)
		}
	})

	// Ports: the replica range, then the system cluster, then the projects', all in one block.
	rb := freePortBase(t, 40)
	cfgA := config.Default()
	cfgA.StateDir = stateA
	cfgA.KeyPath = filepath.Join(stateA, "master.key")
	cfgA.Supervisor = config.SupervisorExec
	cfgA.Domain = "supavise.test"
	cfgA.TLS.Mode = "off"
	cfgA.BinPath = supaviseBinary(t)
	cfgA.Backup.Backend = "file://" + filepath.Join(stateA, "backups")
	cfgA.Backup.ArchiveTimeoutSeconds = 30
	cfgA.Ports.ReplicaBase = rb
	cfgA.Ports.SystemPostgres, cfgA.Ports.SystemGoTrue, cfgA.Ports.ProjectBase = rb+30, rb+31, rb+32
	if err := cfgA.CheckReplicaPorts(); err != nil {
		t.Fatal(err)
	}
	cfgPathA := writeConfig(t, cfgA)
	t.Setenv("SUPAVISE_CONFIG", cfgPathA)

	cfgB := *cfgA
	cfgB.StateDir = stateB
	cfgB.KeyPath = filepath.Join(stateB, "master.key")
	cfgPathB := writeConfig(t, &cfgB)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	restoreCmd := func(cfg *config.Config, path string) func(string) string {
		return func(ref string) string { return backup.RestoreCommandFor(cfg, ref, ref, path) }
	}
	oo := lifecycle.OpenOptions{Artifacts: arts, ConfigPath: cfgPathA, RestoreCommandFor: restoreCmd(cfgA, cfgPathA)}
	t.Cleanup(func() {
		if err := lifecycle.StopAll(context.Background(), cfgA, oo); err != nil {
			t.Errorf("StopAll: %v", err)
		}
	})
	nodeA, err := lifecycle.InitSystem(ctx, cfgA, oo, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nodeA.Close)

	store, err := backup.OpenStore(ctx, cfgA.Backup)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := backup.New(backup.Options{
		Config: cfgA, Registry: nodeA.Registry, Store: store, Secrets: nodeA.Secrets,
		Access: backup.AccessFromRegistry(cfgA, nodeA.Registry, nodeA.Secrets), ConfigPath: cfgPathA,
		RecoveryTimeout: 3 * time.Minute, RecoveryPoll: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetManager(nodeA.Engine)

	p, err := nodeA.Engine.Create(ctx, lifecycle.CreateRequest{Name: "rr", Class: "micro"})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := nodeA.Engine.Keys(ctx, p.Ref)
	if err != nil {
		t.Fatal(err)
	}

	// Node B: its own directory and supervisor; the registry it reads is A's (the replicated copy, on
	// a real second node).
	supB, err := units.New(&cfgB, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	planeB := lifecycle.NewPostgresPlane(&cfgB, supB, arts, nodeA.Registry, lifecycle.PlaneOptions{
		ConfigPath: cfgPathB, ArchiveTimeout: 30, RestoreCommandFor: restoreCmd(&cfgB, cfgPathB),
	})
	t.Cleanup(func() { _ = planeB.Stop(context.Background(), p.Ref) })
	member := func(self string) cluster.Membership {
		n1 := registry.Node{ID: "n1", Name: "primary", State: registry.NodeActive}
		n2 := registry.Node{ID: "n2", Name: "second", State: registry.NodeActive}
		s := n1
		if self == "n2" {
			s = n2
		}
		return cluster.NewStatic(cluster.Snapshot{Self: s, Nodes: []registry.Node{n1, n2}, Leader: "n1", Epoch: 1, Role: cluster.RoleFollower})
	}
	agentOpts := func(cfg *config.Config, plane ReplicaPlane, self string) AgentOptions {
		return AgentOptions{Cfg: cfg, Plane: plane, Registry: nodeA.Registry, Keys: nodeA.Engine.Keys, Members: member(self),
			Seeder: testSeeder(svc), Poll: 500 * time.Millisecond, StallTimeout: 5 * time.Minute}
	}
	e := &rrEnv{t: t, cfgA: cfgA, cfgB: &cfgB, nodeA: nodeA, planeB: planeB, project: p, keys: keys, svc: svc,
		agentA: NewNodeAgent(agentOpts(cfgA, nodeA.Plane, "n1")), agentB: NewNodeAgent(agentOpts(&cfgB, planeB, "n2"))}
	e.agentA.Start(ctx)
	e.agentB.Start(ctx)
	return e
}

func TestIntegrationReplicaBlocks(t *testing.T) {
	e := newReplicaEnv(t)
	ctx := context.Background()
	prim, repl := e.ports()
	ref := e.project.Ref
	id := registry.ReplicaIdentifier(ref, "us-east-1", "abc123")
	keys := e.keys

	// 1. The project has data, pg_cron with a job and pg_net, and a base backup.
	e.exec(prim.Postgres, `create table public.items (id int primary key, v text);
		insert into public.items select g, 'row ' || g from generate_series(1, 100) g;
		grant select on public.items to anon;
		create extension if not exists pg_cron; create extension if not exists pg_net;
		create table public.cron_marker (id bigserial primary key, at timestamptz not null default now());
		select cron.schedule('rr-marker', '10 seconds', 'insert into public.cron_marker default values')`)
	if _, err := e.svc.BaseBackup(ctx, ref); err != nil {
		t.Fatal(err)
	}
	e.exec(prim.Postgres, `insert into public.items select g, 'after the backup ' || g from generate_series(101, 150) g`)

	// 2. Node B builds the standby from the base backup, replays the archive and streams.
	st, err := e.agentB.Ensure(ctx, peerapi.InstanceSpec{Identifier: id, Ref: ref, Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ensure: %+v", st)
	eventually(t, 10*time.Minute, "the replica setup", func() string {
		s, err := e.agentB.Observe(ctx, id)
		if err != nil {
			return err.Error()
		}
		if s.Error != "" {
			t.Fatalf("setup failed at %s: %s: %s", s.Step, s.Error, s.Detail)
		}
		if s.Step != StepCompleted {
			return "step " + s.Step
		}
		return ""
	})
	obs, err := e.agentB.Observe(ctx, id)
	if err != nil || obs.Role != "replica" || !obs.InRecovery || obs.ReceiverStatus != "streaming" || !obs.PostgRESTReady {
		t.Fatalf("replica status = %+v, %v", obs, err)
	}

	// 3. It is a hot standby of the project: data from before and after the backup, settings, streaming.
	if n := e.count(repl.Postgres, `select count(*) from public.items`); n != 150 {
		t.Fatalf("the replica has %d rows, want 150", n)
	}
	e.exec(prim.Postgres, `insert into public.items select g, 'streamed ' || g from generate_series(151, 200) g`)
	eventually(t, 30*time.Second, "streamed rows", func() string {
		if n := e.count(repl.Postgres, `select count(*) from public.items`); n != 200 {
			return fmt.Sprintf("%d rows", n)
		}
		return ""
	})
	if got := e.text(repl.Postgres, `select current_setting('hot_standby') || ' ' || current_setting('hot_standby_feedback') || ' ' || pg_is_in_recovery()::text`); got != "on on true" {
		t.Fatalf("standby settings = %q", got)
	}
	if n := e.count(prim.Postgres, `select count(*) from pg_stat_replication where application_name = '`+id+`' and state = 'streaming'`); n != 1 {
		t.Fatalf("the primary sees %d streaming standbys named %s", n, id)
	}
	if _, err := e.sql(repl.Postgres).Exec(ctx, `insert into public.items values (999, 'write')`); err == nil {
		t.Fatal("the standby accepted a write")
	}
	// pg-meta and the parameterized-query path log in to the replica's port with a password, as they do to
	// the primary's: postgres above, and the read-only role the Management API creates on the primary
	// (it replicates, so it can log in once the standby has replayed its creation).
	roPass := "ro-" + keys.AdminPassword[:12]
	e.exec(prim.Postgres, `create role supavise_read_only login bypassrls password '`+roPass+`'`)
	e.exec(prim.Postgres, `grant pg_read_all_data to supavise_read_only`)
	eventually(t, 30*time.Second, "supavise_read_only logging in to the replica with its password", func() string {
		c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://supavise_read_only:%s@127.0.0.1:%d/postgres?sslmode=disable", roPass, repl.Postgres))
		if err != nil {
			return err.Error()
		}
		defer c.Close(ctx)
		var n int
		if err := c.QueryRow(ctx, `select count(*) from public.items`).Scan(&n); err != nil || n != 200 {
			return fmt.Sprintf("read %d rows, %v", n, err)
		}
		return ""
	})

	// 4. pg_cron and pg_net workers run on the primary and idle on the standby (spike S2). Each one is
	// counted by itself: a primary that runs only one of them would prove nothing about the other.
	workers := map[string]string{
		"pg_cron": `select count(*) from pg_stat_activity where backend_type ilike '%pg_cron%'`,
		"pg_net":  `select count(*) from pg_stat_activity where backend_type ilike '%pg_net%'`,
	}
	const backends = `select string_agg(backend_type, ', ') from pg_stat_activity where backend_type <> 'client backend'`
	for _, name := range []string{"pg_cron", "pg_net"} {
		if n := e.count(prim.Postgres, workers[name]); n == 0 {
			t.Fatalf("the control failed: no %s worker on the primary: %s", name, e.text(prim.Postgres, backends))
		}
		if n := e.count(repl.Postgres, workers[name]); n != 0 {
			t.Fatalf("%d %s workers on the standby: %s", n, name, e.text(repl.Postgres, backends))
		}
	}

	// 5. PostgREST on the standby sees a DDL change within the reload interval (spike S3), reads the
	// standby, listens on the primary, and refuses writes.
	restURL := fmt.Sprintf("http://127.0.0.1:%d", repl.PostgREST)
	if code, body := httpStatus(restURL+"/items?select=id&id=lt.3", keys.AnonKey, "GET", ""); code != 200 {
		t.Fatalf("GET /items on the replica's PostgREST = %d %s", code, body)
	}
	// The timer is what is under test, so the NOTIFY that would tell PostgREST of the change without
	// it is taken away: with the event triggers off, nothing but SIGUSR1 can make the table visible.
	t.Logf("%d event triggers notify PostgREST of DDL changes; they are off for the next change", e.pgrstTriggers(false))
	e.exec(prim.Postgres, `create table public.rr_probe (id int primary key, v text); insert into public.rr_probe values (1, 'x'); grant select on public.rr_probe to anon`)
	eventually(t, 30*time.Second, "the new table on the standby", func() string {
		if n := e.count(repl.Postgres, `select count(*) from pg_class where relname = 'rr_probe'`); n != 1 {
			return fmt.Sprintf("%d tables", n)
		}
		return ""
	})
	// Without the timer the schema cache is not rebuilt, however long the table has been on the standby.
	for stop := time.Now().Add(5 * time.Second); time.Now().Before(stop); time.Sleep(500 * time.Millisecond) {
		if code, body := httpStatus(restURL+"/rr_probe?select=id", keys.AnonKey, "GET", ""); code == 200 {
			t.Fatalf("the new table was visible through the replica's PostgREST with no notification and no reload: %s", body)
		}
	}
	// With it every change is visible within one interval.
	const reloadEvery = 2 * time.Second
	rctx, stopReload := context.WithCancel(ctx)
	reloadDone := make(chan struct{})
	go func() {
		defer close(reloadDone)
		e.planeB.RunSchemaReload(rctx, reloadEvery, func(context.Context) ([]string, error) { return []string{ref}, nil })
	}()
	t.Cleanup(func() { stopReload(); <-reloadDone })
	start := time.Now()
	eventually(t, 3*reloadEvery, "the new table in the replica's PostgREST after the reload timer started", func() string {
		if code, body := httpStatus(restURL+"/rr_probe?select=id", keys.AnonKey, "GET", ""); code != 200 {
			return fmt.Sprintf("%d %s", code, body)
		}
		return ""
	})
	t.Logf("the DDL change was visible through the replica's PostgREST %s after the reload timer started", time.Since(start).Round(100*time.Millisecond))
	stopReload()
	<-reloadDone
	e.pgrstTriggers(true)
	if code, _ := httpStatus(restURL+"/items", keys.ServiceRoleKey, "POST", `{"id": 1000, "v": "nope"}`); code < 400 {
		t.Fatalf("a write through the replica's PostgREST answered %d", code)
	}
	if n := e.count(prim.Postgres, `select count(*) from pg_stat_activity where application_name = 'postgrest-replica' and query ilike 'listen%'`); n < 1 {
		t.Fatal("the replica's PostgREST has no LISTEN session on the primary")
	}
	if n := e.count(repl.Postgres, `select count(*) from pg_stat_activity where application_name = 'postgrest-replica'`); n < 1 {
		t.Fatal("the replica's PostgREST has no session on the replica")
	}

	// 6. Switchover. The primary stops, B's standby is promoted onto the canonical port once it has
	// replayed the primary's shutdown checkpoint.
	if err := e.nodeA.Plane.Stop(ctx, ref); err != nil {
		t.Fatal(err)
	}
	ci, err := e.nodeA.Plane.FinalCheckpoint(ref)
	if err != nil || !ci.ShutDown() {
		t.Fatalf("final checkpoint = %+v, %v", ci, err)
	}
	if st, err := e.agentB.Do(ctx, id, peerapi.ActionPromote, peerapi.InstanceAction{Epoch: 2, WaitLSN: ci.Checkpoint, TimeoutSeconds: 120}); err != nil || st.Role != "primary" {
		t.Fatalf("promote: %+v, %v", st, err)
	}
	if got := e.text(prim.Postgres, `select pg_is_in_recovery()::text || ' ' || current_setting('hot_standby')`); !strings.HasPrefix(got, "false") {
		t.Fatalf("after the promotion: %q", got)
	}
	if n := e.count(prim.Postgres, `select count(*) from public.items`); n != 200 {
		t.Fatalf("the promoted cluster has %d rows, want 200", n)
	}
	if b, err := os.ReadFile(e.cfgB.Paths().PromoteOK(ref)); err != nil || string(b) != "2\n" {
		t.Fatalf("promote.ok = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(e.cfgB.Paths().PostgresData(ref), "standby.signal")); !os.IsNotExist(err) {
		t.Fatalf("standby.signal after the promotion: %v", err)
	}
	conf, _ := os.ReadFile(filepath.Join(e.cfgB.Paths().PostgresData(ref), "postgresql.auto.conf"))
	for _, s := range []string{"primary_conninfo", "restore_command", "recovery_target"} {
		if strings.Contains(string(conf), s) {
			t.Fatalf("%s is still in postgresql.auto.conf:\n%s", s, conf)
		}
	}
	e.exec(prim.Postgres, `insert into public.items values (2001, 'written on the new primary')`)
	// The registry names the new home (design 2.10.3, step 5) before the old one is demoted: the agent
	// refuses to demote the node the registry still names the home.
	if err := e.nodeA.Registry.CreateNode(ctx, &registry.Node{ID: "n2", Name: "second", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	cl, err := e.nodeA.Registry.GetCluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.nodeA.Registry.SetProjectNode(ctx, ref, "n2", cl.Epoch); err != nil {
		t.Fatal(err)
	}
	// The new primary's PostgREST and GoTrue start on the canonical ports.
	p, _ := e.nodeA.Registry.GetProject(ctx, ref)
	if err := e.planeB.Start(ctx, p, keys); err != nil {
		t.Fatalf("start the new home: %v", err)
	}
	if code, body := httpStatus(fmt.Sprintf("http://127.0.0.1:%d/items?select=id&id=eq.2001", prim.PostgREST), keys.AnonKey, "GET", ""); code != 200 || !strings.Contains(body, "2001") {
		t.Fatalf("GET on the new primary's PostgREST = %d %s", code, body)
	}
	// pg_cron and pg_net start at the promotion without a restart (spike S2): the job inserts markers
	// again, and only this cluster can be inserting them, the old primary being down.
	before := e.count(prim.Postgres, `select count(*) from public.cron_marker`)
	eventually(t, 2*time.Minute, "pg_cron on the promoted cluster", func() string {
		if n := e.count(prim.Postgres, `select count(*) from public.cron_marker`); n < before+2 {
			return fmt.Sprintf("%d markers, had %d", n, before)
		}
		return ""
	})
	for _, name := range []string{"pg_cron", "pg_net"} {
		eventually(t, time.Minute, name+" worker on the promoted cluster", func() string {
			if n := e.count(prim.Postgres, workers[name]); n == 0 {
				return e.text(prim.Postgres, backends)
			}
			return ""
		})
	}

	// 7. The old primary becomes a standby of the new one, in place (design 2.10.3, step 7).
	oldID := registry.ReplicaIdentifier(ref, "us-east-1", "def456")
	if st, err := e.agentA.Do(ctx, oldID, peerapi.ActionDemote, peerapi.InstanceAction{Epoch: 2}); err != nil || st.Role != "replica" {
		t.Fatalf("demote: %+v, %v", st, err)
	}
	eventually(t, 2*time.Minute, "the demoted cluster streaming from the new primary", func() string {
		s, err := e.agentA.Observe(ctx, oldID)
		if err != nil {
			return err.Error()
		}
		if s.Role != "replica" || s.ReceiverStatus != "streaming" || !s.PostgRESTReady {
			return fmt.Sprintf("%+v", s)
		}
		return ""
	})
	e.exec(prim.Postgres, `insert into public.items values (2002, 'after the demotion')`)
	eventually(t, 30*time.Second, "a row of the new primary on the demoted cluster", func() string {
		if n := e.count(repl.Postgres, `select count(*) from public.items where id in (2001, 2002)`); n != 2 {
			return fmt.Sprintf("%d of 2 rows", n)
		}
		return ""
	})
	if got := e.text(repl.Postgres, `select pg_is_in_recovery()::text`); got != "true" {
		t.Fatalf("the demoted cluster: in recovery = %s", got)
	}
}
