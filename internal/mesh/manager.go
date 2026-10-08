package mesh

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// AddrResolver supplies addresses to try for a node when the peer_addr in its registry row is
// missing or stale. The cluster's resolver asks EC2 for the instance's current address.
type AddrResolver interface {
	// Addrs returns host:port values for n, best first.
	Addrs(ctx context.Context, n registry.Node) ([]string, error)
}

// Options configure a Manager.
type Options struct {
	Topology Topology
	// Creds returns the node's current credentials (a renewal replaces them); nil while the node
	// has none, in which case it can neither accept nor open a session.
	Creds    func() *Credentials
	Resolver AddrResolver
	// Mux serves the peer API; nil means DefaultMux.
	Mux *Mux
	// Authz decides forward streams; nil refuses them all.
	Authz *Authorizer
	Log   *slog.Logger

	// OnPing is told every answer to a ping this node sent, with the round trip.
	OnPing func(node string, p peerapi.Ping, rtt time.Duration)

	// The rest are for tests; the zero values are the production ones.
	TCPDial         func(ctx context.Context, addr string) (net.Conn, error)
	Now             func() time.Time
	PingEvery       time.Duration // 5 s
	Tick            time.Duration // 2 s: how often Run looks at the nodes
	DialDelay       time.Duration // 3 s: how long the higher node id waits for the lower to dial
	RevokeGrace     time.Duration // 10 s: how long a session outlives its node's admission
	AnonReadTimeout time.Duration // 10 s: how long a caller with no certificate has to send its request
	AnonLife        time.Duration // 2 min: how long its session may be open before it is closed once idle
	AnonFirstStream time.Duration // 10 s: how long its session has to open its first stream
	OneShotLife     time.Duration // 2 min: how long a node's short session (OneShot) may stay open
	MaxAnonSessions int           // 32: sessions of callers with no certificate open at once
	MaxAnonPerAddr  int           // 4: of which one address may hold this many
	MaxShakesPerIP  int           // 16: TLS handshakes in flight from one address, any caller
}

// What a caller with no certificate (a joiner) may cost. Its request is a few KiB, so the body is
// bounded far below maxRPCBody; its session is short, small (anonMuxConfig) and few (in all, and from one
// address), it has to open a stream soon after the handshake, and it may have only so many streams open.
// An open session is closed at anonSessionMax whatever it is doing.
const (
	maxAnonBody    = 64 << 10
	maxAnonStreams = 16
	anonSessionMax = 15 * time.Minute
)

