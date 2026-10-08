package backup

// Integration tests of SeedReplica over a real PostgreSQL (see integration_test.go for what they
// need): a seeded directory starts as a hot standby, streams from the primary, takes over, and
// keeps archiving under the same ref; the archive-only variant drains the archive and promotes.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// replicaFixture is a running source cluster archiving to a file store, a registry row for it and
// a Service that can take base backups of it. The archive_command and restore_command are the
// real supavise binary reading the config file in root.
type replicaFixture struct {
	t    *testing.T
	e    *testEnv
	bin  string
	root string
	st   *FileStore
	src  *pgInstance
	c    *pgx.Conn
}

func newReplicaFixture(t *testing.T) *replicaFixture {
	t.Helper()
	bin := pgBinDir(t)
	supavise := supaviseBinary(t)
	ctx := context.Background()
	root := t.TempDir()
	archiveDir := filepath.Join(root, "archive")
	st, err := NewFileStore(archiveDir)
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEnv(t)
	e.now = time.Now() // base backups run on the real clock here
	e.svc.opt.Now = time.Now
	cfgPath := filepath.Join(root, "supavise.toml")
	writeFile(t, cfgPath, []byte(fmt.Sprintf("state_dir = %q\nbin_path = %q\n\n[backup]\nbackend = %q\nretention_days = 7\n",
		filepath.Join(root, "state"), supavise, "file://"+archiveDir)))
	e.cfg.BinPath = supavise
	e.svc.opt.Store, e.store = st, nil
	e.svc.opt.ConfigPath = cfgPath

	src := newSourceCluster(t, bin, root, testRef, ArchiveSettings(e.cfg, testRef, cfgPath))
	src.start()
	e.addProject(t, testRef)
	e.svc.opt.Access = &portAccess{ports: map[string]int{testRef: src.port}}
	e.svc.opt.DataDir = func(string) string { return src.dir }
	return &replicaFixture{t: t, e: e, bin: bin, root: root, st: st, src: src, c: src.connect(ctx)}
}

