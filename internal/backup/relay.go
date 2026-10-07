package backup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/secrets"
)

// The WAL relay
//
// A project's Postgres runs user code (SQL functions, extensions such as pg_net and http,
// an agent with execute_sql). It must not hold the credentials of the backup backend, and
// it must not be able to read or overwrite another project's WAL or base backups. So its
// archive_command and restore_command do not open the backend themselves: they talk to the
// daemon over a unix socket that lives in the project's own directory
// (config.Paths.WALSocket), and the daemon, which holds the credentials, does the storage
// I/O. The systemd unit of project X bind-mounts only X's socket directory, so the mount
// namespace decides who can reach which relay (deploy/systemd/README.md).
//
// Each socket serves exactly one project. A push is always for that project; a fetch may
// name another project only when the daemon recorded that as the source of this project's
// restore (config.Paths.RestoreSources, a file outside every unit's view).
//
// The contract of archive_command is unchanged: a push answers success only after the
// compressed file is durable in the backend (the same PushWALReader that `sbctl wal push`
// uses), and fetch distinguishes "not in the archive" (404, exit 1) from "cannot read the
// archive" (anything else, exit 126). When the daemon is down the socket does not answer:
// archive_command fails and Postgres retries it, which is the correct behavior, because
// nothing here buffers WAL anywhere else.

const (
	relayPushPath  = "/v1/wal/push"
	relayFetchPath = "/v1/wal/fetch"
	relayPingPath  = "/v1/ping"

	// relayMaxWAL bounds a pushed file: a WAL segment is 16 MB by default and at most 1 GB.
	relayMaxWAL = 1 << 30
	// relayConcurrency bounds the transfers served at once across every project.
	relayConcurrency = 16
	// relayDrainTimeout bounds how long a push handler reads the unread rest of a request
	// body before it answers.
	relayDrainTimeout = 5 * time.Second
	// relayDefaultInterval is how often the daemon looks for projects whose relay it does not serve yet.
	relayDefaultInterval = 3 * time.Second
)

// RelayOptions configures a Relay.
type RelayOptions struct {
	Config *config.Config
	// Service returns the backup service that does the storage I/O (only its Store is
	// used). It is called per request until it succeeds once; a backend that cannot be
	// opened answers 503 and is tried again.
	Service func(ctx context.Context) (*Service, error)
	// Refs lists the projects to serve. Default: every directory under the state
	// directory's projects/ that holds a WAL directory (config.Paths.WALDir).
	Refs func() []string
	// Socket returns the socket path of ref. Default: config.Paths.WALSocket.
	Socket func(ref string) string
	// Sources returns the other projects whose archive ref may fetch from (the source of
	// a restore to a new project). Default: the lines of config.Paths.RestoreSources(ref).
	Sources func(ref string) []string
	// Only restricts the relay to these projects plus any that Ensure names (a CLI command
	// that needs WAL archived for the projects it works on while the daemon is down). Nil
	// serves every project Refs lists, as the daemon does.
	Only []string
	// PeerCheck decides whether the process on the other end of a connection to ref's
	// socket may use it. Default: the systemd unit of the peer (relay_peer.go).
	PeerCheck func(c net.Conn, ref string) error
	// Interval is how often Run looks for projects to serve (default 3 seconds).
	Interval time.Duration
	Log      *slog.Logger
}

// Relay serves the per-project WAL sockets.
type Relay struct {
	opt RelayOptions
	sem chan struct{}

	// A relay never replaces a socket that answers: the daemon and a CLI relay (which serves
	// while the daemon is down) can run at once, and the one that listens first keeps the
	// project until it stops.
	mu     sync.Mutex // guards ls, pinned, refMu and done; never held across I/O
	ls     map[string]*relayListener
	pinned map[string]bool
	refMu  map[string]*sync.Mutex
	done   bool

	// svcMu guards svc and is held while the backend opens (up to the factory's timeout),
	// which must not block Ensure and Reconcile.
	svcMu sync.Mutex
	svc   *Service
}

type relayListener struct {
	ln   net.Listener
	srv  *http.Server
	path string
	ino  fileID
}

// NewRelay returns a Relay. Nothing listens until Run or Ensure.
func NewRelay(o RelayOptions) *Relay {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Interval <= 0 {
		o.Interval = relayDefaultInterval
	}
	if o.PeerCheck == nil {
		o.PeerCheck = checkRelayPeer
	}
	if o.Socket == nil && o.Config != nil {
		o.Socket = o.Config.Paths().WALSocket
	}
	if o.Refs == nil && o.Config != nil {
		o.Refs = func() []string { return projectsWithWALDir(o.Config) }
	}
	if o.Sources == nil && o.Config != nil {
		o.Sources = func(ref string) []string { return readRestoreSources(o.Config, ref) }
	}
	return &Relay{opt: o, sem: make(chan struct{}, relayConcurrency), ls: map[string]*relayListener{}, pinned: map[string]bool{}, refMu: map[string]*sync.Mutex{}}
}

