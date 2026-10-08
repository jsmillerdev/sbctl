package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// Register adds the endpoints to m:
//
//	GET  /peer/v1/ping            any node
//	GET  /peer/v1/join            a joiner with no certificate: a challenge
//	POST /peer/v1/join            a joiner: the proof, the request; the answer holds its certificate
//	POST /peer/v1/join/confirm    a joining node, once its system standby streams
//	POST /peer/v1/rejoin          a fenced node, to be let back in
//	POST /peer/v1/certs/renew     an active node, for a new certificate
//	GET  /peer/v1/config          a follower, for the cluster-scoped settings (leader only)
//	GET  /peer/v1/certs           a follower, for the certificate store (leader only)
//	POST /peer/v1/report          a node, for what it observes (leader only)
func (p *PeerAPI) Register(m *mesh.Mux) {
	m.Handle("GET "+peerapi.PathPing, mesh.PingHandler(p.Ping))
	m.Handle("GET "+peerapi.PathJoin, func(w http.ResponseWriter, r *http.Request) {
		if err := p.Authority.needLeader(); err != nil {
			respond(w, err)
			return
		}
		mesh.RespondJSON(w, http.StatusOK, p.Authority.Challenge())
	})
	m.Handle("POST "+peerapi.PathJoin, func(w http.ResponseWriter, r *http.Request) {
		var req peerapi.JoinRequest
		if !mesh.DecodeBody(w, r, &req) {
			return
		}
		resp, err := p.Authority.Join(r.Context(), req)
		if err != nil {
			respond(w, err)
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
			respond(w, err)
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
			respond(w, err)
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
			respond(w, err)
			return
		}
		mesh.RespondJSON(w, http.StatusOK, resp)
	})
	m.Handle("GET "+peerapi.PathConfig, func(w http.ResponseWriter, r *http.Request) {
		if err := p.Authority.needLeader(); err != nil {
			respond(w, err)
			return
		}
		b, err := config.MarshalCluster(p.Cfg)
		if err != nil {
			respond(w, err)
			return
		}
		sum := sha256.Sum256(b)
		mesh.RespondJSON(w, http.StatusOK, peerapi.ClusterConfig{Revision: hex.EncodeToString(sum[:]), TOML: string(b)})
	})
	m.Handle("GET "+peerapi.PathCerts, p.certs)
	m.Handle("POST "+peerapi.PathReport, func(w http.ResponseWriter, r *http.Request) {
		if err := p.Authority.needLeader(); err != nil {
			respond(w, err)
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

func respond(w http.ResponseWriter, err error) {
	var ce *Error
	switch {
	case errors.As(err, &ce):
		mesh.RespondError(w, ce.Status, ce.Code, ce.Message)
	case errors.Is(err, registry.ErrNotFound):
		mesh.RespondError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, registry.ErrReadOnly):
		mesh.RespondError(w, http.StatusServiceUnavailable, "not_leader", "this node cannot write to the registry")
	default:
		mesh.RespondError(w, http.StatusInternalServerError, "", err.Error())
	}
}

// maxCertStore bounds the certificate snapshot.
const maxCertStore = 32 << 20

// certs answers GET /peer/v1/certs: every file of the certificate store, with an ETag over their
// names, modes and contents, and 304 for a follower that has the current one. The locks the
// issuer holds while it works are left out.
func (p *PeerAPI) certs(w http.ResponseWriter, r *http.Request) {
	if err := p.Authority.needLeader(); err != nil {
		respond(w, err)
		return
	}
	snap, etag, err := SnapshotCerts(p.Cfg.Paths().Certs())
	if err != nil {
		respond(w, err)
		return
	}
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	mesh.RespondJSON(w, http.StatusOK, snap)
}

// SnapshotCerts reads the certificate store under root and returns it with its ETag. A store that
// does not exist is empty.
func SnapshotCerts(root string) (peerapi.CertSnapshot, string, error) {
	snap := peerapi.CertSnapshot{Files: []peerapi.CertFile{}}
	h := sha256.New()
	total := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == root {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() {
			if d.Name() == "locks" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) { // removed while walking
				return nil
			}
			return err
		}
		if total += len(b); total > maxCertStore {
			return errors.New("cluster: the certificate store is larger than the peer API will send")
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		snap.Files = append(snap.Files, peerapi.CertFile{Path: filepath.ToSlash(rel), Mode: uint32(fi.Mode().Perm()), Data: b})
		return nil
	})
	if err != nil {
		return peerapi.CertSnapshot{}, "", err
	}
	sort.Slice(snap.Files, func(i, j int) bool { return snap.Files[i].Path < snap.Files[j].Path })
	for _, f := range snap.Files {
		h.Write([]byte(f.Path))
		h.Write([]byte{0, byte(f.Mode >> 8), byte(f.Mode)})
		sum := sha256.Sum256(f.Data)
		h.Write(sum[:])
	}
	return snap, `"` + hex.EncodeToString(h.Sum(nil)) + `"`, nil
}
