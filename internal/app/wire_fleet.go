package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
	"github.com/supavise/supavise/internal/units"
)

// wireFleet connects the shared services to the cluster work (design 2.6). It serves the credential
// endpoint of Storage on AWS wherever it is set up, and on a node that belongs to a cluster it provides
// the ports the other hooks read (fleet.Fleet, fleet.PeerRefresher, replicas.Pooler, failover.Fleet,
// failover.LocalServices, a server check for the artifacts), puts the services in follower mode when
// this node follows (no bin/prepare and no tenant writes on a standby, cold units whose ports are
// forwarders), serves the Supavisor tenant refresh endpoint, and reconciles the services with the role
// when it changes. A server with no cluster is a leader for good and startFleet is all it needs.
//
// Which profile the services start in at boot is the fleet package's to decide, from the system
// cluster itself (fleet.Deps.Follower), so startFleet needs no help. This hook adds what changes
// while the daemon runs:
//
//   - the peer endpoint POST /peer/v1/fleet/refresh/{tenant}, which makes this node's Supavisor
//     forget a tenant once the node's standby has replayed the leader's change, and
//     fleet.PeerRefresher, which the leader's callers use to send it;
//   - the role reconciliation: a promotion starts the services the follower parked, a demotion
//     stops them, a fence stops all;
//   - the credential endpoint of Storage on AWS ([fleet] storage_s3_role_arn).
func wireFleet(ctx context.Context, w *Wire) error {
	log := w.Log.With("component", "fleet")
	if err := wireStorageCredentials(w, log); err != nil {
		return err
	}
	rpc, clustered := Get[mesh.Mesh](w)
	mem, haveMembership := Get[cluster.Membership](w)
	if !clustered || !haveMembership {
		return nil // no cluster: the node is a leader for good, and startFleet is all it needs
	}
	lz := fleet.NewLazy(fleet.Deps{Cfg: w.Cfg, Log: log})
	lz.Bind(w.Node.Registry, w.Node.Secrets)
	fl := lz.Fleet()
	Provide[fleet.Fleet](w, fl)

	// The leader tells the other nodes that run Supavisor to drop a tenant once their standby has
	// replayed the change (PeerRefresh is the receiving half). The WAL position to wait for comes from
	// the registry's pool; a registry without one (an in-memory one) has no WAL, and the peers then
	// refresh at once.
	rs := &fleetRefreshServer{mem: mem, log: log, pr: fleet.PeerRefresh{Refresh: fl, Runs: supavisorRendered(w.Cfg)}}
	peers := &peerRefresher{mem: mem, rpc: rpc, log: log}
	if p, ok := w.Node.Registry.(interface{ Pool() *pgxpool.Pool }); ok && p.Pool() != nil {
		replay := fleet.PGReplay{Pool: p.Pool()}
		rs.pr.Replay, peers.lsn = replay, replay.CurrentLSN
	}
	Provide[fleet.PeerRefresher](w, peers)
	registerFleetRefresh(rs)

	// What the other packages ask of the shared services, as the ports they declare: the pooler tenant
	// of a replica (replicas.Pooler) and the tenants of a project that moves (failover.Fleet).
	deps := fleet.Deps{Cfg: w.Cfg, Log: log, Registry: w.Node.Registry, Secrets: w.Node.Secrets}
	Provide[replicas.Pooler](w, &replicaPooler{fleet: fl, deps: deps, peers: peers, log: log})
	Provide[failover.Fleet](w, &projectTenants{fleet: fl, deps: deps, peers: peers, log: log})

	if w.Cfg.Supervisor != config.SupervisorSystemd || w.Node.Supervisor == nil {
		// The daemon starts the shared services only under systemd (startFleet), so a move has none to
		// stop or start.
		w.Off("failover.LocalServices", "the shared services run under systemd; this node's supervisor is "+w.Cfg.Supervisor)
		return nil
	}
	mgr, err := fleet.NewManager(fleet.Deps{Cfg: w.Cfg, Log: log, Registry: w.Node.Registry, Secrets: w.Node.Secrets,
		Supervisor: w.Node.Supervisor, Artifacts: w.Node.Artifacts})
	if err != nil {
		return err
	}
	Provide[failover.LocalServices](w, &localServices{mgr: mgr, sup: w.Node.Supervisor})
	// Studio is optional (a node without its artifact runs the other services and says so in its status),
	// so a server move does not wait for it: the preflight renders the services a leader needs without it.
	checkMgr, err := fleet.NewManager(fleet.Deps{Cfg: w.Cfg, Log: log, Registry: w.Node.Registry, Secrets: w.Node.Secrets,
		Supervisor: w.Node.Supervisor, Artifacts: w.Node.Artifacts, Skip: []string{config.SvcStudio}})
	if err != nil {
		return err
	}
	w.AddServerCheck(artifactsCheck(checkMgr, func() string { return mem.Self().ID }))

	booted := cluster.RoleLeader
	if b, ok := Get[cluster.BootDecision](w); ok && b.Role != "" {
		booted = b.Role
	}
	w.Go("fleet role", func(ctx context.Context) error {
		apply := func(ctx context.Context, mode fleet.Mode) error {
			// startFleet may still be starting the services the other way; a newer role cancels this wait.
			if err := w.fleetMu.Lock(ctx); err != nil {
				return err
			}
			defer w.fleetMu.Unlock()
			return mgr.Apply(ctx, mode)
		}
		followRole(ctx, mem.Watch(ctx), booted, apply, log, roleRetry)
		return nil
	})
	return nil
}