// projectsWithWALDir lists the refs that have a WAL directory under projects/.
func projectsWithWALDir(c *config.Config) []string {
	ents, err := os.ReadDir(filepath.Join(c.Paths().Root, "projects"))
	if err != nil {
		return nil
	}
	var refs []string
	for _, e := range ents {
		name := e.Name()
		if !e.IsDir() || (name != config.SystemRef && !secrets.ValidRef(name)) {
			continue
		}
		if fi, err := os.Stat(c.Paths().WALDir(name)); err == nil && fi.IsDir() {
			refs = append(refs, name)
		}
	}
	return refs
}

// readRestoreSources reads config.Paths.RestoreSources(ref): one project ref per line.
func readRestoreSources(c *config.Config, ref string) []string {
	b, err := os.ReadFile(c.Paths().RestoreSources(ref))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && validRef(l) == nil {
			out = append(out, l)
		}
	}
	return out
}

// Run serves until ctx ends: it listens on the socket of every project that has a WAL
// directory, looks for new ones every Interval, and closes everything when ctx is done.
func (r *Relay) Run(ctx context.Context) error {
	r.Reconcile()
	t := time.NewTicker(r.opt.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.Close()
			return nil
		case <-t.C:
			r.Reconcile()
		}
	}
}

// Reconcile starts a listener for every project that needs one and stops those of
// projects whose directory is gone.
func (r *Relay) Reconcile() {
	want := map[string]bool{}
	for _, ref := range r.opt.Refs() {
		if r.opt.Only != nil && !slices.Contains(r.opt.Only, ref) && !r.isPinned(ref) {
			continue
		}
		want[ref] = true
		if err := r.Ensure(ref); err != nil {
			r.opt.Log.Warn("wal relay: cannot serve project", "ref", ref, "error", err)
		}
	}
	r.mu.Lock()
	var gone []*relayListener
	for ref, l := range r.ls {
		if !want[ref] {
			gone = append(gone, l)
			delete(r.ls, ref)
			delete(r.pinned, ref)
		}
	}
	r.mu.Unlock()
	for _, l := range gone {
		l.shutdown()
	}
}

func (r *Relay) isPinned(ref string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pinned[ref]
}

// Ensure makes ref's socket answer now (the lifecycle engine calls it before a cluster
// starts, so the first archive_command does not wait for the next Reconcile). It does
// nothing when the project's WAL directory does not exist, and leaves a socket alone that
// another process already serves.
func (r *Relay) Ensure(ref string) error {
	if err := validRef(ref); err != nil {
		return err
	}
	path := r.opt.Socket(ref)
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || !fi.IsDir() {
		return nil
	}
	// One Ensure per project at a time; the relay-wide lock is held only to read and
	// change the maps, never across the ping, a shutdown or a listen.
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return errors.New("relay closed")
	}
	rl := r.refMu[ref]
	if rl == nil {
		rl = &sync.Mutex{}
		r.refMu[ref] = rl
	}
	r.mu.Unlock()
	rl.Lock()
	defer rl.Unlock()

	r.mu.Lock()
	l := r.ls[ref]
	r.mu.Unlock()
	if l != nil {
		if id, ok := socketID(path); ok && id == l.ino {
			return nil
		}
		// The socket file was removed or replaced (the directory was recreated): serve again.
		r.mu.Lock()
		if r.ls[ref] == l {
			delete(r.ls, ref)
		}
		r.mu.Unlock()
		l.shutdown()
	}
	if RelayPing(context.Background(), path) == nil {
		return nil // another process serves this project
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	// The relay unlinks its socket itself, and only while the path is still its own
	// (relayListener.shutdown): the runtime's unlink on Close would remove another
	// process's socket that replaced ours.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		os.Remove(path)
		return err
	}
	id, _ := socketID(path)
	srv := &http.Server{
		Handler:           r.handler(ref),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(r.opt.Log.Handler(), slog.LevelDebug),
	}
	nl := &relayListener{ln: ln, srv: srv, path: path, ino: id}
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		nl.shutdown()
		return errors.New("relay closed")
	}
	r.ls[ref] = nl
	if r.opt.Only != nil {
		r.pinned[ref] = true
	}
	r.mu.Unlock()
	go func() {
		pl := peerListener{Listener: ln,
			check: func(c net.Conn) error { return r.opt.PeerCheck(c, ref) },
			deny:  func(err error) { r.opt.Log.Warn("wal relay: connection refused", "ref", ref, "error", err) }}
		if err := srv.Serve(pl); err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.opt.Log.Warn("wal relay: listener stopped", "ref", ref, "error", err)
		}
	}()
	r.opt.Log.Debug("wal relay: serving", "ref", ref, "socket", path)
	return nil
}

