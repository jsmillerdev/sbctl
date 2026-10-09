package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/fsutil"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
)

// Certificates on a cluster (design 2.8.4). The leader obtains and renews every certificate, as a
// server on its own does. A follower mirrors the leader's CertMagic store and serves what it mirrored:
// it asks the CA for nothing, so two nodes never issue for the same name, and a custom hostname whose DNS
// still points at the old server does not fail HTTP-01 on the new one. A follower that becomes the
// leader loads the mirrored tree as its own and carries on, with the ACME account the tree holds.

// CertSync makes a proxy mirror the leader's certificates while its node follows and issue them
// while it leads.
type CertSync struct {
	// Role says which of the two the node does now. The proxy follows it.
	Role *CertRole
	// Source fetches the leader's CertMagic store while the node follows.
	Source CertSource
	// Interval is the time between fetches; zero means a minute. A fetch also happens at once
	// when a handshake asks for a name no mirrored certificate covers, at most every ten seconds.
	Interval time.Duration
}

// CertSource fetches the CertMagic store of the leader.
type CertSource interface {
	// Certs returns the store. etag is the tag of the store the caller applied last, "" for none; the
	// source may answer ErrCertsNotModified instead of sending the same store again.
	Certs(ctx context.Context, etag string) (peerapi.CertSnapshot, error)
}

// ErrCertsNotModified is what a CertSource returns when the leader's store is the one etag names.
var ErrCertsNotModified = errors.New("proxy: the leader's certificates have not changed")

// MeshCerts is the CertSource that asks the leader over the peer API (GET /peer/v1/certs).
type MeshCerts struct {
	RPC mesh.RPC
	// Leader is the id of the node to ask; ok is false while no leader is known.
	Leader func() (node string, ok bool)
}

// Certs implements CertSource. mesh.RPC has no request headers, so the tag travels as the etag query
// parameter, which the handler reads like If-None-Match; a transport that drops it just gets the whole
// store each time.
func (m MeshCerts) Certs(ctx context.Context, etag string) (peerapi.CertSnapshot, error) {
	var snap peerapi.CertSnapshot
	node, ok := m.Leader()
	if !ok {
		return snap, errors.New("proxy: no leader is known to fetch the certificates from")
	}
	p := peerapi.PathCerts
	if etag != "" {
		p += "?etag=" + url.QueryEscape(etag)
	}
	err := m.RPC.Call(ctx, node, http.MethodGet, p, nil, &snap)
	var re *mesh.RemoteError
	if errors.As(err, &re) && re.Status == http.StatusNotModified {
		return snap, ErrCertsNotModified
	}
	return snap, err
}

// CertRole says whether the node issues certificates (managing) or mirrors the leader's (synced).
// A node that leads manages; one that follows, or is fenced, is synced. wire_proxy.go sets it from the
// cluster membership; whatever reconciles the node's role at a promotion or demotion may call
// Promote and Demote too, and the proxy switches at once.
type CertRole struct {
	mu       sync.Mutex
	managing bool
	watchers map[chan bool]struct{}
}

// NewCertRole returns a role that starts as managing or synced.
func NewCertRole(managing bool) *CertRole {
	return &CertRole{managing: managing, watchers: map[chan bool]struct{}{}}
}

// Managing reports whether the node issues the certificates.
func (r *CertRole) Managing() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.managing
}

// Promote switches to managing: the node is the leader now.
func (r *CertRole) Promote() { r.set(true) }

// Demote switches to synced: the node follows, or is fenced.
func (r *CertRole) Demote() { r.set(false) }

func (r *CertRole) set(managing bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.managing == managing {
		return
	}
	r.managing = managing
	for ch := range r.watchers {
		select {
		case <-ch: // a watcher that has not read the last change sees only the latest
		default:
		}
		ch <- managing
	}
}

// Watch delivers the role now and after every change, coalescing changes a slow reader missed. The
// channel closes when ctx ends.
func (r *CertRole) Watch(ctx context.Context) <-chan bool {
	ch := make(chan bool, 1)
	r.mu.Lock()
	ch <- r.managing
	r.watchers[ch] = struct{}{}
	r.mu.Unlock()
	go func() {
		<-ctx.Done()
		r.mu.Lock()
		delete(r.watchers, ch)
		close(ch)
		r.mu.Unlock()
	}()
	return ch
}

// The store is mirrored below these two directories of CertMagic's root: certificates and the ACME
// accounts. Locks, OCSP staples and pending challenge tokens belong to the node that makes them.
var mirrorRoots = []string{"certificates", "acme"}

const challengeTokensDir = "challenge_tokens"

