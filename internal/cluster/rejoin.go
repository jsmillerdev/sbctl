package cluster

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// RejoinOptions describe `supavise node rejoin`.
type RejoinOptions struct {
	Cfg        *config.Config
	ConfigPath string
	// Leader is host:port of the current leader; empty takes the address the fenced record kept.
	Leader string
	// Version and Pins: see JoinOptions.
	Version string
	Pins    map[string]string
	// StopLocal stops everything that could write as a primary (FenceLocal). Required.
	StopLocal func(ctx context.Context) error
	// Seed builds the system standby from the leader's archive and starts it; see SeedFunc. Required.
	Seed SeedFunc
	// DSNs are the sockets the standby answers on.
	DSNs          []string
	StreamTimeout time.Duration
	// Streaming: see JoinOptions.Streaming.
	Streaming func(ctx context.Context) (string, error)
	Log       *slog.Logger
	Now       func() time.Time
}

// RejoinResult says what the rejoin moved aside.
type RejoinResult struct {
	NodeID string
	System peerapi.SystemBootstrap
	// Diverged are the data directories that were set aside, with their new names.
	Diverged []string
}

// Rejoin brings a fenced node back as a follower. It asks the current leader to let it in (the leader
// puts the node back to joining and names the base backup of the system cluster), stops whatever
// still runs here, sets the system cluster's and every project's data directory aside under
// data.diverged-<epoch> (kept for [failover] keep_diverged_days), seeds a new system standby from the
// leader's archive, waits for it to stream and confirms. The node keeps its identity and certificate.
// Its replicas are rebuilt by the replica controller once the node is active. The fenced record is
// removed last, which is what lets the daemon start normally.
func Rejoin(ctx context.Context, o RejoinOptions) (*RejoinResult, error) {
	if o.StopLocal == nil || o.Seed == nil {
		return nil, errors.New("cluster: a rejoin needs a way to stop the local clusters and to seed the standby")
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	rec, err := ReadFenced(o.Cfg)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errors.New("cluster: this node is not fenced; there is nothing to rejoin (a node that was removed joins again with a token from `supavise node token`)")
	}
	if rec.Removed {
		return nil, errors.New("cluster: this node was removed from the cluster; it joins again with a token from `supavise node token` (`supavise node join`), not with rejoin")
	}
	dir := config.ClusterDir(o.ConfigPath)
	creds, err := LoadCredentials(dir)
	if err != nil {
		return nil, fmt.Errorf("cluster: a rejoin keeps the node's identity, and it cannot be read: %w", err)
	}
	addr, leaderID := o.Leader, rec.Leader
	if addr == "" {
		addr = rec.Peers[rec.Leader]
	}
	if addr == "" {
		return nil, fmt.Errorf("cluster: the address of the leader (%s) is not known; pass --leader host:port", rec.Leader)
	}

	getCreds := func() *mesh.Credentials { return creds }
	// The leader's node id is the one the record names; when the operator named another address the
	// id is learned from the certificate it presents.
	c, err := mesh.DialClient(ctx, addr, leaderID, mesh.ClientTLS(getCreds, leaderID, nil, now))
	if err != nil {
		return nil, fmt.Errorf("cluster: cannot reach the leader at %s: %w", addr, err)
	}
	var resp peerapi.RejoinResponse
	err = c.Call(ctx, "POST", peerapi.PathRejoin, peerapi.RejoinRequest{Version: o.Version, ArtifactPins: o.Pins}, &resp)
	c.Close()
	if err != nil {
		return nil, fmt.Errorf("cluster: the leader would not take this node back: %w", err)
	}
	if err := o.StopLocal(ctx); err != nil {
		return nil, fmt.Errorf("cluster: stopping the local clusters: %w", err)
	}
	moved, err := MoveDiverged(o.Cfg, resp.System.Epoch-1, now())
	if err != nil {
		return nil, err
	}
	if resp.ClusterConfig != "" {
		p := filepath.Join(config.ConfigDDir(o.ConfigPath), config.ClusterConfigFile)
		if err := writeFile(p, []byte(resp.ClusterConfig), 0o600); err != nil {
			return nil, fmt.Errorf("cluster: writing %s: %w", p, err)
		}
	}
	st := &JoinState{NodeID: creds.NodeID, Leader: addr, System: resp.System, At: now().UTC()}
	if err := writeJSON(filepath.Join(dir, JoinStateFile), st); err != nil {
		return nil, err
	}
	jo := &JoinOptions{Cfg: o.Cfg, ConfigPath: o.ConfigPath, Seed: o.Seed, DSNs: o.DSNs, StreamTimeout: o.StreamTimeout, Streaming: o.Streaming, Log: log, Now: o.Now}
	if err := jo.stream(ctx, dir, st); err != nil {
		return nil, fmt.Errorf("%w; run `supavise node join --resume` to continue (the old data is kept under data.diverged-*)", err)
	}
	_ = os.Remove(filepath.Join(dir, JoinStateFile))
	if err := ClearFenced(o.Cfg); err != nil {
		return nil, err
	}
	return &RejoinResult{NodeID: creds.NodeID, System: resp.System, Diverged: moved}, nil
}