func (o *Options) fill() {
	if o.Mux == nil {
		o.Mux = DefaultMux
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.TCPDial == nil {
		o.TCPDial = func(ctx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	for p, d := range map[*time.Duration]time.Duration{&o.PingEvery: 5 * time.Second, &o.Tick: 2 * time.Second,
		&o.DialDelay: 3 * time.Second, &o.RevokeGrace: 10 * time.Second, &o.AnonReadTimeout: 10 * time.Second,
		&o.AnonLife: 2 * time.Minute, &o.AnonFirstStream: 10 * time.Second, &o.OneShotLife: 2 * time.Minute} {
		if *p <= 0 {
			*p = d
		}
	}
	for p, d := range map[*int]int{&o.MaxAnonSessions: 32, &o.MaxAnonPerAddr: 4, &o.MaxShakesPerIP: 16} {
		if *p <= 0 {
			*p = d
		}
	}
}

// Manager keeps one multiplexed session per peer node, opens and accepts streams on it, serves
// the peer API on rpc streams and forwards on forward streams. It implements Mesh.
type Manager struct {
	o     Options
	admit AdmitFunc
	srv   *http.Server
	rpcCh chan net.Conn

	rpcOnce sync.Once
	anon    atomic.Int32 // sessions of callers with no certificate that are open
	anonIP  perAddr      // of which each address holds so many
	oneShot perAddr      // sessions of a node's command-line tools (OneShot), by node
	shaking perAddr      // TLS handshakes in flight, by address

	mu       sync.Mutex
	sessions map[string]*peerConn
	dials    map[string]*dialState
	ctx      context.Context // set by Run; streams and pings end with it
}

var _ Mesh = (*Manager)(nil)

// peerConn is one session to one node.
type peerConn struct {
	node    string
	sess    Session
	dialed  bool // this node opened the TCP connection
	since   time.Time
	rtt     atomic.Int64 // nanoseconds of the last ping; 0 before the first
	lastPng atomic.Pointer[pingState]
	// revokedSince is when the registry stopped admitting the node (0 while it does).
	revokedSince atomic.Int64
	retiring     atomic.Bool
}

type pingState struct {
	At   time.Time
	Ping peerapi.Ping
}

// dialState is the single-flight and the back-off of connecting to one node.
type dialState struct {
	flight   chan struct{} // non-nil while a connect is running
	failedAt time.Time
	failErr  error
	nextTry  time.Time
	backoff  time.Duration
	downFrom time.Time // when Run first saw the node without a session
}

// New builds a Manager. Run starts its upkeep and Serve its listener.
func New(o Options) *Manager {
	o.fill()
	m := &Manager{o: o, rpcCh: make(chan net.Conn, 64), sessions: map[string]*peerConn{}, dials: map[string]*dialState{}, ctx: context.Background()}
	m.admit = AdmitFromTopology(o.Topology)
	m.srv = &http.Server{
		Handler:           http.HandlerFunc(m.serveRPC),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
		ErrorLog:          slog.NewLogLogger(o.Log.Handler(), slog.LevelDebug),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if rc, ok := c.(*rpcConn); ok {
				return WithPeer(ctx, rc.peer)
			}
			return ctx
		},
	}
	return m
}

func (m *Manager) self() string { return m.o.Topology.Self().ID }

func (m *Manager) credentials() *Credentials {
	if m.o.Creds == nil {
		return nil
	}
	return m.o.Creds()
}

// Run serves the peer API and keeps a session to every active node until ctx ends: the lower node
// id of a pair dials at once, the higher one after DialDelay, and whichever side can connect
// establishes the session that both use. Sessions to nodes the registry no longer admits are
// closed after RevokeGrace. It closes every session when ctx ends.
func (m *Manager) Run(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.serveRPCStreams(ctx)
	tick := time.NewTicker(m.o.Tick)
	defer tick.Stop()
	m.upkeep(ctx)
	for {
		select {
		case <-ctx.Done():
			m.Close()
			return nil
		case <-tick.C:
			m.upkeep(ctx)
		}
	}
}

// serveRPCStreams starts the HTTP server that answers the rpc streams, once.
func (m *Manager) serveRPCStreams(ctx context.Context) {
	m.rpcOnce.Do(func() {
		go func() { _ = m.srv.Serve(&chanListener{ch: m.rpcCh, done: ctx.Done()}) }()
	})
}

// Close closes every session and the peer API server.
func (m *Manager) Close() {
	_ = m.srv.Close()
	m.mu.Lock()
	all := make([]*peerConn, 0, len(m.sessions))
	for _, pc := range m.sessions {
		all = append(all, pc)
	}
	m.sessions = map[string]*peerConn{}
	m.mu.Unlock()
	for _, pc := range all {
		_ = pc.sess.Close()
	}
}

// upkeep revokes sessions the registry no longer admits and starts a connect to each active node
// that has no session and that this node is the one to dial.
func (m *Manager) upkeep(ctx context.Context) {
	self := m.self()
	now := m.o.Now()
	nodes := m.o.Topology.Nodes()
	byID := make(map[string]registry.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	m.mu.Lock()
	sessions := make([]*peerConn, 0, len(m.sessions))
	for _, pc := range m.sessions {
		sessions = append(sessions, pc)
	}
	m.mu.Unlock()
	for _, pc := range sessions {
		n := byID[pc.node]
		if _, err := m.admit(n.ID, n.CertSerial); err == nil && n.ID != "" {
			pc.revokedSince.Store(0)
			continue
		}
		if since := pc.revokedSince.Load(); since == 0 {
			pc.revokedSince.Store(now.UnixNano())
		} else if now.Sub(time.Unix(0, since)) >= m.o.RevokeGrace {
			m.o.Log.Info("mesh: closing the session of a node the registry no longer admits", "node", pc.node)
			_ = pc.sess.Close()
		}
	}
	for _, n := range nodes {
		if n.ID == self || n.State != registry.NodeActive {
			continue
		}
		st := m.dialStateOf(n.ID)
		if m.connected(n.ID) {
			m.mu.Lock()
			st.downFrom = time.Time{}
			st.backoff = 0
			m.mu.Unlock()
			continue
		}
		m.mu.Lock()
		if st.downFrom.IsZero() {
			st.downFrom = now
		}
		due := now.After(st.nextTry) && (lessID(self, n.ID) || now.Sub(st.downFrom) >= m.o.DialDelay)
		m.mu.Unlock()
		if due {
			go func() {
				cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				if _, err := m.ensure(cctx, n.ID); err != nil {
					m.o.Log.Debug("mesh: no session", "node", n.ID, "error", err)
				}
			}()
		}
	}
}

func (m *Manager) dialStateOf(node string) *dialState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.dials[node]
	if st == nil {
		st = &dialState{}
		m.dials[node] = st
	}
	return st
}

func (m *Manager) connected(node string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	pc := m.sessions[node]
	return pc != nil && !pc.sess.IsClosed()
}

// lessID orders node ids numerically: n2 before n10.
func lessID(a, b string) bool {
	na, _ := strconv.Atoi(strings.TrimPrefix(a, "n"))
	nb, _ := strconv.Atoi(strings.TrimPrefix(b, "n"))
	if na != nb {
		return na < nb
	}
	return a < b
}

// Connected implements Mesh.
func (m *Manager) Connected(node string) bool { return m.connected(node) }

// RTT implements Mesh.
func (m *Manager) RTT(node string) (time.Duration, bool) {
	m.mu.Lock()
	pc := m.sessions[node]
	m.mu.Unlock()
	if pc == nil || pc.rtt.Load() == 0 {
		return 0, false
	}
	return time.Duration(pc.rtt.Load()), true
}

// Peers implements Mesh: the nodes with a session up now, by id.
func (m *Manager) Peers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for id, pc := range m.sessions {
		if !pc.sess.IsClosed() {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessID(out[i], out[j]) })
	return out
}

// LastPing is the last answer to a ping of node and when it came.
func (m *Manager) LastPing(node string) (peerapi.Ping, time.Time, bool) {
	m.mu.Lock()
	pc := m.sessions[node]
	m.mu.Unlock()
	if pc == nil {
		return peerapi.Ping{}, time.Time{}, false
	}
	if st := pc.lastPng.Load(); st != nil {
		return st.Ping, st.At, true
	}
	return peerapi.Ping{}, time.Time{}, false
}

// Pinger is implemented by a Mesh that remembers the last ping of each node.
type Pinger interface {
	LastPing(node string) (p peerapi.Ping, at time.Time, ok bool)
}

var _ Pinger = (*Manager)(nil)

// peerOf is the Peer of a node id as the registry has it now.
func (m *Manager) peerOf(node string) Peer {
	if node == "" {
		return Peer{}
	}
	for _, n := range m.o.Topology.Nodes() {
		if n.ID == node {
			return Peer{Node: node, State: n.State}
		}
	}
	return Peer{Node: node, State: registry.NodeLeft}
}

// ensure returns the session to node, opening one when there is none. Callers that ask at the
// same time share one attempt, and an attempt that failed less than a second ago is not repeated.
func (m *Manager) ensure(ctx context.Context, node string) (*peerConn, error) {
	for {
		m.mu.Lock()
		if pc := m.sessions[node]; pc != nil && !pc.sess.IsClosed() {
			m.mu.Unlock()
			return pc, nil
		}
		st := m.dials[node]
		if st == nil {
			st = &dialState{}
			m.dials[node] = st
		}
		if st.flight != nil {
			wait := st.flight
			m.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, fmt.Errorf("%w: %v", ErrNoSession, ctx.Err())
			}
		}
		if st.failErr != nil && m.o.Now().Sub(st.failedAt) < time.Second {
			err := st.failErr
			m.mu.Unlock()
			return nil, err
		}
		st.flight = make(chan struct{})
		m.mu.Unlock()

		err := m.connect(ctx, node)

		m.mu.Lock()
		close(st.flight)
		st.flight = nil
		if err != nil && ctx.Err() == nil { // a caller that gave up does not hold the others off
			st.failErr, st.failedAt = err, m.o.Now()
			st.backoff = min(max(2*st.backoff, time.Second), 30*time.Second)
			st.nextTry = m.o.Now().Add(st.backoff)
		} else if err == nil {
			st.failErr = nil
		}
		m.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
}

// connect opens a session to node: it tries the registry's address for the node and then the
// resolver's, and registers the first session that completes its handshake.
func (m *Manager) connect(ctx context.Context, node string) error {
	var n registry.Node
	found := false
	for _, x := range m.o.Topology.Nodes() {
		if x.ID == node {
			n, found = x, true
		}
	}
	if !found {
		return fmt.Errorf("%w: no node %s in the registry", ErrNoSession, node)
	}
	if m.credentials() == nil {
		return fmt.Errorf("%w: this node has no certificate", ErrNoSession)
	}
	addrs := m.addrs(ctx, n)
	if len(addrs) == 0 {
		return fmt.Errorf("%w: node %s has no address", ErrNoSession, node)
	}
	var last error
	for _, addr := range addrs {
		pc, err := m.dialAddr(ctx, node, addr)
		if err != nil {
			last = err
			continue
		}
		m.register(pc)
		return nil
	}
	return fmt.Errorf("%w: %v", ErrNoSession, last)
}

func (m *Manager) addrs(ctx context.Context, n registry.Node) []string {
	var out []string
	seen := map[string]bool{}
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	add(n.PeerAddr)
	if m.o.Resolver != nil {
		rs, err := m.o.Resolver.Addrs(ctx, n)
		if err != nil {
			m.o.Log.Debug("mesh: address lookup failed", "node", n.ID, "error", err)
		}
		for _, a := range rs {
			add(a)
		}
	}
	return out
}

func (m *Manager) dialAddr(ctx context.Context, node, addr string) (*peerConn, error) {
	conn, err := m.o.TCPDial(ctx, addr)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(conn, clientTLS(m.credentials, node, m.admit, m.o.Now))
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("handshake with %s: %w", addr, err)
	}
	sess, err := NewSession(tc, true)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &peerConn{node: node, sess: sess, dialed: true, since: m.o.Now()}, nil
}

