// Package mesh is the transport between the nodes of a cluster: one mutually authenticated
// TLS 1.3 port per node ([node] peer_listen, 7443), one multiplexed session per pair of nodes,
// and on it two kinds of stream. Either side of a session opens streams.
//
// A forward stream carries raw bytes to a loopback port on the node at the far end. It is how a
// project's canonical ports, its replica ports and the shared-service ports are made to answer
// on every node: on the node that runs the service the port is the service; on every other node
// a listener on the same port forwards each connection through the mesh (invariant I3). The
// proxy, Supavisor, Realtime, Storage, Functions, pg-meta and backups therefore keep dialing
// 127.0.0.1:<port> whatever node a project lives on.
//
// An rpc stream carries one HTTP/1.1 request and its response: the peer API (peerapi).
//
// This file is the contract the other packages are written against: the stream vocabulary, the
// interfaces that move bytes and calls, and the registration point for peer API handlers. The
// mesh itself (certificate handling, the session manager, the peer server, the forwarder
// reconciler) lives in the other files of the package.
package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/config"
)

// ALPN is the application protocol the mesh negotiates in the TLS handshake.
const ALPN = "supavise-mesh/1"

// Stream types, the "t" of a stream header.
const (
	StreamForward = "fwd"
	StreamRPC     = "rpc"
)

// Kind says which loopback port a forward stream is for. It is an enum, never a port number: the
// target resolves the kind and the ref to a port from its own copy of the registry, and refuses
// the stream when the registry says the service does not run there.
type Kind string

// The kinds of a project (they need a ref) and of the shared services (they do not).
const (
	// KindPostgres, KindGoTrue and KindPostgREST are the canonical ports of a project:
	// config.PortsFor(ref, seq). The target must be the project's home.
	KindPostgres  Kind = "postgres"
	KindGoTrue    Kind = "gotrue"
	KindPostgREST Kind = "postgrest"
	// KindReplicaPostgres and KindReplicaPostgREST are the replica ports: config.ReplicaPorts(ref,
	// seq). The target must hold a replica row of ref.
	KindReplicaPostgres  Kind = "replica-postgres"
	KindReplicaPostgREST Kind = "replica-postgrest"

	// The shared services, "svc:<name>". Only the leader runs them, so the target must be the leader.
	KindAdmin       Kind = "svc:admin" // the Management API's loopback listener
	KindStudio      Kind = "svc:studio"
	KindPGMeta      Kind = "svc:pgmeta"
	KindRealtime    Kind = "svc:realtime"
	KindStorage     Kind = "svc:storage"
	KindImgproxy    Kind = "svc:imgproxy"
	KindEdgeRuntime Kind = "svc:edge-runtime"
)

// ProjectKinds and ServiceKinds list the kinds in the order the forwarder reconciler binds them.
var (
	ProjectKinds = []Kind{KindPostgres, KindGoTrue, KindPostgREST, KindReplicaPostgres, KindReplicaPostgREST}
	ServiceKinds = []Kind{KindAdmin, KindStudio, KindPGMeta, KindRealtime, KindStorage, KindImgproxy, KindEdgeRuntime}
)

// IsService reports whether k is one of the shared services, which are not tied to a ref.
func (k Kind) IsService() bool {
	for _, s := range ServiceKinds {
		if s == k {
			return true
		}
	}
	return false
}

// Valid reports whether k is one of the kinds above.
func (k Kind) Valid() bool {
	if k.IsService() {
		return true
	}
	for _, p := range ProjectKinds {
		if p == k {
			return true
		}
	}
	return false
}

// Errors of the mesh. Wrapped errors answer errors.Is.
var (
	// ErrNoSession: there is no session to the node and none could be opened.
	ErrNoSession = errors.New("mesh: no session to that node")
	// ErrRefused: the far end refused the stream, and an rpc call learned it. The reason is in the
	// wrapped text; the cases are an unknown ref, a project that is not homed there (or has no
	// replica there), a shared service asked of a node that is not the leader, and a peer the
	// registry no longer admits.
	ErrRefused = errors.New("mesh: the node refused the stream")
	// ErrNoPort: the kind has no port for that project (the system project has no PostgREST).
	ErrNoPort = errors.New("mesh: no such port")
)

