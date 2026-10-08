package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// ErrRegistryUnreachable: no candidate DSN answered within the wait.
var ErrRegistryUnreachable = errors.New("cluster: the system cluster does not answer")

// BootDecision is what the daemon decides about its role before it opens the node (design 2.10.8):
// the node that opens read-only and runs no Migrate (a follower), the node that opens as the
// leader, or the node that refuses to start a primary (fenced).
type BootDecision struct {
	Role Role
	// Joined: the server belongs to a cluster as far as its files say. False for a server that never
	// joined; it behaves as the single server it is, and none of the other fields mean anything.
	Joined bool
	// SelfID is the node's id, from its certificate.
	SelfID string
	// Epoch is the epoch the node starts at; for a fenced node the higher epoch that fenced it.
	Epoch int64
	// Leader is the node the evidence names as leader (the node itself, or for a fenced node the other).
	Leader string
	// Reason is why the node is fenced.
	Reason string
	// DSN is the registry DSN whose cluster answered the probe: open the registry there.
	DSN string
}

// PeerView is what one reachable peer said it believes.
type PeerView struct {
	Node   string
	Epoch  int64
	Leader string
}

// Evidence is everything the boot decision looks at for a node whose system cluster is a primary.
type Evidence struct {
	SelfID string
	// Self, Epoch and Leader are the local record: this node's row and the cluster row in its own
	// copy of the registry.
	Self   registry.Node
	Epoch  int64
	Leader string
	// Peers answered a ping; peers that did not are absent.
	Peers []PeerView
	// Marker is the leader marker in the backup store; nil when none was written or the store did not answer.
	Marker *backup.LeaderMarker
}

// Decide is the rule of 2.10.8 for a node that finds its system cluster to be a primary. It starts
// as the leader unless a source it can reach says another node holds the leadership at its epoch or a
// higher one, or the local record says it was demoted. A node that can reach nobody starts only when
// its own record shows no demotion.
//
// A source that names this node as leader at a higher epoch than the local record is a promotion that
// the registry has not caught up with yet (the failover procedure wrote the marker and promoted the
// cluster): the node leads at that epoch, and the claims of nodes that still name the old leader at the
// old epoch are stale. A node that is a primary while the registry names another leader, and that no
// source names, is a stale or accidental primary and does not start.
func Decide(ev Evidence) BootDecision {
	d := BootDecision{Role: RoleLeader, Joined: true, SelfID: ev.SelfID, Epoch: ev.Epoch, Leader: ev.SelfID}
	fenced := func(epoch int64, leader, why string) BootDecision {
		return BootDecision{Role: RoleFenced, Joined: true, SelfID: ev.SelfID, Epoch: epoch, Leader: leader, Reason: why}
	}
	if st := ev.Self.State; st == registry.NodeFenced || st == registry.NodeLeft {
		return fenced(ev.Epoch, ev.Leader, fmt.Sprintf("this node is %s in its own copy of the registry", st))
	}
	type claim struct {
		who    string
		epoch  int64
		leader string
	}
	var claims []claim
	for _, p := range ev.Peers {
		if p.Leader != "" {
			claims = append(claims, claim{"node " + p.Node, p.Epoch, p.Leader})
		}
	}
	if m := ev.Marker; m != nil && m.Leader != "" {
		claims = append(claims, claim{"the leader marker in the backup store", m.Epoch, m.Leader})
	}
	raised := ev.Epoch
	for _, c := range claims {
		if c.leader == ev.SelfID && c.epoch > raised {
			raised = c.epoch
		}
	}
	for _, c := range claims {
		if c.leader != ev.SelfID && c.epoch >= raised {
			return fenced(c.epoch, c.leader, fmt.Sprintf("%s says node %s leads at epoch %d; this node's epoch is %d", c.who, c.leader, c.epoch, raised))
		}
	}
	if ev.Leader != ev.SelfID && raised == ev.Epoch {
		return fenced(ev.Epoch, ev.Leader, fmt.Sprintf("the registry names node %s as leader at epoch %d and nothing names this node", ev.Leader, ev.Epoch))
	}
	d.Epoch = raised
	return d
}