// Serve accepts connections on ln until it is closed: each completes a TLS handshake, which the
// registry may refuse, and becomes a session. A client with no certificate is a joiner; its session
// is served but not kept, and the request policy lets it reach the join endpoint only.
func (m *Manager) Serve(ctx context.Context, ln net.Listener) error {
	m.serveRPCStreams(ctx)
	go func() { <-ctx.Done(); _ = ln.Close() }()
	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			if !retryableAccept(err) {
				return err
			}
			delay = acceptDelay(delay)
			m.o.Log.Warn("mesh: accepting a connection failed; trying again", "error", err, "wait", delay)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil
			}
			continue
		}
		delay = 0
		go m.accept(ctx, conn)
	}
}

// retryableAccept reports whether an error of Accept is about this connection or about resources
// that come back (a timeout, no file descriptors left, a connection reset before it was accepted)
// and not about the listener: the loop that accepts goes on after the first and ends on the second.
func retryableAccept(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	for _, e := range []error{syscall.EMFILE, syscall.ENFILE, syscall.ECONNABORTED, syscall.ENOBUFS, syscall.ENOMEM} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// acceptDelay is the wait after a failed Accept: 5 ms, doubling up to a second.
func acceptDelay(prev time.Duration) time.Duration {
	return min(max(2*prev, 5*time.Millisecond), time.Second)
}

func (m *Manager) accept(ctx context.Context, conn net.Conn) {
	if m.credentials() == nil {
		_ = conn.Close()
		return
	}
	ip := remoteIP(conn)
	if !m.shaking.take(ip, m.o.MaxShakesPerIP) { // a handshake that does not finish holds its slot for 10 s
		m.o.Log.Debug("mesh: too many handshakes from one address", "remote", ip)
		_ = conn.Close()
		return
	}
	tc := tls.Server(conn, serverTLS(m.credentials, m.admit, m.o.Now))
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := tc.HandshakeContext(hctx)
	m.shaking.drop(ip)
	if err != nil {
		m.o.Log.Debug("mesh: handshake refused", "remote", conn.RemoteAddr().String(), "error", err)
		_ = conn.Close()
		return
	}
	node := ""
	if certs := tc.ConnectionState().PeerCertificates; len(certs) > 0 {
		node, _ = NodeIDOf(certs[0])
	}
	if node == "" { // a joiner: serve it without keeping the session
		m.serveAnonymous(tc, ip)
		return
	}
	if tc.ConnectionState().ServerName == OneShotName { // a tool of the node: its own session, not the node's
		m.serveOneShot(tc, node)
		return
	}
	sess, err := NewSession(tc, false)
	if err != nil {
		_ = conn.Close()
		return
	}
	m.register(&peerConn{node: node, sess: sess, since: m.o.Now()})
}

func remoteIP(c net.Conn) string {
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return ""
	}
	return host
}