// RemoteError is a peer API answer with a status outside 2xx.
type RemoteError struct {
	Node    string
	Status  int
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("mesh: node %s answered %d: %s", e.Node, e.Status, e.Message)
}

// Header is the first line of every stream, written as one line of JSON ending in a newline.
type Header struct {
	// T is StreamForward or StreamRPC.
	T string `json:"t"`
	// Kind is set on a forward stream.
	Kind Kind `json:"kind,omitempty"`
	// Ref is the project of a forward stream whose kind is not a shared service.
	Ref string `json:"ref,omitempty"`
}

var refRe = regexp.MustCompile(`^(system|[a-z]{20})$`)

// Validate checks the header the way the receiving end must before it opens a port: a known
// type and kind, a ref that has the shape of one exactly when the kind needs one, and nothing
// on an rpc stream.
func (h Header) Validate() error {
	switch h.T {
	case StreamRPC:
		if h.Kind != "" || h.Ref != "" {
			return errors.New("mesh: an rpc stream names no kind and no ref")
		}
	case StreamForward:
		if !h.Kind.Valid() {
			return fmt.Errorf("mesh: unknown kind %q", h.Kind)
		}
		switch {
		case h.Kind.IsService() && h.Ref != "":
			return fmt.Errorf("mesh: %s takes no ref", h.Kind)
		case !h.Kind.IsService() && !refRe.MatchString(h.Ref):
			return fmt.Errorf("mesh: %s needs a project ref, not %q", h.Kind, h.Ref)
		}
	default:
		return fmt.Errorf("mesh: unknown stream type %q", h.T)
	}
	return nil
}

// maxHeader bounds a header line, so that a peer cannot make the other end buffer without limit.
const maxHeader = 1024

// WriteHeader writes h and its newline to w in one Write.
func WriteHeader(w io.Writer, h Header) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadHeader reads one header line from r, one byte at a time so that it consumes nothing of
// what follows it, and validates it. A line longer than 1 KiB is an error.
func ReadHeader(r io.Reader) (Header, error) {
	var line bytes.Buffer
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, one); err != nil {
			return Header{}, fmt.Errorf("mesh: reading the stream header: %w", err)
		}
		if one[0] == '\n' {
			break
		}
		if line.Len() >= maxHeader {
			return Header{}, errors.New("mesh: the stream header is too long")
		}
		line.WriteByte(one[0])
	}
	var h Header
	dec := json.NewDecoder(&line)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return Header{}, fmt.Errorf("mesh: the stream header is not valid: %w", err)
	}
	return h, h.Validate()
}

// LocalPort is the loopback port a forward stream of kind k for project ref (registry sequence
// seq) reaches on a node that serves it, and the port a forwarder binds on a node that does not.
// The two sides compute the same number from the same configuration, which is what makes a
// forwarder transparent. ErrNoPort when the project has no such service.
func LocalPort(cfg *config.Config, k Kind, ref string, seq int) (int, error) {
	var port int
	switch k {
	case KindPostgres:
		port = cfg.PortsFor(ref, seq).Postgres
	case KindGoTrue:
		port = cfg.PortsFor(ref, seq).GoTrue
	case KindPostgREST:
		port = cfg.PortsFor(ref, seq).PostgREST
	case KindReplicaPostgres:
		port = cfg.ReplicaPorts(ref, seq).Postgres
	case KindReplicaPostgREST:
		port = cfg.ReplicaPorts(ref, seq).PostgREST
	case KindAdmin:
		_, p, err := net.SplitHostPort(cfg.Listen.Admin)
		if err != nil {
			return 0, fmt.Errorf("mesh: [listen] admin %q: %w", cfg.Listen.Admin, err)
		}
		port, _ = strconv.Atoi(p)
	case KindStudio:
		port = cfg.Ports.Studio
	case KindPGMeta:
		port = cfg.Ports.PGMeta
	case KindRealtime:
		port = cfg.Ports.Realtime
	case KindStorage:
		port = cfg.Ports.Storage
	case KindImgproxy:
		port = cfg.Ports.Imgproxy
	case KindEdgeRuntime:
		port = cfg.Ports.EdgeRuntime
	default:
		return 0, fmt.Errorf("mesh: unknown kind %q", k)
	}
	if port <= 0 {
		return 0, fmt.Errorf("%w: %s of %s", ErrNoPort, k, ref)
	}
	return port, nil
}

