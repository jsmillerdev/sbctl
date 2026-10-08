package mesh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// Forwarders keep invariant I3: on this node every port of a service that runs elsewhere answers.
// For each project homed on another node it listens on the canonical Postgres, GoTrue and PostgREST
// ports; for each replica on another node, on the replica ports (when this node holds no replica
// of the project itself); on a node that is not the leader, on the shared-service ports. Each
// connection becomes a forward stream to the node that runs the service. A forwarder binds only
// when the registry says the service is not local, and a port that something already listens on
// (the service itself, still stopping or starting) is retried every Interval; when the registry
// moves a service, the listener follows or goes. A node that is fenced forwards nothing.
type Forwarders struct {
	Cfg      *config.Config
	Topology Topology
	Source   Source
	Dialer   Dialer
	Log      *slog.Logger
	// Fenced reports whether this node is fenced; nil means it is not.
	Fenced func() bool
	// Interval is how often the desired set is recomputed even when nothing announced a change
	// (retry of a port still in use, a new leader). Zero means 2 s.
	Interval time.Duration
	// Listen binds a loopback port; nil binds 127.0.0.1:<port>.
	Listen func(port int) (net.Listener, error)

	mu        sync.Mutex
	ls        map[int]*fwdListener
	suspended map[string]time.Time
	failures  map[int]int
	kick      chan struct{}
}

// target is where the connections of one listener go.
type target struct {
	kind Kind
	ref  string
	node string
}

type fwdListener struct {
	port   int
	ln     net.Listener
	target atomic.Pointer[target]
}

// Run keeps the listeners in step with the registry until ctx ends, then closes them.
func (f *Forwarders) Run(ctx context.Context) error {
	f.init()
	defer f.closeAll()
	changes, err := f.Source.Subscribe(ctx)
	if err != nil {
		f.log().Debug("mesh: forwarders poll the registry", "reason", err.Error())
		changes = nil
	}
	every := f.Interval
	if every <= 0 {
		every = 2 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	var debounce <-chan time.Time
	f.Reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-changes:
			if !ok {
				changes = nil
				continue
			}
			if debounce == nil {
				debounce = time.After(150 * time.Millisecond)
			}
		case <-f.kick:
			f.Reconcile(ctx)
		case <-debounce:
			debounce = nil
			f.Reconcile(ctx)
		case <-tick.C:
			f.Reconcile(ctx)
		}
	}
}

func (f *Forwarders) init() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ls == nil {
		f.ls = map[int]*fwdListener{}
		f.suspended = map[string]time.Time{}
		f.failures = map[int]int{}
		f.kick = make(chan struct{}, 1)
	}
}

func (f *Forwarders) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Ports lists the ports that have a listener now and where each forwards to, for tests and status.
func (f *Forwarders) Ports() map[int]string {
	f.init()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int]string{}
	for p, l := range f.ls {
		if t := l.target.Load(); t != nil {
			out[p] = fmt.Sprintf("%s/%s -> %s", t.kind, t.ref, t.node)
		}
	}
	return out
}

// Suspend closes the listeners of ref's canonical and replica ports, so that a service that is about
// to run here (a replica being promoted starts Postgres on the canonical port) can bind, and keeps
// them closed until the returned function is called or ten minutes pass. The registry moves the
// project's home while the suspension is on, after which no forwarder of ref is wanted on this node.
func (f *Forwarders) Suspend(ref string) (resume func()) {
	f.init()
	f.mu.Lock()
	f.suspended[ref] = time.Now().Add(10 * time.Minute)
	for p, l := range f.ls {
		if t := l.target.Load(); t != nil && t.ref == ref {
			_ = l.ln.Close()
			delete(f.ls, p)
		}
	}
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		delete(f.suspended, ref)
		f.mu.Unlock()
		select {
		case f.kick <- struct{}{}:
		default:
		}
	}
}

