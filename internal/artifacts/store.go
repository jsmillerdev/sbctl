package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/ctxutil"
	"github.com/supavise/supavise/internal/fsutil"
)

// ErrNotFetched is returned by Dir when the artifact is not unpacked yet.
var ErrNotFetched = errors.New("artifacts: not fetched; run `supavise artifacts fetch`")

// markerFile is written at the root of every unpacked artifact.
const markerFile = ".supavise-artifact.json"

// Marker records where an unpacked artifact came from.
type Marker struct {
	Service   string    `json:"service"`
	Tag       string    `json:"tag"`
	Platform  string    `json:"platform"`
	SHA256    string    `json:"sha256"`
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
	// AdoptedFrom is the directory a Studio build was linked from instead of downloaded: the same
	// build, installed under its upstream tag alone by a release from before the patch set
	// (fetchStudio). SHA256 and Source are then that directory's.
	AdoptedFrom string `json:"adopted_from,omitempty"`
}

// Store fetches, verifies and unpacks artifacts into config.Paths.Artifact(svc, tag).
// It is safe for concurrent use; concurrent fetches of one artifact are serialized.
type Store struct {
	cfg      *config.Config
	versions *Versions
	platform string
	client   *http.Client
	log      *slog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// Option customises a Store.
type Option func(*Store)

// WithLogger sets the logger; the default discards.
func WithLogger(l *slog.Logger) Option { return func(s *Store) { s.log = l } }

// WithVersions replaces the versions loaded from internal/versions/versions.yaml.
func WithVersions(v *Versions) Option { return func(s *Store) { s.versions = v } }

// New builds a Store for cfg. Versions come from cfg.Artifacts.VersionsFile or the
// embedded internal/versions/versions.yaml; the platform is cfg.Platform or the running one.
func New(cfg *config.Config, opts ...Option) (*Store, error) {
	platform, err := Platform(cfg)
	if err != nil {
		return nil, err
	}
	s := &Store{
		cfg:      cfg,
		platform: platform,
		client:   &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 60 * time.Second, TLSHandshakeTimeout: 20 * time.Second}},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		locks:    map[string]*sync.Mutex{},
	}
	for _, o := range opts {
		o(s)
	}
	if s.versions == nil {
		if s.versions, err = LoadVersions(cfg); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Versions returns the pinned versions the store uses.
func (s *Store) Versions() *Versions { return s.versions }

// Platform returns the platform whose builds the store fetches.
func (s *Store) Platform() string { return s.platform }

// Tag returns the pinned tag of svc.
func (s *Store) Tag(svc string) (string, error) { return s.versions.Tag(svc) }

// Path is where svc's artifact lives (or will live) on disk.
func (s *Store) Path(svc string) (string, error) {
	tag, err := s.versions.Tag(svc)
	if err != nil {
		return "", err
	}
	return s.cfg.Paths().Artifact(svc, tag), nil
}

// Dir returns the unpacked artifact root of svc, or ErrNotFetched.
func (s *Store) Dir(svc string) (string, error) {
	tag, err := s.versions.Tag(svc)
	if err != nil {
		return "", err
	}
	return s.DirFor(svc, tag)
}

// DirFor returns the unpacked artifact root of svc at tag, which need not be the pinned one (a
// project keeps running the versions it was last upgraded to), or ErrNotFetched.
func (s *Store) DirFor(svc, tag string) (string, error) {
	dir := s.cfg.Paths().Artifact(svc, tag)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w (%s %s)", ErrNotFetched, svc, tag)
		}
		return "", err
	}
	return dir, nil
}

// cacheDir is where downloaded archives are kept.
func (s *Store) cacheDir() string {
	if d := s.cfg.Artifacts.CacheDir; d != "" {
		return d
	}
	return filepath.Join(s.cfg.Paths().Artifacts(), ".cache")
}

func (s *Store) lock(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.locks[key]
	if m == nil {
		m = &sync.Mutex{}
		s.locks[key] = m
	}
	return m
}

