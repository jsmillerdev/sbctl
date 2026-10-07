// Package functions puts what the Management API stores for Edge Functions (deployments,
// sources, sealed secrets) where the runtime reads it: for each project, a generation
// directory per deployed function, a symlink that makes one generation live, and one
// environment file with the project's keys and secrets. The Deno main service in
// functions-main/ resolves a request to exactly these files.
//
//	<state>/system/edge-runtime/tenants/<ref>/functions-env.json    jwt secret, SUPABASE_* values, secrets (0600)
//	<state>/system/edge-runtime/tenants/<ref>/functions/<slug>      symlink to .gen/<slug>.<version>.<random>
//	<state>/system/edge-runtime/tenants/<ref>/functions/.gen/<...>/ the uploaded files and .sbctl-function.json
//
// The tree lives in the Edge Runtime's own state directory because that is the one place
// its systemd unit sees; the projects' directories (clusters, sockets, unit files) stay
// out of its mount namespace. A deployment writes a new generation and renames a new
// symlink over the old one, so a request sees the old function or the new one, never a
// mixture or a gap. A project that is deleted loses its tree at the next sync of that
// project or the next reconcile (RemoveFiles does it at once).
package functions

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/OWNER/sbctl/internal/api"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// Store is the part of api.Store the syncer reads.
type Store interface {
	ListFunctions(ctx context.Context, ref string) ([]api.Function, error)
	FunctionFiles(ctx context.Context, ref, slug string) ([]api.FunctionFile, error)
	ListFunctionSecrets(ctx context.Context, ref string) ([]api.FunctionSecret, error)
}

// Deps are the collaborators of a Syncer.
type Deps struct {
	Cfg      *config.Config
	Registry registry.Registry
	// Secrets opens the sealed function secrets.
	Secrets secrets.Secrets
	Store   Store
	// Keys returns the decrypted credentials of a project (lifecycle.Manager.Keys).
	Keys func(ctx context.Context, ref string) (*secrets.ProjectKeys, error)
	Log  *slog.Logger
	// Now is the clock; empty means time.Now.
	Now func() time.Time
}

