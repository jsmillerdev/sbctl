package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"

	functionsmain "github.com/OWNER/sbctl/functions-main"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/units"
)

// edgeRuntimeHealthPath is answered by the main service itself (functions-main/src/handler.ts),
// the same path the Supabase CLI's own functions service probes.
const edgeRuntimeHealthPath = "/_internal/health"

// mainServiceMarkerEnv carries the hash of the main service's files. The runtime ignores
// it; it makes the rendered environment change, and so Start restart the unit, when the
// binary brings a new main service.
const mainServiceMarkerEnv = "SBCTL_FUNCTIONS_MAIN_SHA256"

// ServicesFor lists the shared services of a node in start order: Services, plus the Edge
// Runtime (before Studio, which stays last) when [functions] enabled is set. Callers that
// fetch artifacts or print a table use it so they agree with Manager.
func ServicesFor(cfg *config.Config) []string {
	if cfg == nil || !cfg.Functions.Enabled {
		return Services
	}
	out := make([]string, 0, len(Services)+1)
	for _, s := range Services {
		if s == config.SvcStudio {
			out = append(out, config.SvcEdgeRuntime)
		}
		out = append(out, s)
	}
	return out
}

func (m *Manager) services() []string { return ServicesFor(m.cfg()) }

// MainServiceDir is where the main service is written: <state>/system/edge-runtime/main.
func MainServiceDir(cfg *config.Config) string {
	return filepath.Join(cfg.Paths().System(config.SvcEdgeRuntime), "main")
}

// edgeRuntimeSpec completes the unit spec of the Edge Runtime. One process serves every
// project: the main service (functions-main/, embedded in this binary and written to
// MainServiceDir) picks the project from the X-Sbctl-Project-Ref header the proxy sets,
// and reads the project's functions and environment from <state>/system/edge-runtime/
// tenants/<ref>/ (written by internal/functions), inside the unit's own state directory. The runtime listens on loopback only. Flags follow the CLI's
// functions service (packages/stack/src/services/Functions.ts) and the compose file of
// supabase/supabase; the limits come from [functions].
func edgeRuntimeSpec(cfg *config.Config, s units.Spec) (units.Spec, error) {
	sum, err := EnsureMainService(cfg)
	if err != nil {
		return units.Spec{}, err
	}
	f := cfg.Functions
	port := strconv.Itoa(cfg.Ports.EdgeRuntime)
	work := cfg.Paths().System(config.SvcEdgeRuntime)
	// The runtime cannot load a main service through a symlinked path (on macOS /tmp is
	// one: "Module not found"), so it is given real paths.
	mainDir := realPath(MainServiceDir(cfg))
	root := cfg.Paths().FunctionsRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return units.Spec{}, err
	}
	root = realPath(root)
	s.Env = map[string]string{
		"EDGE_RUNTIME_PORT":                port,
		"SBCTL_FUNCTIONS_ROOT":             root,
		"SBCTL_FUNCTIONS_MEMORY_MB":        strconv.Itoa(f.Memory()),
		"SBCTL_FUNCTIONS_WALL_CLOCK_SEC":   strconv.Itoa(f.WallClock()),
		"SBCTL_FUNCTIONS_IDLE_TIMEOUT_SEC": strconv.Itoa(f.IdleTimeout()),
		"SBCTL_FUNCTIONS_CPU_SOFT_MS":      strconv.Itoa(f.CPUSoft()),
		"SBCTL_FUNCTIONS_CPU_HARD_MS":      strconv.Itoa(f.CPUHard()),
		// The runtime keeps one module cache for the whole process (it reads DENO_DIR once,
		// and a worker cannot change it), so remote imports of all projects share this
		// directory. The cache is content-addressed by URL and holds public modules only.
		"DENO_DIR":           filepath.Join(work, "deno"),
		"HOME":               work,
		mainServiceMarkerEnv: sum,
	}
	args := []string{"bin/edge-runtime", "start",
		"--ip", "127.0.0.1",
		"--port", port,
		"--main-service", mainDir,
		"--policy", "per_worker",
		"--user-worker-request-idle-timeout", strconv.Itoa(f.IdleTimeout() * 1000),
		"--graceful-exit-timeout", "10",
	}
	if n := f.Parallelism(); n > 0 {
		args = append(args, "--max-parallelism", strconv.Itoa(n))
	}
	s.Exec = args
	if f.MemoryMax != "" {
		s.Limits.MemoryMax = f.MemoryMax
	}
	return s, nil
}

// realPath resolves symlinks in p, or returns p when that is not possible.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// mainFiles returns the embedded main service as path to content, paths slash-separated.
func mainFiles() (map[string][]byte, error) {
	out := map[string][]byte{}
	err := fs.WalkDir(functionsmain.Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(functionsmain.Files, p)
		out[p] = b
		return err
	})
	return out, err
}

// MainServiceHash is the SHA-256 over the names and contents of the embedded main service.
func MainServiceHash() (string, error) {
	files, err := mainFiles()
	if err != nil {
		return "", err
	}
	return hashFiles(files), nil
}

func hashFiles(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%d:%s:%d:", len(n), n, len(files[n]))
		h.Write(files[n])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// EnsureMainService writes the main service to MainServiceDir, replacing files whose
// content differs and removing files of an older version, and returns its hash.
func EnsureMainService(cfg *config.Config) (string, error) {
	files, err := mainFiles()
	if err != nil {
		return "", err
	}
	dir := MainServiceDir(cfg)
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, body) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return "", err
		}
		if err := writeAtomic(p, body, 0o640); err != nil {
			return "", err
		}
	}
	// Files an older binary wrote and this one no longer has.
	var stale []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr == nil {
			if _, ok := files[filepath.ToSlash(rel)]; !ok {
				stale = append(stale, p)
			}
		}
		return nil
	})
	slices.Sort(stale)
	var errs []error
	for _, p := range stale {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return hashFiles(files), errors.Join(errs...)
}

func writeAtomic(p string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
