package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/artifacts"
	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/health"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/notimpl"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/units"
	"github.com/supavise/supavise/internal/versions"
)

// identityPoll, fencedPoll and retirePoll are how often a single server looks for a cluster identity,
// a fenced node looks for its record to be gone, and a follower looks at its own row. Variables so that tests can shorten them.
var (
	identityPoll = 5 * time.Second
	fencedPoll   = 3 * time.Second
	retirePoll   = 2 * time.Second
)

// unreachableAfter is how long an active peer may have no session before node_unreachable is raised.
const unreachableAfter = time.Minute

// ErrRoleChanged is what the daemon stops with when the role it started in no longer holds: its system
// cluster was promoted or demoted, it was fenced, or the server was given a cluster identity. The
// error ends Serve, systemd starts the daemon again, and the boot decision (cluster.DecideBoot) opens
// the node in the role it has now. Opening a registry read-only or writable, starting or not starting
// the shared services and binding or not binding the loopback ports are all decided there, once,
// instead of being undone and redone in a running process.
var ErrRoleChanged = errors.New("the node's role in the cluster changed; restarting")

// RegistryDSNs are the sockets the system cluster may answer on: the system port, where a primary
// listens, and the replica port, where a standby of the system cluster listens on a follower. Code
// that needs the registry of a server that may be either tries them in this order.
func RegistryDSNs(cfg *config.Config) []string {
	standby := *cfg
	standby.Ports.SystemPostgres = cfg.ReplicaPorts(config.SystemRef, 0).Postgres
	return []string{lifecycle.RegistryDSN(cfg), lifecycle.RegistryDSN(&standby)}
}

// decideBoot is the role decision of design 2.10.8, taken before the node opens. A server that never
// joined a cluster gets the leader's role without a look at anything.
func decideBoot(ctx context.Context, cfg *config.Config, o Options, log *slog.Logger) (cluster.BootDecision, error) {
	return cluster.DecideBoot(ctx, cluster.BootEnv{
		Cfg: cfg, ConfigPath: o.ConfigPath, DSNs: RegistryDSNs(cfg), Wait: registryWait,
		Marker: &lazyMarker{cfg: cfg}, Resolver: awsResolver(cfg, nil), Log: log.With("component", "cluster"),
	})
}

// lazyMarker reads the leader marker from the backup store, opening the store on first use: the
// decision for a server with no cluster never gets as far as asking.
type lazyMarker struct {
	cfg  *config.Config
	once sync.Once
	m    backup.EpochMarkerStore
	err  error
}

func (l *lazyMarker) store(ctx context.Context) (backup.EpochMarkerStore, error) {
	l.once.Do(func() {
		st, err := backup.OpenStore(ctx, l.cfg.Backup)
		if err != nil {
			l.err = err
			return
		}
		svc, err := backup.New(backup.Options{Config: l.cfg, Store: st})
		if err != nil {
			l.err = err
			return
		}
		m, ok := any(svc).(backup.EpochMarkerStore)
		if !ok {
			l.err = errors.New("the backup service keeps no leader marker")
			return
		}
		l.m = m
	})
	return l.m, l.err
}

func (l *lazyMarker) ReadLeaderMarker(ctx context.Context) (*backup.LeaderMarker, error) {
	m, err := l.store(ctx)
	if err != nil {
		return nil, err
	}
	return m.ReadLeaderMarker(ctx)
}

func (l *lazyMarker) WriteLeaderMarker(ctx context.Context, mk backup.LeaderMarker) error {
	m, err := l.store(ctx)
	if err != nil {
		return err
	}
	return m.WriteLeaderMarker(ctx, mk)
}

// awsResolver returns the resolver that asks EC2 for a peer's address, on a server that records an AWS
// stack, and nil elsewhere.
func awsResolver(cfg *config.Config, self func() registry.Node) mesh.AddrResolver {
	if cfg.AWS.StackName == "" {
		return nil
	}
	c, err := awsapi.New(awsapi.Config{})
	if err != nil {
		return nil
	}
	return &cluster.AWSResolver{EC2: c.EC2, Self: self}
}

