package functions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/OWNER/sbctl/internal/api"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

const (
	refA = "aaaaaaaaaaaaaaaaaaaa"
	refB = "bbbbbbbbbbbbbbbbbbbb"
)

type env struct {
	t     *testing.T
	cfg   *config.Config
	reg   *registry.Memory
	store *api.MemoryStore
	sec   secrets.Secrets
	s     *Syncer

	mu   sync.Mutex
	keys map[string]*secrets.ProjectKeys
	now  time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Domain = "example.test"
	cfg.TLS.Mode = "off"
	e := &env{t: t, cfg: cfg, reg: registry.NewMemory(), store: api.NewMemoryStore(), sec: sec,
		keys: map[string]*secrets.ProjectKeys{}, now: time.Now()}
	s, err := New(Deps{Cfg: cfg, Registry: e.reg, Secrets: sec, Store: e.store, Now: func() time.Time {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.now
	}, Keys: func(_ context.Context, ref string) (*secrets.ProjectKeys, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if k, ok := e.keys[ref]; ok {
			return k, nil
		}
		return nil, registry.ErrNotFound
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.s = s
	return e
}

func (e *env) addProject(ref string, status registry.Status) *registry.Project {
	e.t.Helper()
	p := &registry.Project{Ref: ref, Name: ref, Status: status, Engine: registry.EnginePostgres, Region: "local"}
	if err := e.reg.CreateProject(context.Background(), p); err != nil {
		e.t.Fatal(err)
	}
	k, err := secrets.NewProjectKeys(ref, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	e.mu.Lock()
	e.keys[ref] = k
	e.mu.Unlock()
	if err := os.MkdirAll(e.cfg.Paths().Project(ref), 0o750); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) deploy(ref, slug string, verify bool, files map[string]string) *api.Function {
	e.t.Helper()
	f := &api.Function{Ref: ref, Slug: slug, Name: slug, Status: "ACTIVE", VerifyJWT: verify, EntrypointPath: "supabase/functions/" + slug + "/index.ts"}
	var ff []api.FunctionFile
	for p, c := range files {
		ff = append(ff, api.FunctionFile{Path: p, Content: []byte(c)})
	}
	if err := e.store.UpsertFunction(context.Background(), f, ff); err != nil {
		e.t.Fatal(err)
	}
	return f
}

func (e *env) sync(ref string) {
	e.t.Helper()
	if err := e.s.SyncProject(context.Background(), ref); err != nil {
		e.t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readEnvDoc(t *testing.T, cfg *config.Config, ref string) envDoc {
	t.Helper()
	var d envDoc
	if err := json.Unmarshal([]byte(readFile(t, EnvPath(cfg, ref))), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func gens(t *testing.T, cfg *config.Config, ref string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(FunctionsDir(cfg, ref), genDirName))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestSyncWritesTheEnvironmentFile(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	sealed, _ := e.sec.Seal([]byte("line one\nline two"))
	sealed2, _ := e.sec.Seal([]byte("v2"))
	if err := e.store.PutFunctionSecrets(context.Background(), refA, map[string][]byte{"MULTILINE": sealed, "OTHER": sealed2}); err != nil {
		t.Fatal(err)
	}
	e.sync(refA)

	fi, err := os.Stat(EnvPath(e.cfg, refA))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("env file mode %v", fi.Mode().Perm())
	}
	d := readEnvDoc(t, e.cfg, refA)
	k := e.keys[refA]
	if d.Version != 1 || d.JWTSecret != k.JWTSecret {
		t.Fatalf("doc %+v", d)
	}
	for name, want := range map[string]string{
		"SUPABASE_URL":              "http://" + refA + ".api.example.test",
		"SUPABASE_ANON_KEY":         k.AnonKey,
		"SUPABASE_SERVICE_ROLE_KEY": k.ServiceRoleKey,
		"SUPABASE_PUBLISHABLE_KEYS": `{"default":"` + k.PublishableKey + `"}`,
		"SUPABASE_SECRET_KEYS":      `{"default":"` + k.SecretKey + `"}`,
	} {
		if d.Supabase[name] != want {
			t.Errorf("%s = %q, want %q", name, d.Supabase[name], want)
		}
	}
	port := e.cfg.PortsFor(refA, 1).Postgres
	if !strings.HasPrefix(d.Supabase["SUPABASE_DB_URL"], "postgres://postgres:") ||
		!strings.Contains(d.Supabase["SUPABASE_DB_URL"], "@127.0.0.1:"+itoa(port)+"/postgres") ||
		!strings.Contains(d.Supabase["SUPABASE_DB_URL"], k.DBPassword) {
		t.Errorf("db url %q (port %d)", d.Supabase["SUPABASE_DB_URL"], port)
	}
	if d.Secrets["MULTILINE"] != "line one\nline two" || d.Secrets["OTHER"] != "v2" {
		t.Errorf("secrets %v", d.Secrets)
	}
	// Nothing of the system project or another project.
	if _, err := os.Stat(EnvPath(e.cfg, refB)); err == nil {
		t.Fatal("env file of a project that does not exist")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestSyncMaterializesAFunctionAndSwapsGenerations(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	code := "supabase/functions/hello/index.ts"
	e.deploy(refA, "hello", true, map[string]string{code: "v1", "supabase/functions/_shared/cors.ts": "export {}"})
	e.sync(refA)

	link := FunctionPath(e.cfg, refA, "hello")
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("function path is not a symlink: %v %v", fi, err)
	}
	if got := readFile(t, filepath.Join(link, code)); got != "v1" {
		t.Fatalf("source %q", got)
	}
	if got := readFile(t, filepath.Join(link, "supabase/functions/_shared/cors.ts")); got != "export {}" {
		t.Fatalf("shared file %q", got)
	}
	m, ok := liveMeta(link)
	if !ok || m.Slug != "hello" || m.Version != 1 || !m.VerifyJWT || m.Entrypoint != code || m.SHA256 == "" {
		t.Fatalf("meta %+v", m)
	}
	first, _ := os.Readlink(link)

	// Unchanged: nothing is rewritten.
	e.sync(refA)
	if again, _ := os.Readlink(link); again != first {
		t.Fatal("an unchanged function got a new generation")
	}

	// A new deployment is a new generation behind the same path; the old one stays for
	// workers that are still answering from it.
	e.deploy(refA, "hello", false, map[string]string{code: "v2"})
	e.sync(refA)
	second, _ := os.Readlink(link)
	if second == first {
		t.Fatal("the link was not swapped")
	}
	if got := readFile(t, filepath.Join(link, code)); got != "v2" {
		t.Fatalf("source after redeploy %q", got)
	}
	if m, _ := liveMeta(link); m.Version != 2 || m.VerifyJWT {
		t.Fatalf("meta after redeploy %+v", m)
	}
	if n := len(gens(t, e.cfg, refA)); n != 2 {
		t.Fatalf("generations after one redeploy: %d, want 2", n)
	}
	if got := readFile(t, filepath.Join(FunctionsDir(e.cfg, refA), first, code)); got != "v1" {
		t.Fatalf("previous generation: %q", got)
	}

	// A third one drops the first.
	e.deploy(refA, "hello", true, map[string]string{code: "v3"})
	e.sync(refA)
	if n := len(gens(t, e.cfg, refA)); n != 2 {
		t.Fatalf("generations after two redeploys: %d, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(FunctionsDir(e.cfg, refA), first)); err == nil {
		t.Fatal("the oldest generation was kept")
	}

	// A metadata-only change (PATCH verify_jwt) takes effect too.
	f, _ := e.store.GetFunction(context.Background(), refA, "hello")
	f.VerifyJWT = false
	if err := e.store.UpsertFunction(context.Background(), f, nil); err != nil {
		t.Fatal(err)
	}
	e.sync(refA)
	if m, _ := liveMeta(link); m.VerifyJWT || m.Version != 4 {
		t.Fatalf("meta after patch %+v", m)
	}
}

func TestSwapIsAtomicForReaders(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	code := "supabase/functions/hello/index.ts"
	e.deploy(refA, "hello", true, map[string]string{code: "version-1"})
	e.sync(refA)
	link := FunctionPath(e.cfg, refA, "hello")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	bad := make(chan string, 1)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b, err := os.ReadFile(filepath.Join(link, code))
				if errors.Is(err, syscall.EINVAL) {
					// macOS can fail a walk through a symlink that is replaced at that
					// very moment (EINVAL, never ENOENT); the Deno main service retries.
					continue
				}
				if err != nil || !strings.HasPrefix(string(b), "version-") {
					select {
					case bad <- "reader saw " + string(b) + " / " + errString(err):
					default:
					}
					return
				}
			}
		}()
	}
	for v := 2; v <= 30; v++ {
		e.deploy(refA, "hello", true, map[string]string{code: "version-" + itoa(v)})
		e.sync(refA)
	}
	close(stop)
	wg.Wait()
	select {
	case msg := <-bad:
		t.Fatal(msg)
	default:
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func TestTwoProjectsKeepTheirOwnFunctionsAndSecrets(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	e.addProject(refB, registry.StatusActiveHealthy)
	code := "supabase/functions/hello/index.ts"
	e.deploy(refA, "hello", true, map[string]string{code: "code of A"})
	e.deploy(refB, "hello", true, map[string]string{code: "code of B"})
	sa, _ := e.sec.Seal([]byte("secret of A"))
	sb, _ := e.sec.Seal([]byte("secret of B"))
	_ = e.store.PutFunctionSecrets(context.Background(), refA, map[string][]byte{"TOKEN": sa})
	_ = e.store.PutFunctionSecrets(context.Background(), refB, map[string][]byte{"TOKEN": sb})
	if err := e.s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(FunctionPath(e.cfg, refA, "hello"), code)) != "code of A" ||
		readFile(t, filepath.Join(FunctionPath(e.cfg, refB, "hello"), code)) != "code of B" {
		t.Fatal("projects share code")
	}
	da, db := readEnvDoc(t, e.cfg, refA), readEnvDoc(t, e.cfg, refB)
	if da.Secrets["TOKEN"] != "secret of A" || db.Secrets["TOKEN"] != "secret of B" {
		t.Fatalf("secrets: %v / %v", da.Secrets, db.Secrets)
	}
	if da.JWTSecret == db.JWTSecret || da.Supabase["SUPABASE_ANON_KEY"] == db.Supabase["SUPABASE_ANON_KEY"] {
		t.Fatal("projects share keys")
	}
	for _, v := range da.Secrets {
		if strings.Contains(v, "of B") {
			t.Fatal("A holds a secret of B")
		}
	}
	// The files of A contain nothing of B.
	for _, p := range []string{EnvPath(e.cfg, refA), FunctionsDir(e.cfg, refA)} {
		_ = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if b, _ := os.ReadFile(path); strings.Contains(string(b), "of B") || strings.Contains(string(b), db.JWTSecret) {
				t.Errorf("%s holds data of project B", path)
			}
			return nil
		})
	}
}

func TestDeleteRemovesTheFunctionAndItsGenerations(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	code := "supabase/functions/hello/index.ts"
	e.deploy(refA, "hello", true, map[string]string{code: "1"})
	e.deploy(refA, "hello-2", true, map[string]string{"supabase/functions/hello-2/index.ts": "other"})
	e.sync(refA)
	e.deploy(refA, "hello", true, map[string]string{code: "2"})
	e.sync(refA)
	if err := e.store.DeleteFunction(context.Background(), refA, "hello"); err != nil {
		t.Fatal(err)
	}
	e.sync(refA)
	if _, err := os.Lstat(FunctionPath(e.cfg, refA, "hello")); err == nil {
		t.Fatal("deleted function is still served")
	}
	left := gens(t, e.cfg, refA)
	if len(left) != 1 || !strings.HasPrefix(left[0], "hello-2.") {
		t.Fatalf("generations left: %v (the generations of hello-2 must survive deleting hello)", left)
	}
	if got := readFile(t, filepath.Join(FunctionPath(e.cfg, refA, "hello-2"), "supabase/functions/hello-2/index.ts")); got != "other" {
		t.Fatalf("hello-2 damaged: %q", got)
	}
}

func TestOrphansAndInterruptedSwapsAreCleanedUp(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	e.deploy(refA, "hello", true, map[string]string{"supabase/functions/hello/index.ts": "1"})
	e.sync(refA)
	dir := FunctionsDir(e.cfg, refA)
	orphan := filepath.Join(dir, genDirName, "gone.7.abc")
	partial := filepath.Join(dir, ".hello.new-123456")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere", partial); err != nil {
		t.Fatal(err)
	}
	e.sync(refA)
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("a fresh orphan generation was removed before its grace period")
	}
	e.mu.Lock()
	e.now = e.now.Add(genGrace + time.Hour)
	e.mu.Unlock()
	old := time.Now().Add(-2 * genGrace)
	_ = os.Chtimes(orphan, old, old)
	e.sync(refA)
	if _, err := os.Stat(orphan); err == nil {
		t.Fatal("an old orphan generation was kept")
	}
	if _, err := os.Lstat(partial); err == nil {
		t.Fatal("a leftover of an interrupted swap was kept")
	}
	if _, err := os.Stat(filepath.Join(FunctionPath(e.cfg, refA, "hello"), "supabase/functions/hello/index.ts")); err != nil {
		t.Fatalf("the live function was damaged: %v", err)
	}
}

func TestBadUploadsAreRefused(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	ctx := context.Background()
	cases := map[string]struct {
		entry string
		files map[string]string
		want  string
	}{
		"traversal":      {"index.ts", map[string]string{"index.ts": "x", "../escape.ts": "y"}, "invalid file path"},
		"absolute":       {"index.ts", map[string]string{"index.ts": "x", "/etc/passwd": "y"}, "invalid file path"},
		"meta file name": {"index.ts", map[string]string{"index.ts": "x", MetaFileName: "{}"}, "cannot contain"},
		"no entrypoint":  {"main.ts", map[string]string{"index.ts": "x"}, "not among the uploaded files"},
		"bad entrypoint": {"../x.ts", map[string]string{"index.ts": "x"}, "entrypoint"},
	}
	for name, c := range cases {
		f := &api.Function{Ref: refA, Slug: "bad", Name: "bad", Status: "ACTIVE", VerifyJWT: true, EntrypointPath: c.entry}
		var ff []api.FunctionFile
		for p, content := range c.files {
			ff = append(ff, api.FunctionFile{Path: p, Content: []byte(content)})
		}
		if err := e.store.UpsertFunction(ctx, f, ff); err != nil {
			t.Fatal(err)
		}
		err := e.s.SyncProject(ctx, refA)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want one containing %q", name, err, c.want)
		}
		if _, err := os.Lstat(FunctionPath(e.cfg, refA, "bad")); err == nil {
			t.Errorf("%s: a refused upload went live", name)
		}
		if _, err := os.Stat(filepath.Join(e.cfg.Paths().Project(refA), "escape.ts")); err == nil {
			t.Errorf("%s: wrote outside the generation", name)
		}
		if n := len(gens(t, e.cfg, refA)); n != 0 {
			t.Errorf("%s: %d generations left behind", name, n)
		}
	}
	// One broken function does not keep the others from going live.
	e.deploy(refA, "fine", true, map[string]string{"supabase/functions/fine/index.ts": "ok"})
	if err := e.s.SyncProject(ctx, refA); err == nil {
		t.Fatal("expected the bad function to be reported")
	}
	if _, err := os.Stat(filepath.Join(FunctionPath(e.cfg, refA, "fine"), "supabase/functions/fine/index.ts")); err != nil {
		t.Fatalf("the good function is not live: %v", err)
	}
}

func TestFunctionWithoutSourcesIsNotServed(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	f := &api.Function{Ref: refA, Slug: "legacy", Name: "legacy", Status: "ACTIVE", VerifyJWT: true}
	if err := e.store.UpsertFunction(context.Background(), f, nil); err != nil {
		t.Fatal(err)
	}
	e.sync(refA)
	if _, err := os.Lstat(FunctionPath(e.cfg, refA, "legacy")); err == nil {
		t.Fatal("a function without sources is served")
	}
}

func TestReconcileFollowsKeyRotationAndSkipsWhatIsNotAProject(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	e.addProject("cccccccccccccccccccc", registry.StatusInitFailed)
	e.addProject("dddddddddddddddddddd", registry.StatusInactive) // paused: still gets its files
	e.addProject(config.SystemRef, registry.StatusActiveHealthy)
	if err := e.s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(EnvPath(e.cfg, "cccccccccccccccccccc")); err == nil {
		t.Error("a failed project got files")
	}
	if _, err := os.Stat(EnvPath(e.cfg, config.SystemRef)); err == nil {
		t.Error("the system project got files")
	}
	if _, err := os.Stat(EnvPath(e.cfg, "dddddddddddddddddddd")); err != nil {
		t.Error("a paused project got no files")
	}
	before := readEnvDoc(t, e.cfg, refA)
	k, err := secrets.NewProjectKeys(refA, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.keys[refA] = k
	e.mu.Unlock()
	if err := e.s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := readEnvDoc(t, e.cfg, refA)
	if after.JWTSecret == before.JWTSecret || after.JWTSecret != k.JWTSecret {
		t.Fatal("rotated keys did not reach the environment file")
	}
}

func TestSyncIgnoresMissingProjectsAndBadRefs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.s.SyncProject(ctx, refA); err != nil {
		t.Fatalf("unknown project: %v", err)
	}
	e.addProject(refB, registry.StatusActiveHealthy)
	if err := os.RemoveAll(e.cfg.Paths().Project(refB)); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SyncProject(ctx, refB); err != nil {
		t.Fatalf("project without a directory: %v", err)
	}
	if _, err := os.Stat(e.cfg.Paths().Project(refB)); err == nil {
		t.Fatal("SyncProject created the project directory")
	}
	for _, bad := range []string{"", "system", "../etc", "AAAAAAAAAAAAAAAAAAAA"} {
		if err := e.s.SyncProject(ctx, bad); err == nil {
			t.Errorf("ref %q accepted", bad)
		}
	}
}

func TestRemoveProject(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	e.deploy(refA, "hello", true, map[string]string{"supabase/functions/hello/index.ts": "1"})
	e.sync(refA)
	if err := e.s.RemoveProject(refA); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{EnvPath(e.cfg, refA), FunctionsDir(e.cfg, refA)} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s survived", p)
		}
	}
	if _, err := os.Stat(e.cfg.Paths().Project(refA)); err != nil {
		t.Fatal("the project directory itself is not ours to remove")
	}
}