// Syncer keeps the files of the runtime equal to the store. It implements api.FunctionsHook.
// It is safe for concurrent use; work on one project is serialized.
type Syncer struct {
	d   Deps
	log *slog.Logger
	now func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

var _ api.FunctionsHook = (*Syncer)(nil)

// New returns a Syncer.
func New(d Deps) (*Syncer, error) {
	if d.Cfg == nil || d.Registry == nil || d.Secrets == nil || d.Store == nil || d.Keys == nil {
		return nil, errors.New("functions: Deps needs Cfg, Registry, Secrets, Store and Keys")
	}
	s := &Syncer{d: d, log: d.Log, now: d.Now, locks: map[string]*sync.Mutex{}}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

func (s *Syncer) lock(ref string) func() {
	s.mu.Lock()
	l, ok := s.locks[ref]
	if !ok {
		l = &sync.Mutex{}
		s.locks[ref] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// FunctionsChanged implements api.FunctionsHook.
func (s *Syncer) FunctionsChanged(ctx context.Context, ref string) error {
	return s.SyncProject(ctx, ref)
}

// eligible reports whether files should exist for a project in this status.
func eligible(p *registry.Project) bool {
	if p.Ref == config.SystemRef {
		return false
	}
	switch p.Status {
	case registry.StatusGoingDown, registry.StatusRemoved, registry.StatusInitFailed:
		return false
	}
	return true
}

// Reconcile syncs every project (one that is going away loses its files) and removes the
// files of projects the registry no longer has. It is what makes key rotation, restored data and a
// deployment whose first attempt failed show up without anyone asking; a failure of one
// project does not stop the others.
func (s *Syncer) Reconcile(ctx context.Context) error {
	ps, err := s.d.Registry.ListProjects(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for i := range ps {
		if ps[i].Ref == config.SystemRef {
			continue
		}
		if err := s.SyncProject(ctx, ps[i].Ref); err != nil {
			s.log.Warn("edge functions: reconcile", "ref", ps[i].Ref, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", ps[i].Ref, err))
		}
	}
	if err := s.collectGone(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Run reconciles once and then every [functions] reconcile_seconds until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	every := time.Duration(s.d.Cfg.Functions.Reconcile()) * time.Second
	for {
		if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("edge functions: reconcile finished with errors", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// SyncProject makes the project's environment file and function generations match the
// store: it writes what is missing or different, and removes functions that are gone. A
// project the registry no longer knows, or that is going away, loses its tree; so does
// one with neither a function nor a secret.
func (s *Syncer) SyncProject(ctx context.Context, ref string) error {
	if err := validRef(ref); err != nil {
		return err
	}
	defer s.lock(ref)()
	return s.syncLocked(ctx, ref)
}

func (s *Syncer) syncLocked(ctx context.Context, ref string) error {
	p, err := s.d.Registry.GetProject(ctx, ref)
	if errors.Is(err, registry.ErrNotFound) {
		return s.removeFiles(ref)
	}
	if err != nil {
		return err
	}
	if !eligible(p) {
		return s.removeFiles(ref)
	}
	cfg := s.d.Cfg
	fns, err := s.d.Store.ListFunctions(ctx, ref)
	if err != nil {
		return err
	}
	stored, err := s.d.Store.ListFunctionSecrets(ctx, ref)
	if err != nil {
		return err
	}
	if len(fns) == 0 && len(stored) == 0 {
		// Nothing to run and nothing to configure: the project's keys are not copied into
		// a file for a project that does not use Edge Functions.
		return s.removeFiles(ref)
	}
	env, err := s.buildEnv(ctx, p, stored)
	if err != nil {
		return err
	}
	dir := FunctionsDir(cfg, ref)
	if err := os.MkdirAll(filepath.Join(dir, genDirName), 0o700); err != nil {
		return err
	}
	if _, err := writeIfChanged(EnvPath(cfg, ref), env, 0o600); err != nil {
		return err
	}
	var errs []error
	want := map[string]bool{}
	for i := range fns {
		f := &fns[i]
		want[f.Slug] = true
		if err := s.syncFunction(ctx, dir, f); err != nil {
			errs = append(errs, fmt.Errorf("function %s: %w", f.Slug, err))
		}
	}
	if err := s.removeStale(dir, want); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// collectGone removes the trees of projects the registry no longer has (a project deleted
// while no sync of it ran, or from the command line). Each candidate is looked up again
// under its lock, so a project created since the caller listed the registry keeps the
// files its first deployment just wrote.
func (s *Syncer) collectGone(ctx context.Context) error {
	ents, err := os.ReadDir(s.d.Cfg.Paths().FunctionsRoot())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ents {
		ref := e.Name()
		if !e.IsDir() || validRef(ref) != nil {
			continue
		}
		unlock := s.lock(ref)
		if _, err := s.d.Registry.GetProject(ctx, ref); errors.Is(err, registry.ErrNotFound) {
			if err := s.removeFiles(ref); err != nil {
				errs = append(errs, err)
			} else {
				s.log.Info("edge functions: removed the files of a project that no longer exists", "ref", ref)
			}
		}
		unlock()
	}
	return errors.Join(errs...)
}

// RemoveProject deletes everything this package keeps for ref. A node that turns the
// feature off for one project uses it; project deletion needs no call (collectGone).
func (s *Syncer) RemoveProject(ref string) error {
	if err := validRef(ref); err != nil {
		return err
	}
	defer s.lock(ref)()
	return s.removeFiles(ref)
}

// removeFiles deletes the tree of ref (the caller holds the lock).
func (s *Syncer) removeFiles(ref string) error { return os.RemoveAll(ProjectDir(s.d.Cfg, ref)) }

// envDoc is functions-env.json.
type envDoc struct {
	Version   int               `json:"version"`
	JWTSecret string            `json:"jwt_secret"`
	Supabase  map[string]string `json:"supabase"`
	Secrets   map[string]string `json:"secrets"`
}

// buildEnv renders functions-env.json for p: the values hosted passes to functions
// (SUPABASE_URL, SUPABASE_ANON_KEY, SUPABASE_SERVICE_ROLE_KEY, SUPABASE_DB_URL), the
// opaque keys as the newer client libraries read them, and the project's secrets.
func (s *Syncer) buildEnv(ctx context.Context, p *registry.Project, stored []api.FunctionSecret) ([]byte, error) {
	k, err := s.d.Keys(ctx, p.Ref)
	if err != nil {
		return nil, fmt.Errorf("project keys: %w", err)
	}
	if k.JWTSecret == "" {
		return nil, fmt.Errorf("project %s has no JWT secret", p.Ref)
	}
	db := url.URL{
		Scheme: "postgres", User: url.UserPassword("postgres", k.DBPassword),
		Host: "127.0.0.1:" + strconv.Itoa(s.d.Cfg.PortsFor(p.Ref, p.Seq).Postgres), Path: "/postgres", RawQuery: "sslmode=disable",
	}
	sb := map[string]string{
		"SUPABASE_URL":              ProjectURL(s.d.Cfg, p.Ref),
		"SUPABASE_ANON_KEY":         k.AnonKey,
		"SUPABASE_SERVICE_ROLE_KEY": k.ServiceRoleKey,
		"SUPABASE_DB_URL":           db.String(),
	}
	if k.PublishableKey != "" {
		sb["SUPABASE_PUBLISHABLE_KEYS"] = jsonObject(map[string]string{"default": k.PublishableKey})
	}
	if k.SecretKey != "" {
		sb["SUPABASE_SECRET_KEYS"] = jsonObject(map[string]string{"default": k.SecretKey})
	}
	sec := make(map[string]string, len(stored))
	for _, e := range stored {
		plain, err := s.d.Secrets.Open(e.Sealed)
		if err != nil {
			return nil, fmt.Errorf("open secret %s: %w", e.Name, err)
		}
		sec[e.Name] = string(plain)
	}
	return json.MarshalIndent(envDoc{Version: 1, JWTSecret: k.JWTSecret, Supabase: sb, Secrets: sec}, "", " ")
}

func jsonObject(m map[string]string) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// meta is .sbctl-function.json inside a generation.
type meta struct {
	Slug      string `json:"slug"`
	Version   int    `json:"version"`
	VerifyJWT bool   `json:"verify_jwt"`
	// Kind is "source" (files, run from the generation directory; the default) or "eszip"
	// (a bundle the CLI built, run from Eszip).
	Kind string `json:"kind,omitempty"`
	// Entrypoint is a path inside the generation for source functions and the module
	// specifier inside the bundle (a file URL) for eszip functions.
	Entrypoint string `json:"entrypoint"`
	ImportMap  string `json:"import_map,omitempty"`
	// Eszip is the file of an eszip function, relative to the generation.
	Eszip string `json:"eszip,omitempty"`
	// SHA256 covers the uploaded files (paths and contents).
	SHA256 string `json:"sha256"`
}

func filesHash(files []api.FunctionFile) string {
	sorted := append([]api.FunctionFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%d:%s:%d:", len(f.Path), f.Path, len(f.Content))
		h.Write(f.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cleanRel validates a stored file path again: the API checked it on upload, but this
// package writes with it, so it does not rely on that.
func cleanRel(p string) (string, error) {
	if p == "" || strings.ContainsAny(p, "\\\x00") || path.IsAbs(p) {
		return "", fmt.Errorf("invalid file path %q", p)
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("invalid file path %q", p)
	}
	return c, nil
}

// syncFunction makes the generation behind <dir>/<slug> the one the store describes.
func (s *Syncer) syncFunction(ctx context.Context, dir string, f *api.Function) error {
	files, err := s.d.Store.FunctionFiles(ctx, f.Ref, f.Slug)
	if err != nil {
		return err
	}
	link := filepath.Join(dir, f.Slug)
	if len(files) == 0 {
		// Created without sources (the legacy JSON create): nothing to serve.
		return removeLink(link, dir, f.Slug, "")
	}
	m := meta{Slug: f.Slug, Version: f.Version, VerifyJWT: f.VerifyJWT, SHA256: filesHash(files)}
	var eszip []byte
	if len(files) == 1 && files[0].Path == api.BundleFileName {
		if eszip, err = decodeBundle(files[0].Content); err != nil {
			return err
		}
		if f.EntrypointPath == "" {
			return errors.New("a bundled function needs its entrypoint")
		}
		m.Kind, m.Entrypoint, m.ImportMap, m.Eszip = kindEszip, f.EntrypointPath, f.ImportMapPath, EszipFileName
	} else {
		if err := checkSourceFiles(&m, f, files); err != nil {
			return err
		}
	}
	if cur, ok := liveMeta(link); ok && cur == m {
		return nil
	}
	gen, err := os.MkdirTemp(filepath.Join(dir, genDirName), fmt.Sprintf("%s.%d.", f.Slug, f.Version))
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(gen)
		}
	}()
	if eszip != nil {
		if err := os.WriteFile(filepath.Join(gen, EszipFileName), eszip, 0o600); err != nil {
			return err
		}
	} else {
		for _, file := range files {
			c, _ := cleanRel(file.Path)
			dst := filepath.Join(gen, filepath.FromSlash(c))
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(dst, file.Content, 0o600); err != nil {
				return err
			}
		}
	}
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(gen, MetaFileName), body, 0o600); err != nil {
		return err
	}
	if err := swapLink(link, filepath.Join(genDirName, filepath.Base(gen))); err != nil {
		return err
	}
	ok = true
	s.collect(dir, f.Slug, filepath.Base(gen))
	return nil
}

const (
	kindEszip = "eszip"
	// EszipFileName is the decompressed bundle inside the generation of an eszip function.
	EszipFileName = "bundle.eszip"
	// maxEszipSize bounds a decompressed bundle (the upload itself is limited to 64 MiB).
	maxEszipSize = 512 << 20
)

// checkSourceFiles validates the files of a source function and fills m's entrypoint and
// import map from f.
func checkSourceFiles(m *meta, f *api.Function, files []api.FunctionFile) (err error) {
	if m.Entrypoint, err = cleanRel(f.EntrypointPath); err != nil {
		return fmt.Errorf("entrypoint: %w", err)
	}
	if f.ImportMapPath != "" {
		if m.ImportMap, err = cleanRel(f.ImportMapPath); err != nil {
			return fmt.Errorf("import map: %w", err)
		}
	}
	paths := make(map[string]bool, len(files))
	for _, file := range files {
		c, err := cleanRel(file.Path)
		if err != nil {
			return err
		}
		if c == MetaFileName || c == api.BundleFileName {
			return fmt.Errorf("a function cannot contain a file named %s", c)
		}
		paths[c] = true
	}
	if !paths[m.Entrypoint] {
		return fmt.Errorf("entrypoint %s is not among the uploaded files", m.Entrypoint)
	}
	if m.ImportMap != "" && !paths[m.ImportMap] {
		return fmt.Errorf("import map %s is not among the uploaded files", m.ImportMap)
	}
	return nil
}

// decodeBundle turns an uploaded bundle ("EZBR" and a Brotli stream, as the Supabase CLI
// sends it) into the plain eszip the runtime loads.
func decodeBundle(body []byte) ([]byte, error) {
	rest, ok := bytes.CutPrefix(body, []byte("EZBR"))
	if !ok {
		return nil, errors.New("the stored bundle does not start with EZBR")
	}
	out, err := io.ReadAll(io.LimitReader(brotli.NewReader(bytes.NewReader(rest)), maxEszipSize+1))
	if err != nil {
		return nil, fmt.Errorf("decompressing the bundle: %w", err)
	}
	if len(out) > maxEszipSize {
		return nil, errors.New("the bundle is too large once decompressed")
	}
	if !bytes.HasPrefix(out, []byte("ESZIP")) {
		return nil, errors.New("the bundle is not an eszip")
	}
	return out, nil
}

// liveMeta reads the metadata of the generation link points at.
func liveMeta(link string) (meta, bool) {
	b, err := os.ReadFile(filepath.Join(link, MetaFileName))
	if err != nil {
		return meta{}, false
	}
	var m meta
	if json.Unmarshal(b, &m) != nil {
		return meta{}, false
	}
	return m, true
}

// swapLink points link at target (relative to link's directory) in one rename, so a reader
// resolves either the old target or the new one.
func swapLink(link, target string) error {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(link), "."+filepath.Base(link)+".new-"+hex.EncodeToString(b[:]))
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	// A directory where the link belongs (something else put it there) cannot be renamed
	// over; it is not ours to keep.
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&fs.ModeSymlink == 0 {
		if err := os.RemoveAll(link); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// removeLink deletes the link of a function that is no longer served and its generations
// (keep names one generation to leave alone).
func removeLink(link, dir, slug, keep string) error {
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	collectGens(dir, slug, keep, "")
	return nil
}

// collect removes the generations of slug other than keep and the one before it. The
// previous generation stays because workers of the old deployment may still be answering
// requests from it.
func (s *Syncer) collect(dir, slug, keep string) {
	collectGens(dir, slug, keep, s.previous(dir, slug, keep))
}

// previous is the newest generation of slug other than keep, which a worker of the
// previous deployment may still be reading.
func (s *Syncer) previous(dir, slug, keep string) string {
	ents, err := os.ReadDir(filepath.Join(dir, genDirName))
	if err != nil {
		return ""
	}
	var best string
	var bestTime time.Time
	for _, e := range ents {
		if e.Name() == keep || !isGenOf(e.Name(), slug) {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().After(bestTime) {
			best, bestTime = e.Name(), fi.ModTime()
		}
	}
	return best
}

// isGenOf reports whether a generation directory name belongs to slug. Names are
// <slug>.<version>.<random>; a slug never contains a dot, so no slug's generations can be
// mistaken for another's (hello-2 is not a generation of hello).
func isGenOf(name, slug string) bool {
	rest, ok := strings.CutPrefix(name, slug+".")
	if !ok {
		return false
	}
	v, _, ok := strings.Cut(rest, ".")
	if !ok {
		return false
	}
	_, err := strconv.Atoi(v)
	return err == nil
}

func collectGens(dir, slug, keep, keep2 string) {
	ents, err := os.ReadDir(filepath.Join(dir, genDirName))
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.Name() == keep || e.Name() == keep2 || !isGenOf(e.Name(), slug) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, genDirName, e.Name()))
	}
}

// removeStale deletes links of functions the store no longer has, leftovers of an
// interrupted swap, and generations nothing refers to any more.
func (s *Syncer) removeStale(dir string, want map[string]bool) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	live := map[string]bool{}
	for _, e := range ents {
		name := e.Name()
		switch {
		case name == genDirName:
		case strings.HasPrefix(name, "."):
			// ".<slug>.new-<random>": a swap that did not finish.
			if fi, err := e.Info(); err == nil && s.now().Sub(fi.ModTime()) > time.Minute {
				_ = os.RemoveAll(filepath.Join(dir, name))
			}
		case want[name]:
			if t, err := os.Readlink(filepath.Join(dir, name)); err == nil {
				live[filepath.Base(t)] = true
			}
		default:
			if err := removeLink(filepath.Join(dir, name), dir, name, ""); err != nil {
				errs = append(errs, err)
			}
		}
	}
	// Generations that no link points at, other than a recent one (a deployment in
	// flight, or the previous generation that old workers still read).
	gens, err := os.ReadDir(filepath.Join(dir, genDirName))
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, g := range gens {
		if live[g.Name()] {
			continue
		}
		if fi, err := g.Info(); err == nil && s.now().Sub(fi.ModTime()) > genGrace {
			_ = os.RemoveAll(filepath.Join(dir, genDirName, g.Name()))
		}
	}
	return errors.Join(errs...)
}

// genGrace is how long an unreferenced generation is kept: the longest a worker of an old
// deployment can live is the wall clock limit, 400 s by default, plus the idle wait.
const genGrace = 15 * time.Minute

// writeIfChanged writes b to p atomically with mode unless the file already holds b with
// that mode. It reports whether it wrote.
func writeIfChanged(p string, b []byte, mode os.FileMode) (bool, error) {
	if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, b) {
		if fi, err := os.Stat(p); err == nil && fi.Mode().Perm() == mode {
			return false, nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, err
	}
	if _, err := io.Copy(tmp, bytes.NewReader(b)); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), p)
}

func validRef(ref string) error {
	if len(ref) != 20 {
		return fmt.Errorf("functions: invalid project ref %q", ref)
	}
	for _, c := range ref {
		if c < 'a' || c > 'z' {
			return fmt.Errorf("functions: invalid project ref %q", ref)
		}
	}
	return nil
}