// AWSIdentity reads the instance id, zone, region and Elastic IP allocation of the instance this
// daemon runs on from the metadata service. Nil when the server is not recorded as an AWS stack or
// the metadata service does not answer.
func AWSIdentity(ctx context.Context, cfg *config.Config) *registry.NodeAWS {
	if cfg.AWS.StackName == "" {
		return nil
	}
	c, err := awsapi.New(awsapi.Config{})
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	id, err := c.IMDS.InstanceID(ctx)
	if err != nil {
		return nil
	}
	n := &registry.NodeAWS{InstanceID: id}
	n.Zone, _ = c.IMDS.AvailabilityZone(ctx)
	n.Region, _ = c.IMDS.Region(ctx)
	if tags, err := c.IMDS.Tags(ctx); err == nil {
		n.AllocationID = tags["supavise:eip"]
	}
	return n
}

// openFollower makes lo open the registry the way a standby's daemon must: read-only, with no
// Migrate and no advisory locks (registry.OpenReadOnly). lifecycle.OpenOptions has no such option
// yet, and a follower cannot run until it has: the node role is decided here and the registry
// handle is the engine's to open. The workstream that adds the option (replica data plane,
// lifecycle) sets it in this function; until then a follower stops with this error rather than
// opening a standby with a registry that tries to migrate it.
func openFollower(lo *lifecycle.OpenOptions, boot cluster.BootDecision) error {
	_ = lo
	return notimpl.For("opening the registry read-only on a follower (lifecycle.OpenOptions)")
}

// serveFenced is the daemon of a node that was replaced as leader. It starts no primary: it records
// that it is fenced (with its peers' addresses, which `node rejoin` needs when the local database is
// stopped), stops whatever the failover left running, raises the critical alert, and answers every
// request on the HTTP port with 503 and the reason. It ends, and systemd starts it again, when the
// record is gone, which is what `supavise node rejoin` does last.
func serveFenced(ctx context.Context, cfg *config.Config, o Options, log *slog.Logger, boot cluster.BootDecision) error {
	log = log.With("component", "cluster")
	rec, err := cluster.ReadFenced(cfg)
	if err != nil {
		return err
	}
	if rec != nil && rec.Removed {
		log.Warn("this node was removed from the cluster and stays down until it joins one again", "reason", rec.Reason)
	} else {
		log.Error("this node is fenced and starts no primary", "epoch", boot.Epoch, "leader", boot.Leader, "reason", boot.Reason)
		notifier := alerts.New(cfg, alerts.Options{Log: log.With("component", "alerts")})
		alerts.SetDefault(notifier)
		_ = alerts.Notify(ctx, alerts.Event{
			Kind: alerts.KindFenced, Severity: alerts.SeverityCritical, Title: "This node is fenced",
			Detail: fmt.Sprintf("%s. It starts no primary and answers 503 until it is rebuilt: run `sudo supavise node rejoin` on it.", boot.Reason),
		})
	}
	if rec == nil {
		r := cluster.FencedRecord{Epoch: boot.Epoch, Leader: boot.Leader, Reason: boot.Reason, At: time.Now().UTC(), Peers: peersFromLocalRegistry(ctx, boot, log)}
		if err := cluster.WriteFenced(cfg, r); err != nil {
			return fmt.Errorf("serve: recording that the node is fenced: %w", err)
		}
		rec = &r
	}
	if sup, err := units.New(cfg, log); err != nil {
		log.Warn("fenced: the units cannot be reached to stop them", "error", err)
	} else {
		stopped, err := cluster.FenceLocal(ctx, cfg, sup, log)
		if err != nil {
			log.Error("fenced: not everything stopped", "error", err)
		}
		if c, ok := sup.(interface{ Close() }); ok {
			c.Close()
		}
		log.Info("fenced: the clusters were stopped", "projects", stopped)
	}

	state := "fenced"
	if rec.Removed {
		state = "down"
	}
	srv := &http.Server{Addr: cfg.Listen.HTTP, ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `{"message":"This Supavise node is %s: %s"}`+"\n", state, rec.Reason)
	})}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("fenced: the 503 answer is not served", "addr", cfg.Listen.HTTP, "error", err)
		}
	}()
	defer srv.Close()
	tick := time.NewTicker(fencedPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if r, err := cluster.ReadFenced(cfg); err == nil && r == nil {
				return fmt.Errorf("%w: the node rejoined the cluster", ErrRoleChanged)
			}
		}
	}
}

