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
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/units"
)

// wireFleet puts the shared services in follower mode when this node follows (design 2.6): no
// bin/prepare and no tenant writes on a standby, cold units whose ports are forwarders, the
// Supavisor tenant refresh endpoint, and the reconciliation that starts or stops the services when the
// role changes. On a leader it does nothing.
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
	Provide[fleet.Fleet](w, lz.Fleet())

	rs := &fleetRefreshServer{mem: mem, log: log}
	if p, ok := w.Node.Registry.(interface{ Pool() *pgxpool.Pool }); ok && p.Pool() != nil {
		replay := fleet.PGReplay{Pool: p.Pool()}
		rs.pr = fleet.PeerRefresh{Replay: replay, Refresh: lz.Fleet(), Runs: supavisorRendered(w.Cfg)}
		Provide[fleet.PeerRefresher](w, &peerRefresher{mem: mem, rpc: rpc, lsn: replay.CurrentLSN, log: log})
	}
	registerFleetRefresh(rs)

	if w.Cfg.Supervisor != config.SupervisorSystemd || w.Node.Supervisor == nil {
		return nil // the daemon starts the shared services only under systemd (startFleet)
	}
	mgr, err := fleet.NewManager(fleet.Deps{Cfg: w.Cfg, Log: log, Registry: w.Node.Registry, Secrets: w.Node.Secrets,
		Supervisor: w.Node.Supervisor, Artifacts: w.Node.Artifacts})
	if err != nil {
		return err
	}
	w.Go("fleet role", func(ctx context.Context) error {
		followRole(ctx, mem.Watch(ctx), mgr.Apply, log, roleRetry)
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

// current is the server the registered handler dispatches to: mesh.DefaultMux is process-wide and
// panics on a second registration, and a daemon started twice in one process (tests) must still work.
var (
	currentRefresh atomic.Pointer[fleetRefreshServer]
	refreshOnce    sync.Once
)

func registerFleetRefresh(s *fleetRefreshServer) {
	currentRefresh.Store(s)
	refreshOnce.Do(func() {
		mesh.Handle("POST "+peerapi.PathFleetRefresh, func(w http.ResponseWriter, r *http.Request) {
			if cur := currentRefresh.Load(); cur != nil {
				cur.ServeHTTP(w, r)
				return
			}
			peerError(w, http.StatusServiceUnavailable, "unavailable", "the fleet is not set up on this node")
		})
	})
}

// ServeHTTP answers POST /peer/v1/fleet/refresh/{tenant}: only the leader asks.
func (s *fleetRefreshServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	peer, _ := mesh.PeerFrom(r.Context())
	if leader, ok := s.mem.Leader(); !ok || peer.Node == "" || peer.Node != leader.ID {
		peerError(w, http.StatusForbidden, "not_leader", "only the leader refreshes a tenant")
		return
	}
	var req peerapi.RefreshRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		peerError(w, http.StatusBadRequest, "bad_request", "the body is not a refresh request")
		return
	}
	if s.pr.Replay == nil && req.LSN != "" {
		peerError(w, http.StatusServiceUnavailable, "unavailable", "this node cannot tell how far its standby has replayed")
		return
	}
	done, err := s.pr.Do(r.Context(), r.PathValue("tenant"), req.LSN)
	switch {
	case errors.Is(err, fleet.ErrInvalid):
		peerError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, fleet.ErrReplayBehind):
		peerError(w, http.StatusServiceUnavailable, "replay_behind", err.Error())
	case err != nil:
		s.log.Warn("tenant refresh failed", "tenant", r.PathValue("tenant"), "from", peer.Node, "error", err)
		peerError(w, http.StatusBadGateway, "refresh_failed", err.Error())
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peerapi.RefreshResult{Refreshed: done})
	}
}

func peerError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(peerapi.Error{Code: code, Message: msg})
}

// peerRefresher is the leader's fleet.PeerRefresher: it sends the refresh to every other active node.
type peerRefresher struct {
	mem cluster.Membership
	rpc mesh.RPC
	// lsn reads the leader's WAL position after its write.
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
	lsn, err := p.lsn(ctx)
	if err != nil {
		return fmt.Errorf("fleet: the leader's WAL position: %w", err)
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

// followRole keeps the shared services in the mode of the node's role. The first snapshot is the
// role the daemon booted in, which startFleet has already put in effect; every change after it is
// applied. A failed apply leaves the services half in step, so it is tried again after retry,
// whatever role is wanted by then, until one works.
func followRole(ctx context.Context, snaps <-chan cluster.Snapshot, apply func(context.Context, fleet.Mode) error, log *slog.Logger, retry time.Duration) {
	var applied, want cluster.Role
	var dirty bool
	var again <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-snaps:
			if !ok {
				return
			}
			if applied == "" {
				applied = s.Role
				continue
			}
			want = s.Role
		case <-again:
		}
		again = nil
		if want == "" || (want == applied && !dirty) {
			continue
		}
		mode := modeFor(want)
		log.Info("the node's role changed; putting the shared services in step", "from", applied, "to", want, "mode", mode.String())
		if err := apply(ctx, mode); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("shared services not yet in step with the node's role; trying again", "role", want, "error", err)
			dirty, again = true, time.After(retry)
			continue
		}
		applied, want, dirty = want, "", false
	}
}
