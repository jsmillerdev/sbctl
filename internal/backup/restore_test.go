package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

func TestPickBackup(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mk := func(h int) Manifest {
		stop := t0.Add(time.Duration(h) * time.Hour)
		return Manifest{ID: backupID(stop.Add(-time.Minute)), StopTime: stop}
	}
	all := []Manifest{mk(0), mk(24), mk(48)}
	for _, tc := range []struct {
		name   string
		target time.Time
		id     string
		want   int // index into all, -1 = error
	}{
		{"newest before target", t0.Add(30 * time.Hour), "", 1},
		{"target after newest", t0.Add(100 * time.Hour), "", 2},
		{"target exactly at stop", t0.Add(24 * time.Hour), "", 1},
		{"target before every backup", t0.Add(-time.Hour), "", -1},
		{"pinned id", t0.Add(100 * time.Hour), all[0].ID, 0},
		{"pinned id finished after target", t0.Add(30 * time.Hour), all[2].ID, -1},
		{"unknown id", t0.Add(30 * time.Hour), "nope", -1},
	} {
		got, err := pickBackup(all, tc.target, tc.id)
		if tc.want < 0 {
			if err == nil {
				t.Errorf("%s: expected an error", tc.name)
			}
			continue
		}
		if err != nil || got.ID != all[tc.want].ID {
			t.Errorf("%s: got %v, %v want %s", tc.name, got, err, all[tc.want].ID)
		}
	}
	if _, err := pickBackup(nil, t0, ""); err == nil {
		t.Error("no backups must be an error")
	}
}

func TestExtractTarRejectsUnsafeEntries(t *testing.T) {
	for name, hdr := range map[string]tar.Header{
		"parent":   {Typeflag: tar.TypeReg, Name: "../evil", Size: 1},
		"nested":   {Typeflag: tar.TypeReg, Name: "a/../../evil", Size: 1},
		"absolute": {Typeflag: tar.TypeReg, Name: "/etc/evil", Size: 1},
		"symlink":  {Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc"},
		"hardlink": {Typeflag: tar.TypeLink, Name: "link", Linkname: "x"},
		"fifo":     {Typeflag: tar.TypeFifo, Name: "fifo"},
	} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		h := hdr
		h.Mode = 0o600
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			tw.Write([]byte("x"))
		}
		tw.Close()
		root := filepath.Join(t.TempDir(), "root")
		os.MkdirAll(root, 0o700)
		if err := extractTar(context.Background(), tar.NewReader(&buf), root); err == nil {
			t.Errorf("%s: extractTar accepted the entry", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil")); err == nil {
			t.Errorf("%s: wrote outside the root", name)
		}
	}
}

// fakeDataDir builds a small cluster-shaped directory.
func fakeDataDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "pgdata")
	for p, body := range map[string]string{
		"PG_VERSION":                      "17\n",
		"postgresql.conf":                 "port = 5432\n",
		"postgresql.auto.conf":            "# auto\n",
		"global/pg_control":               "control",
		"base/1/1259":                     "relation",
		"base/1/t3_1234":                  "temp relation: excluded",
		"base/1/pg_internal.init":         "excluded",
		"pg_wal/000000010000000000000001": "excluded: WAL comes from the archive",
		"pg_wal/archive_status/x.ready":   "excluded",
		"pg_replslot/slot1/state":         "excluded",
		"pg_stat_tmp/global.stat":         "excluded",
		"pg_notify/0000":                  "excluded",
		"base/pgsql_tmp/pgsql_tmp123":     "excluded",
		"postmaster.pid":                  "123\n",
		"postmaster.opts":                 "opts",
		"backup_label":                    "stale label",
		"pg_tblspc/.keep":                 "",
	} {
		writeFile(t, filepath.Join(d, p), []byte(body))
	}
	return d
}