// peersFromLocalRegistry reads the other nodes' peer addresses from the local database, while it
// still runs, for the fenced record. Best effort.
func peersFromLocalRegistry(ctx context.Context, boot cluster.BootDecision, log *slog.Logger) map[string]string {
	if boot.DSN == "" {
		return nil
	}
	reg, err := registry.OpenReadOnly(ctx, boot.DSN)
	if err != nil {
		log.Warn("fenced: the local registry cannot be read", "error", err)
		return nil
	}
	defer reg.Close()
	nodes, err := reg.ListNodes(ctx)
	if err != nil {
		return nil
	}
	return cluster.PeersOf(nodes, boot.SelfID)
}

// wireMesh starts the peer server on [node] peer_listen, the sessions to the other nodes and the
// forwarder reconciler (design 2.5), and provides mesh.Mesh and cluster.Membership. It also
// registers the peer API endpoints of the mesh itself (ping, join, certs, config, report intake) with
// mesh.Handle. On a node with no cluster it does nothing but wait for a cluster identity to appear.
func wireMesh(ctx context.Context, w *Wire) error {
	mesh.ResetDefaultMux()
	boot, _ := Get[cluster.BootDecision](w)
	dir := config.ClusterDir(w.Options.ConfigPath)
	if !boot.Joined {
		// `supavise node token` gives the founding server its certificate; the daemon restarts to
		// become the leader of a cluster. Until then nothing changes.
		w.Go("cluster identity", func(ctx context.Context) error {
			tick := time.NewTicker(identityPoll)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-tick.C:
					if cluster.Joined(dir) {
						return fmt.Errorf("%w: this server was given a cluster identity", ErrRoleChanged)
					}
				}
			}
		})
		return nil
	}
	return startCluster(ctx, w, boot, dir)
}

