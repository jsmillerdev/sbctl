package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// PeerAPI is the membership part of the peer API: the endpoints this package serves. The other
// endpoints belong to the workstreams that register them with mesh.Handle.
type PeerAPI struct {
	Authority *Authority
	Topology  mesh.Topology
	Cfg       *config.Config
	Reports   *Reports
	// Ping is what this node answers to a ping.
	Ping func() peerapi.Ping
	Now  func() time.Time
}

// Register adds the endpoints to m. The certificate store (GET /peer/v1/certs) is not among them: the
// proxy owns the store's layout and registers that endpoint itself with mesh.Handle, and a second
// registration of the pattern would panic when the daemon starts.
//
//	GET  /peer/v1/ping            any node
//	GET  /peer/v1/join            a joiner with no certificate: a challenge
//	POST /peer/v1/join            a joiner: the proof, the request; the answer holds its certificate
//	POST /peer/v1/join/confirm    a joining node, once its system standby streams
//	POST /peer/v1/rejoin          a fenced node, to be let back in
//	POST /peer/v1/certs/renew     an active node, for a new certificate
//	GET  /peer/v1/config          a follower, for the cluster-scoped settings (leader only)
//	POST /peer/v1/report          a node, for what it observes (leader only)
func (p *PeerAPI) Register(m *mesh.Mux) {
	m.Handle("GET "+peerapi.PathPing, mesh.PingHandler(p.Ping))
	m.Handle("GET "+peerapi.PathJoin, func(w http.ResponseWriter, r *http.Request) {
		if err := p.Authority.needLeader(); err != nil {
			respond(w, r, err)
			return
		}
		peer, _ := mesh.PeerFrom(r.Context())
		ch, err := p.Authority.ChallengeFrom(peer.Remote)
		if err != nil {
			respond(w, r, err)
			return
		}
		mesh.RespondJSON(w, http.StatusOK, ch)
	})
	m.Handle("POST "+peerapi.PathJoin, func(w http.ResponseWriter, r *http.Request) {
		var req peerapi.JoinRequest
		if !mesh.DecodeBody(w, r, &req) {
			return
		}
		resp, err := p.Authority.Join(r.Context(), req)
		if err != nil {
			respond(w, r, err)
			return
		}
		mesh.RespondJSON(w, http.StatusOK, resp)
	})
	m.Handle("POST "+peerapi.PathJoinConfirm, func(w http.ResponseWriter, r *http.Request) {
		var c peerapi.JoinConfirm
		if !mesh.DecodeBody(w, r, &c) {
			return
		}
		peer, _ := mesh.PeerFrom(r.Context())
		if err := p.Authority.Confirm(r.Context(), peer.Node, c); err != nil {
			respond(w, r, err)
			return
		}
		mesh.RespondJSON(w, http.StatusNoContent, nil)
	})
	m.Handle("POST "+peerapi.PathRejoin, func(w http.ResponseWriter, r *http.Request) {
		var req peerapi.RejoinRequest
		if !mesh.DecodeBody(w, r, &req) {
			return
		}
		peer, _ := mesh.PeerFrom(r.Context())
		resp, err := p.Authority.Rejoin(r.Context(), peer.Node, req)
		if err != nil {
			respond(w, r, err)
			return
		}
		mesh.RespondJSON(w, http.StatusOK, resp)
	})
	m.Handle("POST "+peerapi.PathCertsRenew, func(w http.ResponseWriter, r *http.Request) {
		var req peerapi.CertRenewRequest
		if !mesh.DecodeBody(w, r, &req) {
			return
		}
		peer, _ := mesh.PeerFrom(r.Context())
		resp, err := p.Authority.Renew(r.Context(), peer.Node, req)
		if err != nil {
			respond(w, r, err)
			return
		}
		mesh.RespondJSON(w, http.StatusOK, resp)
	})
	m.Handle("GET "+peerapi.PathConfig, func(w http.ResponseWriter, r *http.Request) {
		if err := p.Authority.needLeader(); err != nil {
			respond(w, r, err)
			return
		}
		b, err := config.MarshalCluster(p.Cfg)
		if err != nil {
			respond(w, r, err)
			return
		}
		sum := sha256.Sum256(b)
		mesh.RespondJSON(w, http.StatusOK, peerapi.ClusterConfig{Revision: hex.EncodeToString(sum[:]), TOML: string(b)})
	})
	m.Handle("POST "+peerapi.PathReport, func(w http.ResponseWriter, r *http.Request) {
		if err := p.Authority.needLeader(); err != nil {
			respond(w, r, err)
			return
		}
		var rep peerapi.Report
		if !mesh.DecodeBody(w, r, &rep) {
			return
		}
		peer, _ := mesh.PeerFrom(r.Context())
		if rep.Node != "" && rep.Node != peer.Node {
			mesh.RespondError(w, http.StatusForbidden, "not_you", "a node reports for itself")
			return
		}
		now := time.Now
		if p.Now != nil {
			now = p.Now
		}
		p.Reports.Put(peer.Node, rep, now())
		mesh.RespondJSON(w, http.StatusNoContent, nil)
	})
}

// respond answers a request that failed with err. A failure that is not one of the typed ones is a 500;
// its text goes to a caller that is a node, and to the log only when the caller has no certificate (a
// joiner reaches the join endpoints, where a database error can carry a host or a table name).
func respond(w http.ResponseWriter, r *http.Request, err error) {
	var ce *Error
	switch {
	case errors.As(err, &ce):
		mesh.RespondError(w, ce.Status, ce.Code, ce.Message)
	case errors.Is(err, registry.ErrNotFound):
		mesh.RespondError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, registry.ErrReadOnly):
		mesh.RespondError(w, http.StatusServiceUnavailable, "not_leader", "this node cannot write to the registry")
	default:
		if peer, _ := mesh.PeerFrom(r.Context()); peer.Node == "" {
			slog.Warn("peer api: a request from a caller with no certificate failed", "path", r.URL.Path, "remote", peer.Remote, "error", err)
			mesh.RespondError(w, http.StatusInternalServerError, "", "the leader could not handle the request")
			return
		}
		mesh.RespondError(w, http.StatusInternalServerError, "", err.Error())
	}
}
