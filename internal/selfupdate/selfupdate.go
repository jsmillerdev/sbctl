// Package selfupdate replaces the supavise binary with a release from GitHub after checking
// it against an ed25519 signature.
//
// A release carries five kinds of asset: the binaries `supavise-linux-amd64` and
// `supavise-linux-arm64`, `SHA256SUMS` (one line per asset, as sha256sum prints them),
// `SHA256SUMS.sig`, the raw 64-byte ed25519 signature of the SHA256SUMS file, and the manifest
// `supavise-release.json` (manifest.go), which the list covers. The public keys are compiled into
// the binary: release_key.pem, and release_key_next.pem while a key rotation is under way
// (keys.go). Update refuses a release whose signature does not verify against either, whose
// manifest does not match the signed list or names another version than the release tag, whose
// checksum list lacks the binary, or whose binary does not match its checksum, and it never
// touches the installed binary before all of those pass. The replacement is one rename in the binary's own directory, so a crash leaves
// either the old or the new file, and the previous binary stays next to it as
// `<name>.prev` for a manual rollback.
package selfupdate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultRepo is the GitHub repository releases come from. Variable so that a fork can
// set it at build time (-ldflags "-X github.com/supavise/supavise/internal/selfupdate.DefaultRepo=owner/name").
var DefaultRepo = "supavise/supavise"

//go:embed release_key.pem
var releaseKeyPEM []byte

// ErrNoKey means this build has no release signing key, so no release can be verified.
var ErrNoKey = errors.New("this build has no release signing key (internal/selfupdate/release_key.pem is not set); update by re-running the installer from a release instead")

// ParsePublicKey reads a PEM (SPKI) ed25519 public key.
func ParsePublicKey(b []byte) (ed25519.PublicKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PUBLIC KEY" {
		return nil, ErrNoKey
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("release key: %w", err)
	}
	pk, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("release key is %T, want ed25519", k)
	}
	return pk, nil
}

// EmbeddedKey returns the public key compiled into the binary, or ErrNoKey.
func EmbeddedKey() (ed25519.PublicKey, error) { return ParsePublicKey(releaseKeyPEM) }

// Asset names.
const (
	SumsAsset    = "SHA256SUMS"
	SigAsset     = "SHA256SUMS.sig"
	maxSumsBytes = 1 << 20
	maxBinary    = 300 << 20
)

// BinaryAsset is the asset name of the supavise binary for a platform such as "linux-amd64".
func BinaryAsset(platform string) string { return "supavise-" + platform }

// Options configure Check and Update.
type Options struct {
	// Repo is "owner/name"; empty means DefaultRepo.
	Repo string
	// APIBase is the GitHub API root; empty means https://api.github.com.
	APIBase string
	// Tag selects a release ("v1.2.3"); empty means the latest non-prerelease.
	Tag string
	// Platform is "linux-amd64" or "linux-arm64"; empty means the running one.
	Platform string
	// Current is the running version ("v1.2.3" or "dev").
	Current string
	// ExecPath is the binary to replace; empty means the running executable.
	ExecPath string
	// Key verifies SHA256SUMS. Keys, when set, replaces it with a list: the signature
	// is valid if any of them verifies it. With neither set the embedded keys apply.
	Key  ed25519.PublicKey
	Keys []ed25519.PublicKey
	HTTP *http.Client
	// Force installs the release even when it is not newer than Current.
	Force bool
	// Out receives progress lines; nil discards them.
	Out io.Writer
	// StageDir is where Stage writes the downloaded binary; empty means the directory of
	// ExecPath.
	StageDir string
	// Probe runs the downloaded binary to check that it starts and returns what it
	// prints for --version; nil means run it with --version. Tests replace it.
	Probe func(ctx context.Context, path string) (string, error)
}

func (o *Options) repo() string {
	if o.Repo != "" {
		return o.Repo
	}
	return DefaultRepo
}

func (o *Options) apiBase() string {
	if o.APIBase != "" {
		return strings.TrimRight(o.APIBase, "/")
	}
	return "https://api.github.com"
}

func (o *Options) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (o *Options) say(format string, args ...any) {
	if o.Out != nil {
		fmt.Fprintf(o.Out, format+"\n", args...)
	}
}

// Release is the part of a GitHub release this package reads.
type Release struct {
	Tag    string
	Assets map[string]string // name -> download URL
}

