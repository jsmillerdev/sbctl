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
	// Preflight, when set, runs before anything is asked of the leader or changed here. An error stops
	// the rejoin with the node as it was: use it for what Seed needs and a refusal later would waste. It
	// must not look at the data directories: the rejoin sets the old data aside after it ran, and a
	// rejoin that is repeated after a seeding that stopped finds that seeding's partial data.
	Preflight func(ctx context.Context) error
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
	if o.Preflight != nil {
		if err := o.Preflight(ctx); err != nil {
			return nil, err
		}
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
	// The record names the leader as it was when the node was fenced, and the node expects that one
	// at the address the record keeps. An address the operator gives may be a later leader's, so no
	// node id is expected there: the certificate is checked against the cluster CA, and the node that
	// answers must be the leader, which the leader's own refusal of anyone else enforces.
	addr, want := o.Leader, ""
	if addr == "" {
		addr, want = rec.Peers[rec.Leader], rec.Leader
	}
	if addr == "" {
		return nil, fmt.Errorf("cluster: the address of the leader (%s) is not known; pass --leader host:port", rec.Leader)
	}

	getCreds := func() *mesh.Credentials { return creds }
	c, err := mesh.DialClient(ctx, addr, firstNonEmpty(want, "leader"), mesh.ClientTLS(getCreds, want, nil, now))
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
	if err := markFollower(dir, creds.NodeID, addr, firstNonEmpty(resp.System.Leader, want), now()); err != nil {
		return nil, fmt.Errorf("cluster: writing %s: %w", FollowerFile, err)
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

// LocalData lists the PGDATA directories of the system project and of every project directory that
// has one: what a rejoin, a reset or a retirement sets aside.
func LocalData(cfg *config.Config) []string {
	out, _ := localData(cfg)
	return out
}

func localData(cfg *config.Config) ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(cfg.Paths().Root, "projects"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		data := cfg.Paths().PostgresData(e.Name())
		if _, err := os.Stat(data); err == nil {
			out = append(out, data)
		}
	}
	return out, nil
}

// MoveDiverged renames the PGDATA of the system project and of every project directory to
// data.diverged-<epoch> (-2, -3 ... when that name is taken), and removes the diverged directories
// that were set aside more than [failover] keep_diverged_days ago. It returns the new paths. Nothing
// is copied: a rename keeps the data and the disk it takes. A rename leaves the directory's own time
// as it was (the last change of its top level, which may be days back: a fenced primary stopped when
// it was fenced), so each directory is stamped with now when it is moved, and that is when its
// retention starts. A directory that cannot be stamped is put back, because the next call would take
// its old time for its age and remove it.
func MoveDiverged(cfg *config.Config, epoch int64, now time.Time) ([]string, error) {
	dirs, err := localData(cfg)
	if err != nil {
		return nil, err
	}
	var moved []string
	for _, data := range dirs {
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
		if err := os.Chtimes(dst, now, now); err != nil {
			if back := os.Rename(dst, data); back != nil {
				return moved, fmt.Errorf("cluster: %s was set aside as %s and its retention could not start (%v), and it could not be put back: %w; remove it by hand when its data is no longer needed", data, dst, err, back)
			}
			return moved, fmt.Errorf("cluster: the retention of %s could not start, so it was left where it was: %w", data, err)
		}
		moved = append(moved, dst)
	}
	pruneDiverged(cfg, now)
	return moved, nil
}

// pruneDiverged removes the diverged directories whose last change (the stamp MoveDiverged gave them)
// is older than the retention.
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
// project directories aside like a rejoin does, records that it was removed, and deletes its cluster
// identity and the cluster settings it was given. The daemon then starts as a node that is down like a
// fenced one, with the reason, until `supavise node join` makes it a member again; the master key
// stays. The record comes first, then the data, then the identity: a node whose data could not all be
// set aside, or whose files cannot be deleted (the unit may not let the daemon write there), still
// comes up down with the reason; the error then names what stayed, and `supavise node join` clears the
// identity. It returns the directories it set aside.
func Retire(ctx context.Context, cfg *config.Config, configPath string, stop func(context.Context) error, now time.Time) ([]string, error) {
	if stop != nil {
		if err := stop(ctx); err != nil {
			return nil, err
		}
	}
	if err := WriteFenced(cfg, FencedRecord{Reason: RemovedReason, Removed: true, At: now.UTC()}); err != nil {
		return nil, err
	}
	moved, err := MoveDiverged(cfg, 0, now)
	if err != nil {
		return moved, fmt.Errorf("cluster: the node is recorded as removed and comes up down, but its data was not all set aside (the identity stays until it is): %w", err)
	}
	return moved, forgetIdentity(configPath)
}

// forgetIdentity deletes the node's certificate, key, the CA it was given, the state of an unfinished
// join and the cluster settings the leader sent. A file that is not there is fine; every other
// failure is reported, after the rest were tried.
func forgetIdentity(configPath string) error {
	dir := config.ClusterDir(configPath)
	var errs []error
	for _, p := range []string{
		filepath.Join(dir, config.NodeCertFile), filepath.Join(dir, config.NodeKeyFile), filepath.Join(dir, config.ClusterCAFile),
		filepath.Join(dir, JoinStateFile), filepath.Join(dir, FollowerFile), filepath.Join(config.ConfigDDir(configPath), config.ClusterConfigFile),
	} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("cluster: the node's identity was not deleted completely (the daemon's unit must allow writes to %s and %s): %w",
			dir, config.ConfigDDir(configPath), errors.Join(errs...))
	}
	return nil
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