// Served lists the projects whose socket this relay serves, sorted.
func (r *Relay) Served() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.ls))
	for ref := range r.ls {
		out = append(out, ref)
	}
	slices.Sort(out)
	return out
}

// Close stops every listener; transfers in flight get 30 seconds.
func (r *Relay) Close() {
	r.mu.Lock()
	r.done = true
	ls := r.ls
	r.ls = map[string]*relayListener{}
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, l := range ls {
		wg.Add(1)
		go func() { defer wg.Done(); l.shutdown() }()
	}
	wg.Wait()
}

func (l *relayListener) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := l.srv.Shutdown(ctx); err != nil {
		l.srv.Close()
	}
	// The listener does not unlink on close (SetUnlinkOnClose(false)): remove the socket
	// only if the path still names our inode; a replacement is left alone.
	if id, ok := socketID(l.path); ok && id == l.ino {
		os.Remove(l.path)
	}
}

func (r *Relay) service(ctx context.Context) (*Service, error) {
	r.svcMu.Lock()
	defer r.svcMu.Unlock()
	if r.svc != nil {
		return r.svc, nil
	}
	if r.opt.Service == nil {
		return nil, errors.New("no backup service configured")
	}
	svc, err := r.opt.Service(ctx)
	if err != nil {
		return nil, err
	}
	r.svc = svc
	return svc, nil
}

// handler serves one project's socket.
func (r *Relay) handler(own string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+relayPingPath, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST "+relayPushPath, func(w http.ResponseWriter, req *http.Request) { r.push(own, w, req) })
	mux.HandleFunc("GET "+relayFetchPath, func(w http.ResponseWriter, req *http.Request) { r.fetch(own, w, req) })
	return mux
}

func relayError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	io.WriteString(w, msg+"\n")
}

func (r *Relay) acquire(ctx context.Context) bool {
	select {
	case r.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *Relay) push(own string, w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	name := q.Get("name")
	if ref := q.Get("ref"); ref != "" && ref != own {
		relayError(w, http.StatusForbidden, "this socket archives "+own+" only")
		return
	}
	if err := checkWALName(name); err != nil {
		relayError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ContentLength < 0 {
		relayError(w, http.StatusLengthRequired, "a WAL push needs a Content-Length")
		return
	}
	if req.ContentLength > relayMaxWAL {
		relayError(w, http.StatusRequestEntityTooLarge, "WAL file too large")
		return
	}
	svc, err := r.service(req.Context())
	if err != nil {
		r.opt.Log.Warn("wal relay: backup backend unavailable", "ref", own, "error", err)
		relayError(w, http.StatusServiceUnavailable, "backup backend unavailable: "+err.Error())
		return
	}
	if !r.acquire(req.Context()) {
		return
	}
	defer func() { <-r.sem }()
	// The body is read to its declared length or the push fails: a client that dies
	// halfway cannot leave a short segment in the archive.
	rc := http.NewResponseController(w)
	body := &pushBody{r: http.MaxBytesReader(w, req.Body, relayMaxWAL), rc: rc}
	err = svc.PushWALReader(req.Context(), own, name, body)
	// PushWALReader returns only after nothing reads body any more, so this drain cannot
	// race with it. Read what is left (a comparison with the archived file stops at the
	// first difference): a client that is still writing when the answer comes would see a
	// broken pipe, not the answer. A client that stalls gets a few seconds, not the handler.
	_ = rc.SetReadDeadline(time.Now().Add(relayDrainTimeout))
	_, _ = io.Copy(io.Discard, body)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrWALConflict):
		r.opt.Log.Error("wal relay: archive already holds a different file under this name", "ref", own, "file", name)
		relayError(w, http.StatusConflict, err.Error())
	default:
		r.opt.Log.Warn("wal relay: push failed", "ref", own, "file", name, "error", err)
		relayError(w, http.StatusInternalServerError, err.Error())
	}
}