// ---- Storage credentials ------------------------------------------------------------------

// newAWS builds the AWS client the credential endpoint calls STS with; tests replace it.
var newAWS = func() (*awsapi.Client, error) { return awsapi.New(awsapi.Config{}) }

// wireStorageCredentials serves supavise-storage's credentials when the node is set up to assume
// a role for it. The listener is bound here, before the daemon renders Storage's unit, so the
// first request finds it. A node that cannot bind or build the endpoint still serves its
// projects; Storage on S3 then fails its requests, which the log says.
func wireStorageCredentials(w *Wire, log *slog.Logger) error {
	if w.Cfg.Fleet.StorageS3RoleARN == "" {
		return nil
	}
	c, err := newAWS()
	if err != nil {
		log.Error("storage credentials: no AWS client; supavise-storage cannot reach its bucket", "error", err)
		return nil
	}
	sc, err := fleet.NewStorageCredentials(fleet.CredentialsOptions{Cfg: w.Cfg, Log: log, AWS: c})
	if err != nil {
		log.Error("storage credentials: the endpoint is not available; supavise-storage cannot reach its bucket", "error", err)
		return nil
	}
	ln, err := fleet.ListenStorageCredentials(w.Cfg)
	if err != nil {
		log.Error("storage credentials: cannot listen; supavise-storage cannot reach its bucket (set [fleet] storage_credentials_port)", "error", err)
		return nil
	}
	log.Info("storage credentials: serving the role to supavise-storage", "role", w.Cfg.Fleet.StorageS3RoleARN, "address", ln.Addr().String())
	w.Go("storage credentials", func(ctx context.Context) error { return sc.Serve(ctx, ln) })
	return nil
}

// ---- the refresh endpoint -----------------------------------------------------------------

// supavisorRendered reports whether this node rendered a Supavisor unit, which is what running
// Supavisor means here: a development node without the fleet has none.
func supavisorRendered(cfg *config.Config) func() bool {
	return func() bool {
		_, err := os.Stat(units.FilesFor(cfg, units.Spec{Service: config.SvcSupavisor}).Run)
		return err == nil
	}
}

type fleetRefreshServer struct {
	mem cluster.Membership
	pr  fleet.PeerRefresh
	log *slog.Logger
}

// registerFleetRefresh serves the refresh endpoint on the process-wide peer mux. wireMesh resets the mux
// when a daemon starts, so a process that starts the daemon twice (tests) registers it once each time.
func registerFleetRefresh(s *fleetRefreshServer) {
	mesh.Handle("POST "+peerapi.PathFleetRefresh, s.ServeHTTP)
}

// ServeHTTP answers POST /peer/v1/fleet/refresh/{tenant}: only the leader asks.
func (s *fleetRefreshServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	peer, _ := mesh.PeerFrom(r.Context())
	if leader, ok := s.mem.Leader(); !ok || peer.Node == "" || peer.Node != leader.ID {
		mesh.RespondError(w, http.StatusForbidden, "not_leader", "only the leader refreshes a tenant")
		return
	}
	var req peerapi.RefreshRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		mesh.RespondError(w, http.StatusBadRequest, "bad_request", "the body is not a refresh request")
		return
	}
	if s.pr.Replay == nil && req.LSN != "" {
		mesh.RespondError(w, http.StatusServiceUnavailable, "unavailable", "this node cannot tell how far its standby has replayed")
		return
	}
	done, err := s.pr.Do(r.Context(), r.PathValue("tenant"), req.LSN)
	switch {
	case errors.Is(err, fleet.ErrInvalid):
		mesh.RespondError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, fleet.ErrReplayBehind):
		mesh.RespondError(w, http.StatusServiceUnavailable, "replay_behind", err.Error())
	case err != nil:
		s.log.Warn("tenant refresh failed", "tenant", r.PathValue("tenant"), "from", peer.Node, "error", err)
		mesh.RespondError(w, http.StatusBadGateway, "refresh_failed", err.Error())
	default:
		mesh.RespondJSON(w, http.StatusOK, peerapi.RefreshResult{Refreshed: done})
	}
}