// startCluster builds the membership, the mesh and the leader's authority for a node that belongs to
// a cluster, and registers the workers.
func startCluster(ctx context.Context, w *Wire, boot cluster.BootDecision, dir string) error {
	cfg, log := w.Cfg, w.Log.With("component", "cluster")
	store, err := cluster.OpenStore(dir)
	if err != nil {
		return err
	}
	der, ok := w.Node.Secrets.(cluster.Deriver)
	if !ok {
		return errors.New("the node's secrets cannot derive the cluster CA")
	}
	ca, err := cluster.NewCA(der)
	if err != nil {
		return err
	}
	selfID := store.Creds().NodeID
	reg := w.Node.Registry
	dsns := RegistryDSNs(cfg)
	if boot.DSN != "" {
		dsns = append([]string{boot.DSN}, dsns...)
	}
	live := cluster.NewLive(cluster.LiveOptions{
		Cfg: cfg, Reg: reg, SelfID: selfID, Boot: boot, Log: log,
		InRecovery: func(ctx context.Context) (bool, error) {
			var last error
			for _, dsn := range dsns {
				rec, err := cluster.InRecovery(ctx, dsn)
				if err == nil {
					return rec, nil
				}
				last = err
			}
			return false, last
		},
		OnFenced: func(rec cluster.FencedRecord) {
			_ = alerts.Notify(context.Background(), alerts.Event{
				Kind: alerts.KindFenced, Severity: alerts.SeverityCritical, Title: "This node is fenced",
				Detail: rec.Reason + ". It stops its clusters and restarts without starting a primary; run `sudo supavise node rejoin` on it once the cause is understood.",
			})
			go func() {
				if _, err := cluster.FenceLocal(context.WithoutCancel(ctx), cfg, w.Node.Supervisor, log); err != nil {
					log.Error("fence: not everything stopped", "error", err)
				}
			}()
		},
	})
	// A node that boots as the leader after a promotion records that it leads (design 2.10.4 step 6);
	// on the node that has led all along this changes nothing.
	if boot.Role == cluster.RoleLeader {
		planned, err := cluster.AssumeLeadership(ctx, reg, selfID, boot.Epoch, time.Now())
		if err != nil {
			return fmt.Errorf("recording that node %s leads at epoch %d: %w", selfID, boot.Epoch, err)
		}
		if planned {
			log.Info("this node took the leadership in a planned switchover; the old leader stays active to be demoted", "epoch", boot.Epoch)
		}
	}
	live.Refresh(ctx)
	Provide[cluster.Membership](w, live)
	Provide(w, live)

	var pins map[string]string
	if v, err := artifacts.ParseVersions(versions.VersionsYAML); err == nil {
		pins = v.Pins()
	}
	auth := &cluster.Authority{
		Reg: reg, CA: ca, Secrets: der, Cfg: cfg, Topology: live, Log: log, Version: w.Options.Version, Pins: pins,
		MasterKey: func() ([]byte, error) { return os.ReadFile(cfg.KeyPath) },
		Changed:   func(ctx context.Context) { live.Refresh(ctx) },
		ReplicationPassword: func(ctx context.Context) (string, error) {
			k, err := w.Node.Engine.Keys(ctx, config.SystemRef)
			if err != nil {
				return "", err
			}
			return k.ReplicationPassword, nil
		},
	}
	if bs, ok := Get[*backup.Service](w); ok {
		if e, ok := any(bs).(backup.BaseBackupEnsurer); ok {
			auth.Ensure = e
		}
	}
	reports := cluster.NewReports()

	schema := &schemaCache{dsns: dsns, log: log}
	skews := &skewState{}
	var monitor *health.Monitor
	if m, ok := Get[*health.Monitor](w); ok {
		monitor = m
	}
	mgr := mesh.New(mesh.Options{
		Topology: live, Creds: store.Creds, Resolver: awsResolver(cfg, live.Self), Log: log,
		Authz: &mesh.Authorizer{Cfg: cfg, Topology: live, Source: reg},
		OnPing: func(node string, p peerapi.Ping, _ time.Duration) {
			live.ObserveEpoch(node, p.Epoch, p.Leader)
			peerSeen(ctx, reg, live, skews, w.Options.Version, node, p, log)
		},
	})
	ping := func() peerapi.Ping {
		leader := ""
		if l, ok := live.Leader(); ok {
			leader = l.ID
		}
		verdict := string(health.Healthy)
		if monitor != nil {
			pctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if r, err := monitor.Report(pctx); err != nil {
				verdict = string(health.Degraded)
			} else {
				verdict = string(r.Verdict)
			}
		}
		return peerapi.Ping{Node: selfID, Epoch: live.Epoch(), Leader: leader, Version: w.Options.Version, Schema: schema.get(), Health: verdict}
	}
	(&cluster.PeerAPI{Authority: auth, Topology: live, Cfg: cfg, Reports: reports, Ping: ping}).Register(mesh.DefaultMux)

	fwd := &mesh.Forwarders{Cfg: cfg, Topology: live, Source: reg, Dialer: mgr, Log: log, Fenced: func() bool { return live.Role() == cluster.RoleFenced }}
	reporter := &cluster.Reporter{
		Self: func() string { return selfID }, Epoch: live.Epoch, IsLeader: live.IsLeader, RPC: mgr, Reports: reports, Log: log,
		Leader: func() (string, bool) { l, ok := live.Leader(); return l.ID, ok },
	}
	renewer := &cluster.Renewer{Store: store, Self: live.Self, IsLeader: live.IsLeader, RPC: mgr, Authority: auth, Log: log,
		Leader: func() (string, bool) { l, ok := live.Leader(); return l.ID, ok }}
	Provide[mesh.Mesh](w, mgr)
	Provide(w, fwd)
	Provide(w, auth)
	Provide(w, reports)
	Provide(w, reporter)

	ln, err := net.Listen("tcp", cfg.PeerListen())
	if err != nil {
		return fmt.Errorf("the peer port: %w", err)
	}
	w.OnStop(func() { _ = ln.Close() })
	w.Go("peer server", func(ctx context.Context) error { return mgr.Serve(ctx, ln) })
	w.Go("mesh sessions", mgr.Run)
	w.Go("forwarders", fwd.Run)
	w.Go("membership", live.Run)
	w.Go("reports", reporter.Run)
	w.Go("certificate renewal", renewer.Run)
	w.Go("leader upkeep", func(ctx context.Context) error {
		leaderUpkeep(ctx, w, auth, live, log)
		return nil
	})
	w.Go("cluster status", func(ctx context.Context) error {
		statusWriter(ctx, cfg, live, mgr, reports, log)
		return nil
	})
	w.Go("retirement", func(ctx context.Context) error {
		return retireWhenRemoved(ctx, w, live, log)
	})
	w.Go("role watch", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return nil
		case <-live.Changed():
			return fmt.Errorf("%w: %s", ErrRoleChanged, live.Why())
		}
	})
	return nil
}

