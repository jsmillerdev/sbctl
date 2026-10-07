package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/OWNER/sbctl/internal/config"
)

// ErrNotFetched is returned by Dir when the artifact is not unpacked yet.
var ErrNotFetched = errors.New("artifacts: not fetched; run `sbctl artifacts fetch`")

// markerFile is written at the root of every unpacked artifact.
const markerFile = ".sbctl-artifact.json"

// Marker records where an unpacked artifact came from.
type Marker struct {
	Service   string    `json:"service"`
	Tag       string    `json:"tag"`
	Platform  string    `json:"platform"`
	SHA256    string    `json:"sha256"`
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
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

// WithHTTPClient replaces the HTTP client (tests, proxies).
func WithHTTPClient(c *http.Client) Option { return func(s *Store) { s.client = c } }

// WithLogger sets the logger; the default discards.
func WithLogger(l *slog.Logger) Option { return func(s *Store) { s.log = l } }

// WithVersions replaces the versions loaded from versions.yaml.
func WithVersions(v *Versions) Option { return func(s *Store) { s.versions = v } }

// New builds a Store for cfg. Versions come from cfg.Artifacts.VersionsFile or the
// embedded versions.yaml; the platform is cfg.Platform or the running one.
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
	dir, err := s.Path(svc)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w (%s)", ErrNotFetched, svc)
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

// fetchStudio installs our Studio build from config.Studio.ArtifactURL, which must
// match config.Studio.ArtifactSHA256.
func (s *Store) fetchStudio(ctx context.Context) (string, error) {
	tag, err := s.versions.Tag(config.SvcStudio)
	if err != nil {
		return "", err
	}
	final := s.cfg.Paths().Artifact(config.SvcStudio, tag)
	l := s.lock(final)
	l.Lock()
	defer l.Unlock()
	if _, err := os.Stat(final); err == nil {
		return final, nil
	}
	url, want := s.cfg.Studio.ArtifactURL, strings.ToLower(strings.TrimSpace(s.cfg.Studio.ArtifactSHA256))
	if url == "" || want == "" {
		return "", errors.New("artifacts: studio.artifact_url and studio.artifact_sha256 must both be set to fetch Studio")
	}
	if len(want) != 64 {
		return "", errors.New("artifacts: studio.artifact_sha256 is not a SHA-256 hex digest")
	}
	archive := filepath.Join(s.cacheDir(), "studio-"+tag+".tar.zst")
	if err := s.ensureArchive(ctx, archive, want, url); err != nil {
		return "", err
	}
	return final, s.install(archive, final, Marker{Service: config.SvcStudio, Tag: tag, Platform: s.platform, SHA256: want, Source: url})
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

// get downloads a small file with a few retries.
func (s *Store) get(ctx context.Context, url string) ([]byte, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
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
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*2) * time.Second):
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
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".unpack-"+filepath.Base(final)+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := Unpack(f, tmp); err != nil {
		return err
	}
	m.FetchedAt = time.Now().UTC()
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, markerFile), append(mb, '\n'), 0o644); err != nil {
		return err
	}
	// MkdirTemp creates 0700; artifacts are public software that the sbctl user runs even
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

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