// maxOneShotPerNode is how many sessions of its command-line tools (OneShot) one node may hold at once.
const maxOneShotPerNode = 4

// serveOneShot serves a node's short call (OneShot) until its session ends. The node was admitted at the
// handshake and its streams are judged as those of any session of the node, but the session is not kept
// in the table, is not pinged, does not replace the one the node's daemon holds and ends after OneShotLife.
func (m *Manager) serveOneShot(conn net.Conn, node string) {
	if !m.oneShot.take(node, maxOneShotPerNode) {
		m.o.Log.Debug("mesh: too many short sessions from one node", "node", node)
		_ = conn.Close()
		return
	}
	defer m.oneShot.drop(node)
	sess, err := NewSession(conn, false)
	if err != nil {
		_ = conn.Close()
		return
	}
	// A short call is short: the session is not in the table, so the sweep that closes the sessions of a
	// node the registry stopped admitting does not see it.
	defer time.AfterFunc(m.o.OneShotLife, func() { _ = sess.Close() }).Stop()
	m.serveStreams(sess, node, "")
}

// serveAnonymous serves the streams of a caller that presented no certificate until its session
// ends. There is room for MaxAnonSessions at a time, MaxAnonPerAddr of them for one address, and a
// session that opens no stream within AnonFirstStream is closed (see reapAnonymous).
func (m *Manager) serveAnonymous(conn net.Conn, remote string) {
	if int(m.anon.Add(1)) > m.o.MaxAnonSessions {
		m.anon.Add(-1)
		m.o.Log.Debug("mesh: too many sessions without a certificate", "remote", remote)
		_ = conn.Close()
		return
	}
	defer m.anon.Add(-1)
	if !m.anonIP.take(remote, m.o.MaxAnonPerAddr) {
		m.o.Log.Debug("mesh: too many sessions without a certificate from one address", "remote", remote)
		_ = conn.Close()
		return
	}
	defer m.anonIP.drop(remote)
	s, err := newAnonSession(conn)
	if err != nil {
		_ = conn.Close()
		return
	}
	sess := &streamSeen{Session: s}
	go m.reapAnonymous(sess)
	m.serveStreams(sess, "", remote)
	_ = sess.Close()
}

