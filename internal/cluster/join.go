package cluster

import (
	"bytes"
	"cmp"
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
	"syscall"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fsutil"
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
	// Reset discards the cluster identity this server already holds (a join that never finished and was
	// reaped, a fenced node that cannot rejoin, a node that was removed while it was down) and joins as a
	// new node. What runs here is stopped with StopLocal and the data is set aside, as a retirement does.
	// The retirement comes before the exchange, so it follows every check that does not spend the token:
	// the token and the master key, Preflight, and a look at the leader (a challenge, thrown away). A
	// refusal that only the exchange can make (the token was used already, the name is taken, the release
	// is outside the window) leaves the server stopped with its data set aside; the operator runs the
	// join again with a new token. Ignored with Resume.
	Reset bool
	// StopLocal stops everything that runs on this server (FenceLocal). Required with Reset.
	StopLocal func(ctx context.Context) error
	// Seed builds and starts the system standby. Required.
	Seed SeedFunc
	// Preflight, when set, runs before anything is asked of the leader or changed here, and not for
	// Resume. An error stops the join with the server as it was: use it for what Seed needs and a
	// refusal later would waste, because the token is spent and the node exists on the leader once the
	// exchange is done (room on the disk, the backup store). It must not look at the data directories:
	// with Reset they are set aside after it ran. Refusing data that Seed would overwrite is Seed's job.
	Preflight func(ctx context.Context) error
	// DSNs are the sockets the system standby answers on, to see it stream.
	DSNs []string
	// StreamTimeout bounds the wait for the standby to stream; zero is 30 minutes.
	StreamTimeout time.Duration
	// Streaming, when set, replaces the look at DSNs: it returns the standby's replay position once it
	// streams (tests).
	Streaming func(ctx context.Context) (lsn string, err error)
	// Rebuild lists the projects a rejoin set aside, which the confirmation hands to the leader to set up replicas of
	// (peerapi.JoinConfirm.Rebuild). Rejoin sets it; a join leaves it empty.
	Rebuild []string
	Log     *slog.Logger
	Now     func() time.Time
}

