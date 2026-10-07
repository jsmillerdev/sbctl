//go:build unix

package units

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/OWNER/sbctl/deploy/systemd"
	"github.com/OWNER/sbctl/internal/config"
)

func TestEnvRoundTrip(t *testing.T) {
	env := map[string]string{
		"PLAIN":      "abc",
		"JSON":       `[{"kty":"oct","k":"a\\b","note":"say \"hi\""}]`,
		"SPACES":     "a b  c # not a comment",
		"EMPTY":      "",
		"DOLLAR":     "$HOME `x` %i",
		"BACKSLASHN": `\n`,
	}
	b, err := FormatEnv(env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseEnv(b)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range env {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, err := FormatEnv(map[string]string{"A": "x\ny"}); err == nil {
		t.Error("newline must be rejected")
	}
	if _, err := FormatEnv(map[string]string{"1BAD": "x"}); err == nil {
		t.Error("bad name must be rejected")
	}
}

func TestFormatRun(t *testing.T) {
	b, err := FormatRun(Spec{Service: config.SvcGoTrue, ArtifactDir: "/art/auth", WorkDir: "/w d",
		PreStart: [][]string{{"bin/auth", "migrate"}}, Exec: []string{"bin/auth", "it's"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "cd '/w d'\n'/art/auth/bin/auth' 'migrate'\nexec '/art/auth/bin/auth' 'it'\\''s'\n"
	if !strings.Contains(string(b), want) {
		t.Fatalf("run script:\n%s\nwant to contain:\n%s", b, want)
	}
	b, err = FormatRun(Spec{Service: config.SvcEdgeBundle, ArtifactDir: "/art/edge", WorkDir: "/w", Log: "/w/it's.log", Exec: []string{"bin/edge-runtime", "bundle"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "cd '/w'\nexec >'/w/it'\\''s.log' 2>&1\nexec '/art/edge/bin/edge-runtime' 'bundle'\n"; !strings.Contains(string(b), want) {
		t.Fatalf("run script with a log:\n%s\nwant to contain:\n%s", b, want)
	}
	if _, err := FormatRun(Spec{Service: "postgres", ArtifactDir: "/a", Exec: []string{"/bin/sh"}}); err == nil {
		t.Error("absolute command must be rejected")
	}
	if _, err := FormatRun(Spec{Service: "postgres", ArtifactDir: "/a", Exec: []string{"../x"}}); err == nil {
		t.Error("escaping command must be rejected")
	}
	if _, err := FormatRun(Spec{Service: "nope", ArtifactDir: "/a"}); err == nil {
		t.Error("unknown launcher must be rejected")
	}
}

func TestParseUnitAndLimits(t *testing.T) {
	svc, ref, err := ParseUnit("sb-edge-runtime.service")
	if err != nil || svc != "edge-runtime" || ref != "" {
		t.Fatal(svc, ref, err)
	}
	svc, ref, err = ParseUnit("sb-postgres@abcdefghijklmnopqrst.service")
	if err != nil || svc != "postgres" || ref != "abcdefghijklmnopqrst" {
		t.Fatal(svc, ref, err)
	}
	if _, _, err := ParseUnit("nginx.service"); err == nil {
		t.Fatal("non-sbctl unit")
	}
	if n, _ := ParseBytes("1G"); n != 1<<30 {
		t.Fatal(n)
	}
	if n, _ := ParseBytes("512M"); n != 512<<20 {
		t.Fatal(n)
	}
	if n, _ := ParseBytes(""); n != ^uint64(0) {
		t.Fatal(n)
	}
	if _, err := ParseBytes("lots"); err == nil {
		t.Fatal("bad size")
	}
	if n, _ := ParseCPUQuota("150%"); n != 1_500_000 {
		t.Fatal(n)
	}
	if _, err := ParseCPUQuota("2"); err == nil {
		t.Fatal("bad quota")
	}
}

// Every embedded template must run the files FilesFor renders, so the systemd and exec
// backends agree on layout.
func TestTemplatesMatchLayout(t *testing.T) {
	cfg := config.Default() // state_dir /var/lib/sbctl, the path the templates hard-code
	check := func(name, svc, ref string) {
		t.Helper()
		b, err := systemd.Read(name)
		if err != nil {
			t.Fatal(err)
		}
		f := FilesFor(cfg, Spec{Service: svc, Ref: ref})
		tmplRef := ref
		if ref != "" && ref != config.SystemRef {
			tmplRef = "%i"
		}
		for _, line := range []string{
			"EnvironmentFile=" + strings.Replace(f.Env, "/"+ref+"/", "/"+tmplRef+"/", 1),
			"ExecStart=" + strings.Replace(f.Run, "/"+ref+"/", "/"+tmplRef+"/", 1),
		} {
			if ref == "" {
				line = strings.Replace(line, "/system/", "/system/", 1)
			}
			if !strings.Contains(string(b), line+"\n") {
				t.Errorf("%s lacks %q", name, line)
			}
		}
		if !strings.Contains(string(b), "User=sbctl") || !strings.Contains(string(b), "Slice=sbctl.slice") {
			t.Errorf("%s must run as sbctl in sbctl.slice", name)
		}
	}
	check("sb-postgres@.service", config.SvcPostgres, "abcdefghijklmnopqrst")
	check("sb-gotrue@.service", config.SvcGoTrue, "abcdefghijklmnopqrst")
	check("sb-postgrest@.service", config.SvcPostgREST, "abcdefghijklmnopqrst")
	for _, s := range []string{"supavisor", "realtime", "storage", "pgmeta", "studio", "imgproxy", "edge-runtime"} {
		check("sb-"+s+".service", s, "")
	}
	for _, name := range []string{"sbctl.slice", "sbctl.service", "50-sbctl.rules"} {
		if _, err := systemd.Read(name); err != nil {
			t.Error(err)
		}
	}
}

func TestInstallTemplates(t *testing.T) {
	d, p := t.TempDir(), t.TempDir()
	changed, err := systemd.Install(d, p)
	if err != nil || len(changed) != len(systemd.Names()) {
		t.Fatalf("first install: %v, %d changed", err, len(changed))
	}
	if changed, err := systemd.Install(d, p); err != nil || len(changed) != 0 {
		t.Fatalf("second install must be a no-op: %v %v", err, changed)
	}
	if _, err := os.Stat(filepath.Join(p, "50-sbctl.rules")); err != nil {
		t.Fatal(err)
	}
	// The daemon starts these through D-Bus; a missing file is "Unit not found" at runtime.
	for _, name := range []string{"sb-basebackup@.timer", "sb-basebackup-prune.timer", "sb-basebackup@.service", "sb-basebackup-prune.service"} {
		if _, err := os.Stat(filepath.Join(d, name)); err != nil {
			t.Errorf("install-units does not install %s: %v", name, err)
		}
	}
}

// fakeArtifact writes a launcher script into an artifact dir.
func fakeArtifact(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin/run"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func execBackend(t *testing.T) (*Exec, *config.Config) {
	t.Helper()
	cfg := config.Default()
	// Unix socket paths and pid files stay short on macOS.
	d, err := os.MkdirTemp("/tmp", "sbu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	cfg.StateDir = d
	cfg.Supervisor = config.SupervisorExec
	e := NewExec(cfg, nil)
	e.StopTimeout, e.PostgresStopTimeout = 3*time.Second, 3*time.Second
	return e, cfg
}

func waitState(t *testing.T, e *Exec, unit string, want State) Status {
	t.Helper()
	var st Status
	for i := 0; i < 100; i++ {
		st, _ = e.Status(context.Background(), unit)
		if st.State == want {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s state = %s, want %s", unit, st.State, want)
	return st
}

func TestExecBackendLifecycle(t *testing.T) {
	e, cfg := execBackend(t)
	ctx := context.Background()
	art := fakeArtifact(t, `echo "started $MARK pid $$"; sleep 300 & sleep 300 & wait`)
	spec := Spec{Service: config.SvcGoTrue, Ref: "abcdefghijklmnopqrst", ArtifactDir: art, Exec: []string{"bin/run"},
		WorkDir: filepath.Join(cfg.StateDir, "w"), Env: map[string]string{"MARK": `x "y"`}}
	os.MkdirAll(spec.WorkDir, 0o755)
	unit := spec.Unit()

	if st, _ := e.Status(ctx, unit); st.State != StateInactive {
		t.Fatalf("before start: %s", st.State)
	}
	if err := e.Render(ctx, spec); err != nil {
		t.Fatal(err)
	}
	files := FilesFor(cfg, spec)
	if fi, err := os.Stat(files.Env); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("env file: %v %v", fi, err)
	}
	// Re-rendering an unchanged spec must not touch the files.
	before, _ := os.Stat(files.Run)
	time.Sleep(20 * time.Millisecond)
	if err := e.Render(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(files.Run); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("unchanged render rewrote the run script")
	}

	if err := e.Start(ctx, unit); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, e, unit, StateActive)
	if st.MainPID == 0 {
		t.Fatal("no main pid")
	}
	if err := e.Start(ctx, unit); err != nil { // idempotent
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if b, _ := os.ReadFile(e.LogPath(unit)); !strings.Contains(string(b), `started x "y"`) {
		t.Fatalf("log = %q", b)
	}
	if st, _ := e.Status(ctx, unit); st.MemoryBytes == 0 {
		t.Error("expected a nonzero process group RSS")
	}

	pid := st.MainPID
	if err := e.Stop(ctx, unit); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, unit, StateInactive)
	if pidAlive(pid) {
		t.Fatal("main process survived Stop")
	}
	// The background sleeps shared the process group and must be gone too.
	if err := syscall.Kill(-pid, 0); err == nil {
		t.Fatalf("process group %d still has members", pid)
	}

	if err := e.Remove(ctx, unit); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(files.Env); err == nil {
		t.Fatal("Remove left the env file")
	}
}

func TestExecPostgresStopUsesSIGINT(t *testing.T) {
	e, cfg := execBackend(t)
	ctx := context.Background()
	// Exits cleanly only on SIGINT, like a postmaster's fast shutdown.
	art := fakeArtifact(t, `trap 'echo got-int; exit 0' INT; trap '' TERM; echo ready; while true; do sleep 0.1; done`)
	spec := Spec{Service: config.SvcPostgres, Ref: "abcdefghijklmnopqrst", ArtifactDir: art, Exec: []string{"bin/run"}, WorkDir: cfg.StateDir}
	if err := e.Render(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(ctx, spec.Unit()); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, spec.Unit(), StateActive)
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if err := e.Stop(ctx, spec.Unit()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("SIGINT should have stopped it promptly")
	}
	if b, _ := os.ReadFile(e.LogPath(spec.Unit())); !strings.Contains(string(b), "got-int") {
		t.Fatalf("postgres-like process did not receive SIGINT: %q", b)
	}
}

func TestExecKillsStubbornProcess(t *testing.T) {
	e, cfg := execBackend(t)
	ctx := context.Background()
	art := fakeArtifact(t, `trap '' TERM; while true; do sleep 0.1; done`)
	spec := Spec{Service: config.SvcPostgREST, Ref: "abcdefghijklmnopqrst", ArtifactDir: art, Exec: []string{"bin/run"}, WorkDir: cfg.StateDir}
	e.Render(ctx, spec)
	if err := e.Start(ctx, spec.Unit()); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, e, spec.Unit(), StateActive)
	if err := e.Stop(ctx, spec.Unit()); err != nil {
		t.Fatal(err)
	}
	if pidAlive(st.MainPID) {
		t.Fatal("SIGKILL fallback did not run")
	}
}

func TestExecStartFailureReportsLog(t *testing.T) {
	e, cfg := execBackend(t)
	ctx := context.Background()
	e.StartGrace = 5 * time.Second // a loaded CI machine can be slow to spawn sh
	art := fakeArtifact(t, `echo "boom: cannot bind" >&2; exit 3`)
	spec := Spec{Service: config.SvcGoTrue, Ref: "abcdefghijklmnopqrst", ArtifactDir: art, Exec: []string{"bin/run"}, WorkDir: cfg.StateDir}
	e.Render(ctx, spec)
	err := e.Start(ctx, spec.Unit())
	if err == nil || !strings.Contains(err.Error(), "boom: cannot bind") {
		t.Fatalf("err = %v", err)
	}
}

func TestExecDetectsCrash(t *testing.T) {
	e, cfg := execBackend(t)
	ctx := context.Background()
	art := fakeArtifact(t, `sleep 0.6; exit 1`)
	spec := Spec{Service: config.SvcGoTrue, Ref: "abcdefghijklmnopqrst", ArtifactDir: art, Exec: []string{"bin/run"}, WorkDir: cfg.StateDir}
	e.Render(ctx, spec)
	if err := e.Start(ctx, spec.Unit()); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, spec.Unit(), StateFailed)
	// Start recovers a failed unit.
	if err := e.Start(ctx, spec.Unit()); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, spec.Unit(), StateActive)
	e.Stop(ctx, spec.Unit())
}

// A second Exec instance (another sbctl invocation) can stop what the first started.
func TestExecAcrossProcesses(t *testing.T) {
	e1, cfg := execBackend(t)
	ctx := context.Background()
	art := fakeArtifact(t, `exec sleep 300`)
	spec := Spec{Service: config.SvcPostgREST, Ref: "abcdefghijklmnopqrst", ArtifactDir: art, Exec: []string{"bin/run"}, WorkDir: cfg.StateDir}
	e1.Render(ctx, spec)
	if err := e1.Start(ctx, spec.Unit()); err != nil {
		t.Fatal(err)
	}
	e2 := NewExec(cfg, nil)
	st := waitState(t, e2, spec.Unit(), StateActive)
	if err := e2.Stop(ctx, spec.Unit()); err != nil {
		t.Fatal(err)
	}
	if pidAlive(st.MainPID) {
		t.Fatal("not stopped")
	}
}

// Every template, Postgres included, must replace /var/lib/sbctl with an empty tmpfs and
// bring back only the artifacts, its own launcher script and its own state directory
// (an allowlist). Binding a project or system directory as a whole would expose the
// siblings' environment files and cluster directories.
func TestTemplatesContainment(t *testing.T) {
	var names []string
	for _, s := range []string{"postgres@", "gotrue@", "postgrest@"} {
		names = append(names, "sb-"+s+".service")
	}
	for _, s := range []string{"supavisor", "realtime", "storage", "pgmeta", "studio", "imgproxy", "edge-runtime"} {
		names = append(names, "sb-"+s+".service")
	}
	line := func(body, key string) string {
		for _, l := range strings.Split(body, "\n") {
			if v, ok := strings.CutPrefix(l, key+"="); ok {
				return v
			}
		}
		return ""
	}
	for _, name := range names {
		b, err := systemd.Read(name)
		if err != nil {
			t.Fatal(err)
		}
		body := string(b)
		svc := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(name, ".service"), "sb-"), "@")
		if !strings.Contains(body, "TemporaryFileSystem=/var/lib/sbctl:ro\n") {
			t.Errorf("%s lacks the tmpfs over /var/lib/sbctl", name)
		}
		if !strings.Contains(body, "InaccessiblePaths=-/etc/sbctl") {
			t.Errorf("%s does not hide /etc/sbctl or the master key", name)
		}
		ro := strings.Fields(line(body, "BindReadOnlyPaths"))
		if len(ro) != 2 || ro[0] != "/var/lib/sbctl/artifacts" || !strings.HasSuffix(ro[1], "/"+svc+".run") {
			t.Errorf("%s: BindReadOnlyPaths must be the artifacts and its own launcher script, got %v", name, ro)
		}
		for _, p := range strings.Fields(line(body, "BindPaths")) {
			switch strings.TrimPrefix(p, "-") {
			case "/var/lib/sbctl/projects/%i", "/var/lib/sbctl/projects/system", "/var/lib/sbctl/projects", "/var/lib/sbctl/backups",
				"/var/lib/sbctl/certs", "/var/lib/sbctl/system", "/var/lib/sbctl":
				t.Errorf("%s binds %s as a whole", name, p)
			}
		}
		if strings.Contains(body, "/var/lib/sbctl/certs") {
			t.Errorf("%s must not mention the certificate directory", name)
		}
		if strings.Contains(line(body, "InaccessiblePaths"), ".env") {
			t.Errorf("%s still hides sibling environment files (denylist); use the allowlist", name)
		}
		if svc == "postgres" && line(body, "InaccessiblePaths") != "-/etc/sbctl/master.key" {
			t.Errorf("%s must hide the master key only (config.toml is read by archive_command)", name)
		}
	}
}

// A launcher that a unit under another uid executes (sb-edge-bundle.service, a dynamic user) is
// world-readable; every other launcher stays private to the sbctl user. The environment file is
// 0600 either way: systemd reads it as root.
func TestRenderPublicRunMode(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	for _, public := range []bool{false, true} {
		spec := Spec{Service: config.SvcEdgeBundle, ArtifactDir: "/art", Exec: []string{"bin/edge-runtime", "bundle"}, Env: map[string]string{"K": "secret"}, PublicRun: public}
		if _, err := renderFiles(cfg, spec); err != nil {
			t.Fatal(err)
		}
		f := FilesFor(cfg, spec)
		want := os.FileMode(0o750)
		if public {
			want = 0o755
		}
		for p, w := range map[string]os.FileMode{f.Run: want, f.Env: 0o600} {
			if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != w {
				t.Errorf("public=%v %s: %v %v, want %v", public, p, fi, err, w)
			}
		}
	}
}
