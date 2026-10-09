package branching

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

func TestCompare(t *testing.T) {
	a := []Migration{mig("1", "a", "s1"), mig("2", "b", "s2"), mig("4", "d", "s4")}
	b := []Migration{mig("1", "a", "s1"), mig("3", "c", "s3"), mig("4", "d", "different")}
	p := Compare(a, b) // source a, target b
	if got := versions(p.Apply); got != "2" {
		t.Errorf("apply = %s", got)
	}
	if got := versions(p.TargetOnly); got != "3" {
		t.Errorf("target only = %s", got)
	}
	if len(p.Conflicts) != 1 || p.Conflicts[0].Version != "4" {
		t.Errorf("conflicts = %+v", p.Conflicts)
	}
	if !reflect.DeepEqual(p.OutOfOrder, []string{"2"}) { // 2 is older than the target's latest, 4
		t.Errorf("out of order = %v", p.OutOfOrder)
	}
	if s := p.Summary(); !strings.Contains(s, "3") || !strings.Contains(s, "different content") {
		t.Errorf("summary = %q", s)
	}
	// Identical histories, and whitespace-only differences, are not divergence.
	same := Compare([]Migration{mig("1", "a", "select  1;")}, []Migration{mig("1", "a", "select 1")})
	if same.Summary() != "" || len(same.Apply) != 0 {
		t.Errorf("same = %+v", same)
	}
	// The source being ahead is a clean fast forward.
	ff := Compare([]Migration{mig("1", "a", "x"), mig("2", "b", "y")}, []Migration{mig("1", "a", "x")})
	if ff.Summary() != "" || versions(ff.Apply) != "2" {
		t.Errorf("fast forward = %+v", ff)
	}
	if empty := Compare(nil, nil); len(empty.Apply) != 0 || empty.Summary() != "" {
		t.Errorf("empty = %+v", empty)
	}
}

