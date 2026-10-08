package cluster

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// JoinStateFile is the file in the cluster directory that records a join that has the certificate
// but is not confirmed, so that `node join --resume` can continue it. It holds the system cluster's
// replication password, so it is 0600; it is removed when the join is confirmed.
const JoinStateFile = "join.json"

// SeedFunc builds this node's standby of the system cluster from the base backup that b names
// (backup.SeedReplica) and starts it. The primary_conninfo of the standby points at the canonical
// system port on this node (127.0.0.1:5433), where the join serves a forwarder to the leader until
// the daemon takes over. A SeedFunc that finds its work already done returns nil: a resumed join
// calls it again.
type SeedFunc func(ctx context.Context, b peerapi.SystemBootstrap) error

// JoinOptions describe a join.
type JoinOptions struct {
	Cfg        *config.Config
	ConfigPath string
	// Token is the parsed join token. Ignored with Resume.
	Token Token
	// Name, Region, Address, PublicHost and Provider describe this node to the leader: the node
	// name (default Cfg.NodeName()), region (default the token's, then Cfg.NodeRegion()), the
	// host:port peers dial (default Cfg.PeerAddr()), the host clients use for its pooler endpoints
	// and its AWS identity.
	Name, Region, Address, PublicHost string
	Provider                          registry.NodeProvider
	// Version and Pins are this release's version and artifact pins, for the leader's version window.
	Version string
	Pins    map[string]string
	// MasterKey, when not nil, is the cluster's master key as the key file holds it (hex): the
	// leader does not send its own. It must be the key the token's CA pin was made from.
	MasterKey []byte
	// Resume continues a join that has its certificate and is not confirmed.
	Resume bool
	// Seed builds and starts the system standby. Required.
	Seed SeedFunc
	// DSNs are the sockets the system standby answers on, to see it stream.
	DSNs []string
	// StreamTimeout bounds the wait for the standby to stream; zero is 30 minutes.
	StreamTimeout time.Duration
	// Streaming, when set, replaces the look at DSNs: it returns the standby's replay position once it
	// streams (tests).
	Streaming func(ctx context.Context) (lsn string, err error)
	Log       *slog.Logger
	Now       func() time.Time
}

// JoinState is the content of join.json.
type JoinState struct {
	NodeID string                  `json:"node_id"`
	Leader string                  `json:"leader"` // host:port of the leader's peer listener
	System peerapi.SystemBootstrap `json:"system"` // how to seed the standby
	At     time.Time               `json:"at"`
}

// JoinResult is what a finished join tells the caller.
type JoinResult struct {
	NodeID string
	System peerapi.SystemBootstrap
}

func (o *JoinOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *JoinOptions) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// Join joins this server to the cluster a token names, in two phases. The first is the exchange
// with the leader: it connects over TLS and accepts the leader only when the root of its chain
// hashes to the token's pin, proves it holds the token, and receives its certificate, the master key
// (unless it supplied one) and the cluster's settings, all of which it writes to disk before it does
// anything else. The second seeds the standby of the system cluster, forwards the standby's
// connection to the leader until the standby streams, and confirms the join, which makes the node
// active. A join that stopped between the two continues with Resume, which needs no token.
func Join(ctx context.Context, o JoinOptions) (*JoinResult, error) {
	if o.Seed == nil {
		return nil, errors.New("cluster: a join needs a way to seed the system standby")
	}
	dir := config.ClusterDir(o.ConfigPath)
	var st *JoinState
	var err error
	if o.Resume {
		if st, err = ReadJoinState(dir); err != nil {
			return nil, err
		}
	} else {
		if Joined(dir) {
			return nil, errors.New("cluster: this server already joined a cluster; use `supavise node join --resume` to finish a join that stopped, or `supavise node rejoin` for a fenced node")
		}
		if st, err = o.exchange(ctx, dir); err != nil {
			return nil, err
		}
	}
	if err := o.stream(ctx, dir, st); err != nil {
		return nil, fmt.Errorf("%w; run `supavise node join --resume` to continue", err)
	}
	_ = os.Remove(filepath.Join(dir, JoinStateFile))
	// A server that was removed from a cluster and joins one again is no longer down.
	if err := ClearFenced(o.Cfg); err != nil {
		return nil, err
	}
	return &JoinResult{NodeID: st.NodeID, System: st.System}, nil
}

// ReadJoinState reads join.json from the cluster directory.
func ReadJoinState(dir string) (*JoinState, error) {
	b, err := os.ReadFile(filepath.Join(dir, JoinStateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("cluster: there is no join to resume (no " + JoinStateFile + " in " + dir + ")")
	}
	if err != nil {
		return nil, err
	}
	var st JoinState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("cluster: %s: %w", JoinStateFile, err)
	}
	return &st, nil
}