func TestDataDirTarRoundTripAndExclusions(t *testing.T) {
	src := fakeDataDir(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	st, err := writeDataDirTar(context.Background(), src, tw)
	if err != nil {
		t.Fatal(err)
	}
	tw.Close()
	if st.Files == 0 {
		t.Fatal("no files counted")
	}
	dst := filepath.Join(t.TempDir(), "out")
	os.MkdirAll(dst, 0o700)
	if err := extractTar(context.Background(), tar.NewReader(&buf), dst); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"PG_VERSION", "postgresql.conf", "global/pg_control", "base/1/1259", "pg_wal", "pg_wal/archive_status", "pg_replslot", "pg_stat_tmp", "pg_notify", "pg_tblspc/.keep"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err != nil {
			t.Errorf("%s missing from the restored tree: %v", p, err)
		}
	}
	for _, p := range []string{
		"base/1/t3_1234", "base/1/pg_internal.init", "pg_wal/000000010000000000000001", "pg_replslot/slot1", "pg_stat_tmp/global.stat",
		"pg_notify/0000", "base/pgsql_tmp", "postmaster.pid", "postmaster.opts", "backup_label",
	} {
		if _, err := os.Stat(filepath.Join(dst, p)); err == nil {
			t.Errorf("%s must be excluded from a base backup", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "base/1/1259")); string(b) != "relation" {
		t.Errorf("relation file = %q", b)
	}
}

func TestDataDirTarRefusesTablespacesSymlinksAndMidInit(t *testing.T) {
	d := fakeDataDir(t)
	os.Symlink("/elsewhere", filepath.Join(d, "pg_tblspc", "16385"))
	if _, err := writeDataDirTar(context.Background(), d, tar.NewWriter(&bytes.Buffer{})); !errors.Is(err, ErrTablespaces) {
		t.Fatalf("tablespace link = %v, want ErrTablespaces", err)
	}
	d = fakeDataDir(t)
	os.Symlink("/etc/passwd", filepath.Join(d, "global", "oops"))
	if _, err := writeDataDirTar(context.Background(), d, tar.NewWriter(&bytes.Buffer{})); err == nil {
		t.Fatal("a stray symlink must fail the backup, not be silently dropped")
	}
	d = fakeDataDir(t)
	writeFile(t, filepath.Join(d, initPendingFile), []byte("pending\n"))
	if _, err := writeDataDirTar(context.Background(), d, tar.NewWriter(&bytes.Buffer{})); err == nil {
		t.Fatal("a cluster that is mid-initialization must not be backed up")
	}
}

// storeBase uploads a base backup built from src as backup id, the way runBase does
// (minus the database calls), and returns its manifest.
func (e *testEnv) storeBase(t *testing.T, ref, src string, stop time.Time, keys *secrets.ProjectKeys) Manifest {
	t.Helper()
	ctx := context.Background()
	m := Manifest{Version: manifestVersion, ID: backupID(stop.Add(-time.Minute)), Ref: ref, Reason: ReasonManual, Timeline: 1,
		StartLSN: "0/2000028", StopLSN: "0/2000100", StartWAL: walName(1, 2), StopWAL: walName(1, 2),
		StartTime: stop.Add(-time.Minute), StopTime: stop, Data: dataName,
		Project: &ManifestProject{Name: "src project", Region: "local"}}
	if _, err := e.svc.streamTar(ctx, m.Dir()+"/"+dataName, func(tw *tar.Writer) error {
		if _, err := writeDataDirTar(ctx, src, tw); err != nil {
			return err
		}
		return writeTarFile(tw, "backup_label", []byte("START WAL LOCATION: 0/2000028\n"))
	}); err != nil {
		t.Fatal(err)
	}
	if keys != nil {
		sealed := map[string][]byte{}
		for n, v := range keys.Map() {
			b, _ := e.sec.Seal([]byte(v))
			sealed[n] = b
		}
		if err := e.svc.writeSecretsFile(ctx, &m, sealed); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.svc.writeManifest(ctx, &m); err != nil {
		t.Fatal(err)
	}
	e.fakeWAL(t, ref, m.StartWAL)
	return m
}

func TestSeederWritesRecoveryFiles(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.cfg.BinPath = "/opt/sbctl/bin/sbctl"
	e.svc.opt.ConfigPath = "/etc/sbctl/other.toml"
	stop := e.now.Add(-time.Hour)
	m := e.storeBase(t, testRef, fakeDataDir(t), stop, nil)

	target := e.now.Add(-30 * time.Minute)
	plan, err := e.svc.PlanRestore(ctx, testRef, target, "")
	if err != nil || plan.Manifest.ID != m.ID {
		t.Fatalf("PlanRestore = %+v, %v", plan, err)
	}
	plan.TargetRef = testRef2

	dd := filepath.Join(t.TempDir(), "new", "postgres")
	if err := e.svc.Seeder(plan)(ctx, nil, dd); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dd); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("data dir = %v, %v (Postgres refuses anything but 0700/0750)", fi, err)
	}
	for _, p := range []string{"PG_VERSION", "base/1/1259", "backup_label", "recovery.signal", "postmaster.opts", "pg_wal/archive_status"} {
		if _, err := os.Stat(filepath.Join(dd, p)); err != nil {
			t.Errorf("%s missing: %v", p, err)
		}
	}
	conf, _ := os.ReadFile(filepath.Join(dd, "postgresql.auto.conf"))
	for _, want := range []string{
		"# auto\n", // the source's own content is kept; ours is appended
		"restore_command = '/opt/sbctl/bin/sbctl --config /etc/sbctl/other.toml wal fetch --ref " + testRef + " %f %p'",
		"archive_command = '/opt/sbctl/bin/sbctl --config /etc/sbctl/other.toml wal push --ref " + testRef2 + " %p'",
		"recovery_target_time = '" + target.UTC().Format("2006-01-02 15:04:05") + "+00'",
		"recovery_target_action = 'promote'",
		"recovery_target_timeline = 'latest'",
	} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("postgresql.auto.conf lacks %q:\n%s", want, conf)
		}
	}
	// A seeder never writes into a directory that already has data.
	if err := e.svc.Seeder(plan)(ctx, nil, dd); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("second seed = %v", err)
	}
}

