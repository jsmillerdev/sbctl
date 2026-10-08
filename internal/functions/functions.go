// Package functions puts what the Management API stores for Edge Functions (deployments,
// sources, sealed secrets) where the runtime reads it: for each project, a generation
// directory per deployed function, a symlink that makes one generation live, and one
// environment file with the project's keys and secrets. The Deno main service in
// internal/functions/mainservice/ resolves a request to exactly these files.
//
//	<state>/system/edge-runtime/tenants/<ref>/functions-env.json    jwt secret, SUPABASE_* values, secrets (0600)
//	<state>/system/edge-runtime/tenants/<ref>/functions/<slug>      symlink to .gen/<slug>.<version>.<random>
//	<state>/system/edge-runtime/tenants/<ref>/functions/.gen/<...>/ the uploaded files and .supavise-function.json
//
// The tree lives in the Edge Runtime's own state directory because that is the one place
// its systemd unit sees; the projects' directories (clusters, sockets, unit files) stay
// out of its mount namespace. A deployment writes a new generation and renames a new
// symlink over the old one, so a request sees the old function or the new one, never a
// mixture or a gap. A project that is deleted loses its tree at the next sync of that
// project or the next reconcile (RemoveFiles does it at once).
package functions

import (
	"bufio"
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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/units"
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
	// Supervisor and Artifacts let the Syncer bundle uploaded sources in the sandbox of
	// supavise-edge-bundle@<ref>.service (lifecycle.Node's Supervisor and Artifacts). Without them, or
	// where the supervisor cannot confine the bundler, source uploads are refused.
	Supervisor units.Supervisor
	Artifacts  ArtifactDirs
}

// Syncer keeps the files of the runtime equal to the store. It implements api.FunctionsHook.
// It is safe for concurrent use; work on one project is serialized.
type Syncer struct {
	d   Deps
	log *slog.Logger
	now func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	// settled remembers, per "<ref>/<slug>", the version stamp of a deployment that was
	// looked at and found to have nothing to serve (no files, sources, an unusable
	// bundle). Reconcile skips it without loading the files again until the stored
	// deployment changes, so a large or hostile upload costs one load, not one per cycle.
	settled map[string]string

	// bundler turns uploaded sources into a bundle; nil, with bundlerWhy saying why, when this
	// node cannot.
	bundler    *Bundler
	bundlerWhy string
}

var (
	_ api.FunctionsHook = (*Syncer)(nil)
	_ api.SourceBundler = (*Syncer)(nil)
)

// New returns a Syncer.
func New(d Deps) (*Syncer, error) {
	if d.Cfg == nil || d.Registry == nil || d.Secrets == nil || d.Store == nil || d.Keys == nil {
		return nil, errors.New("functions: Deps needs Cfg, Registry, Secrets, Store and Keys")
	}
	s := &Syncer{d: d, log: d.Log, now: d.Now, locks: map[string]*sync.Mutex{}, settled: map[string]string{}}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	switch {
	case d.Supervisor == nil || d.Artifacts == nil:
		s.bundlerWhy = "the API server was not given a supervisor and an artifact store to run the bundler with"
	default:
		b, err := NewBundler(d.Cfg, d.Supervisor, d.Artifacts, s.log)
		if err != nil {
			s.bundlerWhy = err.Error()
		}
		s.bundler = b
	}
	return s, nil
}