// retireWhenRemoved watches this node's own row, which the registry copy it follows keeps current:
// `supavise node rm` sets it left, and the node then retires itself (cluster.Retire) and stops, to
// restart as a node that is down until it joins a cluster again. It needs the row to read left on
// two polls in a row. The leader never retires: the leader's row is not removed while it leads.
func retireWhenRemoved(ctx context.Context, w *Wire, live *cluster.Live, log *slog.Logger) error {
	seen := 0
	tick := time.NewTicker(retirePoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		if live.IsLeader() || live.Self().State != registry.NodeLeft {
			seen = 0
			continue
		}
		if seen++; seen < 2 {
			continue
		}
		log.Warn("this node was removed from the cluster; stopping what runs and setting its data aside")
		moved, err := cluster.Retire(context.WithoutCancel(ctx), w.Cfg, w.Options.ConfigPath, func(ctx context.Context) error {
			_, err := cluster.FenceLocal(ctx, w.Cfg, w.Node.Supervisor, log)
			return err
		}, time.Now())
		if err != nil {
			return fmt.Errorf("retiring the node: %w", err)
		}
		log.Info("the node retired", "data_set_aside", moved)
		return fmt.Errorf("%w: the node was removed from the cluster", ErrRoleChanged)
	}
}

// skewState remembers which peers are outside the version window, so that the alert is raised and
// resolved when that changes and not on every ping.
type skewState struct {
	mu   sync.Mutex
	skew map[string]bool
}

func (s *skewState) changed(node string, skewed bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.skew == nil {
		s.skew = map[string]bool{}
	}
	if s.skew[node] == skewed {
		return false
	}
	s.skew[node] = skewed
	return true
}

// peerSeen records what a ping says about a peer. Every node raises node_version_skew while the peer's
// release is outside the window; the leader also keeps the peer's version in its row.
func peerSeen(ctx context.Context, reg registry.Registry, live *cluster.Live, skews *skewState, version, node string, p peerapi.Ping, log *slog.Logger) {
	err := cluster.CheckVersionWindow(version, p.Version, nil, nil)
	if skews.changed(node, err != nil) {
		ev := alerts.Event{Kind: alerts.KindNodeVersionSkew, Title: "A node runs a release outside the window", Key: "version_skew/" + node}
		if err != nil {
			ev.Severity = alerts.SeverityWarning
			ev.Detail = fmt.Sprintf("node %s: %v. Upgrade the node that lags.", node, err)
		} else {
			ev.Resolved = true
		}
		_ = alerts.Notify(ctx, ev)
	}
	if !live.IsLeader() || p.Version == "" {
		return
	}
	if n, err := reg.GetNode(ctx, node); err == nil && n.Version != p.Version {
		n.Version = p.Version
		if err := reg.UpdateNode(ctx, n); err != nil {
			log.Debug("a node's version was not recorded", "node", node, "error", err)
		}
	}
}

// schemaCache holds the registry migrations the node's database has applied, for the ping, and
// raises standby_behind while the database holds migrations this binary lacks.
type schemaCache struct {
	dsns []string
	log  *slog.Logger

	mu     sync.Mutex
	at     time.Time
	text   string
	behind bool
}

func (s *schemaCache) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.at) < time.Minute {
		return s.text
	}
	s.at = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, dsn := range s.dsns {
		applied, err := registry.AppliedMigrations(ctx, dsn)
		if err != nil {
			continue
		}
		s.text = peerapi.SchemaString(applied)
		ahead := peerapi.SchemaAhead(applied, registry.MigrationNames())
		if (len(ahead) > 0) != s.behind {
			s.behind = len(ahead) > 0
			ev := alerts.Event{Kind: alerts.KindStandbyBehind, Title: "The registry is newer than this binary", Key: "standby_behind"}
			if s.behind {
				ev.Severity = alerts.SeverityWarning
				ev.Detail = fmt.Sprintf("The registry has migrations this release lacks (%v). Running instances are left alone; upgrade this node.", ahead)
			} else {
				ev.Resolved = true
			}
			_ = alerts.Notify(ctx, ev)
		}
		break
	}
	return s.text
}

