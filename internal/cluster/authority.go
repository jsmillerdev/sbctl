package cluster

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// Error is a refusal of the leader's membership endpoints: an HTTP status, a code the caller can
// act on and a sentence. The handlers write it as a peerapi.Error.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func fail(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

// JoiningTimeout is how long a node may stay joining before the leader removes it and revokes
// its certificate.
const JoiningTimeout = time.Hour

// Authority is the leader's side of membership: it creates join tokens, admits a joiner that proves
// it holds one, confirms it, renews certificates, lets a fenced node back in and retires
// nodes that never finished joining. Only the leader may use it; every method fails on another node.
type Authority struct {
	Reg      registry.Registry
	CA       *CA
	Secrets  Deriver
	Cfg      *config.Config
	Topology mesh.Topology
	Log      *slog.Logger
	// MasterKey returns the content of the master key file (hex text), which a joiner that has no
	// key receives.
	MasterKey func() ([]byte, error)
	// Version and Pins are this release's version and artifact pins, for the version window.
	Version string
	Pins    map[string]string
	// Ensure, when set, makes sure the system project has a base backup younger than the replica
	// bootstrap age and names it in the join answer. Without it the joiner seeds from the newest one.
	Ensure backup.BaseBackupEnsurer
	// Changed, when set, is called after the authority changed a node's row (a joiner admitted, a
	// join confirmed, a fenced node taken back), before the answer goes out, so that the node's own
	// view of the cluster, which peers admit certificates by, holds the change when the peer comes
	// back a moment later.
	Changed func(ctx context.Context)
	Now     func() time.Time

	mu     sync.Mutex
	nonces map[string]time.Time
}

func (a *Authority) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Authority) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

func (a *Authority) changed(ctx context.Context) {
	if a.Changed != nil {
		a.Changed(ctx)
	}
}

func (a *Authority) needLeader() error {
	if !a.Topology.IsLeader() {
		return fail(http.StatusServiceUnavailable, "not_leader", "this node is not the leader")
	}
	return nil
}

// TokenOptions are the flags of `supavise node token`.
type TokenOptions struct {
	// Name restricts the token to one node name; Region is the joiner's default region.
	Name, Region string
	// TTL is how long the token works.
	TTL time.Duration
}

// Token limits.
const (
	MinTokenTTL = time.Minute
	MaxTokenTTL = 7 * 24 * time.Hour
)

// IssueToken creates a one-time join token and returns it in the form `node join` reads. It
// refuses on a node that is not the leader, with a file:// backup backend (a second server could not
// read its first copy), with no address for the joiner to dial and for a name that is taken or invalid.
// The first token names the cluster after the domain.
func (a *Authority) IssueToken(ctx context.Context, o TokenOptions) (string, error) {
	if err := a.needLeader(); err != nil {
		return "", err
	}
	if a.Cfg.FileBackup() {
		return "", errors.New("joining needs S3-compatible backups: [backup] backend is a file:// path, which a second server cannot read")
	}
	addr := a.Cfg.PeerAddr()
	if addr == "" {
		return "", errors.New("the joiner needs an address to dial: set [node] peer_address (host:port) or public_ip in config.toml")
	}
	if o.TTL == 0 {
		o.TTL = time.Hour
	}
	if o.TTL < MinTokenTTL || o.TTL > MaxTokenTTL {
		return "", fmt.Errorf("--ttl must be between %s and %s", MinTokenTTL, MaxTokenTTL)
	}
	if o.Name != "" {
		if !registry.ValidNodeName(o.Name) {
			return "", fmt.Errorf("%q is not a node name: lower case letters, digits and hyphens, at most 41 characters", o.Name)
		}
		if _, err := a.Reg.GetNodeByName(ctx, o.Name); err == nil {
			return "", fmt.Errorf("a node called %q is already in the cluster", o.Name)
		} else if !errors.Is(err, registry.ErrNotFound) {
			return "", err
		}
	}
	if o.Region != "" && !config.ValidRegion(o.Region) {
		return "", fmt.Errorf("%q is not a region (%s)", o.Region, strings.Join(config.Regions, ", "))
	}
	cl, err := a.Reg.GetCluster(ctx)
	if err != nil {
		return "", err
	}
	if cl.Name == "" {
		name := a.Cfg.BaseDomain()
		if name == "" {
			name = "supavise"
		}
		if err := a.Reg.SetClusterName(ctx, name); err != nil {
			return "", err
		}
	}
	id := newTokenID()
	secret := tokenSecret(a.Secrets, id)
	exp := a.now().Add(o.TTL)
	if err := a.Reg.CreateJoinToken(ctx, &registry.JoinToken{ID: id, SecretHash: SecretHash(secret), NodeName: o.Name, ExpiresAt: exp}); err != nil {
		return "", err
	}
	_, _ = a.Reg.DeleteExpiredJoinTokens(ctx, a.now().Add(-24*time.Hour))
	return Token{
		V: 1, ID: id, Leader: addr, CAFpr: a.CA.Fingerprint(), Secret: base64.RawURLEncoding.EncodeToString(secret),
		Exp: exp.Unix(), Region: o.Region, Name: o.Name,
	}.Encode(), nil
}

