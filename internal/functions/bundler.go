package functions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/OWNER/sbctl/internal/api"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/units"
)

// Server-side bundling.
//
// `supabase functions deploy --use-api`, the CLI when Docker is not running, and Studio's
// function editor upload source files, not a bundle. A function must never run from source
// files (its relative imports could reach other projects' files, see the package README),
// so the node turns the upload into an eszip first, with the edge-runtime artifact's own
// `bundle` command, and serves only that. Bundling reads the imports of someone else's
// code from the disk, which is the same attack as running it, so the command runs in the
// sandbox of sb-edge-bundle@<ref>.service: a uid of its own (a dynamic user: the node's other
// processes are the sbctl user's, and a mount namespace does not stop a process from opening
// /proc/<pid>/root of another process of its own uid), a mount namespace that shows nothing of
// the node but the upload and the artifacts, no loopback services, no instance metadata, a
// memory limit and a timeout. The unit is a template with one instance per project, and each
// instance has its own uid and its own module cache: what one project's upload made the bundler
// download (a private npm package, fetched with the uploader's .npmrc) is never in reach of
// another project's upload.
//
// The unit's uid is not the daemon's, so what it may touch is handed over through the file
// system: the sources are world-readable and owned by the daemon (the unit cannot change
// them), and the two files it writes, the eszip and its own log, are created by the daemon in
// a directory the unit cannot create anything in, and made world-writable. The unit cannot
// make, replace or delete any other file, so the daemon can always remove the whole scratch
// directory afterwards.

const (
	// bundleTimeoutDefault bounds one bundling, from the start of the unit to its end.
	bundleTimeoutDefault = 2 * time.Minute
	// bundleDenoTimeoutSec is passed to `edge-runtime bundle --timeout`, a little under bundleTimeout.
	bundleDenoTimeoutSec = 100
	// bundleMaxEszip bounds the bundler's output.
	bundleMaxEszip = 64 << 20
	// bundleMaxLog is how much of the bundler's output is shown to the uploader.
	bundleMaxLog = 4 << 10
	// bundleCacheMax is the size of a project's module cache (remote imports) above which it
	// is emptied.
	bundleCacheMax = 512 << 20
	bundleQueueMax = 8
	// sandboxCacheRoot is the parent of the module caches of the bundler under the systemd
	// unit: the unit of project <ref> has <root>/<ref> (CacheDirectory=sb-edge-bundle/%i in
	// sb-edge-bundle@.service), private to the instance's uid, so the daemon cannot see it and
	// the unit empties it itself (ExecStartPre) above 512 MiB. The exec backend, which has no
	// such unit, keeps the cache in the project's state directory (execCacheDir).
	sandboxCacheRoot = "/var/cache/sb-edge-bundle"
	// sandboxWorkDir is the working directory of the bundler under the unit: the one place
	// besides the cache it may write to (a private /tmp that goes away with the run).
	sandboxWorkDir = "/tmp"
)

// bundleTimeout is a variable so that tests can shorten it.
var bundleTimeout = bundleTimeoutDefault

// ArtifactDirs locates unpacked artifacts (lifecycle.Node's Artifacts).
type ArtifactDirs interface {
	Dir(svc string) (string, error)
}

// Bundler runs `edge-runtime bundle` for uploaded sources, one upload at a time (all the
// instances share one scratch directory, and a bundling can use a few hundred MB).
type Bundler struct {
	cfg       *config.Config
	sup       units.Supervisor
	artifacts ArtifactDirs
	log       *slog.Logger
	// slots is the queue: one holder runs, up to bundleQueueMax wait.
	slots chan struct{}
	run   chan struct{}
}