func TestUpTo(t *testing.T) {
	ms := []Migration{mig("1", "", ""), mig("2", "", ""), mig("3", "", "")}
	if got, err := upTo(ms, ""); err != nil || len(got) != 3 {
		t.Fatalf("all: %v %v", got, err)
	}
	if got, err := upTo(ms, "2"); err != nil || len(got) != 2 {
		t.Fatalf("2: %v %v", got, err)
	}
	if _, err := upTo(ms, "9"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestWALRange(t *testing.T) {
	got, err := walRange("000000010000000000000003", "000000010000000000000005", 16<<20)
	if err != nil || !reflect.DeepEqual(got, []string{"000000010000000000000003", "000000010000000000000004", "000000010000000000000005"}) {
		t.Fatalf("simple: %v %v", got, err)
	}
	// 16 MB segments: 256 per log file, so FF is followed by the next log's 00.
	got, err = walRange("0000000100000000000000FE", "000000010000000100000001", 16<<20)
	if err != nil || len(got) != 4 || got[2] != "000000010000000100000000" || got[3] != "000000010000000100000001" {
		t.Fatalf("log rollover: %v %v", got, err)
	}
	// 64 MB segments: 64 per log file.
	got, err = walRange("00000001000000000000003F", "000000010000000100000000", 64<<20)
	if err != nil || len(got) != 2 {
		t.Fatalf("64MB rollover: %v %v", got, err)
	}
	if got, _ := walRange("000000010000000000000007", "000000010000000000000007", 16<<20); len(got) != 1 {
		t.Fatalf("single: %v", got)
	}
	for _, c := range [][2]string{{"000000010000000000000005", "000000010000000000000003"}, {"000000010000000000000001", "000000020000000000000001"}, {"short", "short"}} {
		if _, err := walRange(c[0], c[1], 16<<20); err == nil {
			t.Errorf("walRange(%v) should fail", c)
		}
	}
}

func TestNamesAndIDs(t *testing.T) {
	for _, ok := range []string{"a", "feature/login", "fix-1.2_x", "A1", strings.Repeat("a", 100)} {
		if err := ValidName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	id := secrets.NewUUID()
	if !IsUUID(id) || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
		t.Errorf("uuid = %s", id)
	}
	d := defaultBranchID("abcdefghijklmnopqrst")
	if d != defaultBranchID("abcdefghijklmnopqrst") || d == defaultBranchID("tsrqponmlkjihgfedcba") || d[14] != '5' || !IsUUID(d) {
		t.Errorf("default id = %s", d)
	}
	if IsUUID("abcdefghijklmnopqrst") || IsUUID("") {
		t.Error("a ref is not a uuid")
	}
}

// fakePGData builds a tiny data directory with the files a copy must keep and drop.
func fakePGData(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "data")
	files := map[string]string{
		"PG_VERSION": "17\n", "postgresql.conf": "x", "postgresql.auto.conf": "y",
		"base/1/1259": strings.Repeat("a", 5000), "base/5/16384": "rel", "base/5/t3_16385": "temp rel",
		"global/pg_control": "control", "global/1262": "g", "pg_wal/000000010000000000000001": "wal",
		"pg_stat_tmp/global.stat": "stat", "pg_replslot/slot1/state": "slot", "pg_subtrans/0000": "sub",
		"postmaster.pid": "123", "postmaster.opts": "opts", "backup_label": "old label", "base/pgsql_tmp/pgsql_tmp1": "tmp",
		"base/5/pg_internal.init": "init", "pg_xact/0000": "xact",
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCopyDataDirSkipsWhatABaseBackupSkips(t *testing.T) {
	src := fakePGData(t)
	dst := filepath.Join(t.TempDir(), "clone")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	st := &CloneStats{}
	cp := &copier{ctx: context.Background(), clone: cloneFile, byteCopy: true, stats: st}
	if err := cp.copyDataDir(src, dst); err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dst, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	want := []string{"PG_VERSION", "base/1/1259", "base/5/16384", "global/1262", "global/pg_control", "pg_xact/0000", "postgresql.auto.conf", "postgresql.conf"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("copied files:\n got %v\nwant %v", got, want)
	}
	// pg_wal and the runtime directories exist, empty (pg_wal with archive_status).
	for _, d := range []string{"pg_wal/archive_status", "pg_replslot", "pg_stat_tmp", "pg_subtrans"} {
		if fi, err := os.Stat(filepath.Join(dst, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(dst, "pg_replslot")); len(ents) != 0 {
		t.Errorf("pg_replslot not empty: %v", ents)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "base/1/1259")); len(b) != 5000 {
		t.Errorf("content lost: %d bytes", len(b))
	}
	if st.Files != len(want) || st.Cloned+st.Copied != st.Files {
		t.Errorf("stats = %+v", st)
	}
	// Changing the clone does not change the source (a clone is not a hard link).
	if err := os.WriteFile(filepath.Join(dst, "base/1/1259"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(src, "base/1/1259")); len(b) != 5000 {
		t.Fatal("writing to the clone changed the source")
	}
	// The same rules as the base backup's tar: every skipped name is one backup skips.
	for _, rel := range []string{"postmaster.pid", "base/5/t3_16385", "base/pgsql_tmp", "backup_label"} {
		if !backup.DataDirSkip(rel, strings.HasSuffix(rel, "pgsql_tmp")) {
			t.Errorf("%s should be skipped", rel)
		}
	}
}

func TestCopyRefusesUnsupportedTrees(t *testing.T) {
	src := fakePGData(t)
	if err := os.Symlink("/elsewhere", filepath.Join(src, "pg_tblspc_link")); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	cp := &copier{ctx: context.Background(), clone: cloneFile, byteCopy: true, stats: &CloneStats{}}
	if err := cp.copyDataDir(src, dst); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink: %v", err)
	}
	pending := fakePGData(t)
	_ = os.WriteFile(filepath.Join(pending, backup.DataDirInitPending), nil, 0o600)
	if err := cp.copyDataDir(pending, t.TempDir()); err == nil || !strings.Contains(err.Error(), "initialization") {
		t.Fatalf("init pending: %v", err)
	}
	// Without byte-copy a file that cannot be cloned is an error.
	strict := &copier{ctx: context.Background(), clone: func(string, string) error { return errNoClone }, stats: &CloneStats{}}
	if err := strict.copyDataDir(fakePGData(t), t.TempDir()); err == nil {
		t.Fatal("expected an error without byte copy")
	}
	// With byte-copy it falls back and counts the copies.
	st := &CloneStats{}
	fb := &copier{ctx: context.Background(), clone: func(string, string) error { return errNoClone }, byteCopy: true, stats: st}
	if err := fb.copyDataDir(fakePGData(t), t.TempDir()); err != nil || st.Copied == 0 || st.Cloned != 0 {
		t.Fatalf("fallback: %v %+v", err, st)
	}
}

func TestDetectCloneOnThisFilesystem(t *testing.T) {
	src := fakePGData(t)
	method, fsys, reason := detectClone(src, filepath.Join(filepath.Dir(src), "branch"))
	t.Logf("detectClone: method=%q fs=%s reason=%q", method, fsys, reason)
	if method == "" && reason == "" {
		t.Fatal("no method and no reason")
	}
	if method != "" && method != cloneMethodName {
		t.Fatalf("method = %q", method)
	}
	// No probe file may be left behind.
	if ents, _ := os.ReadDir(filepath.Join(filepath.Dir(src), "branch")); len(ents) != 0 {
		t.Fatalf("probe left %v", ents)
	}
}

func TestNotifyRefusesPrivateAddressesByDefault(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()

	h := newHarness(t, nil)
	b := h.create("n", func(in *CreateInput) { in.NotifyURL = srv.URL })
	h.mustState(b, registry.BranchMigrationsPassed)
	h.svc.Drain(context.Background()) // the notification is sent after the state shows
	if hits.Load() != 0 {
		t.Fatal("the default client reached a loopback address")
	}

	h2 := newHarness(t, func(c *config.Config) { c.Branching.AllowPrivateNotifyURLs = true })
	b2 := h2.create("n", func(in *CreateInput) { in.NotifyURL = srv.URL })
	h2.mustState(b2, registry.BranchMigrationsPassed)
	h2.svc.Drain(context.Background())
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 (allowed)", hits.Load())
	}
}

func TestInternalAddr(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "::1", "fd00::1", "fe80::1", "0.0.0.0",
		"100.64.0.1", "100.100.100.200", "100.127.255.255", "198.18.0.1", "198.19.255.255", "192.0.0.8", "240.0.0.1", "255.255.255.254", "64:ff9b::a00:1", "::ffff:100.100.100.200"} {
		if !internalAddr(netip.MustParseAddr(a).Unmap()) {
			t.Errorf("%s should be refused", a)
		}
	}
	for _, a := range []string{"8.8.8.8", "100.63.255.255", "100.128.0.1", "198.17.255.255", "198.20.0.1", "192.0.2.1", "2606:4700::1111"} {
		if internalAddr(netip.MustParseAddr(a).Unmap()) {
			t.Errorf("%s should be allowed", a)
		}
	}
}

func TestNotifyBody(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		body.Store(string(b[:n]))
	}))
	defer srv.Close()
	h := newHarness(t, func(c *config.Config) { c.Branching.AllowPrivateNotifyURLs = true })
	b := h.create("n", func(in *CreateInput) { in.NotifyURL = srv.URL })
	h.svc.Drain(context.Background()) // the notification is sent after the state shows
	got, _ := body.Load().(string)
	for _, want := range []string{`"branch_name":"n"`, `"operation":"create"`, `"status":"MIGRATIONS_PASSED"`, `"project_ref":"` + b.Ref + `"`, `"parent_project_ref":"` + parentRef + `"`} {
		if !strings.Contains(got, want) {
			t.Errorf("notification %q lacks %s", got, want)
		}
	}
}