// BootEnv is what DecideBoot needs from the node.
type BootEnv struct {
	Cfg *config.Config
	// ConfigPath is the config file, from which the cluster directory follows.
	ConfigPath string
	// DSNs are the sockets the system cluster may answer on: a primary on the system port, a standby
	// on the replica port. The first that answers decides.
	DSNs []string
	// Wait bounds how long the system cluster may take to answer (systemd starts the daemon after the
	// start of supavise-postgres@system, not its readiness).
	Wait time.Duration
	// Marker reads the leader marker; nil means the node has no way to (the backup store is not open
	// or does not implement it) and the marker is not consulted.
	Marker backup.EpochMarkerStore
	// Resolver finds more addresses for a peer; may be nil.
	Resolver mesh.AddrResolver
	Log      *slog.Logger
	Now      func() time.Time
	// PeerTimeout and MarkerTimeout bound the evidence gathering; the zero values are 4 s and 5 s.
	PeerTimeout, MarkerTimeout time.Duration
}

func (e *BootEnv) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *BootEnv) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// DecideBoot decides the role before the node opens. A server whose cluster directory holds no node
// certificate, and that is not fenced, is the single server it always was: no probe, no registry
// read, the leader. Otherwise it waits for the system cluster and asks it: a standby is a follower;
// a primary is a leader unless the evidence (Decide) says it was replaced.
func DecideBoot(ctx context.Context, env BootEnv) (BootDecision, error) {
	dir := config.ClusterDir(env.ConfigPath)
	if rec, err := ReadFenced(env.Cfg); err != nil {
		return BootDecision{}, err
	} else if rec != nil {
		d := BootDecision{Role: RoleFenced, Joined: true, Epoch: rec.Epoch, Leader: rec.Leader, Reason: rec.Reason}
		if creds, err := LoadCredentials(dir); err == nil {
			d.SelfID = creds.NodeID
		}
		return d, nil
	}
	if !Joined(dir) {
		return BootDecision{Role: RoleLeader}, nil
	}
	creds, err := LoadCredentials(dir)
	if err != nil {
		return BootDecision{}, fmt.Errorf("cluster: the node certificate cannot be read: %w", err)
	}
	dsn, inRecovery, err := probeSystem(ctx, env.DSNs, env.Wait)
	if err != nil {
		return BootDecision{}, err
	}
	if inRecovery {
		return BootDecision{Role: RoleFollower, Joined: true, SelfID: creds.NodeID, DSN: dsn}, nil
	}
	ev, err := env.gather(ctx, dsn, creds)
	if err != nil {
		return BootDecision{}, err
	}
	d := Decide(ev)
	d.DSN = dsn
	return d, nil
}

// probeSystem asks each candidate DSN whether its cluster is in recovery, retrying until wait ends.
func probeSystem(ctx context.Context, dsns []string, wait time.Duration) (dsn string, inRecovery bool, err error) {
	deadline := time.Now().Add(wait)
	delay := 250 * time.Millisecond
	var last error
	for {
		for _, d := range dsns {
			rec, err := InRecovery(ctx, d)
			if err == nil {
				return d, rec, nil
			}
			last = err
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return "", false, fmt.Errorf("%w: %v", ErrRegistryUnreachable, last)
		}
		select {
		case <-ctx.Done():
			return "", false, fmt.Errorf("%w: %v", ErrRegistryUnreachable, last)
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
}

// InRecovery asks the cluster at dsn whether it is a standby (pg_is_in_recovery()).
func InRecovery(ctx context.Context, dsn string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return false, err
	}
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return false, err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var rec bool
	err = conn.QueryRow(ctx, `select pg_is_in_recovery()`).Scan(&rec)
	return rec, err
}

// gather reads the local record, asks the peers and reads the marker.
func (e *BootEnv) gather(ctx context.Context, dsn string, creds *mesh.Credentials) (Evidence, error) {
	reg, err := registry.OpenReadOnly(ctx, dsn)
	if err != nil {
		return Evidence{}, fmt.Errorf("cluster: reading the local registry: %w", err)
	}
	defer reg.Close()
	cl, err := reg.GetCluster(ctx)
	if err != nil {
		return Evidence{}, fmt.Errorf("cluster: reading the local cluster row: %w", err)
	}
	nodes, err := reg.ListNodes(ctx)
	if err != nil {
		return Evidence{}, err
	}
	ev := Evidence{SelfID: creds.NodeID, Epoch: cl.Epoch, Leader: cl.Leader}
	for _, n := range nodes {
		if n.ID == creds.NodeID {
			ev.Self = n
		}
	}
	peerWait, markerWait := e.PeerTimeout, e.MarkerTimeout
	if peerWait <= 0 {
		peerWait = 4 * time.Second
	}
	if markerWait <= 0 {
		markerWait = 5 * time.Second
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	admit := mesh.AdmitFromNodes(func() []registry.Node { return nodes })
	for _, n := range nodes {
		if n.ID == creds.NodeID || n.State != registry.NodeActive {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, peerWait)
			defer cancel()
			p, err := AskPeer(pctx, creds, admit, n, e.Resolver, e.now)
			if err != nil {
				e.logger().Info("boot: a peer did not answer", "node", n.ID, "error", err)
				return
			}
			mu.Lock()
			ev.Peers = append(ev.Peers, PeerView{Node: n.ID, Epoch: p.Epoch, Leader: p.Leader})
			mu.Unlock()
		}()
	}
	if e.Marker != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mctx, cancel := context.WithTimeout(ctx, markerWait)
			defer cancel()
			m, err := e.Marker.ReadLeaderMarker(mctx)
			if err != nil {
				e.logger().Info("boot: the leader marker could not be read", "error", err)
				return
			}
			mu.Lock()
			ev.Marker = m
			mu.Unlock()
		}()
	}
	wg.Wait()
	return ev, nil
}