func (f *replicaFixture) mustExec(sql string) {
	f.t.Helper()
	if _, err := f.c.Exec(context.Background(), sql); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// archiveNow switches WAL on the source and waits until the archiver has shipped the segment.
func (f *replicaFixture) archiveNow() {
	f.t.Helper()
	ctx := context.Background()
	var seg string
	if err := f.c.QueryRow(ctx, "select pg_walfile_name(pg_current_wal_lsn())").Scan(&seg); err != nil {
		f.t.Fatal(err)
	}
	f.mustExec("select pg_switch_wal()")
	waitFor(f.t, "WAL archiving of "+seg, 60*time.Second, func() (bool, error) {
		var ok bool
		err := f.c.QueryRow(ctx, "select coalesce(last_archived_wal >= $1, false) from pg_stat_archiver", seg).Scan(&ok)
		return ok, err
	})
}

// standby starts the seeded directory as a cluster of its own on a free port.
func (f *replicaFixture) standby(dir string) (*pgInstance, *pgx.Conn) {
	f.t.Helper()
	p := &pgInstance{t: f.t, bin: f.bin, dir: dir, port: freePort(f.t), log: filepath.Join(f.root, "standby.log")}
	p.start()
	return p, p.connect(context.Background())
}

func waitIDs(t *testing.T, c *pgx.Conn, want string) {
	t.Helper()
	waitFor(t, "rows "+want, 60*time.Second, func() (bool, error) {
		ids, err := tryQueryIDs(c)
		if err != nil {
			return false, err
		}
		return idsString(ids) == want, fmt.Errorf("rows = %v", ids)
	})
}

func tryQueryIDs(c *pgx.Conn) ([]int, error) {
	rows, err := c.Query(context.Background(), "select id from public.t order by id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func TestSeedReplicaStreamsAndTakesOver(t *testing.T) {
	f := newReplicaFixture(t)
	ctx := context.Background()
	f.mustExec("create table public.t (id int primary key)")
	f.mustExec("insert into public.t values (1)")
	f.mustExec("create role supabase_replication_admin replication login password 'replpw'")

	// The first call takes a base backup; the second finds it young enough.
	b1, err := f.e.svc.EnsureBase(ctx, testRef, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := f.e.svc.EnsureBase(ctx, testRef, time.Hour)
	if err != nil || BackupIDOf(b1) == "" || BackupIDOf(b1) != BackupIDOf(b2) {
		t.Fatalf("EnsureBase twice = %+v, %+v, %v; want the same backup", b1, b2, err)
	}
	if ms, _ := f.e.svc.ListBackups(ctx, testRef); len(ms) != 1 {
		t.Fatalf("base backups = %d, want 1", len(ms))
	}
	f.mustExec("insert into public.t values (2)") // after the backup: reaches the standby through WAL

	dd := filepath.Join(f.root, "replica", "pgdata")
	if err := f.e.svc.SeedReplica(ctx, ReplicaSeedPlan{Ref: testRef, Identifier: testReplicaID, DataDir: dd,
		BackupID: BackupIDOf(b1), PrimaryPort: f.src.port, ReplicationPassword: "replpw"}); err != nil {
		t.Fatal(err)
	}
	rep, rc := f.standby(dd)

	var inRecovery bool
	if err := rc.QueryRow(ctx, "select pg_is_in_recovery()").Scan(&inRecovery); err != nil || !inRecovery {
		t.Fatalf("pg_is_in_recovery = %v, %v; the seeded directory must start as a standby", inRecovery, err)
	}
	waitIDs(t, rc, "[1 2]")
	waitFor(t, "the WAL receiver to stream", 60*time.Second, func() (bool, error) {
		var status string
		err := rc.QueryRow(ctx, "select coalesce((select status from pg_stat_wal_receiver), '')").Scan(&status)
		return status == "streaming", fmt.Errorf("status %q, %v", status, err)
	})
	waitFor(t, "the primary to list "+testReplicaID+" as streaming", 60*time.Second, func() (bool, error) {
		var n int
		err := f.c.QueryRow(ctx, "select count(*) from pg_stat_replication where application_name = $1 and state = 'streaming'", testReplicaID).Scan(&n)
		return n == 1, fmt.Errorf("%d streaming walsenders named %s, %v", n, testReplicaID, err)
	})
	settings := map[string]string{}
	rows, err := rc.Query(ctx, "select name, setting from pg_settings where name in ('hot_standby','archive_mode','recovery_target_timeline','restore_command','primary_conninfo')")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n, v string
		if err := rows.Scan(&n, &v); err != nil {
			t.Fatal(err)
		}
		settings[n] = v
	}
	rows.Close()
	if settings["hot_standby"] != "on" || settings["archive_mode"] != "on" || settings["recovery_target_timeline"] != "latest" ||
		!strings.Contains(settings["restore_command"], "wal fetch --ref "+testRef) || !strings.Contains(settings["primary_conninfo"], "application_name="+testReplicaID) {
		t.Fatalf("standby settings = %v", settings)
	}

	// Streaming, not the archive: a small commit does not complete a WAL segment.
	f.mustExec("insert into public.t values (3)")
	waitIDs(t, rc, "[1 2 3]")
	if _, err := rc.Exec(ctx, "insert into public.t values (99)"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("write on the standby = %v; want a read-only error", err)
	}

	// Planned takeover: the primary stops (its walsender flushes what is left), the standby is promoted
	// and archives under the same ref on the next timeline.
	f.src.stop()
	var promoted bool
	if err := rc.QueryRow(ctx, "select pg_promote(true, 60)").Scan(&promoted); err != nil || !promoted {
		t.Fatalf("pg_promote = %v, %v", promoted, err)
	}
	if _, err := rc.Exec(ctx, "insert into public.t values (4)"); err != nil {
		t.Fatalf("write on the promoted standby: %v", err)
	}
	if _, err := rc.Exec(ctx, "select pg_switch_wal()"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the promoted standby to archive its timeline", 60*time.Second, func() (bool, error) {
		_, err := f.st.Stat(ctx, walKey(testRef, "00000002.history"))
		return err == nil, err
	})
	waitFor(t, "a WAL file of timeline 2 in the archive", 60*time.Second, func() (bool, error) {
		objs, err := f.st.List(ctx, walDir(testRef)+"00000002")
		return len(objs) > 1, err // the history file and at least one segment
	})
	rep.stop()
}

// The standby of `supavise failover --restore-missing`: no primary to stream from. It replays the
// archive to its end and is promoted there.
func TestSeedReplicaArchiveStandbyDrainsAndPromotes(t *testing.T) {
	f := newReplicaFixture(t)
	ctx := context.Background()
	f.mustExec("create table public.t (id int primary key)")
	f.mustExec("insert into public.t values (1)")
	if _, err := f.e.svc.EnsureBase(ctx, testRef, 0); err != nil {
		t.Fatal(err)
	}
	f.mustExec("insert into public.t values (2)")
	f.mustExec("insert into public.t values (3)")
	f.archiveNow()

	dd := filepath.Join(f.root, "restored", "pgdata")
	if err := f.e.svc.SeedReplica(ctx, ReplicaSeedPlan{Ref: testRef, DataDir: dd}); err != nil {
		t.Fatal(err)
	}
	_, rc := f.standby(dd)
	waitIDs(t, rc, "[1 2 3]")
	var inRecovery bool
	var receivers int
	if err := rc.QueryRow(ctx, "select pg_is_in_recovery(), (select count(*) from pg_stat_wal_receiver)").Scan(&inRecovery, &receivers); err != nil || !inRecovery || receivers != 0 {
		t.Fatalf("archive standby: in recovery %v, receivers %d, %v", inRecovery, receivers, err)
	}
	var promoted bool
	if err := rc.QueryRow(ctx, "select pg_promote(true, 60)").Scan(&promoted); err != nil || !promoted {
		t.Fatalf("pg_promote = %v, %v", promoted, err)
	}
	if _, err := rc.Exec(ctx, "insert into public.t values (4)"); err != nil {
		t.Fatalf("write on the promoted standby: %v", err)
	}
	waitIDs(t, rc, "[1 2 3 4]")
}