// NewBundler returns a Bundler. It fails when the supervisor cannot sandbox and the node
// has not accepted that explicitly ([functions] bundle_unsandboxed), so a node never
// bundles uploads unconfined by accident.
func NewBundler(cfg *config.Config, sup units.Supervisor, artifacts ArtifactDirs, log *slog.Logger) (*Bundler, error) {
	if sb, ok := sup.(units.Sandboxer); !ok || !sb.Sandboxed() {
		if !cfg.Functions.BundleUnsandboxed {
			return nil, fmt.Errorf("this node's supervisor cannot confine the bundler (only systemd units are sandboxed), so uploaded sources are not bundled; set [functions] bundle_unsandboxed only on a development machine that serves nobody else's projects")
		}
		if log != nil {
			log.Warn("edge functions: bundling uploaded sources WITHOUT a sandbox ([functions] bundle_unsandboxed); an upload can import any file this user can read")
		}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Bundler{cfg: cfg, sup: sup, artifacts: artifacts, log: log,
		slots: make(chan struct{}, bundleQueueMax+1), run: make(chan struct{}, 1)}, nil
}

// BundleInput is one upload.
type BundleInput struct {
	// Ref is the project the upload belongs to: it selects the unit instance and with it the
	// module cache the bundling may read and fill.
	Ref string
	// Files are the uploaded sources, paths relative to the project's working directory.
	Files []api.FunctionFile
	// Entrypoint and ImportMap are paths among Files (ImportMap may be empty).
	Entrypoint string
	ImportMap  string
	// Static are the patterns of files to embed, relative to the same root.
	Static []string
}

// Bundle turns the sources into the stored upload form: "EZBR" and the Brotli stream of
// the eszip the runtime serves. The second result is the file URL of the entrypoint where
// it was bundled (see the comment at its computation).
func (b *Bundler) Bundle(ctx context.Context, in BundleInput) (bundle []byte, entry string, err error) {
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	default:
		return nil, "", fmt.Errorf("%w: %d uploads are already waiting to be bundled", api.ErrBundlingBusy, bundleQueueMax)
	}
	select {
	case b.run <- struct{}{}:
		defer func() { <-b.run }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	if err := validRef(in.Ref); err != nil {
		return nil, "", fmt.Errorf("bundling needs the project the upload belongs to: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, bundleTimeout)
	defer cancel()

	art, err := b.artifacts.Dir(config.SvcEdgeRuntime)
	if err != nil {
		return nil, "", fmt.Errorf("%w: the edge-runtime artifact is not fetched: %v", api.ErrBundlingUnavailable, err)
	}
	sandboxed := false
	if sb, ok := b.sup.(units.Sandboxer); ok {
		sandboxed = sb.Sandboxed()
	}
	stateDir := b.cfg.Paths().EdgeBundleDir()
	work := filepath.Join(stateDir, "work")
	// Stale files of an earlier upload (a crash) never mix with this one.
	if err := os.RemoveAll(work); err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(work)
	src, outDir := filepath.Join(work, "src"), filepath.Join(work, "out")
	for _, d := range []string{src, outDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, "", err
		}
	}
	if real, err := filepath.EvalSymlinks(work); err == nil {
		work = real
		src, outDir = filepath.Join(work, "src"), filepath.Join(work, "out")
	}
	if err := writeSources(src, in.Files); err != nil {
		return nil, "", err
	}
	out := filepath.Join(outDir, "out.eszip")
	logFile := filepath.Join(outDir, "bundle.log")
	if err := handOver(work, src, out, logFile); err != nil {
		return nil, "", err
	}

	args, env, err := bundleCommand(in, src, out)
	if err != nil {
		return nil, "", err
	}
	env["NO_COLOR"] = "1"
	workDir := work
	if sandboxed {
		// Under the unit the module cache is the instance's own (this project's), and the
		// sources and the output directory are not writable for it.
		cache := path.Join(sandboxCacheRoot, in.Ref)
		env["DENO_DIR"] = path.Join(cache, "deno")
		env["HOME"] = cache
		workDir = sandboxWorkDir
	} else {
		// No unit, no private directory: the project's own state directory holds the cache
		// (and goes with the project), never one shared with other projects.
		cache := b.cfg.Paths().ProjectService(in.Ref, config.SvcEdgeBundle)
		trimCache(filepath.Join(cache, "deno"), bundleCacheMax)
		if err := os.MkdirAll(filepath.Join(cache, "deno"), 0o750); err != nil {
			return nil, "", err
		}
		if real, err := filepath.EvalSymlinks(cache); err == nil {
			cache = real
		}
		env["DENO_DIR"] = filepath.Join(cache, "deno")
		env["HOME"] = cache
	}
	// The cache that earlier versions of the bundler shared between all projects.
	_ = os.RemoveAll(filepath.Join(stateDir, "deno"))
	spec := units.Spec{
		Service: config.SvcEdgeBundle, Ref: in.Ref, ArtifactDir: art, WorkDir: workDir, Log: logFile,
		Exec: append([]string{"bin/edge-runtime"}, args...), Env: env,
		Limits:    config.Limits{MemoryMax: "1G", CPUQuota: "100%"},
		PublicRun: sandboxed,
	}
	unit := spec.Unit()
	if err := b.sup.Render(ctx, spec); err != nil {
		return nil, "", fmt.Errorf("rendering %s: %w", unit, err)
	}
	// A unit left over from a request that went away must not run into this one.
	_ = b.sup.Stop(ctx, unit)
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer scancel()
		_ = b.sup.Stop(sctx, unit)
	}()

	started := time.Now()
	startErr := b.sup.Start(ctx, unit)
	if startErr == nil {
		startErr = b.waitDone(ctx, unit, sandboxed)
	} else if !sandboxed && strings.Contains(startErr.Error(), "exited right after start") {
		// The exec backend reports any quick exit as a failure of Start: the output file says.
		startErr = nil
	}
	// The uploader sees what the bundler said, without the scratch path it ran in.
	scrub := strings.NewReplacer("file://"+(&url.URL{Path: src}).EscapedPath()+"/", "file:///", "file://"+src+"/", "file:///", src+"/", "")
	tail := scrub.Replace(readTail(logFile, bundleMaxLog))
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, "", &api.BundleError{Msg: fmt.Sprintf("bundling did not finish within %s%s", bundleTimeout, quoteLog(tail))}
		}
		return nil, "", ctx.Err()
	}
	eszip, rerr := readLimited(out, bundleMaxEszip)
	if startErr != nil || rerr != nil {
		if tail == "" && startErr != nil {
			// Nothing came out of the bundler: the unit did not run at all.
			return nil, "", fmt.Errorf("running %s: %w", unit, startErr)
		}
		return nil, "", &api.BundleError{Msg: "the function could not be bundled" + quoteLog(tail)}
	}
	if !bytes.HasPrefix(eszip, []byte("ESZIP")) {
		return nil, "", &api.BundleError{Msg: "the bundler wrote no eszip" + quoteLog(tail)}
	}
	// The bundle records its own entrypoint (a key relative to the root directory the bundler
	// picked, metadata.entrypoint) and edge-runtime v1.77.4 starts from that key; the URL given
	// at worker creation is the fallback for bundles that record none. It is a file URL of
	// the entrypoint where it was bundled.
	entry = (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(src, filepath.FromSlash(in.Entrypoint)))}).String()
	compressed, err := compress(eszip)
	if err != nil {
		return nil, "", err
	}
	b.log.Info("edge functions: bundled uploaded sources", "files", len(in.Files), "eszip_bytes", len(eszip), "took", time.Since(started).Round(time.Millisecond))
	return compressed, entry, nil
}