// desired computes the listeners this node should have from the registry as it is now.
func (f *Forwarders) desired(ctx context.Context) (map[int]target, error) {
	want := map[int]target{}
	if f.Fenced != nil && f.Fenced() {
		return want, nil
	}
	self := f.Topology.Self().ID
	active := map[string]bool{}
	for _, n := range f.Topology.Nodes() {
		if n.State == registry.NodeActive {
			active[n.ID] = true
		}
	}
	projects, err := f.Source.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	replicas, err := f.Source.ListReplicas(ctx, "")
	if err != nil {
		return nil, err
	}
	byRef := map[string][]registry.Replica{}
	for _, r := range replicas {
		byRef[r.Ref] = append(byRef[r.Ref], r)
	}
	// A port belongs to the first target that names it. The replica and canonical ranges do not
	// overlap (config validation), but a shared-service port could coincide with a project port in
	// a hand-made config; the project, added first, keeps it.
	add := func(k Kind, ref string, seq int, node string) {
		port, err := LocalPort(f.Cfg, k, ref, seq)
		if _, taken := want[port]; err != nil || taken || !active[node] {
			return
		}
		want[port] = target{kind: k, ref: ref, node: node}
	}
	for _, p := range projects {
		if p.NodeID != self {
			for _, k := range []Kind{KindPostgres, KindGoTrue, KindPostgREST} {
				add(k, p.Ref, p.Seq, p.NodeID)
			}
		}
		// The replica ports are the replica itself on the node that holds it and a forwarder to
		// it everywhere else. A project with several replicas has one replica port: the oldest
		// replica answers it.
		rs := byRef[p.Ref]
		if len(rs) == 0 || hasReplicaOn(rs, self) {
			continue
		}
		oldest := rs[0]
		for _, r := range rs[1:] {
			if r.CreatedAt.Before(oldest.CreatedAt) {
				oldest = r
			}
		}
		for _, k := range []Kind{KindReplicaPostgres, KindReplicaPostgREST} {
			add(k, p.Ref, p.Seq, oldest.NodeID)
		}
	}
	if !f.Topology.IsLeader() {
		if lead, ok := f.Topology.Leader(); ok && lead.ID != self {
			for _, k := range ServiceKinds {
				add(k, "", 0, lead.ID)
			}
		}
	}
	return want, nil
}

// Reconcile makes the listeners match the registry: it closes the ones no longer wanted,
// retargets the ones whose service moved to another node, and binds the missing ones. A port that
// cannot be bound is tried again by the next call.
func (f *Forwarders) Reconcile(ctx context.Context) {
	f.init()
	want, err := f.desired(ctx)
	if err != nil {
		f.log().Warn("mesh: forwarders cannot read the registry", "error", err)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for ref, until := range f.suspended {
		if now.After(until) {
			delete(f.suspended, ref)
		}
	}
	for port, t := range want {
		if _, held := f.suspended[t.ref]; held {
			delete(want, port)
			continue
		}
		if l := f.ls[port]; l != nil {
			l.target.Store(&t)
			continue
		}
		ln, err := f.listen(port)
		if err != nil {
			if n := f.failures[port]; n%30 == 0 { // once, then every 30th try
				f.log().Info("mesh: cannot forward a port yet", "port", port, "kind", string(t.kind), "ref", t.ref, "error", err)
			}
			f.failures[port]++
			continue
		}
		delete(f.failures, port)
		l := &fwdListener{port: port, ln: ln}
		l.target.Store(&t)
		f.ls[port] = l
		go f.accept(l)
	}
	for port, l := range f.ls {
		if _, ok := want[port]; !ok {
			_ = l.ln.Close()
			delete(f.ls, port)
		}
	}
	for port := range f.failures {
		if _, ok := want[port]; !ok {
			delete(f.failures, port)
		}
	}
}

func (f *Forwarders) listen(port int) (net.Listener, error) {
	if f.Listen != nil {
		return f.Listen(port)
	}
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

func (f *Forwarders) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for p, l := range f.ls {
		_ = l.ln.Close()
		delete(f.ls, p)
	}
}

func (f *Forwarders) accept(l *fwdListener) {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		go f.forward(l, c)
	}
}

func (f *Forwarders) forward(l *fwdListener, c net.Conn) {
	t := l.target.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	st, err := f.Dialer.Dial(ctx, t.node, Header{T: StreamForward, Kind: t.kind, Ref: t.ref})
	cancel()
	if err != nil {
		f.log().Debug("mesh: forward failed", "port", l.port, "node", t.node, "error", err)
		_ = c.Close()
		return
	}
	Pipe(c, st)
}

// ForwardOne listens on 127.0.0.1:port and forwards each connection to the service kind (of ref) on
// node, until ctx ends. A node that has no registry to follow yet (a joiner streaming its first
// copy of the system cluster) uses it for the one port it needs.
func ForwardOne(ctx context.Context, d Dialer, port int, node string, kind Kind, ref string, log *slog.Logger) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	f := &Forwarders{Dialer: d, Log: log}
	l := &fwdListener{port: port, ln: ln}
	l.target.Store(&target{kind: kind, ref: ref, node: node})
	go func() { <-ctx.Done(); _ = ln.Close() }()
	f.accept(l)
	return nil
}