// pushBody is the request body of a push. Interrupt makes a Read that is blocked on a
// slow or stalled client return, so PushWALReader can stop reading before it returns.
type pushBody struct {
	r  io.Reader
	rc *http.ResponseController
}

func (b *pushBody) Read(p []byte) (int, error) { return b.r.Read(p) }

// Interrupt fails every pending and later read of the connection until the deadline is set again.
func (b *pushBody) Interrupt() { _ = b.rc.SetReadDeadline(time.Now()) }

func (r *Relay) fetch(own string, w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	ref, name := q.Get("ref"), q.Get("name")
	if ref == "" {
		ref = own
	}
	if err := validRef(ref); err != nil {
		relayError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ref != own && !slices.Contains(r.opt.Sources(own), ref) {
		relayError(w, http.StatusForbidden, own+" may not read the archive of "+ref)
		return
	}
	if err := checkWALName(name); err != nil {
		relayError(w, http.StatusBadRequest, err.Error())
		return
	}
	svc, err := r.service(req.Context())
	if err != nil {
		r.opt.Log.Warn("wal relay: backup backend unavailable", "ref", own, "error", err)
		relayError(w, http.StatusServiceUnavailable, "backup backend unavailable: "+err.Error())
		return
	}
	if !r.acquire(req.Context()) {
		return
	}
	defer func() { <-r.sem }()
	rc, err := svc.OpenWAL(req.Context(), ref, name)
	if errors.Is(err, ErrNoWAL) {
		relayError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		r.opt.Log.Warn("wal relay: fetch failed", "ref", own, "source", ref, "file", name, "error", err)
		relayError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rc.Close()
	// Read the first block before the status line, so an unreadable object is a 500 and
	// not a 200 that breaks halfway.
	br := bufio.NewReaderSize(rc, 1<<20)
	if _, err := br.Peek(1); err != nil && !errors.Is(err, io.EOF) {
		r.opt.Log.Warn("wal relay: fetch failed", "ref", own, "source", ref, "file", name, "error", err)
		relayError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := io.Copy(w, br); err != nil {
		r.opt.Log.Warn("wal relay: fetch broke off", "ref", own, "source", ref, "file", name, "error", err)
		// Abort the connection: the client sees a truncated chunked body and fails.
		panic(http.ErrAbortHandler)
	}
}

// ---- client ------------------------------------------------------------------

// ErrRelayDown means nothing answered on the relay socket (the daemon is not running, or
// the project's relay is not set up). archive_command fails and Postgres retries it.
var ErrRelayDown = errors.New("backup: the WAL relay does not answer (is sbctl.service running?)")

func relayClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}}
}

func relayDo(ctx context.Context, socket string, req *http.Request) (*http.Response, error) {
	resp, err := relayClient(socket).Do(req.WithContext(ctx))
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return nil, fmt.Errorf("%w: %v", ErrRelayDown, err)
		}
		return nil, err
	}
	return resp, nil
}

func relayFailure(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(b))
	switch resp.StatusCode {
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrWALConflict, msg)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNoWAL, msg)
	}
	return fmt.Errorf("backup: WAL relay answered %s: %s", resp.Status, msg)
}

// RelayPing returns nil when a relay answers on socket.
func RelayPing(ctx context.Context, socket string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequest(http.MethodGet, "http://relay"+relayPingPath, nil)
	resp, err := relayDo(ctx, socket, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return relayFailure(resp)
	}
	return nil
}

// RelayPush sends the WAL file at path to the relay on socket and returns nil only when
// the daemon reports it durable in the backend. ref is the project the caller believes it
// archives for; the relay refuses a socket that belongs to another one.
func RelayPush(ctx context.Context, socket, ref, path string) error {
	name := filepath.Base(path)
	if err := checkWALName(name); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	u := relayPushPath + "?name=" + name + "&ref=" + ref
	req, err := http.NewRequest(http.MethodPost, "http://relay"+u, f)
	if err != nil {
		return err
	}
	req.ContentLength = fi.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := relayDo(ctx, socket, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return relayFailure(resp)
	}
	return nil
}

// RelayFetch reads the archived WAL file name of ref through the relay on socket and
// writes it to dest (through a temporary file and a rename). A file the archive does not
// hold yields an error wrapping ErrNoWAL.
func RelayFetch(ctx context.Context, socket, ref, name, dest string) error {
	if err := checkWALName(name); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, "http://relay"+relayFetchPath+"?name="+name+"&ref="+ref, nil)
	if err != nil {
		return err
	}
	resp, err := relayDo(ctx, socket, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return relayFailure(resp)
	}
	return writeFileAtomic(ctx, dest, resp.Body, name)
}