// waitDone waits until the unit has stopped (the exec backend returns from Start while the
// process runs; a systemd one-shot unit returns when it has finished). The exec backend
// reports every process that has gone, whatever its exit status, as failed, so there the
// output file decides and failed is just "finished".
func (b *Bundler) waitDone(ctx context.Context, unit string, sandboxed bool) error {
	for {
		st, err := b.sup.Status(ctx, unit)
		if err != nil {
			return err
		}
		switch st.State {
		case units.StateActive, units.StateActivating, units.StateDeactivating:
		case units.StateFailed:
			if !sandboxed {
				return nil
			}
			return errors.New("the bundler exited with an error")
		default:
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

var denoConfigRE = regexp.MustCompile(`(?i)^deno\.jsonc?$`)

// bundleCommand builds the arguments and environment of `edge-runtime bundle`, the way the
// Supabase CLI builds them for its own bundling (apps/cli/src/shared/functions/deploy.ts,
// bundleFunctionWithDocker): the import map is passed unless it is the deno.json next to
// the entrypoint, which Deno finds itself, and package.json discovery is switched off unless
// the entrypoint's directory has one and there is no import map.
func bundleCommand(in BundleInput, src, out string) (args []string, env map[string]string, err error) {
	abs := func(rel string) string { return filepath.Join(src, filepath.FromSlash(rel)) }
	args = []string{"bundle", "--entrypoint", abs(in.Entrypoint), "--output", out,
		"--timeout", fmt.Sprint(bundleDenoTimeoutSec)}
	env = map[string]string{}
	entryDir := path.Dir(in.Entrypoint)
	if in.ImportMap != "" && !(denoConfigRE.MatchString(path.Base(in.ImportMap)) && path.Dir(in.ImportMap) == entryDir) {
		args = append(args, "--import-map", abs(in.ImportMap))
	}
	hasPackageJSON := false
	for _, f := range in.Files {
		if f.Path == path.Join(entryDir, "package.json") {
			hasPackageJSON = true
		}
	}
	if !(in.ImportMap == "" && hasPackageJSON) {
		env["DENO_NO_PACKAGE_JSON"] = "1"
	}
	for _, p := range in.Static {
		clean, ok := cleanPattern(p)
		if !ok {
			return nil, nil, &api.BundleError{Msg: fmt.Sprintf("invalid static file pattern %q", p)}
		}
		args = append(args, "--static", abs(clean))
	}
	return args, env, nil
}

// cleanPattern makes a static file pattern safe to join under the sources: relative, no "..".
func cleanPattern(p string) (string, bool) {
	p = strings.ReplaceAll(p, "\\", "/")
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", false
		}
	}
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	return p, p != "" && p != "." && !strings.ContainsRune(p, 0)
}

// writeSources lays the upload out under dir as regular files. The upload's own paths are
// already free of "..", and nothing it contains is a link: the files are created here.
func writeSources(dir string, files []api.FunctionFile) error {
	seen := map[string]bool{}
	for _, f := range files {
		rel, ok := cleanPattern(f.Path)
		if !ok {
			return &api.BundleError{Msg: fmt.Sprintf("invalid file name %q", f.Path)}
		}
		if seen[rel] {
			return &api.BundleError{Msg: fmt.Sprintf("the file %q is uploaded twice", f.Path)}
		}
		seen[rel] = true
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return &api.BundleError{Msg: fmt.Sprintf("the file %q conflicts with another path of the upload", f.Path)}
		}
		if err := os.WriteFile(p, f.Content, 0o644); err != nil {
			return &api.BundleError{Msg: fmt.Sprintf("the file %q conflicts with another path of the upload", f.Path)}
		}
	}
	return nil
}