// peerRefresher is the leader's fleet.PeerRefresher: it sends the refresh to every other active node.
type peerRefresher struct {
	mem cluster.Membership
	rpc mesh.RPC
	// lsn reads the leader's WAL position after its write; nil sends no position, and the peers refresh
	// at once.
	lsn func(ctx context.Context) (string, error)
	log *slog.Logger
}

// peerRefreshTimeout bounds one node's answer, replay wait included (10 s).
const peerRefreshTimeout = 20 * time.Second

func (p *peerRefresher) RefreshPeers(ctx context.Context, tenant string) error {
	self := p.mem.Self().ID
	var peers []string
	for _, n := range p.mem.Nodes() {
		if n.ID != self && n.State == registry.NodeActive {
			peers = append(peers, n.ID)
		}
	}
	if len(peers) == 0 {
		return nil
	}
	var lsn string
	if p.lsn != nil {
		var err error
		if lsn, err = p.lsn(ctx); err != nil {
			return fmt.Errorf("fleet: the leader's WAL position: %w", err)
		}
	}
	errs := make([]error, len(peers))
	var wg sync.WaitGroup
	for i, id := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, peerRefreshTimeout)
			defer cancel()
			var res peerapi.RefreshResult
			if err := p.rpc.Call(cctx, id, http.MethodPost, peerapi.FleetRefreshPath(tenant), peerapi.RefreshRequest{LSN: lsn}, &res); err != nil {
				errs[i] = fmt.Errorf("node %s: %w", id, err)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// ---- role changes -------------------------------------------------------------------------

// roleRetry is how long followRole waits before it applies a role again after a failure: a
// forwarder that still holds a service's port, or a unit that needs a moment, usually heals.
var roleRetry = 5 * time.Second

func modeFor(r cluster.Role) fleet.Mode {
	switch r {
	case cluster.RoleFollower:
		return fleet.ModeFollower
	case cluster.RoleFenced:
		return fleet.ModeStopped
	}
	return fleet.ModeLeader
}

// followRole keeps the shared services in the mode of the node's role. It is an early, best-effort
// reconciliation: the daemon restarts on every role change (Live.Changed ends Serve with ErrRoleChanged)
// and startFleet puts the units in step at that start, so what followRole does makes a fence take effect
// at once and leaves the restart to make the result final. booted is the role the daemon
// started in, which startFleet has already put in effect (empty: the first snapshot says it); a
// snapshot of any other role, the first one included, is applied. An apply that fails leaves the
// services half in step, so it is tried again after retry, whatever role is wanted by then, until one
// works; until then a snapshot of the same role starts nothing, and one of another role is applied at
// once. An apply that is still running when a snapshot of another role arrives is cancelled, so that
// a fence does not wait behind a promotion's start (Realtime and Supavisor run migrations before they
// answer); the services it left half started are put in step by the next apply.
func followRole(ctx context.Context, snaps <-chan cluster.Snapshot, booted cluster.Role, apply func(context.Context, fleet.Mode) error, log *slog.Logger, retry time.Duration) {
	applied, want := booted, cluster.Role("")
	var dirty bool
	// waiting is the role whose apply failed and is held back until again fires; empty: nothing is held.
	var waiting cluster.Role
	var again <-chan time.Time
	type outcome struct {
		role cluster.Role
		err  error
	}
	var running struct {
		role   cluster.Role
		cancel context.CancelFunc
	}
	done := make(chan outcome, 1)
	defer func() {
		if running.cancel != nil {
			running.cancel()
			<-done
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-snaps:
			if !ok {
				return
			}
			if applied == "" {
				applied = s.Role // the role the daemon booted in
				continue
			}
			want = s.Role
			if running.cancel != nil && s.Role != running.role {
				running.cancel()
			}
		case o := <-done:
			running.cancel()
			running.cancel = nil
			switch {
			case ctx.Err() != nil:
				return
			case errors.Is(o.err, context.Canceled):
				dirty = true // cancelled for a newer role: what it started is put in step by the next apply
			case o.err != nil:
				log.Error("shared services not yet in step with the node's role; trying again", "role", o.role, "error", o.err)
				dirty, again = true, time.After(retry)
				if want == o.role { // a newer role is not held back by the failure of an older one
					waiting = o.role
				}
			default:
				applied, dirty = o.role, false
				if want == applied {
					want = ""
				}
			}
		case <-again:
			again, waiting = nil, ""
		}
		if running.cancel != nil || want == "" || (want == applied && !dirty) || (waiting != "" && waiting == want) {
			continue
		}
		waiting = ""
		role, mode := want, modeFor(want)
		log.Info("the node's role changed; putting the shared services in step", "from", applied, "to", role, "mode", mode.String())
		actx, cancel := context.WithCancel(ctx)
		running.role, running.cancel = role, cancel
		go func() { done <- outcome{role, apply(actx, mode)} }()
	}
}