// JoinState is the content of join.json.
type JoinState struct {
	NodeID string                  `json:"node_id"`
	Leader string                  `json:"leader"` // host:port of the leader's peer listener
	System peerapi.SystemBootstrap `json:"system"` // how to seed the standby
	At     time.Time               `json:"at"`
	// Rebuild lists the projects a rejoin set aside, kept here so that `node join --resume` and a second
	// `node rejoin` hand them to the leader too: the data is under data.diverged-* by then, and nothing
	// else says which projects the node had.
	Rebuild []string `json:"rebuild,omitempty"`
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
		// The seeding is under way or done; the preflight ran when the join began.
		if st, err = ReadJoinState(dir); err != nil {
			return nil, err
		}
	} else {
		// What can be checked offline comes before anything is changed here, a reset's retirement included.
		secret, keyBody, err := o.inputs()
		if err != nil {
			return nil, err
		}
		if o.Preflight != nil {
			if err := o.Preflight(ctx); err != nil {
				return nil, err
			}
		}
		if Joined(dir) {
			if err := o.startOver(ctx); err != nil {
				return nil, err
			}
		}
		if st, err = o.exchange(ctx, dir, secret, keyBody); err != nil {
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

// CheckInputs checks what a join can check without the leader: the token has not expired, its secret
// is whole, and a master key the joiner supplies is the one the token's CA pin was made from. The join
// command calls it before it asks for a confirmation, and Join calls it before it changes anything.
func (o *JoinOptions) CheckInputs() error {
	_, _, err := o.inputs()
	return err
}

// inputs is CheckInputs, and returns the token's secret and the master key the joiner supplies (nil
// when the leader sends it).
func (o *JoinOptions) inputs() (secret, keyBody []byte, err error) {
	tok := o.Token
	if !tok.Expires().After(o.now()) {
		return nil, nil, errors.New("cluster: the join token has expired; ask the leader for a new one with `supavise node token`")
	}
	secret, err = base64.RawURLEncoding.DecodeString(tok.Secret)
	if err != nil || len(secret) == 0 {
		return nil, nil, errors.New("cluster: the join token's secret is damaged")
	}
	if o.MasterKey != nil {
		if err := keyMatchesPin(o.MasterKey, tok.CAFpr); err != nil {
			return nil, nil, fmt.Errorf("cluster: that master key is not the one this cluster was made with (%w)", err)
		}
		keyBody = o.MasterKey
	} else if have, err := os.ReadFile(o.Cfg.KeyPath); err == nil {
		// This server has a master key of its own, which the leader's would not replace (writeKey keeps
		// it). Finding out after the exchange would cost the token, the leader's row for this node and
		// the wait for the leader to reap it.
		if err := keyMatchesPin(have, tok.CAFpr); err != nil {
			return nil, nil, fmt.Errorf("cluster: %s holds another key than the cluster's (%w); move it away if this server holds nothing worth keeping, or pass the cluster's key with --master-key-file", o.Cfg.KeyPath, err)
		}
	}
	return secret, keyBody, nil
}

// keyMatchesPin checks that the master key in body (the key file's text) derives the CA the join token
// pins. The error says which half failed.
func keyMatchesPin(body []byte, pin string) error {
	sec, err := secrets.Load(body)
	if err != nil {
		return err
	}
	ca, err := NewCA(sec)
	if err != nil {
		return err
	}
	if !strings.EqualFold(ca.Fingerprint(), pin) {
		return errors.New("the CA it derives is not the one the token pins")
	}
	return nil
}

// reachLeader asks the leader the token names for a challenge, the first step of every join, and
// throws it away. A reset retires this server before the exchange; the leader being unreachable, not the
// leader or not the one the token pins would leave the server down with its data set aside, so
// it is looked at while nothing has changed. What only the exchange can tell (a token used already, a
// name taken, a release outside the window) cannot be looked at without spending the token.
func (o *JoinOptions) reachLeader(ctx context.Context) error {
	tok := o.Token
	c, err := mesh.DialClient(ctx, tok.Leader, "leader", mesh.PinnedTLS(tok.CAFpr, nil, o.now))
	if err != nil {
		return fmt.Errorf("cluster: cannot reach the leader at %s: %w (nothing was changed on this server)", tok.Leader, err)
	}
	defer c.Close()
	var ch peerapi.JoinChallenge
	if err := c.Call(ctx, "GET", peerapi.PathJoin, nil, &ch); err != nil {
		return fmt.Errorf("cluster: the leader at %s does not take joins: %w (nothing was changed on this server)", tok.Leader, err)
	}
	return nil
}

// startOver deals with a server that already holds a cluster identity when a join with a token
// begins. The identity of a node that was removed is revoked, and the record of the removal says so:
// it is deleted without more ado (its files stay when the daemon's unit did not let it delete them).
// Any other identity may be a working member's, so it is refused unless Reset says to discard it.
func (o *JoinOptions) startOver(ctx context.Context) error {
	if rec, _ := ReadFenced(o.Cfg); rec != nil && rec.Removed {
		return forgetIdentity(o.ConfigPath)
	}
	if !o.Reset {
		return errors.New("cluster: this server already joined a cluster; use `supavise node join --resume` to finish a join that stopped, `supavise node rejoin` for a fenced node, " +
			"or `supavise node join --reset` to give up its place in that cluster and join as a new node (run `supavise node rm` for the old node on its leader first)")
	}
	if o.StopLocal == nil {
		return errors.New("cluster: a reset needs a way to stop the local clusters")
	}
	if err := o.reachLeader(ctx); err != nil {
		return err
	}
	_, err := Retire(ctx, o.Cfg, o.ConfigPath, o.StopLocal, o.now())
	return err
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

// exchange is phase one. secret and keyBody are what inputs checked.
func (o *JoinOptions) exchange(ctx context.Context, dir string, secret, keyBody []byte) (*JoinState, error) {
	tok := o.Token
	name := cmp.Or(o.Name, tok.Name, o.Cfg.NodeName())
	region := cmp.Or(o.Region, tok.Region, o.Cfg.NodeRegion())
	address := cmp.Or(o.Address, o.Cfg.PeerAddr())
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
	// The mark before the certificate, whose presence is what Joined looks for: a server that holds the
	// identity of a joined node always holds the mark.
	if err := markFollower(dir, resp.NodeID, tok.Leader, resp.System.Leader, o.now()); err != nil {
		return nil, fmt.Errorf("cluster: writing %s: %w", FollowerFile, err)
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
	if err := o.forwardSystem(fctx, c, port, leader); err != nil {
		return err
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
	if err := c.Call(ctx, "POST", peerapi.PathJoinConfirm, peerapi.JoinConfirm{NodeID: st.NodeID, ReplayLSN: lsn, Rebuild: unionRefs(o.Rebuild, st.Rebuild)}, nil); err != nil {
		return fmt.Errorf("cluster: the leader did not confirm the join: %w", err)
	}
	return nil
}

// portWait is how long forwardSystem waits for the system cluster's port when something still holds it; a variable
// so that a test can shorten it.
var portWait = time.Minute

// forwardSystem starts the forwarder that carries the system cluster's port to the leader, and returns once it
// listens. The port belongs to the PostgreSQL that ran here until a moment ago (a rejoin, a join --resume that finds
// the old cluster still going down): a listener that is still there is waited for, so that the command can be run
// again at once after a failure and does not depend on how long the old cluster takes to stop. Any other failure to
// listen is returned.
func (o *JoinOptions) forwardSystem(ctx context.Context, c mesh.Dialer, port int, leader string) error {
	deadline := time.Now().Add(portWait)
	for {
		fwdErr := make(chan error, 1)
		go func() { fwdErr <- mesh.ForwardOne(ctx, c, port, leader, mesh.KindPostgres, config.SystemRef, o.log()) }()
		select { // ForwardOne returns at once when it cannot listen
		case err := <-fwdErr:
			if err == nil {
				return nil
			}
			if !errors.Is(err, syscall.EADDRINUSE) || !time.Now().Before(deadline) {
				return fmt.Errorf("cluster: cannot forward the system cluster's port %d to the leader: %w", port, err)
			}
			o.log().Info("the system cluster's port is still held; waiting for it", "port", port, "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		case <-time.After(200 * time.Millisecond):
			return nil
		}
	}
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
	conn, err := connect(ctx, dsn)
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
	return fsutil.WriteJSON(path, v, 0o600, writeOpts)
}