// Fetch makes svc's pinned artifact available and returns its directory. An artifact
// that is already unpacked is returned as is. Otherwise the archive is downloaded from
// <base_url>/<tag>/<tag>-<platform>.tar.zst (or reused from the cache), verified against
// the release SHA256SUMS, unpacked next to its final location and renamed into place.
// svc config.SvcStudio fetches the Studio build from config.Studio instead.
func (s *Store) Fetch(ctx context.Context, svc string) (string, error) {
	if svc == config.SvcStudio {
		return s.fetchStudio(ctx)
	}
	tag, err := s.versions.Tag(svc)
	if err != nil {
		return "", err
	}
	return s.FetchTag(ctx, svc, tag)
}

// FetchTag is Fetch for an explicit release tag of svc (not Studio) instead of the pinned one: the
// lifecycle uses it to bring back the artifacts of a project's recorded versions when they are
// gone from disk.
func (s *Store) FetchTag(ctx context.Context, svc, tag string) (string, error) {
	if svc == config.SvcStudio {
		return "", errors.New("artifacts: Studio is fetched from [studio] artifact_url, not by tag")
	}
	final := s.cfg.Paths().Artifact(svc, tag)
	l := s.lock(final)
	l.Lock()
	defer l.Unlock()
	if _, err := os.Stat(final); err == nil {
		return final, nil
	}

	name := archiveName(tag, s.platform)
	want, err := s.releaseDigest(ctx, tag, name)
	if err != nil {
		return "", err
	}
	archive := filepath.Join(s.cacheDir(), name)
	if err := s.ensureArchive(ctx, archive, want, strings.TrimRight(s.cfg.Artifacts.BaseURL, "/")+"/"+tag+"/"+name); err != nil {
		return "", err
	}
	return final, s.install(archive, final, Marker{Service: svc, Tag: tag, Platform: s.platform, SHA256: want, Source: name})
}

// fetchStudio installs the Studio build the versions pin (Versions.StudioBuild, the upstream tag
// and the patch set) under artifacts/studio/<build>, and returns that directory. A build that is
// there is used as it is. Otherwise:
//
//   - A release from before the patch set installed the build under the upstream tag alone
//     (artifacts/studio/<tag>). When that directory holds the pinned build, it is linked into place
//     file by file and nothing is downloaded; the old directory stays for the Studio that still runs
//     from it and for a rollback. Its share/supavise/build-info.json says which build it is; a build
//     without one (a stand-in) counts when its marker has the digest config.Studio names.
//   - Else the archive at config.Studio.ArtifactURL is downloaded, checked against
//     config.Studio.ArtifactSHA256 and unpacked.
//
// The directory of a build holds that build and no other. A URL whose file name is the release
// asset of another build (StudioAsset) is refused before anything is downloaded, and an archive
// whose build-info names another build is refused before it is put in place: a config.toml that
// still names an older build cannot put it where the binary looks for a newer one. `supavise
// upgrade` hands the fetch the release's own asset through the environment
// (SUPAVISE_STUDIO_ARTIFACT_URL and _SHA256, from the signed checksum list).
func (s *Store) fetchStudio(ctx context.Context) (string, error) {
	build, err := s.versions.Tag(config.SvcStudio)
	if err != nil {
		return "", err
	}
	final := s.cfg.Paths().Artifact(config.SvcStudio, build)
	l := s.lock(final)
	l.Lock()
	defer l.Unlock()
	if _, err := os.Stat(final); err == nil {
		return final, nil
	}
	url, want := s.cfg.Studio.ArtifactURL, strings.ToLower(strings.TrimSpace(s.cfg.Studio.ArtifactSHA256))
	// Versions without a patch set name the build by the tag alone and cannot tell builds apart.
	named, recognized := StudioBuildOfURL(url, s.platform)
	stale := recognized && named != build && s.versions.Studio.Patchset > 0
	if s.adoptStudio(build, final, want, stale) {
		return final, nil
	}
	if url == "" || want == "" {
		return "", errors.New("artifacts: studio.artifact_url and studio.artifact_sha256 must both be set to fetch Studio")
	}
	if len(want) != 64 {
		return "", errors.New("artifacts: studio.artifact_sha256 is not a SHA-256 hex digest")
	}
	if stale {
		return "", fmt.Errorf("artifacts: studio.artifact_url names the Studio build %s and this binary runs %s; refusing to install one build in place of the other (`sudo supavise upgrade` fetches the release's build, and `sudo supavise system converge` fetches the build of the installed release)", named, build)
	}
	archive := filepath.Join(s.cacheDir(), "studio-"+build+".tar.zst")
	if err := s.ensureArchive(ctx, archive, want, url); err != nil {
		return "", err
	}
	m := Marker{Service: config.SvcStudio, Tag: build, Platform: s.platform, SHA256: want, Source: url}
	return final, s.installChecked(archive, final, m, s.checkStudioBuild)
}