func TestBranchingConfig(t *testing.T) {
	cases := map[string]struct {
		ttl string
		d   string
		ok  bool
		bad bool
	}{
		"default": {"", "168h0m0s", true, false}, "days": {"3d", "72h0m0s", true, false}, "hours": {"36h", "36h0m0s", true, false},
		"off": {"off", "0s", false, false}, "zero": {"0", "0s", false, false}, "junk": {"soon", "", false, true}, "neg": {"-1h", "", false, true},
	}
	for name, c := range cases {
		d, ok, err := config.Branching{DefaultTTL: c.ttl}.TTL()
		if (err != nil) != c.bad || (!c.bad && (d.String() != c.d || ok != c.ok)) {
			t.Errorf("%s: %v %v %v", name, d, ok, err)
		}
	}
	cfg := config.Default()
	cfg.Branching.DefaultTTL = "soon"
	if err := cfg.Validate(); err == nil {
		t.Error("bad ttl accepted by Validate")
	}
	cfg.Branching.DefaultTTL, cfg.Branching.Clone = "", "magic"
	if err := cfg.Validate(); err == nil {
		t.Error("bad clone mode accepted by Validate")
	}
	if pp, tot := (config.Branching{}).Limits(); pp != 10 || tot != 50 {
		t.Errorf("limits = %d %d", pp, tot)
	}
}