func TestProjectURL(t *testing.T) {
	cfg := config.Default()
	cfg.Domain = "example.test"
	cases := []struct {
		name  string
		mod   func()
		wantU string
	}{
		{"default https", func() {}, "https://" + refA + ".api.example.test"},
		{"tls off on port 80", func() { cfg.TLS.Mode = "off" }, "http://" + refA + ".api.example.test"},
		{"tls off on another port", func() { cfg.Listen.HTTP = "127.0.0.1:40080" }, "http://" + refA + ".api.example.test:40080"},
		{"tls on another port", func() { cfg.TLS.Mode = "auto"; cfg.Listen.HTTPS = ":8443" }, "https://" + refA + ".api.example.test:8443"},
		{"template", func() { cfg.Functions.ProjectURLTemplate = "http://{ref}.x.test:1" }, "http://" + refA + ".x.test:1"},
	}
	for _, c := range cases {
		c.mod()
		if got := ProjectURL(cfg, refA); got != c.wantU {
			t.Errorf("%s: %q, want %q", c.name, got, c.wantU)
		}
	}
	empty := config.Default()
	if ProjectURL(empty, refA) != "" {
		t.Error("no domain must give no URL")
	}
}

func TestNewNeedsItsDependencies(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Fatal("New accepted empty Deps")
	}
}

