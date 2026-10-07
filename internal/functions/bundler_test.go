package functions

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/OWNER/sbctl/internal/api"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/units"
)

// fakeEdgeRuntime is a stand-in for bin/edge-runtime: "bundle" writes an eszip-looking file that
// names its entrypoint, unless the entrypoint contains BROKEN (it then complains and fails) or
// SLOW (it then sleeps). It records its arguments and environment next to the output.
const fakeEdgeRuntime = `#!/bin/sh
[ "$1" = bundle ] || { echo "unexpected command $1" >&2; exit 2; }
shift
while [ $# -gt 0 ]; do
  case "$1" in
    --entrypoint) e=$2; shift ;;
    --output) o=$2; shift ;;
  esac
  shift
done
echo "DENO_DIR=$DENO_DIR NO_PKG=$DENO_NO_PACKAGE_JSON HOME=$HOME" >&2
if grep -q BROKEN "$e"; then
  printf 'error: Module not found "file:///nowhere/missing.ts"\n' >&2
  exit 1
fi
if grep -q SLOW "$e"; then sleep 30; fi
if grep -q LATE "$e"; then sleep 1; fi
printf 'ESZIP2.3 file://%s' "$e" >"$o"
`

type bundlerRig struct {
	cfg *config.Config
	b   *Bundler
	art string
	sup *recordingSup
}

// recordingSup wraps the exec backend and keeps the spec of the last Render.
type recordingSup struct {
	*units.Exec
	mu   sync.Mutex
	last units.Spec
}

func (r *recordingSup) Render(ctx context.Context, s units.Spec) error {
	r.mu.Lock()
	r.last = s
	r.mu.Unlock()
	return r.Exec.Render(ctx, s)
}

type dirs map[string]string

func (d dirs) Dir(svc string) (string, error) {
	if p, ok := d[svc]; ok {
		return p, nil
	}
	return "", errors.New("not fetched")
}