// studioBuildInfo is the part of a Studio build's share/supavise/build-info.json (studio/build.sh)
// that names the build.
type studioBuildInfo struct {
	Service     string `json:"service"`
	UpstreamTag string `json:"upstream_tag"`
	Patchset    int    `json:"patchset"`
	Platform    string `json:"platform"`
}

// readStudioBuildInfo reads the build-info of a Studio tree; a tree without one, or with one that
// is not a supavise-studio build's (a stand-in), has none (os.ErrNotExist).
func readStudioBuildInfo(dir string) (*studioBuildInfo, error) {
	b, err := os.ReadFile(filepath.Join(dir, "share", "supavise", "build-info.json"))
	if err != nil {
		return nil, err
	}
	var bi studioBuildInfo
	if err := json.Unmarshal(b, &bi); err != nil {
		return nil, fmt.Errorf("artifacts: %s: build-info.json: %w", dir, err)
	}
	if bi.Service != "supavise-studio" {
		return nil, fmt.Errorf("artifacts: %s is not a supavise-studio build: %w", dir, os.ErrNotExist)
	}
	return &bi, nil
}

// checkStudioBuild refuses an unpacked Studio tree whose build-info names a build other than the
// pinned one. A tree without build-info (a stand-in) names none and passes.
func (s *Store) checkStudioBuild(dir string) error {
	bi, err := readStudioBuildInfo(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	got, want := StudioBuildName(bi.UpstreamTag, bi.Patchset), s.versions.StudioBuild()
	if s.versions.Studio.Patchset == 0 { // the build is named by its tag alone: any patch set of it
		got = bi.UpstreamTag
	}
	if got != want {
		return fmt.Errorf("artifacts: the downloaded Studio archive is the build %s and this binary runs %s; refusing it", got, want)
	}
	if bi.Platform != "" && bi.Platform != s.platform {
		return fmt.Errorf("artifacts: the downloaded Studio archive is built for %s, this node is %s", bi.Platform, s.platform)
	}
	return nil
}

// adoptStudio links the pinned Studio build into final when a release from before the patch set
// installed it under the upstream tag alone (see fetchStudio). want is the digest config.Studio
// names, and stale says its URL names another build, in which case a stand-in without
// build-info is not taken on the digest alone. It reports whether final now holds the build; a
// link that fails is logged and the caller downloads instead.
func (s *Store) adoptStudio(build, final, want string, stale bool) bool {
	tag := s.versions.Studio.Tag
	if build == tag {
		return false // the build is named by its tag: there is no older name to look under
	}
	old := s.cfg.Paths().Artifact(config.SvcStudio, tag)
	if _, err := os.Stat(old); err != nil {
		return false
	}
	m, _ := ReadMarker(old)
	bi, err := readStudioBuildInfo(old)
	switch {
	case err == nil:
		if StudioBuildName(bi.UpstreamTag, bi.Patchset) != build || (bi.Platform != "" && bi.Platform != s.platform) {
			return false
		}
	case errors.Is(err, os.ErrNotExist):
		if stale || m == nil || want == "" || !strings.EqualFold(m.SHA256, want) {
			return false
		}
	default:
		return false
	}
	nm := Marker{Service: config.SvcStudio, Tag: build, Platform: s.platform, AdoptedFrom: old}
	if m != nil {
		nm.SHA256, nm.Source = m.SHA256, m.Source
	}
	if err := s.linkInstall(old, final, nm); err != nil {
		s.log.Warn("could not link the installed Studio build into its new directory; downloading it", "from", old, "to", final, "error", err)
		return false
	}
	s.log.Info("Studio build linked into place from its directory before the patch set", "build", build, "from", old)
	return true
}

// linkInstall makes final a copy of the tree src made of hard links (symlinks are copied, and
// src's marker and Next's runtime cache are left out), with marker m, through a sibling temp
// directory renamed into place like install. Nothing writes an artifact's files in place (the
// Studio runtime rewrites its templated files through a temp file and a rename), so the two trees
// never see each other's changes.
func (s *Store) linkInstall(src, final string, m Marker) error {
	parent := filepath.Dir(final)
	tmp, err := os.MkdirTemp(parent, ".link-"+filepath.Base(final)+"-")
	if err != nil {
		return err
	}
	defer removeTree(tmp)
	if err := linkTree(src, tmp); err != nil {
		return err
	}
	return s.finish(tmp, final, m)
}

// linkTree fills the existing directory dst with hard links to the regular files of src, copies of
// its symlinks and directories of the same modes.
func linkTree(src, dst string) error {
	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirs []dirMode
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == markerFile {
			return nil
		}
		target := filepath.Join(dst, rel)
		switch t := d.Type(); {
		case t.IsDir():
			if d.Name() == "cache" && filepath.Base(filepath.Dir(p)) == ".next" {
				return filepath.SkipDir // written by the Studio that runs from src, not part of the build
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			if err := os.Mkdir(target, 0o755); err != nil {
				return err
			}
			dirs = append(dirs, dirMode{target, fi.Mode().Perm()})
		case t&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, target)
		case t.IsRegular():
			return os.Link(p, target)
		default:
			return fmt.Errorf("artifacts: %s is not a file, a directory or a symlink", p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Modes last, deepest first, so that a read-only directory was writable while it was filled.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return err
		}
	}
	return nil
}

// releaseDigest returns the SHA-256 listed for name in the release's SHA256SUMS. The
// file is cached next to the archives so that a re-fetch works offline.
func (s *Store) releaseDigest(ctx context.Context, tag, name string) (string, error) {
	cached := filepath.Join(s.cacheDir(), tag+".SHA256SUMS")
	url := strings.TrimRight(s.cfg.Artifacts.BaseURL, "/") + "/" + tag + "/SHA256SUMS"
	body, err := s.get(ctx, url)
	if err != nil {
		if b, rerr := os.ReadFile(cached); rerr == nil {
			s.log.Warn("using cached SHA256SUMS", "tag", tag, "error", err)
			return parseSHA256SUMS(b, name)
		}
		return "", err
	}
	want, err := parseSHA256SUMS(body, name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.cacheDir(), 0o755); err == nil {
		_ = writeAtomic(cached, body)
	}
	return want, nil
}

// fetchAttempts bounds the tries of one download; retryDelay(n) is the wait before try n (1, 2, 4
// and 8 seconds), long enough to ride out a release host's brief run of 5xx answers.
const fetchAttempts = 5

func retryDelay(attempt int) time.Duration { return time.Second << (attempt - 1) }

// get downloads a small file with a few retries.
func (s *Store) get(ctx context.Context, url string) ([]byte, error) {
	var last error
	for attempt := 0; attempt < fetchAttempts; attempt++ {
		if attempt > 0 {
			if err := ctxutil.Sleep(ctx, retryDelay(attempt)); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.client.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("artifacts: GET %s: 404", url)
		}
		if err != nil || resp.StatusCode != http.StatusOK {
			last = fmt.Errorf("artifacts: GET %s: status %d: %v", url, resp.StatusCode, err)
			continue
		}
		return b, nil
	}
	return nil, last
}

// ensureArchive guarantees that archive exists and hashes to want, downloading from
// url when it does not. A cached file with a different hash is replaced.
func (s *Store) ensureArchive(ctx context.Context, archive, want, url string) error {
	if got, err := fileSHA256(archive); err == nil {
		if got == want {
			return nil
		}
		s.log.Warn("cached archive has the wrong digest; downloading again", "archive", archive)
	}
	if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
		return err
	}
	s.log.Info("downloading artifact", "url", url)
	var last error
	for attempt := 0; attempt < fetchAttempts; attempt++ {
		if attempt > 0 {
			if err := ctxutil.Sleep(ctx, retryDelay(attempt)); err != nil {
				return err
			}
		}
		if last = s.download(ctx, url, archive, want); last == nil {
			return nil
		}
		var de *digestError
		if errors.As(last, &de) || ctx.Err() != nil {
			return last // a wrong digest is not transient
		}
	}
	return last
}