// testdata/hello.ezbr is the body `supabase functions deploy` (CLI 2.119.0) sent for a
// one-file function: "EZBR" and a Brotli-compressed eszip.
func helloBundle(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/hello.ezbr")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeBundle(t *testing.T) {
	out, err := decodeBundle(helloBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "ESZIP2") || len(out) < 1000 {
		t.Fatalf("decoded %d bytes starting %q", len(out), out[:8])
	}
	for name, in := range map[string][]byte{
		"no magic":   []byte("ESZIP2.3 plain"),
		"not brotli": append([]byte("EZBR"), []byte("this is not a brotli stream at all, surely")...),
		"not eszip":  nil,
	} {
		if name == "not eszip" {
			// a valid Brotli stream whose content is not an eszip
			var buf bytes.Buffer
			w := brotli.NewWriter(&buf)
			_, _ = w.Write([]byte("hello world"))
			_ = w.Close()
			in = append([]byte("EZBR"), buf.Bytes()...)
		}
		if _, err := decodeBundle(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSyncMaterializesABundledFunction(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	entry := "file:///private/tmp/work/supabase/functions/hello/index.ts"
	f := &api.Function{Ref: refA, Slug: "hello", Name: "hello", Status: "ACTIVE", VerifyJWT: false, EntrypointPath: entry, ImportMapPath: "file:///x/deno.json"}
	files := []api.FunctionFile{{Path: api.BundleFileName, Content: helloBundle(t)}}
	if err := e.store.UpsertFunction(context.Background(), f, files); err != nil {
		t.Fatal(err)
	}
	e.sync(refA)
	link := FunctionPath(e.cfg, refA, "hello")
	m, ok := liveMeta(link)
	if !ok || m.Kind != "eszip" || m.Entrypoint != entry || m.Eszip != EszipFileName || m.VerifyJWT || m.Version != 1 {
		t.Fatalf("meta %+v", m)
	}
	b := readFile(t, filepath.Join(link, EszipFileName))
	if !strings.HasPrefix(b, "ESZIP2") {
		t.Fatalf("bundle.eszip starts with %q", b[:8])
	}
	// The compressed upload is not kept in the generation, and nothing else is written.
	ents, _ := os.ReadDir(link)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != ".sbctl-function.json,bundle.eszip" {
		t.Fatalf("generation holds %v", names)
	}
	first, _ := os.Readlink(link)
	e.sync(refA)
	if again, _ := os.Readlink(link); again != first {
		t.Fatal("an unchanged bundle got a new generation")
	}
	// Redeploying as source files replaces the bundle.
	e.deploy(refA, "hello", true, map[string]string{"supabase/functions/hello/index.ts": "x"})
	e.sync(refA)
	m, _ = liveMeta(link)
	if m.Kind != "" || m.Eszip != "" {
		t.Fatalf("meta after a source deploy: %+v", m)
	}
	if _, err := os.Stat(filepath.Join(link, EszipFileName)); err == nil {
		t.Fatal("the old bundle is still served")
	}
}

func TestBadBundlesAreRefused(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	ctx := context.Background()
	f := &api.Function{Ref: refA, Slug: "bad", Name: "bad", Status: "ACTIVE", VerifyJWT: true, EntrypointPath: "file:///x/index.ts"}
	cases := map[string][]api.FunctionFile{
		"garbage":    {{Path: api.BundleFileName, Content: []byte("EZBRgarbage garbage garbage")}},
		"extra file": {{Path: api.BundleFileName, Content: helloBundle(t)}, {Path: "index.ts", Content: []byte("x")}},
	}
	for name, files := range cases {
		if err := e.store.UpsertFunction(ctx, f, files); err != nil {
			t.Fatal(err)
		}
		err := e.s.SyncProject(ctx, refA)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := os.Lstat(FunctionPath(e.cfg, refA, "bad")); err == nil {
			t.Errorf("%s: went live", name)
		}
	}
	// A bundle without an entrypoint cannot start.
	f.EntrypointPath = ""
	if err := e.store.UpsertFunction(ctx, f, []api.FunctionFile{{Path: api.BundleFileName, Content: helloBundle(t)}}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SyncProject(ctx, refA); err == nil || !strings.Contains(err.Error(), "entrypoint") {
		t.Fatalf("bundle without entrypoint: %v", err)
	}
}