// Challenge starts a join: a nonce the joiner mixes into its proof. A nonce works once and for two
// minutes, and the leader remembers at most 1024 of them.
func (a *Authority) Challenge() peerapi.JoinChallenge {
	nonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	exp := a.now().Add(2 * time.Minute)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.nonces == nil {
		a.nonces = map[string]time.Time{}
	}
	for k, t := range a.nonces {
		if a.now().After(t) {
			delete(a.nonces, k)
		}
	}
	for len(a.nonces) >= 1024 { // the oldest goes: a flood of challenges cannot grow the table
		var oldest string
		var at time.Time
		for k, t := range a.nonces {
			if oldest == "" || t.Before(at) {
				oldest, at = k, t
			}
		}
		delete(a.nonces, oldest)
	}
	a.nonces[string(nonce)] = exp
	return peerapi.JoinChallenge{Nonce: nonce, ExpiresAt: exp}
}

// takeNonce removes the nonce and reports whether it was valid.
func (a *Authority) takeNonce(nonce []byte) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.nonces[string(nonce)]
	delete(a.nonces, string(nonce))
	return ok && !a.now().After(exp)
}

// Join admits a joiner that proves it holds a token. It checks, in this order: this node is the
// leader, the nonce is one it gave and has not seen used, the token exists and the keyed proof of
// its secret is right, the token has not been used or expired, the request is well formed, and the
// joiner's version is within the window. Then it creates the node (joining), signs its certificate,
// creates the system replica row and uses the token. A joiner that fails any check is sent nothing
// and the token stays usable, except when the token itself is the problem.
func (a *Authority) Join(ctx context.Context, req peerapi.JoinRequest) (*peerapi.JoinResponse, error) {
	if err := a.needLeader(); err != nil {
		return nil, err
	}
	if !a.takeNonce(req.Nonce) {
		return nil, fail(http.StatusForbidden, "bad_nonce", "the challenge is unknown, used or expired; ask for a new one")
	}
	tok, err := a.Reg.GetJoinToken(ctx, req.TokenID)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, fail(http.StatusForbidden, "bad_token", "the join token is not valid")
	} else if err != nil {
		return nil, err
	}
	secret := tokenSecret(a.Secrets, req.TokenID)
	if !proofOK(SecretHash(secret), tok.SecretHash) || !proofOK(req.Proof, Proof(secret, req.Nonce, req.CSR, req.Name)) {
		return nil, fail(http.StatusForbidden, "bad_proof", "the join token is not valid")
	}
	now := a.now()
	switch {
	case tok.UsedAt != nil:
		return nil, fail(http.StatusConflict, "token_used", "that join token was used already; create another with `supavise node token`")
	case !now.Before(tok.ExpiresAt):
		return nil, fail(http.StatusGone, "token_expired", "that join token expired; create another with `supavise node token`")
	}
	if err := a.checkJoinRequest(ctx, tok, req); err != nil {
		return nil, err
	}

	node := &registry.Node{
		Name: req.Name, Region: req.Region, PublicHost: req.PublicHost, PeerAddr: req.PeerAddr,
		Provider: req.Provider, Version: req.Version, State: registry.NodeJoining,
	}
	if err := a.Reg.CreateNode(ctx, node); err != nil {
		if errors.Is(err, registry.ErrConflict) {
			return nil, fail(http.StatusConflict, "name_taken", "a node called %q is already in the cluster", req.Name)
		}
		return nil, err
	}
	undo := func() { a.removeJoining(context.WithoutCancel(ctx), node.ID) }
	issued, err := a.CA.IssueCSR(req.CSR, node.ID, now, NodeCertTTL)
	if err != nil {
		undo()
		return nil, fail(http.StatusBadRequest, "bad_csr", "%v", err)
	}
	if err := a.Reg.SetNodeCert(ctx, node.ID, issued.Serial); err != nil {
		undo()
		return nil, err
	}
	sys, err := a.systemBootstrap(ctx, node)
	if err != nil {
		undo()
		return nil, err
	}
	if _, err := a.Reg.UseJoinToken(ctx, req.TokenID, now); err != nil {
		undo() // another joiner used it first, or it expired in the meantime
		if errors.Is(err, registry.ErrTokenExpired) {
			return nil, fail(http.StatusGone, "token_expired", "that join token expired; create another with `supavise node token`")
		}
		return nil, fail(http.StatusConflict, "token_used", "that join token was used already; create another with `supavise node token`")
	}
	a.changed(ctx)
	resp := &peerapi.JoinResponse{NodeID: node.ID, Cert: issued.PEM(), CA: a.CA.PEM(), System: *sys}
	if resp.ClusterConfig, err = a.clusterConfig(); err != nil {
		a.log().Error("join: the cluster settings were not rendered; the node was admitted", "node", node.ID, "error", err)
	}
	if !req.WithoutKey {
		if a.MasterKey == nil {
			return nil, errors.New("cluster: no way to read the master key")
		}
		if resp.MasterKey, err = a.MasterKey(); err != nil {
			return nil, err
		}
	}
	a.log().Info("node joining", "node", node.ID, "name", node.Name, "region", node.Region, "version", node.Version)
	return resp, nil
}