type digestError struct{ url, got, want string }

func (e *digestError) Error() string {
	return fmt.Sprintf("artifacts: %s has SHA-256 %s, expected %s", e.url, e.got, e.want)
}

func (s *Store) download(ctx context.Context, url, dest, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("artifacts: GET %s: status %d", url, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".part-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return &digestError{url: url, got: got, want: want}
	}
	return os.Rename(tmp.Name(), dest)
}

// install unpacks archive into a sibling temp directory and renames it to final, so
// that final either does not exist or is complete.
func (s *Store) install(archive, final string, m Marker) error {
	return s.installChecked(archive, final, m, nil)
}

// installChecked is install with check run on the unpacked tree before it is put in place; an
// error from it leaves final as it was.
func (s *Store) installChecked(archive, final string, m Marker, check func(dir string) error) error {
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".unpack-"+filepath.Base(final)+"-")
	if err != nil {
		return err
	}
	defer removeTree(tmp)
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := Unpack(f, tmp); err != nil {
		return err
	}
	if check != nil {
		if err := check(tmp); err != nil {
			return err
		}
	}
	return s.finish(tmp, final, m)
}

// finish writes the marker into the complete tree tmp, hands it to the state directory's owner and
// renames it to final.
func (s *Store) finish(tmp, final string, m Marker) error {
	m.FetchedAt = time.Now().UTC()
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, markerFile), append(mb, '\n'), 0o644); err != nil {
		return err
	}
	// MkdirTemp creates 0700; artifacts are public software that the supavise user runs even
	// when root fetched them, so the root of the tree must be traversable.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	if err := handOver(tmp, s.cfg.StateDir); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		if _, serr := os.Stat(final); serr == nil {
			return nil // another process installed it first
		}
		return err
	}
	s.log.Info("artifact installed", "service", m.Service, "tag", m.Tag, "dir", final)
	return nil
}

// removeTree removes a temp tree that was not put in place (nothing, once it was renamed). An
// artifact can hold read-only directories, whose entries only go once the directory is writable.
func removeTree(dir string) {
	if os.RemoveAll(dir) == nil {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}

// ReadMarker returns the marker of an unpacked artifact, if it has one.
func ReadMarker(dir string) (*Marker, error) {
	b, err := os.ReadFile(filepath.Join(dir, markerFile))
	if err != nil {
		return nil, err
	}
	var m Marker
	return &m, json.Unmarshal(b, &m)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeAtomic replaces path with b through a temporary file of its own, so two writers never share
// one.
func writeAtomic(path string, b []byte) error {
	return fsutil.WriteFile(path, b, 0o644, fsutil.Options{})
}