// AskPeer pings one peer over a session of its own and returns its answer. It tries the address in
// the registry first and then the resolver's.
func AskPeer(ctx context.Context, creds *mesh.Credentials, admit mesh.AdmitFunc, n registry.Node, res mesh.AddrResolver, now func() time.Time) (peerapi.Ping, error) {
	if now == nil {
		now = time.Now
	}
	addrs := []string{}
	if n.PeerAddr != "" {
		addrs = append(addrs, n.PeerAddr)
	}
	if res != nil {
		if more, err := res.Addrs(ctx, n); err == nil {
			addrs = append(addrs, more...)
		}
	}
	if len(addrs) == 0 {
		return peerapi.Ping{}, fmt.Errorf("node %s has no address", n.ID)
	}
	var last error
	for _, addr := range addrs {
		c, err := mesh.DialClient(ctx, addr, n.ID, mesh.ClientTLS(func() *mesh.Credentials { return creds }, n.ID, admit, now))
		if err != nil {
			last = err
			continue
		}
		var p peerapi.Ping
		err = c.Call(ctx, "GET", peerapi.PathPing, nil, &p)
		c.Close()
		if err != nil {
			last = err
			continue
		}
		return p, nil
	}
	return peerapi.Ping{}, last
}

// AssumeLeadership records, in the registry of a node that has just booted as the leader, that it
// leads: the cluster row names it at the epoch the boot decision settled on. It is the step that
// follows a promotion (the failover procedure promoted the system cluster; the registry on it still
// names the old leader). The old leader's row becomes fenced, unless the cluster row announces
// maintenance on that node, which is how a planned switchover tells the new leader that the old one
// stopped on purpose and is to be demoted in place: its row stays active. The failover procedure
// clears the announcement when the move is done. A node that already leads at that epoch or a
// higher one changes nothing; a SetLeader that the registry refuses (the epoch is not above its)
// is returned, and the node is not the leader.
func AssumeLeadership(ctx context.Context, reg registry.Registry, self string, epoch int64, now time.Time) (planned bool, err error) {
	cl, err := reg.GetCluster(ctx)
	if err != nil {
		return false, err
	}
	if cl.Leader == self && cl.Epoch >= epoch {
		return false, nil
	}
	old := cl.Leader
	planned = old != self && cl.Maintenance.Active(now) && cl.Maintenance.Node == old
	if err := reg.SetLeader(ctx, self, epoch); err != nil {
		return false, err
	}
	if old != self && !planned {
		if err := reg.SetNodeState(ctx, old, registry.NodeFenced); err != nil && !errors.Is(err, registry.ErrNotFound) {
			return false, err
		}
	}
	return planned, nil
}