// streamSeen is a Session that notes whether the far end ever opened a stream.
type streamSeen struct {
	Session
	seen atomic.Bool
}

func (s *streamSeen) AcceptStream() (net.Conn, error) {
	st, err := s.Session.AcceptStream()
	if err == nil {
		s.seen.Store(true)
	}
	return st, err
}

// reapAnonymous closes sess when it opened no stream within AnonFirstStream, once it has been open for
// AnonLife and has no stream, and at anonSessionMax in any case. A joiner opens its first stream as soon
// as the handshake is done and needs its session for the time of its requests; a stranger that keeps one
// open (the keepalive holds it) gets no more than that.
func (m *Manager) reapAnonymous(sess *streamSeen) {
	life, first := m.o.AnonLife, m.o.AnonFirstStream
	tick := time.NewTicker(min(max(min(life, first)/4, 10*time.Millisecond), 5*time.Second))
	defer tick.Stop()
	start := time.Now()
	for {
		select {
		case <-sess.CloseChan():
			return
		case <-tick.C:
		}
		age := time.Since(start)
		if age >= anonSessionMax || (age >= first && !sess.seen.Load()) || (age >= life && sess.NumStreams() == 0) {
			_ = sess.Close()
			return
		}
	}
}

// perAddr counts what each remote address holds at once.
type perAddr struct {
	mu sync.Mutex
	n  map[string]int
}

