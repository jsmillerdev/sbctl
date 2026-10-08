package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// clusterConfigSync is the config-d step of `supavise system converge` on a server that follows a
// leader: it asks the leader for the cluster-scoped settings (GET /peer/v1/config over the mesh) and
// rewrites config.d/10-cluster.toml when they differ, so that a setting changed on the leader (a
// backup backend, the TLS mode, [replicas] and [failover]) reaches the followers without a rejoin.
//
// The file is written 0600 and owned by the supavise user, as `supavise node join` writes it: it can
// hold secrets (DNS provider tokens, backup and Storage keys). Only cluster-scoped keys are taken
// from the leader: a text that sets a node-local key (a port, the node's name) is refused, because
// the leader has no say over those.
type clusterConfigSync struct {
	// ConfigPath is config.toml, which fixes where config.d and the node's identity are.
	ConfigPath string
	UID, GID   int
	// Fetch asks the leader for its cluster settings. It returns (nil, nil) when this server leads or
	// is not in a cluster (nothing to fetch); a test replaces it.
	Fetch func(ctx context.Context) (*peerapi.ClusterConfig, error)
}

// Sync implements hostsetup.ConfigSyncer.
func (s *clusterConfigSync) Sync(ctx context.Context, dryRun bool) ([]string, error) {
	cc, err := s.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	if cc == nil || cc.TOML == "" {
		return nil, nil
	}
	sum := sha256.Sum256([]byte(cc.TOML))
	if cc.Revision != hex.EncodeToString(sum[:]) {
		return nil, errors.New("the leader's cluster settings do not match their revision: refusing to write them")
	}
	if err := onlyClusterKeys(cc.TOML); err != nil {
		return nil, err
	}
	path := filepath.Join(config.ConfigDDir(s.ConfigPath), config.ClusterConfigFile)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == cc.TOML {
		return nil, nil
	}
	if dryRun {
		return []string{path}, nil
	}
	if err := writeFileAtomic(path, []byte(cc.TOML), 0o600, s.UID, s.GID); err != nil {
		return nil, err
	}
	return []string{path}, nil
}

// onlyClusterKeys refuses a text that does not parse, or that sets a key which is not
// cluster-scoped (config.IsClusterKey).
func onlyClusterKeys(text string) error {
	var m map[string]any
	if err := toml.Unmarshal([]byte(text), &m); err != nil {
		return fmt.Errorf("the leader's cluster settings do not parse: %w", err)
	}
	var bad []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			if sub, ok := v.(map[string]any); ok {
				walk(prefix+k+".", sub)
			} else if !config.IsClusterKey(prefix + k) {
				bad = append(bad, prefix+k)
			}
		}
	}
	walk("", m)
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("the leader's cluster settings set keys that are not cluster-scoped (%s): refusing to write them", strings.Join(bad, ", "))
	}
	return nil
}

// newClusterConfigSync is the step for this server, or nil when it has no mesh identity (it is not
// in a cluster): the step is left out of the list then. Whether this server leads is found when the
// step runs, from the registry.
func newClusterConfigSync(cfg *config.Config, configPath string) hostsetup.ConfigSyncer {
	dir := config.ClusterDir(configPath)
	if !cluster.Joined(dir) {
		return nil
	}
	uid, gid := supaviseOwner()
	s := &clusterConfigSync{ConfigPath: configPath, UID: uid, GID: gid}
	s.Fetch = func(ctx context.Context) (*peerapi.ClusterConfig, error) { return fetchLeaderConfig(ctx, cfg, dir) }
	return s
}

// fetchLeaderConfig reads the registry for the leader and its address, and asks the leader for its
// cluster settings with this node's mesh identity. A server that leads, or that no other node
// shares a registry with, has nothing to fetch.
func fetchLeaderConfig(ctx context.Context, cfg *config.Config, clusterDir string) (*peerapi.ClusterConfig, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	reg, err := registry.OpenExisting(ctx, lifecycle.SystemSocketDSN(cfg, "supavise")+" pool_max_conns=2")
	if err != nil {
		return nil, fmt.Errorf("cannot read the registry to find the leader: %w", err)
	}
	defer reg.Close()
	cl, err := reg.GetCluster(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot read the cluster: %w", err)
	}
	self, err := lifecycle.SelfNode(ctx, reg, cfg, true)
	if err != nil {
		return nil, err
	}
	if cl.Leader == "" || cl.Leader == self {
		return nil, nil
	}
	lead, err := reg.GetNode(ctx, cl.Leader)
	if err != nil {
		return nil, fmt.Errorf("cannot read the leader %s: %w", cl.Leader, err)
	}
	if lead.PeerAddr == "" {
		return nil, fmt.Errorf("the registry has no address for the leader %s", lead.ID)
	}
	creds, err := cluster.LoadCredentials(clusterDir)
	if err != nil {
		return nil, fmt.Errorf("this server's mesh identity: %w", err)
	}
	c, err := mesh.DialClient(ctx, lead.PeerAddr, lead.ID, mesh.ClientTLS(func() *mesh.Credentials { return creds }, lead.ID, nil, time.Now))
	if err != nil {
		return nil, fmt.Errorf("cannot reach the leader %s at %s: %w", lead.ID, lead.PeerAddr, err)
	}
	defer c.Close()
	var cc peerapi.ClusterConfig
	if err := c.Call(ctx, "GET", peerapi.PathConfig, nil, &cc); err != nil {
		return nil, fmt.Errorf("the leader %s would not give its cluster settings: %w", lead.ID, err)
	}
	return &cc, nil
}