// handOver sets the modes the unit's uid needs (the daemon's umask is 0027, which would hide
// everything from it): the sources and the directories above them readable by everyone, and
// the output files created empty and writable by everyone. The output directory itself stays
// writable for the daemon only, so the unit can fill the two files and create nothing else.
func handOver(work, src string, outFiles ...string) error {
	if err := filepath.WalkDir(work, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o755)
		}
		if p == src || strings.HasPrefix(p, src+string(filepath.Separator)) {
			return os.Chmod(p, 0o644)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, p := range outFiles {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o666); err != nil {
			return err
		}
	}
	return nil
}

// trimCache empties the module cache when it has grown past limit bytes.
func trimCache(dir string, limit int64) {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, ierr := d.Info(); ierr == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	if n > limit {
		_ = os.RemoveAll(dir)
	}
}

func readLimited(p string, limit int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("the bundle is larger than %d MiB", limit>>20)
	}
	if len(b) == 0 {
		return nil, errors.New("empty output")
	}
	return b, nil
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// readTail returns the last max bytes of a log file, without color codes.
func readTail(p string, max int64) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		_, _ = f.Seek(fi.Size()-max, io.SeekStart)
	}
	b, _ := io.ReadAll(io.LimitReader(f, max))
	return strings.TrimSpace(ansiRE.ReplaceAllString(string(bytes.ToValidUTF8(b, nil)), ""))
}

func quoteLog(s string) string {
	if s == "" {
		return ""
	}
	return ":\n" + s
}

// compress returns "EZBR" and the Brotli stream of an eszip, the form the Supabase CLI
// uploads and the API stores (quality 6, as the CLI uses).
func compress(eszip []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("EZBR")
	w := brotli.NewWriterLevel(&buf, 6)
	if _, err := w.Write(eszip); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
