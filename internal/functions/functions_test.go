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
	"sync/atomic"
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
	// loads counts FunctionFiles calls: loading the files of a function is the expensive step.
	loads atomic.Int64
}

type countingStore struct {
	Store
	n *atomic.Int64
}

func (c countingStore) FunctionFiles(ctx context.Context, ref, slug string) ([]api.FunctionFile, error) {
	c.n.Add(1)
	return c.Store.FunctionFiles(ctx, ref, slug)
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
	s, err := New(Deps{Cfg: cfg, Registry: e.reg, Secrets: sec, Store: countingStore{e.store, &e.loads}, Now: func() time.Time {
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
	return p
}

// eszipOf is the plain "eszip" these tests store for a function whose code is code.
func eszipOf(code string) string { return "ESZIP2.3 " + code }

// bundleOf is what the CLI uploads for it: "EZBR" and a Brotli stream.
func bundleOf(code string) []byte {
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	_, _ = w.Write([]byte(eszipOf(code)))
	_ = w.Close()
	return append([]byte("EZBR"), buf.Bytes()...)
}

func entryOf(slug string) string { return "file:///src/" + slug + "/index.ts" }

// deploy stores a bundled function (what `supabase functions deploy` uploads).
func (e *env) deploy(ref, slug string, verify bool, code string) *api.Function {
	e.t.Helper()
	f := &api.Function{Ref: ref, Slug: slug, Name: slug, Status: "ACTIVE", VerifyJWT: verify, EntrypointPath: entryOf(slug)}
	files := []api.FunctionFile{{Path: api.BundleFileName, Content: bundleOf(code)}}
	if err := e.store.UpsertFunction(context.Background(), f, files); err != nil {
		e.t.Fatal(err)
	}
	return f
}

// deploySource stores source files (what `supabase functions deploy --use-api` uploads).
func (e *env) deploySource(ref, slug string, verify bool, files map[string]string) *api.Function {
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
	code := EszipFileName
	e.deploy(refA, "hello", true, "v1")
	e.sync(refA)

	link := FunctionPath(e.cfg, refA, "hello")
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("function path is not a symlink: %v %v", fi, err)
	}
	if got := readFile(t, filepath.Join(link, code)); got != eszipOf("v1") {
		t.Fatalf("bundle %q", got)
	}
	m, ok := liveMeta(link)
	if !ok || m.Slug != "hello" || m.Version != 1 || !m.VerifyJWT || m.Kind != "eszip" || m.Entrypoint != entryOf("hello") || m.SHA256 == "" || m.Stamp == "" {
		t.Fatalf("meta %+v", m)
	}
	first, _ := os.Readlink(link)

	// Unchanged: nothing is rewritten, and the stored files are not even loaded again.
	loads := e.loads.Load()
	e.sync(refA)
	if again, _ := os.Readlink(link); again != first {
		t.Fatal("an unchanged function got a new generation")
	}
	if n := e.loads.Load(); n != loads {
		t.Fatalf("an unchanged function loaded its files again (%d loads)", n-loads)
	}

	// A new deployment is a new generation behind the same path; the old one stays for
	// workers that are still answering from it.
	e.deploy(refA, "hello", false, "v2")
	e.sync(refA)
	second, _ := os.Readlink(link)
	if second == first {
		t.Fatal("the link was not swapped")
	}
	if got := readFile(t, filepath.Join(link, code)); got != eszipOf("v2") {
		t.Fatalf("bundle after redeploy %q", got)
	}
	if m, _ := liveMeta(link); m.Version != 2 || m.VerifyJWT {
		t.Fatalf("meta after redeploy %+v", m)
	}
	if n := len(gens(t, e.cfg, refA)); n != 2 {
		t.Fatalf("generations after one redeploy: %d, want 2", n)
	}
	if got := readFile(t, filepath.Join(FunctionsDir(e.cfg, refA), first, code)); got != eszipOf("v1") {
		t.Fatalf("previous generation: %q", got)
	}

	// A third one drops the first.
	e.deploy(refA, "hello", true, "v3")
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

func TestReconcileLoadsNothingForUnchangedFunctions(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	e.addProject(refB, registry.StatusActiveHealthy)
	e.deploy(refA, "hello", true, "v1")
	e.deploy(refB, "hello", true, "v1")
	// Functions with nothing to serve are looked at once, too.
	e.deploySource(refA, "legacy", true, map[string]string{"supabase/functions/legacy/index.ts": "x"})
	if err := e.s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := e.loads.Load(); n != 3 {
		t.Fatalf("first reconcile loaded files %d times, want 3", n)
	}
	for i := 0; i < 3; i++ {
		if err := e.s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.loads.Load(); n != 3 {
		t.Fatalf("later reconciles loaded files again: %d", n)
	}
	// A change loads exactly the changed function.
	e.deploy(refB, "hello", true, "v2")
	if err := e.s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := e.loads.Load(); n != 4 {
		t.Fatalf("a redeploy loaded files %d times in total, want 4", n)
	}
	if got := readFile(t, filepath.Join(FunctionPath(e.cfg, refB, "hello"), EszipFileName)); got != eszipOf("v2") {
		t.Fatalf("redeploy not live: %q", got)
	}
}

func TestSettledFunctionsAreForgottenWithTheirProject(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	e.deploySource(refA, "legacy", true, map[string]string{"supabase/functions/legacy/index.ts": "x"})
	e.sync(refA)
	e.sync(refA)
	if n := e.loads.Load(); n != 1 {
		t.Fatalf("loads %d, want 1", n)
	}
	if err := e.s.RemoveProject(refA); err != nil {
		t.Fatal(err)
	}
	e.sync(refA)
	if n := e.loads.Load(); n != 2 {
		t.Fatalf("after RemoveProject the function must be looked at again: %d loads", n)
	}
}

func TestSwapIsAtomicForReaders(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	code := EszipFileName
	e.deploy(refA, "hello", true, "version-1")
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
				if err != nil || !strings.HasPrefix(string(b), eszipOf("version-")) {
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
		e.deploy(refA, "hello", true, "version-"+itoa(v))
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
	code := EszipFileName
	e.deploy(refA, "hello", true, "code of A")
	e.deploy(refB, "hello", true, "code of B")
	sa, _ := e.sec.Seal([]byte("secret of A"))
	sb, _ := e.sec.Seal([]byte("secret of B"))
	_ = e.store.PutFunctionSecrets(context.Background(), refA, map[string][]byte{"TOKEN": sa})
	_ = e.store.PutFunctionSecrets(context.Background(), refB, map[string][]byte{"TOKEN": sb})
	if err := e.s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(FunctionPath(e.cfg, refA, "hello"), code)) != eszipOf("code of A") ||
		readFile(t, filepath.Join(FunctionPath(e.cfg, refB, "hello"), code)) != eszipOf("code of B") {
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
	e.deploy(refA, "hello", true, "1")
	e.deploy(refA, "hello-2", true, "other")
	e.sync(refA)
	e.deploy(refA, "hello", true, "2")
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
	if got := readFile(t, filepath.Join(FunctionPath(e.cfg, refA, "hello-2"), EszipFileName)); got != eszipOf("other") {
		t.Fatalf("hello-2 damaged: %q", got)
	}
}

func TestOrphansAndInterruptedSwapsAreCleanedUp(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	e.deploy(refA, "hello", true, "1")
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
	if _, err := os.Stat(filepath.Join(FunctionPath(e.cfg, refA, "hello"), EszipFileName)); err != nil {
		t.Fatalf("the live function was damaged: %v", err)
	}
}

// Source functions are never served: a function that runs from real files can import
// files outside its directory (relative specifiers), and in the shared tenants tree
// those are other projects' environment files and code.
func TestSourceFunctionsAreNotMaterialized(t *testing.T) {
	e := newEnv(t)
	e.addProject(refB, registry.StatusActiveHealthy)
	e.addProject(refA, registry.StatusActiveHealthy)
	e.deploy(refB, "hello", true, "code of B")
	e.sync(refB)
	ctx := context.Background()
	hostile := map[string]string{
		"supabase/functions/steal/index.ts": `import env from "../../../../../../../` + refB + `/functions-env.json" with { type: "json" }`,
		"../escape.ts":                      "x",
	}
	f := e.deploySource(refA, "steal", true, hostile)
	if err := e.s.SyncProject(ctx, refA); err != nil {
		t.Fatalf("a stored source function must be skipped, not fail the project: %v", err)
	}
	if _, err := os.Lstat(FunctionPath(e.cfg, refA, "steal")); err == nil {
		t.Fatal("a source function went live")
	}
	if n := len(gens(t, e.cfg, refA)); n != 0 {
		t.Fatalf("%d generations written for a source function", n)
	}
	var found []string
	_ = filepath.WalkDir(e.cfg.Paths().FunctionsRoot(), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(p, "escape.ts") || strings.HasSuffix(p, "steal/index.ts") {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Fatalf("source files were written: %v", found)
	}
	// A bundle deployed over it goes live.
	f.EntrypointPath = entryOf("steal")
	if err := e.store.UpsertFunction(ctx, f, []api.FunctionFile{{Path: api.BundleFileName, Content: bundleOf("bundled")}}); err != nil {
		t.Fatal(err)
	}
	e.sync(refA)
	if got := readFile(t, filepath.Join(FunctionPath(e.cfg, refA, "steal"), EszipFileName)); got != eszipOf("bundled") {
		t.Fatalf("bundle %q", got)
	}
	// And a source deploy over a live bundle takes it out of service.
	e.deploySource(refA, "steal", true, hostile)
	e.sync(refA)
	if _, err := os.Lstat(FunctionPath(e.cfg, refA, "steal")); err == nil {
		t.Fatal("a source redeploy left the old bundle live")
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
	const failed, paused = "cccccccccccccccccccc", "dddddddddddddddddddd"
	e.addProject(refA, registry.StatusActiveHealthy)
	e.addProject(failed, registry.StatusInitFailed)
	e.addProject(paused, registry.StatusInactive) // paused: the proxy does not serve it, so no files
	e.addProject(config.SystemRef, registry.StatusActiveHealthy)
	for _, ref := range []string{refA, failed, paused} {
		e.deploy(ref, "hello", true, "1")
	}
	if err := e.s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(EnvPath(e.cfg, failed)); err == nil {
		t.Error("a failed project got files")
	}
	if _, err := os.Stat(EnvPath(e.cfg, config.SystemRef)); err == nil {
		t.Error("the system project got files")
	}
	if _, err := os.Stat(EnvPath(e.cfg, paused)); err == nil {
		t.Error("a paused project got files")
	}
	// Resuming brings them back.
	if err := e.reg.SetProjectStatus(context.Background(), paused, registry.StatusActiveHealthy); err != nil {
		t.Fatal(err)
	}
	e.sync(paused)
	if _, err := os.Stat(EnvPath(e.cfg, paused)); err != nil {
		t.Error("a resumed project got no files")
	}
	// Pausing takes them away again, the keys included.
	if err := e.reg.SetProjectStatus(context.Background(), paused, registry.StatusPausing); err != nil {
		t.Fatal(err)
	}
	e.sync(paused)
	if _, err := os.Stat(ProjectDir(e.cfg, paused)); err == nil {
		t.Error("a pausing project kept its files")
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

func TestAProjectThatDoesNotUseFunctionsGetsNoFiles(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	e.sync(refA)
	for _, p := range []string{EnvPath(e.cfg, refA), FunctionsDir(e.cfg, refA)} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s exists for a project without functions or secrets", p)
		}
	}
	// A secret alone is enough to need the file (secrets are set before the first deploy).
	sealed, _ := e.sec.Seal([]byte("v"))
	if err := e.store.PutFunctionSecrets(context.Background(), refA, map[string][]byte{"K": sealed}); err != nil {
		t.Fatal(err)
	}
	e.sync(refA)
	if readEnvDoc(t, e.cfg, refA).Secrets["K"] != "v" {
		t.Fatal("secret missing")
	}
	// Removing the last function and the last secret removes the files again.
	e.deploy(refA, "hello", true, "1")
	e.sync(refA)
	_ = e.store.DeleteFunction(context.Background(), refA, "hello")
	_ = e.store.DeleteFunctionSecrets(context.Background(), refA, []string{"K"})
	e.sync(refA)
	for _, p := range []string{EnvPath(e.cfg, refA), FunctionsDir(e.cfg, refA)} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s survived the removal of everything", p)
		}
	}
}

func TestFilesOfProjectsThatAreGoneAreCollected(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addProject(refA, registry.StatusActiveHealthy)
	e.addProject(refB, registry.StatusActiveHealthy)
	for _, ref := range []string{refA, refB} {
		e.deploy(ref, "hello", true, "1")
		e.sync(ref)
	}
	// Deleted from the registry (what Engine.Delete does last): the files go with the next
	// sync of that project or the next reconcile.
	if err := e.reg.DeleteProject(ctx, refA); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ProjectDir(e.cfg, refA)); err == nil {
		t.Fatal("the tree of a deleted project survived a reconcile")
	}
	if _, err := os.Stat(EnvPath(e.cfg, refB)); err != nil {
		t.Fatalf("another project lost its files: %v", err)
	}
	// SyncProject alone does the same, and so does a project that is going down.
	e.deploy(refB, "hello", true, "2")
	if err := e.reg.SetProjectStatus(ctx, refB, registry.StatusGoingDown); err != nil {
		t.Fatal(err)
	}
	e.sync(refB)
	if _, err := os.Stat(ProjectDir(e.cfg, refB)); err == nil {
		t.Fatal("a project that is going down kept its files")
	}
	// Junk next to the project trees is left alone.
	junk := filepath.Join(e.cfg.Paths().FunctionsRoot(), "not-a-ref")
	if err := os.MkdirAll(junk, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(junk); err != nil {
		t.Fatal("a directory that is not a project ref was removed")
	}
}

// listsNothing hides every project from ListProjects, as a snapshot taken before a project
// was created would.
type listsNothing struct{ registry.Registry }

func (listsNothing) ListProjects(context.Context) ([]registry.Project, error) { return nil, nil }

func TestACollectNeverRemovesAProjectCreatedAfterTheListing(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	e.deploy(refA, "hello", true, "1")
	e.sync(refA)
	s, err := New(Deps{Cfg: e.cfg, Registry: listsNothing{e.reg}, Secrets: e.sec, Store: e.store, Keys: e.s.d.Keys})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(EnvPath(e.cfg, refA)); err != nil {
		t.Fatalf("a project that exists lost its files: %v", err)
	}
}

func TestSyncOfAnUnknownProjectRemovesLeftoversAndBadRefsAreRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := os.MkdirAll(FunctionsDir(e.cfg, refA), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SyncProject(ctx, refA); err != nil {
		t.Fatalf("unknown project: %v", err)
	}
	if _, err := os.Stat(ProjectDir(e.cfg, refA)); err == nil {
		t.Fatal("leftovers of an unknown project were kept")
	}
	for _, bad := range []string{"", "system", "../etc", "AAAAAAAAAAAAAAAAAAAA"} {
		if err := e.s.SyncProject(ctx, bad); err == nil {
			t.Errorf("ref %q accepted", bad)
		}
		if err := RemoveFiles(e.cfg, bad); err == nil {
			t.Errorf("RemoveFiles accepted %q", bad)
		}
	}
}

func TestRemoveProject(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	e.deploy(refA, "hello", true, "1")
	e.sync(refA)
	if err := e.s.RemoveProject(refA); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{EnvPath(e.cfg, refA), FunctionsDir(e.cfg, refA), ProjectDir(e.cfg, refA)} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s survived", p)
		}
	}
	if _, err := os.Stat(e.cfg.Paths().FunctionsRoot()); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	e.sync(refA)
	if err := RemoveFiles(e.cfg, refA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ProjectDir(e.cfg, refA)); err == nil {
		t.Fatal("RemoveFiles left the tree")
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
	var out bytes.Buffer
	if err := decodeBundle(&out, bytes.TrimPrefix(helloBundle(t), []byte("EZBR"))); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "ESZIP2") || out.Len() < 1000 {
		t.Fatalf("decoded %d bytes starting %q", out.Len(), out.Bytes()[:8])
	}
	brotliOf := func(b []byte) []byte {
		var buf bytes.Buffer
		w := brotli.NewWriter(&buf)
		_, _ = w.Write(b)
		_ = w.Close()
		return buf.Bytes()
	}
	for name, in := range map[string][]byte{
		"not brotli": []byte("this is not a brotli stream at all, surely"),
		"not eszip":  brotliOf([]byte("hello world")),
		"empty":      brotliOf(nil),
	} {
		var sink bytes.Buffer
		err := decodeBundle(&sink, in)
		var p permanent
		if err == nil || !errors.As(err, &p) {
			t.Errorf("%s: %v, want a permanent error", name, err)
		}
	}
}

// A bomb: a small upload that expands past the cap is stopped while it streams to disk.
func TestDecodeBundleStopsAtTheSizeCap(t *testing.T) {
	var plain bytes.Buffer
	plain.WriteString("ESZIP2.3")
	plain.Write(make([]byte, maxEszipSize))
	var buf bytes.Buffer
	w := brotli.NewWriterLevel(&buf, 1)
	_, _ = w.Write(plain.Bytes())
	_ = w.Close()
	if buf.Len() > 1<<20 {
		t.Fatalf("the test bomb is %d bytes; it should compress to almost nothing", buf.Len())
	}
	path := filepath.Join(t.TempDir(), "bundle.eszip")
	err := writeBundleFile(path, append([]byte("EZBR"), buf.Bytes()...))
	var p permanent
	if err == nil || !errors.As(err, &p) || !strings.Contains(err.Error(), "once decompressed") {
		t.Fatalf("bomb: %v", err)
	}
	if fi, _ := os.Stat(path); fi != nil && fi.Size() > maxEszipSize+1 {
		t.Fatalf("wrote %d bytes of a bomb", fi.Size())
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
	// Redeploying as source files takes it out of service.
	e.deploySource(refA, "hello", true, map[string]string{"supabase/functions/hello/index.ts": "x"})
	e.sync(refA)
	if _, err := os.Lstat(link); err == nil {
		t.Fatal("the old bundle is still served after a source deploy")
	}
}

func TestBadBundlesAreRefused(t *testing.T) {
	e := newEnv(t)
	e.addProject(refA, registry.StatusActiveHealthy)
	ctx := context.Background()
	f := &api.Function{Ref: refA, Slug: "bad", Name: "bad", Status: "ACTIVE", VerifyJWT: true, EntrypointPath: "file:///x/index.ts"}
	// A bundle that cannot be decoded is reported once, when it is first looked at (the
	// deploy that stored it gets the error); later syncs of the same deployment skip it.
	if err := e.store.UpsertFunction(ctx, f, []api.FunctionFile{{Path: api.BundleFileName, Content: []byte("EZBRgarbage garbage garbage")}}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SyncProject(ctx, refA); err == nil {
		t.Error("garbage: accepted")
	}
	loads := e.loads.Load()
	if err := e.s.SyncProject(ctx, refA); err != nil {
		t.Errorf("garbage, second sync: %v", err)
	}
	if e.loads.Load() != loads {
		t.Error("a refused bundle was loaded again")
	}
	if _, err := os.Lstat(FunctionPath(e.cfg, refA, "bad")); err == nil {
		t.Error("garbage: went live")
	}
	if n := len(gens(t, e.cfg, refA)); n != 0 {
		t.Errorf("%d generations left behind", n)
	}
	// A bundle next to another file is not a bundle upload: it is treated as sources.
	files := []api.FunctionFile{{Path: api.BundleFileName, Content: helloBundle(t)}, {Path: "index.ts", Content: []byte("x")}}
	if err := e.store.UpsertFunction(ctx, f, files); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SyncProject(ctx, refA); err != nil {
		t.Errorf("extra file: %v", err)
	}
	if _, err := os.Lstat(FunctionPath(e.cfg, refA, "bad")); err == nil {
		t.Error("extra file: went live")
	}
	// A bundle without an entrypoint cannot start.
	f.EntrypointPath = ""
	if err := e.store.UpsertFunction(ctx, f, []api.FunctionFile{{Path: api.BundleFileName, Content: helloBundle(t)}}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SyncProject(ctx, refA); err == nil || !strings.Contains(err.Error(), "entrypoint") {
		t.Fatalf("bundle without entrypoint: %v", err)
	}
	// One broken function does not keep the others from going live.
	e.deploy(refA, "fine", true, "ok")
	if err := e.s.SyncProject(ctx, refA); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(FunctionPath(e.cfg, refA, "fine"), EszipFileName)); got != eszipOf("ok") {
		t.Fatalf("the good function is not live: %q", got)
	}
}
