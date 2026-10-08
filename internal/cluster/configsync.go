package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// ConfigSync keeps config.d/10-cluster.toml, the cluster-scoped settings a join writes, in step with
// the leader's: `supavise system converge` runs it (hostsetup.ConfigSyncer) so that a change of the
// domain, the backup store or the replica and failover settings reaches every node. It asks the leader
// for GET /peer/v1/config over a session of its own (mesh.OneShot, so that the leader does not take it for
// the daemon's session), which needs only the node's certificate, and writes
// the file (0600, the mode of the secrets a cluster setting may hold) when the text differs.
//
// It is for a follower. A server that never joined, a founder and the leader itself have nothing to
// fetch, and a leader that cannot be reached is no reason to fail the host's convergence: the file
// stays as it is and the next run tries again, which the log says.
type ConfigSync struct {
	Cfg        *config.Config
	ConfigPath string
	// DSNs are the sockets the system cluster may answer on, where the registry names the leader and
	// its address; the address the node joined is the fallback.
	DSNs []string
	// Where, when set, replaces the registry look: the leader's node id and address.
	Where func(ctx context.Context) (id, addr string, err error)
	Log   *slog.Logger
	Now   func() time.Time
}

func (c *ConfigSync) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *ConfigSync) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// Sync implements hostsetup.ConfigSyncer: it fetches the leader's settings and rewrites the file when
// they differ (dryRun only reports), and returns the files it changed or would change.
func (c *ConfigSync) Sync(ctx context.Context, dryRun bool) ([]string, error) {
	dir := config.ClusterDir(c.ConfigPath)
	if !IsFollower(dir) {
		return nil, nil
	}
	creds, err := LoadCredentials(dir)
	if err != nil {
		return nil, fmt.Errorf("cluster: the node certificate cannot be read: %w", err)
	}
	id, addr, err := c.where(ctx, dir, creds.NodeID)
	if err != nil {
		c.log().Warn("cluster settings: the leader is not known; the file stays as it is", "error", err)
		return nil, nil
	}
	if id == creds.NodeID {
		return nil, nil // this node leads: its config.toml is the source
	}
	var resp peerapi.ClusterConfig
	if err := c.fetch(ctx, creds, id, addr, &resp); err != nil {
		c.log().Warn("cluster settings: the leader did not answer; the file stays as it is", "leader", addr, "error", err)
		return nil, nil
	}
	path := filepath.Join(config.ConfigDDir(c.ConfigPath), config.ClusterConfigFile)
	have, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil && bytes.Equal(have, []byte(resp.TOML)) {
		return nil, nil
	}
	if dryRun {
		return []string{path}, nil
	}
	if err := writeFile(path, []byte(resp.TOML), 0o600); err != nil {
		return nil, fmt.Errorf("cluster: writing %s: %w", path, err)
	}
	return []string{path}, nil
}

func (c *ConfigSync) where(ctx context.Context, dir, self string) (id, addr string, err error) {
	if c.Where != nil {
		return c.Where(ctx)
	}
	if dsn, rec, perr := probeSystem(ctx, c.DSNs, 3*time.Second); perr == nil && rec {
		// A standby: its registry copy names the leader. (A node whose system cluster is a primary leads.)
		if reg, rerr := registry.OpenReadOnly(ctx, dsn); rerr == nil {
			defer reg.Close()
			if cl, cerr := reg.GetCluster(ctx); cerr == nil {
				if n, nerr := reg.GetNode(ctx, cl.Leader); nerr == nil && n.PeerAddr != "" {
					return n.ID, n.PeerAddr, nil
				}
			}
		}
	} else if perr == nil {
		return self, "", nil
	}
	// The registry is not at hand: the leader the node joined, as the follower mark kept it, by address and
	// (a mark written by an older release has none) by node id, which the answer must name.
	b, rerr := os.ReadFile(filepath.Join(dir, FollowerFile))
	if rerr != nil {
		return "", "", rerr
	}
	var m struct {
		Leader   string `json:"leader"`
		LeaderID string `json:"leader_id"`
	}
	if jerr := json.Unmarshal(b, &m); jerr != nil || m.Leader == "" {
		return "", "", errors.New("the registry cannot be read and the node kept no leader address")
	}
	return m.LeaderID, m.Leader, nil
}

func (c *ConfigSync) fetch(ctx context.Context, creds *mesh.Credentials, id, addr string, out *peerapi.ClusterConfig) error {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// The call is made beside the daemon, which holds the node's session to the leader: it asks as a short
	// call and is not taken for that session (mesh.OneShot).
	cl, err := mesh.DialClient(cctx, addr, firstNonEmpty(id, "leader"), mesh.OneShot(mesh.ClientTLS(func() *mesh.Credentials { return creds }, id, nil, c.now)))
	if err != nil {
		return err
	}
	defer cl.Close()
	return cl.Call(cctx, "GET", peerapi.PathConfig, nil, out)
}