func (a *Authority) clusterConfig() (string, error) {
	b, err := config.MarshalCluster(a.Cfg)
	return string(b), err
}

func (a *Authority) checkJoinRequest(ctx context.Context, tok *registry.JoinToken, req peerapi.JoinRequest) error {
	if !registry.ValidNodeName(req.Name) {
		return fail(http.StatusBadRequest, "bad_name", "%q is not a node name: lower case letters, digits and hyphens, at most 41 characters", req.Name)
	}
	if tok.NodeName != "" && tok.NodeName != req.Name {
		return fail(http.StatusForbidden, "bad_name", "that token admits the node %q, not %q", tok.NodeName, req.Name)
	}
	if _, err := a.Reg.GetNodeByName(ctx, req.Name); err == nil {
		return fail(http.StatusConflict, "name_taken", "a node called %q is already in the cluster", req.Name)
	} else if !errors.Is(err, registry.ErrNotFound) {
		return err
	}
	if req.Region != "" && !config.ValidRegion(req.Region) {
		return fail(http.StatusBadRequest, "bad_region", "%q is not a region", req.Region)
	}
	if req.PeerAddr != "" {
		if _, _, err := net.SplitHostPort(req.PeerAddr); err != nil {
			return fail(http.StatusBadRequest, "bad_address", "the peer address %q is not host:port", req.PeerAddr)
		}
	}
	if len(req.PublicHost) > 253 || len(req.PeerAddr) > 300 {
		return fail(http.StatusBadRequest, "bad_address", "the public host or peer address is too long")
	}
	if err := CheckVersionWindow(a.Version, req.Version, a.Pins, req.ArtifactPins); err != nil {
		return fail(http.StatusConflict, "version_skew", "%v", err)
	}
	if csr, err := x509.ParseCertificateRequest(req.CSR); err != nil || csr.CheckSignature() != nil {
		return fail(http.StatusBadRequest, "bad_csr", "the certificate request is not valid")
	}
	return nil
}