// exchange is phase one.
func (o *JoinOptions) exchange(ctx context.Context, dir string) (*JoinState, error) {
	tok := o.Token
	if !tok.Expires().After(o.now()) {
		return nil, errors.New("cluster: the join token has expired; ask the leader for a new one with `supavise node token`")
	}
	secret, err := base64.RawURLEncoding.DecodeString(tok.Secret)
	if err != nil || len(secret) == 0 {
		return nil, errors.New("cluster: the join token's secret is damaged")
	}
	var keyBody []byte
	if o.MasterKey != nil {
		sec, err := secrets.Load(o.MasterKey)
		if err != nil {
			return nil, err
		}
		ca, err := NewCA(sec)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(ca.Fingerprint(), tok.CAFpr) {
			return nil, errors.New("cluster: that master key is not the one this cluster was made with (the CA it derives is not the one the token pins)")
		}
		keyBody = o.MasterKey
	}
	name := firstNonEmpty(o.Name, tok.Name, o.Cfg.NodeName())
	region := firstNonEmpty(o.Region, tok.Region, o.Cfg.NodeRegion())
	address := firstNonEmpty(o.Address, o.Cfg.PeerAddr())
	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	csr, err := NewCSR(key, name)
	if err != nil {
		return nil, err
	}

	c, err := mesh.DialClient(ctx, tok.Leader, "leader", mesh.PinnedTLS(tok.CAFpr, nil, o.now))
	if err != nil {
		return nil, fmt.Errorf("cluster: cannot reach the leader at %s: %w", tok.Leader, err)
	}
	defer c.Close()
	var ch peerapi.JoinChallenge
	if err := c.Call(ctx, "GET", peerapi.PathJoin, nil, &ch); err != nil {
		return nil, fmt.Errorf("cluster: asking the leader for a challenge: %w", err)
	}
	req := peerapi.JoinRequest{
		TokenID: tok.ID, Nonce: ch.Nonce, Proof: Proof(secret, ch.Nonce, csr, name), CSR: csr,
		Name: name, Region: region, PublicHost: o.PublicHost, PeerAddr: address, Provider: o.Provider,
		Version: o.Version, ArtifactPins: o.Pins, WithoutKey: keyBody != nil,
	}
	var resp peerapi.JoinResponse
	if err := c.Call(ctx, "POST", peerapi.PathJoin, req, &resp); err != nil {
		return nil, fmt.Errorf("cluster: the leader refused the join: %w", err)
	}

	creds, err := o.verifyResponse(tok, key, resp)
	if err != nil {
		return nil, err
	}
	if keyBody == nil {
		keyBody = bytes.TrimSpace(resp.MasterKey)
		if _, err := secrets.Load(keyBody); err != nil {
			return nil, fmt.Errorf("cluster: the master key the leader sent is not valid: %w", err)
		}
	}
	// Everything is checked; now it goes to disk. The certificate is written last of the identity, and
	// the join state after it: a join that stopped anywhere before leaves a server that can join again.
	if err := o.writeKey(keyBody); err != nil {
		return nil, err
	}
	if resp.ClusterConfig != "" {
		p := filepath.Join(config.ConfigDDir(o.ConfigPath), config.ClusterConfigFile)
		if err := writeFile(p, []byte(resp.ClusterConfig), 0o600); err != nil {
			return nil, fmt.Errorf("cluster: writing %s: %w", p, err)
		}
	}
	if err := SaveIdentity(dir, key, creds.Cert.Certificate[0], creds.CA.Raw); err != nil {
		return nil, err
	}
	st := &JoinState{NodeID: resp.NodeID, Leader: tok.Leader, System: resp.System, At: o.now().UTC()}
	if err := writeJSON(filepath.Join(dir, JoinStateFile), st); err != nil {
		return nil, err
	}
	o.log().Info("the leader admitted this node", "node", resp.NodeID, "leader", resp.System.Leader)
	return st, nil
}