// Latest fetches the release o selects.
func Latest(ctx context.Context, o Options) (*Release, error) {
	p := "/repos/" + o.repo() + "/releases/latest"
	if o.Tag != "" {
		p = "/repos/" + o.repo() + "/releases/tags/" + url.PathEscape(o.Tag)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.apiBase()+p, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "supavise-self-update")
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		if o.Tag != "" {
			return nil, fmt.Errorf("release %s not found in %s", o.Tag, o.repo())
		}
		return nil, fmt.Errorf("%s has no published release", o.repo())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github answered %s for %s", resp.Status, p)
	}
	var j struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&j); err != nil {
		return nil, fmt.Errorf("release metadata: %w", err)
	}
	r := &Release{Tag: j.TagName, Assets: map[string]string{}}
	for _, a := range j.Assets {
		r.Assets[a.Name] = a.URL
	}
	if r.Tag == "" {
		return nil, errors.New("release metadata has no tag")
	}
	return r, nil
}

// Newer reports whether tag is a later version than current. A current version that is
// not a release ("dev", a commit-describe string) is older than every release.
func Newer(tag, current string) bool {
	a, ok := ParseVersion(tag)
	if !ok {
		return false
	}
	b, ok := ParseVersion(current)
	if !ok {
		return true
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// ParseVersion reads "v1.2.3"; a suffix ("-rc1", "-4-gabcdef", "-dirty") is dropped, so a
// build made after a tag counts as that tag.
func ParseVersion(s string) ([3]int, bool) {
	var v [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// Result says what Update did.
type Result struct {
	Tag      string
	Replaced bool   // false when already up to date
	Previous string // path of the previous binary, when replaced
}

// Update installs the selected release over ExecPath, or reports that it is current.
func Update(ctx context.Context, o Options) (*Result, error) {
	keys, err := o.keys()
	if err != nil {
		return nil, err
	}
	if o.Platform == "" {
		return nil, errors.New("selfupdate: Options.Platform is required")
	}
	rel, err := Latest(ctx, o)
	if err != nil {
		return nil, err
	}
	o.say("release %s", rel.Tag)
	if o.Tag == "" && !o.Force && !Newer(rel.Tag, o.Current) {
		o.say("already up to date (%s)", o.Current)
		return &Result{Tag: rel.Tag}, nil
	}
	if o.Tag != "" && !o.Force && rel.Tag == o.Current {
		o.say("already running %s", o.Current)
		return &Result{Tag: rel.Tag}, nil
	}
	asset := BinaryAsset(o.Platform)
	if rel.Assets[asset] == "" {
		return nil, fmt.Errorf("release %s has no asset %s", rel.Tag, asset)
	}
	ver, err := o.verify(ctx, rel, keys)
	if err != nil {
		return nil, err
	}
	// A release states the oldest version it upgrades from. --force installs anyway; a downgrade
	// is not an upgrade and is not held to it.
	if !o.Force && Newer(rel.Tag, o.Current) {
		if err := ver.Manifest.CheckUpgradeFrom(o.Current); err != nil {
			return nil, err
		}
	}
	st, err := ver.Stage(ctx, o)
	if err != nil {
		return nil, err
	}
	return st.Install()
}

// Staged is a release binary that has been downloaded, checked against the signed list and run
// with --version, and that sits next to the file it will replace. Install swaps it in; Discard
// throws it away. `supavise upgrade` stages the binary first, reads its pins, and installs it only
// after the node has been backed up.
type Staged struct {
	*Verified
	// Path is the verified binary.
	Path string
	// Reported is what it printed for --version.
	Reported string

	exe string
	o   Options
}

// Stage downloads the binary of the verified release for o.Platform, checks it against the checksum
// in the signed list, and checks that it starts and reports the tag it was published under.
// Nothing that is installed changes. The file is created in StageDir, or in the directory of
// ExecPath, so that Install is one rename.
func (ver *Verified) Stage(ctx context.Context, o Options) (*Staged, error) {
	rel := ver.Release
	asset := BinaryAsset(o.Platform)
	if rel.Assets[asset] == "" {
		return nil, fmt.Errorf("release %s has no asset %s", rel.Tag, asset)
	}
	want, err := ChecksumFor(ver.Sums, asset)
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", rel.Tag, err)
	}
	o.say("signature verified (%s); downloading %s", keyName(ver.Key), asset)

	exe := o.ExecPath
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return nil, err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return nil, err
		}
	}
	dir := filepath.Dir(exe)
	if o.StageDir != "" {
		dir = o.StageDir
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(exe)+".new-*")
	if err != nil {
		return nil, fmt.Errorf("cannot write next to %s (run as root): %w", exe, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpPath) }
	h := sha256.New()
	if err := o.stream(ctx, rel.Assets[asset], io.MultiWriter(tmp, h)); err != nil {
		cleanup()
		return nil, fmt.Errorf("download %s: %w", asset, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		cleanup()
		return nil, fmt.Errorf("%s does not match its checksum (got %s, signed list says %s)", asset, got, want)
	}
	if err := tmp.Chmod(0o755); err != nil {
		cleanup()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	probe := o.Probe
	if probe == nil {
		probe = runVersion
	}
	reported, err := probe(ctx, tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("the downloaded binary does not start: %w", err)
	}
	// The signature covers the checksums, not the release tag, which comes from unsigned
	// GitHub metadata. A binary that does not name the tag it was published under is an
	// older (validly signed) release attached to a newer tag: a downgrade.
	if !ReportsVersion(reported, rel.Tag) {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("release %s ships a binary that reports %q: refusing to install (an older signed binary under a newer tag would be a downgrade)", rel.Tag, strings.TrimSpace(reported))
	}
	return &Staged{Verified: ver, Path: tmpPath, Reported: reported, exe: exe, o: o}, nil
}

// Discard removes the staged file.
func (s *Staged) Discard() { _ = os.Remove(s.Path) }

// Install keeps the old inode reachable as <name>.prev and renames the staged binary over the
// installed one.
func (s *Staged) Install() (*Result, error) {
	prev := s.exe + ".prev"
	_ = os.Remove(prev)
	if err := os.Link(s.exe, prev); err != nil {
		prev = ""
	}
	if err := os.Rename(s.Path, s.exe); err != nil {
		os.Remove(s.Path)
		return nil, err
	}
	s.o.say("installed %s at %s", s.Release.Tag, s.exe)
	return &Result{Tag: s.Release.Tag, Replaced: true, Previous: prev}, nil
}

// Studio returns the dashboard build the release ships for platform, as the signed list names it:
// the asset name, its download URL and its SHA-256. ok is false when there is none.
func (ver *Verified) Studio(platform string) (name, url, sha string, ok bool) {
	suffix := "-" + platform + ".tar.zst"
	sc := bufio.NewScanner(bytes.NewReader(ver.Sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 || len(f[0]) != 64 {
			continue
		}
		n := strings.TrimPrefix(f[1], "*")
		if strings.HasPrefix(n, "supavise-studio-") && strings.HasSuffix(n, suffix) && ver.Release.Assets[n] != "" {
			return n, ver.Release.Assets[n], strings.ToLower(f[0]), true
		}
	}
	return "", "", "", false
}

// Compare orders two release tags: negative when a is older than b, positive when it is newer. ok
// is false when either is not a release version. A suffix ("-rc1", "-4-gabcdef") is dropped.
func Compare(a, b string) (c int, ok bool) {
	x, ok1 := ParseVersion(a)
	y, ok2 := ParseVersion(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	for i := range x {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

func runVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// ReportsVersion reports whether the output of `supavise --version` ("supavise version v1.2.3")
// names tag as one of its words.
func ReportsVersion(output, tag string) bool {
	for _, f := range strings.Fields(output) {
		if f == tag {
			return true
		}
	}
	return false
}

// VerifySums checks the signature of the checksum list.
func VerifySums(key ed25519.PublicKey, sums, sig []byte) error {
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%s is %d bytes, want a %d-byte ed25519 signature", SigAsset, len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(key, sums, sig) {
		return errors.New("signature of SHA256SUMS does not verify against the release key: refusing to install")
	}
	return nil
}

// ChecksumFor returns the hex SHA-256 listed for name in a sha256sum-format list.
func ChecksumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		if strings.TrimPrefix(f[1], "*") == name {
			if len(f[0]) != 64 {
				return "", fmt.Errorf("malformed checksum for %s", name)
			}
			if _, err := hex.DecodeString(f[0]); err != nil {
				return "", fmt.Errorf("malformed checksum for %s", name)
			}
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s lists no checksum for %s", SumsAsset, name)
}

func (o *Options) fetch(ctx context.Context, u string, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	if err := o.streamLimit(ctx, u, &buf, limit); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (o *Options) stream(ctx context.Context, u string, w io.Writer) error {
	return o.streamLimit(ctx, u, w, maxBinary)
}

func (o *Options) streamLimit(ctx context.Context, u string, w io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "supavise-self-update")
	resp, err := o.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", u, resp.Status)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("%s is larger than %d bytes", u, limit)
	}
	return nil
}