// systemBootstrap creates (or finds) the system replica row of node and names the base backup it
// seeds from.
func (a *Authority) systemBootstrap(ctx context.Context, node *registry.Node) (*peerapi.SystemBootstrap, error) {
	cl, err := a.Reg.GetCluster(ctx)
	if err != nil {
		return nil, err
	}
	rs, err := a.Reg.ListReplicas(ctx, config.SystemRef)
	if err != nil {
		return nil, err
	}
	id := ""
	for _, r := range rs {
		if r.NodeID == node.ID {
			id = r.Identifier
		}
	}
	if id == "" {
		region := node.Region
		if region == "" {
			region = a.Cfg.NodeRegion()
		}
		id = registry.ReplicaIdentifier(config.SystemRef, region, shortID())
		if err := a.Reg.CreateReplica(ctx, &registry.Replica{Identifier: id, Ref: config.SystemRef, NodeID: node.ID, Origin: registry.ReplicaSystem}); err != nil {
			return nil, err
		}
	}
	backupID := ""
	if a.Ensure != nil {
		b, err := a.Ensure.EnsureBase(ctx, config.SystemRef, a.Cfg.Replicas.BootstrapMaxAge())
		if err != nil {
			return nil, fail(http.StatusServiceUnavailable, "no_base_backup", "the base backup of the system cluster could not be made: %v", err)
		}
		backupID = path.Base(strings.TrimRight(b.Location, "/")) // the manifest id ends the backup's location
	}
	return &peerapi.SystemBootstrap{Identifier: id, BackupID: backupID, Leader: cl.Leader, Epoch: cl.Epoch}, nil
}

// shortID is the six character id of a replica identifier.
func shortID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// removeJoining retires a node that never finished joining: its certificate no longer works
// (state left) and its system replica row goes.
func (a *Authority) removeJoining(ctx context.Context, id string) {
	if rs, err := a.Reg.ListReplicasOn(ctx, id); err == nil {
		for _, r := range rs {
			if r.Ref == config.SystemRef {
				_ = a.Reg.DeleteReplica(ctx, r.Identifier)
			}
		}
	}
	// A node that was created and never certified can go; one that holds a certificate is kept as
	// left, so that its id is never reused and its serial stays revoked.
	if n, err := a.Reg.GetNode(ctx, id); err == nil && n.CertSerial == "" {
		if a.Reg.DeleteNode(ctx, id) == nil {
			return
		}
	}
	_ = a.Reg.SetNodeState(ctx, id, registry.NodeLeft)
}

// Confirm makes a joining node active once its system standby streams. The caller is the node that
// the TLS handshake identified; it can confirm only itself.
func (a *Authority) Confirm(ctx context.Context, caller string, c peerapi.JoinConfirm) error {
	if err := a.needLeader(); err != nil {
		return err
	}
	if caller == "" || caller != c.NodeID {
		return fail(http.StatusForbidden, "not_you", "a node confirms only its own join")
	}
	n, err := a.Reg.GetNode(ctx, caller)
	if err != nil {
		return err
	}
	if n.State != registry.NodeJoining {
		return fail(http.StatusConflict, "not_joining", "node %s is %s, not joining", caller, n.State)
	}
	if err := a.Reg.SetNodeState(ctx, caller, registry.NodeActive); err != nil {
		return err
	}
	if rs, err := a.Reg.ListReplicasOn(ctx, caller); err == nil {
		for _, r := range rs {
			if r.Ref == config.SystemRef && r.InitStep != registry.ReplicaStepDone {
				_ = a.Reg.SetReplicaStatus(ctx, r.Identifier, string(registry.StatusActiveHealthy), registry.ReplicaStepDone, "")
			}
		}
	}
	a.changed(ctx)
	a.log().Info("node joined", "node", caller, "replay_lsn", c.ReplayLSN)
	return nil
}