// fakeManager records Create calls and runs the seeder into a directory.
type fakeManager struct {
	lifecycle.Manager
	e        *testEnv
	created  []lifecycle.CreateRequest
	paused   []string
	resumed  []string
	dataDir  string
	failSeed bool
}

func (f *fakeManager) Create(ctx context.Context, req lifecycle.CreateRequest) (*registry.Project, error) {
	f.created = append(f.created, req)
	p := &registry.Project{Ref: req.Ref, Name: req.Name, Engine: registry.EnginePostgres, Status: registry.StatusActiveHealthy}
	if err := req.Seed(ctx, p, f.dataDir); err != nil {
		return nil, err
	}
	if err := f.e.reg.CreateProject(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}
func (f *fakeManager) Pause(_ context.Context, ref string) error {
	f.paused = append(f.paused, ref)
	return nil
}
func (f *fakeManager) Resume(_ context.Context, ref string) error {
	f.resumed = append(f.resumed, ref)
	return nil
}

func TestRestoreAsNewProjectKeysAndRequest(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	src := e.addProject(t, testRef)
	e.storeBase(t, testRef, fakeDataDir(t), e.now.Add(-time.Hour), src)
	fm := &fakeManager{e: e, dataDir: filepath.Join(t.TempDir(), "restored")}
	e.svc.opt.Manager = fm

	p, err := e.svc.Restore(ctx, testRef, e.now.Add(-time.Minute), testRef2)
	if err != nil {
		t.Fatal(err)
	}
	if p.Ref != testRef2 || len(fm.created) != 1 {
		t.Fatalf("project %+v, creates %d", p, len(fm.created))
	}
	req := fm.created[0]
	if req.Ref != testRef2 || req.Seed == nil || req.Keys == nil || req.DBPassword != src.DBPassword {
		t.Fatalf("CreateRequest = %+v", req)
	}
	k := req.Keys
	// What lives inside the restored cluster is reused.
	if k.DBPassword != src.DBPassword || k.AdminPassword != src.AdminPassword || k.PGSodiumRootKey != src.PGSodiumRootKey ||
		k.AuthenticatorPassword != src.AuthenticatorPassword || k.AuthAdminPassword != src.AuthAdminPassword || k.StorageAdminPassword != src.StorageAdminPassword {
		t.Error("database credentials and the pgsodium key must be the source's")
	}
	// What an API client can hold is new.
	if k.JWTSecret == src.JWTSecret || k.PublishableKey == src.PublishableKey || k.SecretKey == src.SecretKey || k.AnonKey == src.AnonKey || k.ServiceRoleKey == src.ServiceRoleKey {
		t.Error("JWT secret and API keys must be new")
	}
	if !strings.HasPrefix(k.PublishableKey, secrets.PrefixPublishable) || !strings.HasPrefix(k.SecretKey, secrets.PrefixSecret) {
		t.Errorf("key formats: %q %q", k.PublishableKey, k.SecretKey)
	}
	for _, tok := range []struct{ jwt, role string }{{k.AnonKey, secrets.RoleAnon}, {k.ServiceRoleKey, secrets.RoleServiceRole}} {
		claims, err := secrets.ParseHS256(tok.jwt, k.JWTSecret)
		if err != nil {
			t.Fatalf("legacy key does not verify with the new secret: %v", err)
		}
		if claims["ref"] != testRef2 || claims["role"] != tok.role {
			t.Errorf("claims = %v, want ref %s role %s", claims, testRef2, tok.role)
		}
		if _, err := secrets.ParseHS256(tok.jwt, src.JWTSecret); err == nil {
			t.Error("the old JWT secret must not verify the new keys")
		}
	}
	if req.Name != "src project (restored)" {
		t.Errorf("name = %q", req.Name)
	}
	if _, err := os.Stat(filepath.Join(fm.dataDir, "recovery.signal")); err != nil {
		t.Errorf("seeder did not run: %v", err)
	}
	// The source is untouched.
	if _, err := e.reg.GetProject(ctx, testRef); err != nil {
		t.Error(err)
	}
	// Restoring onto an existing ref is refused before anything is created.
	if _, err := e.svc.Restore(ctx, testRef, e.now.Add(-time.Minute), testRef2); err == nil {
		t.Error("restore over an existing project must fail")
	}
	if len(fm.created) != 1 {
		t.Error("a refused restore must not call Create")
	}
}

func TestRestoreInPlaceNeedsForce(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.addProject(t, testRef)
	e.storeBase(t, testRef, fakeDataDir(t), e.now.Add(-time.Hour), nil)
	fm := &fakeManager{e: e}
	e.svc.opt.Manager = fm
	for _, as := range []string{"", testRef} {
		if _, err := e.svc.RestoreWith(ctx, testRef, e.now, as, RestoreOptions{}); !errors.Is(err, ErrForceRequired) {
			t.Fatalf("in-place without force (as=%q) = %v", as, err)
		}
	}
	if _, err := e.svc.Restore(ctx, testRef, e.now, ""); !errors.Is(err, ErrForceRequired) {
		t.Fatalf("Backup.Restore in place = %v", err)
	}
	if len(fm.paused) != 0 {
		t.Fatal("nothing may be stopped without --force")
	}
}

func TestRestoreInPlaceMovesOldDataAside(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	keys := e.addProject(t, testRef)
	_ = keys
	e.storeBase(t, testRef, fakeDataDir(t), e.now.Add(-time.Hour), nil)
	dd := e.svc.opt.DataDir(testRef)
	writeFile(t, filepath.Join(dd, "PG_VERSION"), []byte("old"))
	fm := &fakeManager{e: e}
	e.svc.opt.Manager = fm

	if _, err := e.svc.RestoreWith(ctx, testRef, e.now, "", RestoreOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if len(fm.paused) != 1 || len(fm.resumed) != 1 {
		t.Fatalf("paused %v resumed %v", fm.paused, fm.resumed)
	}
	if b, _ := os.ReadFile(filepath.Join(dd, "PG_VERSION")); string(b) != "17\n" {
		t.Errorf("data dir was not replaced: %q", b)
	}
	asides, _ := filepath.Glob(dd + ".pre-restore-*")
	if len(asides) != 1 {
		t.Fatalf("old data not kept: %v", asides)
	}
	if b, _ := os.ReadFile(filepath.Join(asides[0], "PG_VERSION")); string(b) != "old" {
		t.Errorf("aside copy = %q", b)
	}
}

func TestRestoreInPlaceRollsBackWhenSeedFails(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.addProject(t, testRef)
	m := e.storeBase(t, testRef, fakeDataDir(t), e.now.Add(-time.Hour), nil)
	// Corrupt the base backup so unpacking fails.
	if err := e.store.Put(ctx, m.Dir()+"/"+dataName, strings.NewReader("not zstd")); err != nil {
		t.Fatal(err)
	}
	dd := e.svc.opt.DataDir(testRef)
	writeFile(t, filepath.Join(dd, "PG_VERSION"), []byte("old"))
	fm := &fakeManager{e: e}
	e.svc.opt.Manager = fm

	if _, err := e.svc.RestoreWith(ctx, testRef, e.now, "", RestoreOptions{Force: true}); err == nil {
		t.Fatal("restore from a corrupt backup succeeded")
	}
	if b, _ := os.ReadFile(filepath.Join(dd, "PG_VERSION")); string(b) != "old" {
		t.Fatalf("original data not restored: %q", b)
	}
	if len(fm.resumed) != 1 {
		t.Errorf("project must be resumed after a failed restore: %v", fm.resumed)
	}
}

func TestPlanRestoreErrors(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if _, err := e.svc.PlanRestore(ctx, testRef, e.now, ""); err == nil || !strings.Contains(err.Error(), "no base backups") {
		t.Fatalf("no backups: %v", err)
	}
	m := e.storeBase(t, testRef, fakeDataDir(t), e.now.Add(-time.Hour), nil)
	if _, err := e.svc.PlanRestore(ctx, testRef, e.now.Add(-2*time.Hour), ""); err == nil || !strings.Contains(err.Error(), "earliest base backup") {
		t.Fatalf("target before the backup: %v", err)
	}
	// The WAL the backup starts from has been lost from the archive.
	if err := e.store.Delete(ctx, walKey(testRef, m.StartWAL)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PlanRestore(ctx, testRef, e.now, ""); err == nil || !strings.Contains(err.Error(), "not in the archive") {
		t.Fatalf("missing start WAL: %v", err)
	}
}

func TestSourceKeysNeedsDatabaseCredentials(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m := Manifest{Ref: testRef}
	if _, err := e.svc.sourceKeys(ctx, &m); err == nil {
		t.Fatal("no credentials anywhere must be an error")
	}
	keys := e.addProject(t, testRef)
	got, err := e.svc.sourceKeys(ctx, &m) // falls back to the registry for backups without a secrets file
	if err != nil || got.DBPassword != keys.DBPassword {
		t.Fatalf("registry fallback = %+v, %v", got, err)
	}
	// A different master key cannot open them, and the error says so.
	other, _ := secrets.New(bytes.Repeat([]byte{7}, 32))
	e.svc.opt.Secrets = other
	if _, err := e.svc.sourceKeys(ctx, &m); err == nil || !strings.Contains(err.Error(), "master key") {
		t.Fatalf("wrong master key = %v", err)
	}
}