// Dialer opens streams to other nodes. Implemented by the session manager; fake it in tests.
type Dialer interface {
	// Dial opens a stream to node (a node id, "n2"), writes h as its header and returns the
	// stream as a connection: raw bytes both ways until either side closes it. It uses the
	// session to the node and, when there is none, opens one from whichever side can connect
	// (ErrNoSession if neither can). It does not wait for the far end to accept the header: a
	// forward stream the far end refuses is closed before any byte, which the caller sees as EOF
	// on its first read.
	Dial(ctx context.Context, node string, h Header) (net.Conn, error)
}

// Forward opens a forward stream of the given kind (and project ref, empty for a shared service)
// to node.
func Forward(ctx context.Context, d Dialer, node string, kind Kind, ref string) (net.Conn, error) {
	return d.Dial(ctx, node, Header{T: StreamForward, Kind: kind, Ref: ref})
}

// RPC makes peer API calls. Implemented by the session manager; fake it in tests.
type RPC interface {
	// Call sends one request to node on an rpc stream: method and path are the peer API's
	// (peerapi paths), in is encoded as the JSON body (nil sends none), and a 2xx answer is
	// decoded into out (nil discards it). Another status is returned as *RemoteError; a failure to
	// reach the node wraps ErrNoSession. The call ends with ctx.
	Call(ctx context.Context, node, method, path string, in, out any) error
}

// Mesh is a node's whole view of its sessions.
type Mesh interface {
	Dialer
	RPC
	// Connected reports whether a session to node is up now.
	Connected(node string) bool
	// RTT is the round-trip time of the last ping to node; ok is false before the first.
	RTT(node string) (d time.Duration, ok bool)
	// Peers lists the nodes with a session up now.
	Peers() []string
}

// Peer is the authenticated far end of a request: the node whose certificate the TLS handshake
// verified against the cluster CA and the registry (state active, serial current).
type Peer struct {
	// Node is the node id. Empty for a request that arrived without a client certificate, which
	// only the join endpoints accept.
	Node string
}

type peerKey struct{}

// WithPeer returns ctx carrying p. The peer server calls it before it hands a request to a handler.
func WithPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

// PeerFrom returns the peer of a request handled by the peer server.
func PeerFrom(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(peerKey{}).(Peer)
	return p, ok
}

// HandlerFunc serves one peer API request. Read the caller with PeerFrom(r.Context()). Answer
// with JSON bodies; a non-2xx status carries a peerapi.Error.
type HandlerFunc func(w http.ResponseWriter, r *http.Request)

// Mux collects the handlers of the peer API.
type Mux struct {
	mu       sync.Mutex
	mux      *http.ServeMux
	patterns []string
}

func NewMux() *Mux { return &Mux{mux: http.NewServeMux()} }

// Handle registers fn for pattern, in net/http's pattern syntax ("POST /peer/v1/fence",
// "GET /peer/v1/instances/{identifier}"). It panics on a pattern that is empty, nil-handled or
// registered already, like http.ServeMux.Handle: a clash is a programming error found at start.
func (m *Mux) Handle(pattern string, fn HandlerFunc) {
	if pattern == "" || fn == nil {
		panic("mesh: Handle needs a pattern and a handler")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mux.HandleFunc(pattern, fn)
	m.patterns = append(m.patterns, pattern)
}

// Patterns lists what was registered, in order.
func (m *Mux) Patterns() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.patterns...)
}

// ServeHTTP routes a request to its handler.
func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }

// DefaultMux is the mux the peer server serves. Every workstream registers its endpoints on it
// with Handle (from its wire hook, before the server starts), so that none edits another's files.
var DefaultMux = NewMux()

// Handle registers fn on DefaultMux.
func Handle(pattern string, fn HandlerFunc) { DefaultMux.Handle(pattern, fn) }