// take reserves one for addr; false when addr holds max already.
func (p *perAddr) take(addr string, max int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n == nil {
		p.n = map[string]int{}
	}
	if p.n[addr] >= max {
		return false
	}
	p.n[addr]++
	return true
}

func (p *perAddr) drop(addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n[addr] <= 1 {
		delete(p.n, addr)
		return
	}
	p.n[addr]--
}

// register makes pc the session to its node. When there already is one (both sides dialed at
// once) the session that the lower node id dialed stays; the other gets no new streams and ends
// when its last one does. Both nodes apply the same rule, so they agree on the survivor.
func (m *Manager) register(pc *peerConn) {
	m.mu.Lock()
	old := m.sessions[pc.node]
	keep := pc
	if old != nil && !old.sess.IsClosed() {
		if m.preferred(old, pc) {
			keep = old
		}
	}
	m.sessions[pc.node] = keep
	m.mu.Unlock()
	if keep == pc {
		go m.watch(pc)
		go m.pingLoop(pc)
		if old != nil && old != pc {
			go m.retire(old)
		}
	} else {
		go m.retire(pc)
	}
	go m.serveStreams(pc.sess, pc.node, "")
}

// preferred reports whether the existing session a beats the new one b.
func (m *Manager) preferred(a, b *peerConn) bool {
	self := m.self()
	lowerDialed := func(pc *peerConn) bool {
		dialer := pc.node
		if pc.dialed {
			dialer = self
		}
		other := self
		if pc.dialed {
			other = pc.node
		}
		return lessID(dialer, other)
	}
	switch la, lb := lowerDialed(a), lowerDialed(b); {
	case la && !lb:
		return true
	case lb && !la:
		return false
	}
	return false // the same side dialed again: the newer one replaces a session that is probably dead
}

// retire closes a session that lost the tie-break once nothing runs on it.
func (m *Manager) retire(pc *peerConn) {
	pc.retiring.Store(true)
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) && !pc.sess.IsClosed() && pc.sess.NumStreams() > 0 {
		select {
		case <-pc.sess.CloseChan():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	_ = pc.sess.Close()
}

// watch removes pc from the table when its session ends.
func (m *Manager) watch(pc *peerConn) {
	<-pc.sess.CloseChan()
	m.mu.Lock()
	if m.sessions[pc.node] == pc {
		delete(m.sessions, pc.node)
	}
	m.mu.Unlock()
}

// serveStreams handles every stream the peer opens on sess until the session ends. remote is the
// peer's IP address, for a caller that has no node. Such a caller has at most maxAnonStreams streams
// open at a time; the others are closed unread.
func (m *Manager) serveStreams(sess Session, node, remote string) {
	var open atomic.Int32
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			// The far end is gone or the session ended. smux reports a remote close here and nowhere
			// else (its CloseChan fires on a local Close or the keepalive timeout), so the session is
			// closed now: watch then drops it from the table, and a node that restarted is not shut out
			// by its own dead session when the tie-break prefers it.
			_ = sess.Close()
			return
		}
		if node == "" {
			if open.Add(1) > maxAnonStreams {
				open.Add(-1)
				_ = st.Close()
				continue
			}
			st = &releaseConn{Conn: st, release: func() { open.Add(-1) }}
		}
		go m.handleStream(st, node, remote)
	}
}

// releaseConn calls release once, when it is closed.
type releaseConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *releaseConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

func (m *Manager) handleStream(st net.Conn, node, remote string) {
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	h, err := ReadHeader(st)
	_ = st.SetReadDeadline(time.Time{})
	if err != nil {
		m.o.Log.Debug("mesh: bad stream header", "node", node, "error", err)
		_ = st.Close()
		return
	}
	peer := m.peerOf(node)
	peer.Remote = remote
	switch h.T {
	case StreamRPC:
		select {
		case m.rpcCh <- &rpcConn{Conn: st, peer: peer}:
		case <-time.After(10 * time.Second):
			_ = st.Close()
		}
	case StreamForward:
		m.serveForward(st, peer, h)
	}
}