// leaderUpkeep does the leader's periodic membership work: it records the node's own row, retires
// nodes that never finished joining and forgets expired join tokens.
func leaderUpkeep(ctx context.Context, w *Wire, auth *cluster.Authority, live *cluster.Live, log *slog.Logger) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	registered := false
	for {
		if live.IsLeader() {
			if !registered {
				registered = registerSelf(ctx, w, live, log)
			}
			if _, err := auth.ReapJoining(ctx); err != nil {
				log.Debug("membership: the joining nodes were not checked", "error", err)
			}
			_, _ = w.Node.Registry.DeleteExpiredJoinTokens(ctx, time.Now().Add(-24*time.Hour))
		} else {
			registered = false
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// registerSelf makes the leader's row say what is true now: this release, the AWS identity, the
// address peers dial. A follower's row is kept by the leader (the join, the pings).
func registerSelf(ctx context.Context, w *Wire, live *cluster.Live, log *slog.Logger) bool {
	n := live.Self()
	if n.ID == "" {
		return false
	}
	n.Version = w.Options.Version
	if a := w.Cfg.PeerAddr(); a != "" {
		n.PeerAddr = a
	}
	if n.PublicHost == "" {
		n.PublicHost = w.Cfg.PublicIP
	}
	if id := AWSIdentity(ctx, w.Cfg); id != nil {
		n.Provider = registry.NodeProvider{AWS: id}
	}
	if err := w.Node.Registry.UpdateNode(ctx, &n); err != nil {
		log.Warn("membership: this node's row was not updated", "error", err)
		return false
	}
	return true
}

// statusWriter writes the daemon's live view of the cluster to the state directory every ten
// seconds, for `supavise status` and `supavise node ls`.
func statusWriter(ctx context.Context, cfg *config.Config, live *cluster.Live, mgr *mesh.Manager, reports *cluster.Reports, log *slog.Logger) {
	downSince := map[string]time.Time{}
	raised := map[string]bool{}
	// unreachable raises node_unreachable for an active peer this node has had no session with for
	// a minute, and resolves it when the session is back.
	unreachable := func(n registry.Node, up bool) {
		if up || n.State != registry.NodeActive {
			delete(downSince, n.ID)
			if raised[n.ID] {
				raised[n.ID] = false
				_ = alerts.Notify(ctx, alerts.Event{Kind: alerts.KindNodeUnreachable, Title: "A node does not answer the mesh", Key: "node_unreachable/" + n.ID, Resolved: true})
			}
			return
		}
		since, ok := downSince[n.ID]
		if !ok {
			downSince[n.ID] = time.Now()
			return
		}
		if time.Since(since) >= unreachableAfter && !raised[n.ID] {
			raised[n.ID] = true
			_ = alerts.Notify(ctx, alerts.Event{Kind: alerts.KindNodeUnreachable, Severity: alerts.SeverityWarning, Title: "A node does not answer the mesh",
				Detail: fmt.Sprintf("node %s (%s) has had no session with this node since %s.", n.ID, n.Name, since.Format(time.RFC3339)), Key: "node_unreachable/" + n.ID})
		}
	}
	write := func() {
		self := live.Self()
		leader := ""
		if l, ok := live.Leader(); ok {
			leader = l.ID
		}
		st := cluster.Status{At: time.Now().UTC(), Node: self.ID, Role: live.Role(), Epoch: live.Epoch(), Leader: leader, Fenced: live.Fenced()}
		for _, n := range live.Nodes() {
			if n.ID == self.ID {
				continue
			}
			p := cluster.PeerStatus{Node: n.ID, Connected: mgr.Connected(n.ID)}
			unreachable(n, p.Connected)
			if rtt, ok := mgr.RTT(n.ID); ok {
				p.RTTMillis = float64(rtt.Microseconds()) / 1000
			}
			if ping, at, ok := mgr.LastPing(n.ID); ok {
				p.LastPing, p.Version, p.Epoch, p.Leader, p.Health = at, ping.Version, ping.Epoch, ping.Leader, ping.Health
			}
			st.Peers = append(st.Peers, p)
		}
		for _, r := range reports.All() {
			for _, in := range r.Instances {
				st.Replicas = append(st.Replicas, cluster.ReplicaStatus{Identifier: in.Identifier, Ref: in.Ref, Node: r.Node, Step: in.Step, Error: in.Error,
					ReceiverStatus: in.ReceiverStatus, LagSeconds: in.LagSeconds, ReportedAt: r.Received})
			}
		}
		if err := cluster.WriteStatus(cfg, st); err != nil {
			log.Debug("cluster status not written", "error", err)
		}
	}
	write()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			write()
		}
	}
}