// verifyResponse checks what the leader sent before anything is written: the CA is the pinned
// one, the certificate chains to it, is for our key and names the node the leader says.
func (o *JoinOptions) verifyResponse(tok Token, key ed25519.PrivateKey, resp peerapi.JoinResponse) (*mesh.Credentials, error) {
	caDER, err := decodePEM(resp.CA, "CERTIFICATE")
	if err != nil || !strings.EqualFold(Fingerprint(caDER), tok.CAFpr) {
		return nil, errors.New("cluster: the CA in the leader's answer is not the one the token pins")
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	certDER, err := decodePEM(resp.Cert, "CERTIFICATE")
	if err != nil {
		return nil, fmt.Errorf("cluster: the certificate in the leader's answer: %w", err)
	}
	creds, err := mesh.NewCredentials(certDER, key, ca)
	if err != nil {
		return nil, err
	}
	if creds.NodeID != resp.NodeID {
		return nil, fmt.Errorf("cluster: the certificate names node %s and the leader says node %s", creds.NodeID, resp.NodeID)
	}
	pub, ok := creds.Cert.Leaf.PublicKey.(ed25519.PublicKey)
	if !ok || !pub.Equal(key.Public()) {
		return nil, errors.New("cluster: the certificate is not for this node's key")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := creds.Cert.Leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: o.now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return nil, fmt.Errorf("cluster: the certificate does not verify against the pinned CA: %w", err)
	}
	return creds, nil
}

// writeKey writes the master key file unless this server has one. A different one is an error,
// never overwritten: it may be the key of data on this server.
func (o *JoinOptions) writeKey(body []byte) error {
	want := append(bytes.TrimSpace(body), '\n')
	have, err := os.ReadFile(o.Cfg.KeyPath)
	switch {
	case err == nil && bytes.Equal(bytes.TrimSpace(have), bytes.TrimSpace(want)):
		return nil
	case err == nil:
		return fmt.Errorf("cluster: %s holds another key than the cluster's; move it away if this server holds nothing worth keeping, or pass the cluster's key with --master-key-file", o.Cfg.KeyPath)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return writeFile(o.Cfg.KeyPath, want, 0o600)
}

// stream is phase two: seed, forward, wait, confirm.
func (o *JoinOptions) stream(ctx context.Context, dir string, st *JoinState) error {
	creds, err := LoadCredentials(dir)
	if err != nil {
		return err
	}
	if creds.NodeID != st.NodeID {
		return fmt.Errorf("cluster: %s names node %s and %s holds the certificate of %s", JoinStateFile, st.NodeID, dir, creds.NodeID)
	}
	leader := st.System.Leader
	if leader == "" {
		leader = registry.FounderNodeID
	}
	getCreds := func() *mesh.Credentials { return creds }
	// A node cannot check the registry before it has one: the chain, the key usage and the name
	// of the node it dialed are checked, the registry's say is not.
	c, err := mesh.DialClient(ctx, st.Leader, leader, mesh.ClientTLS(getCreds, leader, nil, o.now))
	if err != nil {
		return fmt.Errorf("cluster: cannot reach the leader at %s: %w", st.Leader, err)
	}
	defer c.Close()

	fctx, stop := context.WithCancel(ctx)
	defer stop()
	port := o.Cfg.PortsFor(config.SystemRef, 0).Postgres
	fwdErr := make(chan error, 1)
	go func() { fwdErr <- mesh.ForwardOne(fctx, c, port, leader, mesh.KindPostgres, config.SystemRef, o.log()) }()
	select { // ForwardOne returns at once when it cannot listen
	case err := <-fwdErr:
		if err != nil {
			return fmt.Errorf("cluster: cannot forward the system cluster's port %d to the leader: %w", port, err)
		}
	case <-time.After(200 * time.Millisecond):
	}

	if err := o.Seed(ctx, st.System); err != nil {
		return fmt.Errorf("cluster: seeding the system standby: %w", err)
	}
	timeout := o.StreamTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	var lsn string
	if o.Streaming != nil {
		lsn, err = o.Streaming(ctx)
	} else {
		lsn, err = WaitStreaming(ctx, o.DSNs, timeout)
	}
	if err != nil {
		return err
	}
	if err := c.Call(ctx, "POST", peerapi.PathJoinConfirm, peerapi.JoinConfirm{NodeID: st.NodeID, ReplayLSN: lsn}, nil); err != nil {
		return fmt.Errorf("cluster: the leader did not confirm the join: %w", err)
	}
	return nil
}

// WaitStreaming waits until the standby at one of dsns is in recovery and its WAL receiver
// streams, and returns its replay position.
func WaitStreaming(ctx context.Context, dsns []string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		for _, dsn := range dsns {
			lsn, ok, err := standbyStreaming(ctx, dsn)
			if err == nil && ok {
				return lsn, nil
			}
			if err != nil {
				last = err
			}
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !time.Now().Before(deadline) {
			if last != nil {
				return "", fmt.Errorf("cluster: the system standby is not streaming after %s: %w", timeout, last)
			}
			return "", fmt.Errorf("cluster: the system standby is not streaming after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func standbyStreaming(ctx context.Context, dsn string) (lsn string, streaming bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", false, err
	}
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return "", false, err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var rec bool
	var status, replay *string
	err = conn.QueryRow(ctx, `select pg_is_in_recovery(),
		(select status from pg_stat_wal_receiver limit 1),
		pg_last_wal_replay_lsn()::text`).Scan(&rec, &status, &replay)
	if err != nil {
		return "", false, err
	}
	l := ""
	if replay != nil {
		l = *replay
	}
	return l, rec && status != nil && *status == "streaming", nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(b, '\n'), 0o600)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