// BundleSources implements api.SourceBundler.
func (s *Syncer) BundleSources(ctx context.Context, in api.SourceBundle) (*api.BundledSource, error) {
	if s.bundler == nil {
		return nil, fmt.Errorf("%w (%s)", api.ErrBundlingUnavailable, s.bundlerWhy)
	}
	bundle, entry, err := s.bundler.Bundle(ctx, BundleInput{Ref: in.Ref, Files: in.Files, Entrypoint: in.Entrypoint, ImportMap: in.ImportMap, Static: in.StaticPatterns})
	if err != nil {
		return nil, err
	}
	return &api.BundledSource{Bundle: bundle, Entrypoint: entry}, nil
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

// eligible reports whether files should exist for a project in this status: the statuses
// the proxy serves (internal/proxy servable). A paused or pausing project keeps nothing
// on disk, so a request that reaches the runtime without passing the proxy's status check
// finds no function and no keys; resuming rebuilds the files from the store.
func eligible(p *registry.Project) bool {
	if p.Ref == config.SystemRef {
		return false
	}
	switch p.Status {
	case registry.StatusInactive, registry.StatusPausing, registry.StatusGoingDown,
		registry.StatusRemoved, registry.StatusInitFailed:
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
	s.forgetSettled(ref, want)
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
func (s *Syncer) removeFiles(ref string) error {
	s.forgetSettled(ref, nil)
	return os.RemoveAll(ProjectDir(s.d.Cfg, ref))
}

func (s *Syncer) isSettled(key, stamp string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settled[key] == stamp
}

func (s *Syncer) settle(key, stamp string) {
	s.mu.Lock()
	s.settled[key] = stamp
	s.mu.Unlock()
}

// forgetSettled drops the entries of ref whose slug is not in keep (all of them when keep is nil).
func (s *Syncer) forgetSettled(ref string, keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.settled {
		r, slug, _ := strings.Cut(k, "/")
		if r == ref && !keep[slug] {
			delete(s.settled, k)
		}
	}
}

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

// meta is .supavise-function.json inside a generation. Only bundles are served: a function
// that runs from real source files can import files outside its own directory through
// relative specifiers (the module loader follows them), which in a tree shared by every
// project would reach other projects' environment files and code. The module specifiers
// inside an eszip are virtual, so a bundled function has nothing of the node to import.
type meta struct {
	Slug      string `json:"slug"`
	Version   int    `json:"version"`
	VerifyJWT bool   `json:"verify_jwt"`
	// Kind is "eszip", the only kind there is (main services of older supavise versions also
	// wrote "source", which the main service refuses).
	Kind string `json:"kind"`
	// Entrypoint is the module specifier inside the bundle (a file URL).
	Entrypoint string `json:"entrypoint"`
	// Eszip is the bundle file, relative to the generation.
	Eszip string `json:"eszip"`
	// Stamp identifies the stored deployment this generation was made from (see fnStamp).
	Stamp string `json:"stamp"`
	// SHA256 covers the upload as stored.
	SHA256 string `json:"sha256"`
}

// fnStamp identifies one stored deployment of f. Every store write of a function (deploy,
// patch, secrets excluded) bumps the version and the update time, so an equal stamp means
// equal files and metadata and the generation on disk needs no work.
func fnStamp(f *api.Function) string {
	return fmt.Sprintf("%d:%d", f.Version, f.UpdatedAt.UnixNano())
}

// current reports whether the generation m describes is the one the store holds for f.
func (m meta) current(f *api.Function) bool {
	return m.Kind == kindEszip && m.Stamp == fnStamp(f) && m.Slug == f.Slug && m.VerifyJWT == f.VerifyJWT
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

// permanent marks an error that loading the same stored deployment again cannot fix.
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

// syncFunction makes the generation behind <dir>/<slug> the one the store describes. It
// decides from the function's version stamp whether anything must happen before it loads
// a single file: the files of a deployment can be large, and Reconcile visits every
// function of every project every few seconds.
func (s *Syncer) syncFunction(ctx context.Context, dir string, f *api.Function) error {
	link := filepath.Join(dir, f.Slug)
	key, stamp := f.Ref+"/"+f.Slug, fnStamp(f)
	if s.isSettled(key, stamp) {
		return nil
	}
	if cur, ok := liveMeta(link); ok && cur.current(f) {
		return nil
	}
	files, err := s.d.Store.FunctionFiles(ctx, f.Ref, f.Slug)
	if err != nil {
		return err
	}
	bundle := -1
	for i, file := range files {
		if file.Path == api.BundleFileName {
			bundle = i
		}
	}
	switch {
	case len(files) == 0:
		// Created without sources (the legacy JSON create): nothing to serve.
		s.settle(key, stamp)
		return removeLink(link, dir, f.Slug, "")
	case bundle < 0:
		// Sources with no bundle: stored before the API bundled uploads or while no runtime
		// was configured.
		s.settle(key, stamp)
		s.log.Warn("edge functions: a function stored as source files without a bundle is not served; deploy it again",
			"ref", f.Ref, "slug", f.Slug, "version", f.Version)
		return removeLink(link, dir, f.Slug, "")
	}
	// An uploaded bundle names its entrypoint in the function record; sources the node
	// bundled itself record the entrypoint inside the bundle next to it.
	entry := f.EntrypointPath
	for _, file := range files {
		if file.Path == api.BundleInfoFileName {
			var info api.SourceBundleInfo
			if err := json.Unmarshal(file.Content, &info); err != nil || info.Entrypoint == "" {
				s.settle(key, stamp)
				return fmt.Errorf("the stored bundle info is unusable: %v", err)
			}
			entry = info.Entrypoint
		}
	}
	if entry == "" {
		s.settle(key, stamp)
		return errors.New("a bundled function needs its entrypoint")
	}
	m := meta{Slug: f.Slug, Version: f.Version, VerifyJWT: f.VerifyJWT, Kind: kindEszip,
		Entrypoint: entry, Eszip: EszipFileName, Stamp: stamp, SHA256: filesHash(files)}
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
	if err := writeBundleFile(filepath.Join(gen, EszipFileName), files[bundle].Content); err != nil {
		var p permanent
		if errors.As(err, &p) {
			s.settle(key, stamp)
		}
		return err
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
	// EszipFileName is the decompressed bundle inside a generation.
	EszipFileName = "bundle.eszip"
	// maxEszipSize bounds a decompressed bundle. Hosted Edge Functions accept bundles of
	// about 20 MB; the upload itself is limited to 64 MiB compressed, and this (32 MiB) keeps a
	// compression bomb from filling the disk or the main service's memory.
	maxEszipSize = 32 << 20
)

// writeBundleFile decompresses an uploaded bundle ("EZBR" and a Brotli stream, as the
// Supabase CLI sends it) into path, streaming, so memory stays small however large the
// result would be. Failures that depend on the upload alone are permanent.
func writeBundleFile(path string, body []byte) (err error) {
	rest, ok := bytes.CutPrefix(body, []byte("EZBR"))
	if !ok {
		return permanent{errors.New("the stored bundle does not start with EZBR")}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	return decodeBundle(f, rest)
}

// decodeBundle copies the plain eszip out of a Brotli stream (without the EZBR prefix)
// to w, refusing anything that is not an eszip or exceeds maxEszipSize.
func decodeBundle(w io.Writer, compressed []byte) error {
	br := bufio.NewReader(brotli.NewReader(bytes.NewReader(compressed)))
	if head, err := br.Peek(len("ESZIP")); err != nil || string(head) != "ESZIP" {
		return permanent{errors.New("the bundle is not an eszip")}
	}
	n, err := io.Copy(w, io.LimitReader(br, maxEszipSize+1))
	if err != nil {
		return permanent{fmt.Errorf("decompressing the bundle: %w", err)}
	}
	if n > maxEszipSize {
		return permanent{fmt.Errorf("the bundle is larger than %d MiB once decompressed", maxEszipSize>>20)}
	}
	return nil
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