// Renew signs a new certificate for the calling node and records its serial. The node keeps using
// the old one until it sees the new serial in its own copy of the registry.
func (a *Authority) Renew(ctx context.Context, caller string, req peerapi.CertRenewRequest) (*peerapi.CertRenewResponse, error) {
	if err := a.needLeader(); err != nil {
		return nil, err
	}
	n, err := a.Reg.GetNode(ctx, caller)
	if err != nil {
		return nil, err
	}
	if n.State != registry.NodeActive {
		return nil, fail(http.StatusConflict, "not_active", "node %s is %s", caller, n.State)
	}
	issued, err := a.CA.IssueCSR(req.CSR, caller, a.now(), NodeCertTTL)
	if err != nil {
		return nil, fail(http.StatusBadRequest, "bad_csr", "%v", err)
	}
	if err := a.Reg.SetNodeCert(ctx, caller, issued.Serial); err != nil {
		return nil, err
	}
	a.changed(ctx)
	return &peerapi.CertRenewResponse{Cert: issued.PEM(), Serial: issued.Serial, NotAfter: issued.NotAfter}, nil
}

// SelfIssue gives the leader its own certificate for the public key pub and records the serial: a
// leader does not ask itself.
func (a *Authority) SelfIssue(ctx context.Context, nodeID string, pub ed25519.PublicKey) (*Issued, error) {
	if err := a.needLeader(); err != nil {
		return nil, err
	}
	issued, err := a.CA.Issue(pub, nodeID, a.now(), NodeCertTTL)
	if err != nil {
		return nil, err
	}
	if err := a.Reg.SetNodeCert(ctx, nodeID, issued.Serial); err != nil {
		return nil, err
	}
	return issued, nil
}

// Rejoin lets a fenced node back in: its row goes to joining, its system replica row is kept (or
// made), and it is told how to rebuild its system standby. The node keeps its identity and its
// certificate; PathJoinConfirm makes it active when its standby streams.
func (a *Authority) Rejoin(ctx context.Context, caller string, req peerapi.RejoinRequest) (*peerapi.RejoinResponse, error) {
	if err := a.needLeader(); err != nil {
		return nil, err
	}
	n, err := a.Reg.GetNode(ctx, caller)
	if err != nil {
		return nil, err
	}
	if n.State != registry.NodeFenced {
		return nil, fail(http.StatusConflict, "not_fenced", "node %s is %s; only a fenced node rejoins", caller, n.State)
	}
	if err := CheckVersionWindow(a.Version, req.Version, a.Pins, req.ArtifactPins); err != nil {
		return nil, fail(http.StatusConflict, "version_skew", "%v", err)
	}
	sys, err := a.systemBootstrap(ctx, n)
	if err != nil {
		return nil, err
	}
	if err := a.Reg.SetNodeState(ctx, caller, registry.NodeJoining); err != nil {
		return nil, err
	}
	cfgText, err := a.clusterConfig()
	if err != nil {
		return nil, err
	}
	a.changed(ctx)
	a.log().Info("fenced node rejoining", "node", caller)
	return &peerapi.RejoinResponse{System: *sys, ClusterConfig: cfgText}, nil
}

// ReapJoining retires the nodes that have been joining for longer than JoiningTimeout, and
// returns their ids.
func (a *Authority) ReapJoining(ctx context.Context) ([]string, error) {
	if !a.Topology.IsLeader() {
		return nil, nil
	}
	nodes, err := a.Reg.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range nodes {
		if n.State == registry.NodeJoining && a.now().Sub(n.JoinedAt) > JoiningTimeout {
			a.removeJoining(ctx, n.ID)
			a.log().Warn("a node never finished joining and was removed", "node", n.ID, "name", n.Name)
			out = append(out, n.ID)
		}
	}
	if len(out) > 0 {
		a.changed(ctx)
	}
	return out, nil
}