// divergedMark is the infix of the directories MoveDiverged creates.
const divergedMark = ".diverged-"

// MoveDiverged renames the PGDATA of the system project and of every project directory to
// data.diverged-<epoch> (-2, -3 ... when that name is taken), and removes the diverged directories
// that are older than [failover] keep_diverged_days. It returns the new paths. Nothing is copied: a
// rename keeps the data and the disk it takes.
func MoveDiverged(cfg *config.Config, epoch int64, now time.Time) ([]string, error) {
	projects := filepath.Join(cfg.Paths().Root, "projects")
	ents, err := os.ReadDir(projects)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var moved []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		data := cfg.Paths().PostgresData(e.Name())
		if _, err := os.Stat(data); err != nil {
			continue
		}
		dst := fmt.Sprintf("%s%s%d", data, divergedMark, epoch)
		for i := 2; ; i++ {
			if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
				break
			}
			dst = fmt.Sprintf("%s%s%d-%d", data, divergedMark, epoch, i)
		}
		if err := os.Rename(data, dst); err != nil {
			return moved, fmt.Errorf("cluster: setting %s aside: %w", data, err)
		}
		moved = append(moved, dst)
	}
	pruneDiverged(cfg, now)
	return moved, nil
}

// pruneDiverged removes the diverged directories whose last change is older than the retention.
func pruneDiverged(cfg *config.Config, now time.Time) {
	keep := cfg.Failover.KeepDiverged()
	projects := filepath.Join(cfg.Paths().Root, "projects")
	ents, _ := os.ReadDir(projects)
	for _, e := range ents {
		dir := cfg.Paths().ProjectService(e.Name(), config.SvcPostgres)
		subs, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, s := range subs {
			if !s.IsDir() || !strings.HasPrefix(s.Name(), "data"+divergedMark) {
				continue
			}
			if fi, err := s.Info(); err == nil && now.Sub(fi.ModTime()) > keep {
				_ = os.RemoveAll(filepath.Join(dir, s.Name()))
			}
		}
	}
}

// RemovedReason is the reason of the record a removed node keeps.
const RemovedReason = "this node was removed from the cluster; join it again with a token from `supavise node token` (`supavise node join`)"

// Retire is what a node that learns it was removed from the cluster does: it stops what runs, sets the
// project directories aside like a rejoin does, deletes its cluster identity and the cluster settings
// it was given, and records that it was removed. The daemon then starts as a node that is down like a
// fenced one, with the reason, until `supavise node join` makes it a member again; the master key
// stays. It returns the directories it set aside.
func Retire(ctx context.Context, cfg *config.Config, configPath string, stop func(context.Context) error, now time.Time) ([]string, error) {
	if stop != nil {
		if err := stop(ctx); err != nil {
			return nil, err
		}
	}
	moved, err := MoveDiverged(cfg, 0, now)
	if err != nil {
		return moved, err
	}
	dir := config.ClusterDir(configPath)
	for _, f := range []string{config.NodeCertFile, config.NodeKeyFile, config.ClusterCAFile, JoinStateFile} {
		_ = os.Remove(filepath.Join(dir, f))
	}
	_ = os.Remove(filepath.Join(config.ConfigDDir(configPath), config.ClusterConfigFile))
	return moved, WriteFenced(cfg, FencedRecord{Reason: RemovedReason, Removed: true, At: now.UTC()})
}

// PeersOf lists the peer addresses of the nodes other than self, for the fenced record.
func PeersOf(nodes []registry.Node, self string) map[string]string {
	out := map[string]string{}
	for _, n := range nodes {
		if n.ID != self && n.PeerAddr != "" {
			out[n.ID] = n.PeerAddr
		}
	}
	return out
}