func newBundlerRig(t *testing.T) *bundlerRig {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Functions.Enabled = true
	cfg.Functions.BundleUnsandboxed = true
	art := t.TempDir()
	if err := os.MkdirAll(filepath.Join(art, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(art, "bin", "edge-runtime"), []byte(fakeEdgeRuntime), 0o755); err != nil {
		t.Fatal(err)
	}
	ex := units.NewExec(cfg, nil)
	ex.StopTimeout = 2 * time.Second
	sup := &recordingSup{Exec: ex}
	b, err := NewBundler(cfg, sup, dirs{config.SvcEdgeRuntime: art}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &bundlerRig{cfg: cfg, b: b, art: art, sup: sup}
}

func decodeEZBR(t *testing.T, b []byte) string {
	t.Helper()
	rest, ok := bytes.CutPrefix(b, []byte("EZBR"))
	if !ok {
		t.Fatalf("no EZBR prefix: %q", b[:min(8, len(b))])
	}
	out, err := io.ReadAll(brotli.NewReader(bytes.NewReader(rest)))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestBundlerBundlesUploadedSourcesAndCleansUp(t *testing.T) {
	r := newBundlerRig(t)
	in := BundleInput{
		Entrypoint: "supabase/functions/hello/index.ts",
		ImportMap:  "supabase/functions/import_map.json",
		Static:     []string{"supabase/functions/hello/*.html"},
		Files: []api.FunctionFile{
			{Path: "supabase/functions/hello/index.ts", Content: []byte("import '../_shared/x.ts'")},
			{Path: "supabase/functions/_shared/x.ts", Content: []byte("export {}")},
			{Path: "supabase/functions/import_map.json", Content: []byte("{}")},
		},
	}
	bundle, entry, err := r.b.Bundle(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	eszip := decodeEZBR(t, bundle)
	if !strings.HasPrefix(eszip, "ESZIP2.3 file://") {
		t.Fatalf("eszip %q", eszip)
	}
	if !strings.HasSuffix(entry, "/src/supabase/functions/hello/index.ts") || !strings.HasPrefix(entry, "file:///") {
		t.Fatalf("entry %q", entry)
	}
	// The arguments follow what the Supabase CLI passes its own bundler.
	spec := r.sup.last
	args := strings.Join(spec.Exec, " ")
	for _, want := range []string{"bin/edge-runtime bundle --entrypoint ", "/src/supabase/functions/hello/index.ts", " --output ", "/out.eszip", " --timeout 100",
		" --import-map ", "/src/supabase/functions/import_map.json", " --static ", "/src/supabase/functions/hello/*.html"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if spec.Service != config.SvcEdgeBundle || spec.Unit() != "sb-edge-bundle.service" || spec.Limits.MemoryMax != "1G" {
		t.Errorf("spec %+v", spec)
	}
	if spec.Env["DENO_NO_PACKAGE_JSON"] != "1" || !strings.HasSuffix(spec.Env["DENO_DIR"], "/system/edge-bundle/deno") {
		t.Errorf("env %v", spec.Env)
	}
	// Nothing of the upload stays: it is someone else's code and secrets in its sources.
	if _, err := os.Stat(filepath.Join(r.cfg.Paths().EdgeBundleDir(), "work")); !os.IsNotExist(err) {
		t.Errorf("the scratch directory stayed: %v", err)
	}
	if st, err := r.sup.Status(context.Background(), spec.Unit()); err != nil || st.State == units.StateActive {
		t.Errorf("unit after bundling: %+v %v", st, err)
	}
}

// A bundling that takes longer than the exec backend's start grace is not a failure: that
// backend reports any process that has gone as failed, and only the output says how it ended.
func TestBundlerAcceptsABundlingThatOutlivesTheStartGrace(t *testing.T) {
	r := newBundlerRig(t)
	if _, _, err := r.b.Bundle(context.Background(), BundleInput{Entrypoint: "index.ts", Files: []api.FunctionFile{{Path: "index.ts", Content: []byte("LATE")}}}); err != nil {
		t.Fatal(err)
	}
}

func TestBundleCommandFollowsTheCLI(t *testing.T) {
	files := func(paths ...string) []api.FunctionFile {
		var out []api.FunctionFile
		for _, p := range paths {
			out = append(out, api.FunctionFile{Path: p})
		}
		return out
	}
	for _, c := range []struct {
		name       string
		in         BundleInput
		importMap  bool // --import-map expected
		noPkgJSON  bool
		wantStatic int
	}{
		{"plain", BundleInput{Entrypoint: "f/a/index.ts", Files: files("f/a/index.ts")}, false, true, 0},
		{"import map elsewhere", BundleInput{Entrypoint: "f/a/index.ts", ImportMap: "f/import_map.json", Files: files("f/a/index.ts")}, true, true, 0},
		{"deno.json beside the entrypoint is found by deno itself", BundleInput{Entrypoint: "f/a/index.ts", ImportMap: "f/a/deno.json", Files: files("f/a/index.ts")}, false, true, 0},
		{"deno.jsonc elsewhere is passed", BundleInput{Entrypoint: "f/a/index.ts", ImportMap: "f/deno.jsonc", Files: files("f/a/index.ts")}, true, true, 0},
		{"package.json beside the entrypoint, no import map", BundleInput{Entrypoint: "f/a/index.ts", Files: files("f/a/index.ts", "f/a/package.json")}, false, false, 0},
		{"package.json with an import map", BundleInput{Entrypoint: "f/a/index.ts", ImportMap: "f/m.json", Files: files("f/a/index.ts", "f/a/package.json")}, true, true, 0},
		{"static files", BundleInput{Entrypoint: "f/a/index.ts", Static: []string{"f/a/*.txt", "f/b.txt"}, Files: files("f/a/index.ts")}, false, true, 2},
	} {
		args, env, err := bundleCommand(c.in, "/s", "/s/../out.eszip")
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		joined := strings.Join(args, " ")
		if has := strings.Contains(joined, "--import-map"); has != c.importMap {
			t.Errorf("%s: --import-map %v in %q", c.name, has, joined)
		}
		if got := env["DENO_NO_PACKAGE_JSON"] == "1"; got != c.noPkgJSON {
			t.Errorf("%s: DENO_NO_PACKAGE_JSON %v", c.name, env)
		}
		if n := strings.Count(joined, "--static"); n != c.wantStatic {
			t.Errorf("%s: %d --static in %q", c.name, n, joined)
		}
	}
	if _, _, err := bundleCommand(BundleInput{Entrypoint: "a.ts", Static: []string{"../../etc/passwd"}}, "/s", "/o"); err == nil {
		t.Error("a static pattern that leaves the sources was accepted")
	}
}

func TestBundlerReportsWhatTheBundlerSaid(t *testing.T) {
	r := newBundlerRig(t)
	_, _, err := r.b.Bundle(context.Background(), BundleInput{Entrypoint: "index.ts", Files: []api.FunctionFile{{Path: "index.ts", Content: []byte("BROKEN")}}})
	var be *api.BundleError
	if !errors.As(err, &be) || !strings.Contains(be.Msg, `Module not found "file:///nowhere/missing.ts"`) {
		t.Fatalf("err = %v", err)
	}
	// A failed bundling leaves nothing behind either, and the next upload works.
	if _, err := os.Stat(filepath.Join(r.cfg.Paths().EdgeBundleDir(), "work")); !os.IsNotExist(err) {
		t.Errorf("scratch after failure: %v", err)
	}
	if _, _, err := r.b.Bundle(context.Background(), BundleInput{Entrypoint: "index.ts", Files: []api.FunctionFile{{Path: "index.ts", Content: []byte("ok")}}}); err != nil {
		t.Fatalf("after a failure: %v", err)
	}
}

func TestBundlerRefusesPathsThatLeaveTheUpload(t *testing.T) {
	r := newBundlerRig(t)
	for _, name := range []string{"../escape.ts", "a/../../escape.ts", ""} {
		_, _, err := r.b.Bundle(context.Background(), BundleInput{Entrypoint: "index.ts", Files: []api.FunctionFile{{Path: "index.ts", Content: []byte("ok")}, {Path: name, Content: []byte("x")}}})
		var be *api.BundleError
		if !errors.As(err, &be) {
			t.Errorf("%q: err = %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(r.cfg.Paths().EdgeBundleDir(), "escape.ts")); err == nil {
		t.Error("a file left the upload")
	}
	// A file and a directory of the same name conflict.
	_, _, err := r.b.Bundle(context.Background(), BundleInput{Entrypoint: "a", Files: []api.FunctionFile{{Path: "a", Content: []byte("x")}, {Path: "a/b", Content: []byte("y")}}})
	var be *api.BundleError
	if !errors.As(err, &be) {
		t.Errorf("conflicting paths: %v", err)
	}
}

func TestBundlerTimesOutAndStopsTheUnit(t *testing.T) {
	old := bundleTimeout
	bundleTimeout = 1500 * time.Millisecond
	defer func() { bundleTimeout = old }()
	r := newBundlerRig(t)
	started := time.Now()
	_, _, err := r.b.Bundle(context.Background(), BundleInput{Entrypoint: "index.ts", Files: []api.FunctionFile{{Path: "index.ts", Content: []byte("SLOW")}}})
	var be *api.BundleError
	if !errors.As(err, &be) || !strings.Contains(be.Msg, "did not finish") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(started) > 10*time.Second {
		t.Fatalf("took %s", time.Since(started))
	}
	st, _ := r.sup.Status(context.Background(), "sb-edge-bundle.service")
	if st.State == units.StateActive {
		t.Fatalf("the unit still runs: %+v", st)
	}
}

func TestBundlerQueueIsBounded(t *testing.T) {
	r := newBundlerRig(t)
	// Fill every slot: one running and the waiting room.
	for i := 0; i < cap(r.b.slots); i++ {
		r.b.slots <- struct{}{}
	}
	_, _, err := r.b.Bundle(context.Background(), BundleInput{Entrypoint: "i.ts", Files: []api.FunctionFile{{Path: "i.ts"}}})
	if !errors.Is(err, api.ErrBundlingBusy) {
		t.Fatalf("err = %v", err)
	}
}

func TestBundlerNeedsASandboxOrAnExplicitOptOut(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	sup := units.NewExec(cfg, nil)
	if _, err := NewBundler(cfg, sup, dirs{}, nil); err == nil || !strings.Contains(err.Error(), "bundle_unsandboxed") {
		t.Fatalf("an unconfined supervisor was accepted: %v", err)
	}
	cfg.Functions.BundleUnsandboxed = true
	if _, err := NewBundler(cfg, sup, dirs{}, nil); err != nil {
		t.Fatalf("explicit opt-out: %v", err)
	}
	// Through the Syncer: no bundler means source uploads are unavailable, with the reason.
	cfg.Functions.BundleUnsandboxed = false
	e := newEnv(t)
	e.cfg.Functions.BundleUnsandboxed = false
	s, err := New(Deps{Cfg: e.cfg, Registry: e.reg, Secrets: e.sec, Store: e.store, Keys: e.s.d.Keys, Supervisor: sup, Artifacts: dirs{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BundleSources(context.Background(), api.SourceBundle{}); !errors.Is(err, api.ErrBundlingUnavailable) || !strings.Contains(err.Error(), "bundle_unsandboxed") {
		t.Fatalf("err = %v", err)
	}
	s2, _ := New(Deps{Cfg: e.cfg, Registry: e.reg, Secrets: e.sec, Store: e.store, Keys: e.s.d.Keys})
	if _, err := s2.BundleSources(context.Background(), api.SourceBundle{}); !errors.Is(err, api.ErrBundlingUnavailable) {
		t.Fatalf("no supervisor: %v", err)
	}
}

type sandboxedSup struct{ *recordingSup }

func (sandboxedSup) Sandboxed() bool { return true }

func TestBundlerWithASandboxedSupervisorNeedsNoOptOut(t *testing.T) {
	r := newBundlerRig(t)
	r.cfg.Functions.BundleUnsandboxed = false
	if _, err := NewBundler(r.cfg, sandboxedSup{r.sup}, dirs{}, nil); err != nil {
		t.Fatalf("a sandboxed supervisor was refused: %v", err)
	}
}

// With SBCTL_TEST_EDGE_RUNTIME=<unpacked edge-runtime artifact> the real `edge-runtime bundle`
// runs (unsandboxed, as the exec backend does): the bundle must include the relative import, be accepted by the
// materializer's decoder, and a missing import must be reported.
func TestBundlerWithTheRealArtifact(t *testing.T) {
	art := os.Getenv("SBCTL_TEST_EDGE_RUNTIME")
	if art == "" {
		t.Skip("SBCTL_TEST_EDGE_RUNTIME is not set")
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Functions.BundleUnsandboxed = true
	ex := units.NewExec(cfg, nil)
	b, err := NewBundler(cfg, ex, dirs{config.SvcEdgeRuntime: art}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A path with a space and a non-ASCII letter checks the specifier the Bundler predicts.
	entry := "supabase/functions/hé llo/index.ts"
	bundle, specifier, err := b.Bundle(context.Background(), BundleInput{
		Entrypoint: entry,
		Files: []api.FunctionFile{
			{Path: entry, Content: []byte("import { who } from '../_shared/who.ts'\nDeno.serve(() => new Response(who()))\n")},
			{Path: "supabase/functions/_shared/who.ts", Content: []byte("export const who = () => 'shared-code-marker'\n")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if err := writeBundleFile(filepath.Join(t.TempDir(), "b.eszip"), bundle); err != nil {
		t.Fatalf("the materializer cannot read the bundle: %v", err)
	}
	if err := decodeBundle(&plain, bytes.TrimPrefix(bundle, []byte("EZBR"))); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plain.Bytes(), []byte("shared-code-marker")) && !bytes.Contains(plain.Bytes(), []byte("_shared/who.ts")) {
		t.Errorf("the relative import is not in the bundle")
	}
	t.Logf("specifier %s, eszip %d bytes", specifier, plain.Len())

	_, _, err = b.Bundle(context.Background(), BundleInput{Entrypoint: "index.ts", Files: []api.FunctionFile{
		{Path: "index.ts", Content: []byte("import './missing.ts'\n")}}})
	var be *api.BundleError
	if !errors.As(err, &be) || !strings.Contains(be.Msg, "missing.ts") {
		t.Fatalf("a missing import: %v", err)
	}
	t.Logf("bundler said: %s", be.Msg)
}