func (m *Manager) serveForward(st net.Conn, peer Peer, h Header) {
	if m.o.Authz == nil {
		_ = st.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	port, err := m.o.Authz.Resolve(ctx, peer, h)
	cancel()
	if err != nil {
		var ref *Refusal
		if errors.As(err, &ref) {
			m.o.Log.Debug("mesh: forward stream refused", "node", peer.Node, "kind", string(h.Kind), "ref", h.Ref, "reason", ref.Reason)
		} else {
			m.o.Log.Warn("mesh: forward stream not served", "node", peer.Node, "kind", string(h.Kind), "ref", h.Ref, "error", err)
		}
		_ = st.Close()
		return
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		m.o.Log.Debug("mesh: nothing listens on the forwarded port", "kind", string(h.Kind), "ref", h.Ref, "port", port, "error", err)
		_ = st.Close()
		return
	}
	Pipe(st, c)
}

// pingLoop pings an active peer every PingEvery: it measures the round trip, hands the answer to
// OnPing, and closes a session whose pings fail three times in a row (the connection is up but the far
// end's handler is not answering). A peer that is not active is not pinged. Its session is a joiner's or
// a fenced node's, and what holds the far end there is `supavise node join` or `node rejoin`, a Client
// that has no peer API to answer with; counting its silence would cut the session that carries the
// standby's stream a few seconds into the join. The state is read at every round, so the first ping
// follows the node's confirmation.
func (m *Manager) pingLoop(pc *peerConn) {
	fails := 0
	for {
		if m.peerOf(pc.node).State != registry.NodeActive {
			fails = 0
		} else {
			fails = m.pingOnce(pc, fails)
		}
		select {
		case <-pc.sess.CloseChan():
			return
		case <-m.runCtx().Done():
			return
		case <-time.After(m.o.PingEvery):
		}
	}
}

// pingOnce sends one ping and returns the number of failures in a row after it.
func (m *Manager) pingOnce(pc *peerConn, fails int) int {
	ctx, cancel := context.WithTimeout(m.runCtx(), 5*time.Second)
	defer cancel()
	start := time.Now()
	var p peerapi.Ping
	err := m.callOn(ctx, pc, "GET", peerapi.PathPing, nil, &p)
	if err == nil || peerAnswered(err) { // a refusal still proves the far end answers
		if err == nil {
			rtt := time.Since(start)
			pc.rtt.Store(int64(rtt))
			pc.lastPng.Store(&pingState{At: m.o.Now(), Ping: p})
			if m.o.OnPing != nil {
				m.o.OnPing(pc.node, p, rtt)
			}
		}
		return 0
	}
	if fails++; fails >= 3 && !pc.sess.IsClosed() {
		m.o.Log.Warn("mesh: closing a session whose pings fail", "node", pc.node, "error", err)
		_ = pc.sess.Close()
	}
	return fails
}

// peerAnswered reports whether err is an answer of the far end, of any status.
func peerAnswered(err error) bool {
	var re *RemoteError
	return errors.As(err, &re)
}

func (m *Manager) runCtx() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ctx
}

// rpcConn is a stream that carries one HTTP request, with the caller it came from.
type rpcConn struct {
	net.Conn
	peer Peer
}

// chanListener hands the rpc streams that the session loops accept to the http.Server. Closing it
// (http.Server.Close does) unblocks Accept, as a listener's Close must.
type chanListener struct {
	ch   chan net.Conn
	done <-chan struct{}

	once      sync.Once
	closeOnce sync.Once
	closed    chan struct{}
}

func (l *chanListener) init() { l.once.Do(func() { l.closed = make(chan struct{}) }) }

func (l *chanListener) Accept() (net.Conn, error) {
	l.init()
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.init()
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "mesh" }
func (pipeAddr) String() string  { return "mesh" }

// Pipe copies between a and b both ways until either direction ends, then closes both. A forward
// stream is a request and a response (Postgres, HTTP), so the end of one direction is the end of
// the exchange: the multiplexed stream has no half close to wait for.
func Pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}