// snapshotTag is the ETag of a store: a hash of every file's path and content, in path order.
func snapshotTag(snap peerapi.CertSnapshot) string {
	files := append([]peerapi.CertFile(nil), snap.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%d:%s:%d:", len(f.Path), f.Path, len(f.Data))
		h.Write(f.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// etagMatches reports whether an If-None-Match header (or an etag parameter) names tag.
func etagMatches(header, tag string) bool {
	for _, v := range strings.Split(header, ",") {
		v = strings.Trim(strings.TrimPrefix(strings.TrimSpace(v), "W/"), `"`)
		if v == "*" || (v != "" && v == tag) {
			return true
		}
	}
	return false
}

// CertsHandler serves GET /peer/v1/certs on the leader: the CertMagic store under dir as a
// CertSnapshot with an ETag, 304 when the caller's If-None-Match (or etag parameter) names it. Only
// a node that authenticated with its certificate may ask, and only the leader answers: its store is the
// one that counts. wire_proxy.go registers it with mesh.Handle.
func CertsHandler(dir string, leader func() bool, log *slog.Logger) mesh.HandlerFunc {
	if log == nil {
		log = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if p, ok := mesh.PeerFrom(r.Context()); !ok || p.Node == "" {
			mesh.RespondError(w, http.StatusForbidden, "forbidden", "the certificate store is for the nodes of the cluster")
			return
		}
		if !leader() {
			mesh.RespondError(w, http.StatusConflict, "not_leader", "this node is not the leader")
			return
		}
		files, err := readCertStore(dir, log)
		if err != nil {
			log.Error("proxy: reading the certificate store for a peer", "err", err)
			mesh.RespondError(w, http.StatusInternalServerError, "", "the certificate store cannot be read")
			return
		}
		snap := peerapi.CertSnapshot{Files: files}
		tag := snapshotTag(snap)
		w.Header().Set("ETag", `"`+tag+`"`)
		if etagMatches(r.Header.Get("If-None-Match"), tag) || etagMatches(r.URL.Query().Get("etag"), tag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snap)
	}
}

// readCertStore lists the mirrored files under dir. A site (certificates/<issuer>/<site>/) whose
// certificate, key and metadata are not all there and do not match each other is left out: CertMagic
// writes the three one after the other, and a follower must never see a half-written renewal.
func readCertStore(dir string, log *slog.Logger) ([]peerapi.CertFile, error) {
	var files []peerapi.CertFile
	for _, top := range mirrorRoots {
		root := filepath.Join(dir, top)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil && p == root && errors.Is(err, fs.ErrNotExist):
				return nil
			case err != nil && errors.Is(err, fs.ErrNotExist):
				return nil // removed while walking
			case err != nil:
				return err
			case d.IsDir() && d.Name() == challengeTokensDir:
				return fs.SkipDir
			case !d.Type().IsRegular():
				return nil
			}
			data, err := os.ReadFile(p)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			files = append(files, peerapi.CertFile{Path: filepath.ToSlash(rel), Mode: uint32(info.Mode().Perm()), Data: data})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	files = completeSites(files, log)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// site is one certificate of the store: certificates/<issuer>/<site>/<site>.{crt,key,json}.
type site struct {
	dir, name      string
	crt, key, meta []byte
}

// siteDir splits a store path into the site's directory and the file's extension; ok is false for a
// file that is not part of a site.
func siteDir(p string) (dir, ext string, ok bool) {
	parts := strings.Split(p, "/")
	if len(parts) != 4 || parts[0] != "certificates" {
		return "", "", false
	}
	return path.Dir(p), path.Ext(parts[3]), true
}

// siteName is the name a site directory holds a certificate for: CertMagic spells the wildcard
// "wildcard_" in file names.
func siteName(dir string) string {
	n := path.Base(dir)
	if rest, ok := strings.CutPrefix(n, "wildcard_"); ok {
		return "*" + rest
	}
	return n
}

// sitesOf collects the complete sites of files: those with a certificate, a key that matches it and
// metadata. The rest of the files (the ACME account) have no site.
func sitesOf(files []peerapi.CertFile) (sites []site, rest []peerapi.CertFile) {
	byDir := map[string]*site{}
	var order []string
	for _, f := range files {
		dir, ext, ok := siteDir(f.Path)
		if !ok {
			rest = append(rest, f)
			continue
		}
		st := byDir[dir]
		if st == nil {
			st = &site{dir: dir, name: siteName(dir)}
			byDir[dir] = st
			order = append(order, dir)
		}
		switch ext {
		case ".crt":
			st.crt = f.Data
		case ".key":
			st.key = f.Data
		case ".json":
			st.meta = f.Data
		}
	}
	for _, dir := range order {
		st := byDir[dir]
		if len(st.crt) == 0 || len(st.key) == 0 || len(st.meta) == 0 {
			continue
		}
		if _, err := tls.X509KeyPair(st.crt, st.key); err != nil {
			continue
		}
		sites = append(sites, *st)
	}
	return sites, rest
}

// completeSites drops the files of sites that are incomplete or torn.
func completeSites(files []peerapi.CertFile, log *slog.Logger) []peerapi.CertFile {
	sites, rest := sitesOf(files)
	whole := map[string]bool{}
	for _, st := range sites {
		whole[st.dir] = true
	}
	out := rest
	for _, f := range files {
		dir, _, ok := siteDir(f.Path)
		switch {
		case !ok:
		case whole[dir]:
			out = append(out, f)
		default:
			log.Debug("proxy: leaving a certificate that is being written out of the snapshot", "dir", dir)
		}
	}
	return out
}

// localPath turns a path of a snapshot into a file below dir, refusing anything that is not in the
// mirrored trees: the snapshot comes from a peer, but a path is still not trusted.
func localPath(dir, p string) (string, error) {
	if p == "" || strings.ContainsAny(p, "\\\x00") || path.IsAbs(p) || path.Clean(p) != p {
		return "", fmt.Errorf("proxy: certificate snapshot: bad path %q", p)
	}
	parts := strings.Split(p, "/")
	if len(parts) < 2 || (parts[0] != mirrorRoots[0] && parts[0] != mirrorRoots[1]) {
		return "", fmt.Errorf("proxy: certificate snapshot: %q is outside the certificate store", p)
	}
	for _, part := range parts {
		if part == ".." || part == challengeTokensDir {
			return "", fmt.Errorf("proxy: certificate snapshot: bad path %q", p)
		}
	}
	return filepath.Join(dir, filepath.FromSlash(p)), nil
}

// applySnapshot makes the mirrored trees under dir hold exactly the files of snap: it writes the
// ones that differ (each atomically, mode 0600 whatever the leader's mode) and removes the ones the
// leader no longer has. A snapshot without files changes nothing: an empty store is more likely a
// leader that has not issued yet than a leader that dropped everything.
func applySnapshot(dir string, snap peerapi.CertSnapshot) error {
	if len(snap.Files) == 0 {
		return nil
	}
	want := make(map[string]bool, len(snap.Files))
	targets := make([]string, len(snap.Files))
	for i, f := range snap.Files {
		p, err := localPath(dir, f.Path)
		if err != nil {
			return err
		}
		targets[i] = p
		want[p] = true
	}
	// Keys before certificates before metadata, so that CertMagic reading while this runs finds a
	// key for the certificate it sees.
	order := make([]int, len(snap.Files))
	for i := range order {
		order[i] = i
	}
	rank := func(p string) int {
		switch path.Ext(p) {
		case ".key":
			return 0
		case ".crt":
			return 1
		}
		return 2
	}
	sort.SliceStable(order, func(a, b int) bool { return rank(snap.Files[order[a]].Path) < rank(snap.Files[order[b]].Path) })
	for _, i := range order {
		if err := writeIfChanged(targets[i], snap.Files[i].Data); err != nil {
			return err
		}
	}
	for _, top := range mirrorRoots {
		root := filepath.Join(dir, top)
		var dirs []string
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil && errors.Is(err, fs.ErrNotExist):
				return nil
			case err != nil:
				return err
			case d.IsDir() && d.Name() == challengeTokensDir:
				return fs.SkipDir
			case d.IsDir():
				if p != root {
					dirs = append(dirs, p)
				}
			case !want[p]:
				if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		for i := len(dirs) - 1; i >= 0; i-- {
			_ = os.Remove(dirs[i]) // only an empty directory goes
		}
	}
	return nil
}

// writeIfChanged writes data to p unless p already holds it.
func writeIfChanged(p string, data []byte) error {
	if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, data) {
		return nil
	}
	return fsutil.WriteFile(p, data, 0o600, fsutil.Options{MkdirMode: 0o700})
}

// certMirror copies the leader's store into the local one, once a minute and when asked.
type certMirror struct {
	dir      string
	src      CertSource
	interval time.Duration
	log      *slog.Logger
	// onSnapshot is told of the store after every fetch that reached the leader and brought files,
	// changed or not, and once at the start of run of what the node already holds on disk.
	onSnapshot func(ctx context.Context, snap peerapi.CertSnapshot)
	poke       chan struct{}
	// pokeGap is the least time between two fetches that pokes ask for.
	pokeGap time.Duration

	mu sync.Mutex // one fetch at a time
	// tag and fetched are the leader's store as last sent; shown is what the disk holds, which is
	// fetched plus the files carried over (see carryOver).
	tag     string
	fetched peerapi.CertSnapshot
	shown   peerapi.CertSnapshot
	carried map[string]bool
	have    bool
}

// pokeEvery is the least time between two fetches that a handshake for an unknown name asks for.
const pokeEvery = 10 * time.Second

func newCertMirror(dir string, cs *CertSync, log *slog.Logger) *certMirror {
	m := &certMirror{dir: dir, src: cs.Source, interval: cs.Interval, log: log, poke: make(chan struct{}, 1), pokeGap: pokeEvery}
	if m.interval <= 0 {
		m.interval = time.Minute
	}
	return m
}

// wake asks for a fetch soon. It never blocks.
func (m *certMirror) wake() {
	select {
	case m.poke <- struct{}{}:
	default:
	}
}

// reset forgets what was fetched and applied, so that the next fetch compares every file with the
// disk. A node does it before it mirrors again, because it may have written its own files while it led.
func (m *certMirror) reset() {
	m.mu.Lock()
	m.tag, m.fetched, m.shown, m.carried, m.have = "", peerapi.CertSnapshot{}, peerapi.CertSnapshot{}, nil, false
	m.mu.Unlock()
}

// run fetches until ctx ends: first the certificates the node already has on disk are told to the cache
// (a restart, or a demotion, must not leave the node without a certificate while the leader answers),
// then the leader's store at once and every interval; a failed fetch is retried after a growing pause
// that never exceeds the interval. A poke (wake) brings the next fetch forward to pokeGap after the
// last one, and never moves it back: a client that keeps asking for a name nobody mirrors must not keep the
// scheduled fetch from happening.
func (m *certMirror) run(ctx context.Context) {
	m.reset()
	m.loadStore(ctx)
	var last time.Time
	var wait time.Duration
	next := time.Now()
	for {
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.poke:
			timer.Stop()
			if at := last.Add(m.pokeGap); at.Before(next) {
				next = at
			}
			continue
		case <-timer.C:
		}
		last = time.Now()
		if err := m.sync(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.Warn("proxy: could not mirror the leader's certificates", "err", err)
			wait = min(max(2*wait, 2*time.Second), m.interval)
		} else {
			wait = m.interval
		}
		next = time.Now().Add(wait)
	}
}

// loadStore tells onSnapshot of the certificates the node's own store holds, and remembers them as
// what is shown, so that the first fetch keeps a site it lacks for one round (carryOver) as it would
// after any other. A node that restarts while the leader cannot be reached serves them until it can.
func (m *certMirror) loadStore(ctx context.Context) {
	files, err := readCertStore(m.dir, m.log)
	if err != nil {
		m.log.Warn("proxy: could not read the certificates the node already has", "err", err)
		return
	}
	if len(files) == 0 {
		return
	}
	snap := peerapi.CertSnapshot{Files: files}
	m.mu.Lock()
	m.shown = snap
	m.mu.Unlock()
	if m.onSnapshot != nil {
		m.onSnapshot(ctx, snap)
	}
}

func (m *certMirror) sync(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	snap, err := m.src.Certs(ctx, m.tag)
	switch {
	case errors.Is(err, ErrCertsNotModified):
		if !m.have {
			return errors.New("the leader answered not modified to a request without a tag")
		}
		snap = m.fetched
	case err != nil:
		return err
	default:
		m.fetched, m.tag = snap, snapshotTag(snap)
	}
	if len(snap.Files) == 0 {
		// A leader that has issued nothing yet: applySnapshot would change nothing on disk, so the
		// cache keeps what it has as well.
		m.have = true
		return nil
	}
	shown := m.carryOver(snap)
	if err := applySnapshot(m.dir, shown); err != nil {
		return err
	}
	m.shown, m.have = shown, true
	if m.onSnapshot != nil {
		m.onSnapshot(ctx, shown)
	}
	return nil
}

// carryOver is snap plus the files the node showed last time that snap lacks, for one round. The
// leader reads its store while CertMagic writes it, and a certificate it finds half written is left
// out of a snapshot (readCertStore); one absence must not delete the follower's copy. A file that is
// missing twice in a row is gone.
func (m *certMirror) carryOver(snap peerapi.CertSnapshot) peerapi.CertSnapshot {
	present := make(map[string]bool, len(snap.Files))
	for _, f := range snap.Files {
		present[f.Path] = true
	}
	out := peerapi.CertSnapshot{Files: append([]peerapi.CertFile(nil), snap.Files...)}
	carried := map[string]bool{}
	for _, f := range m.shown.Files {
		if present[f.Path] || m.carried[f.Path] {
			continue
		}
		out.Files = append(out.Files, f)
		carried[f.Path] = true
	}
	m.carried = carried
	return out
}
