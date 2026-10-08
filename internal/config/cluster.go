package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// The sections below configure read replicas, the second server and failover. Every key has a
// code default, so a config.toml from before them loads unchanged and a node that sets none of
// them behaves as it did.

// Node is the [node] section: this server's identity in a cluster. All of it is node-local; the
// installer and `supavise node join` write it, never the leader's cluster file.
type Node struct {
	// Name is the node's name in the registry (lower case letters, digits and hyphens, at most
	// 41 characters). Empty means the host name (NodeName).
	Name string `toml:"name"`
	// PeerListen is where the mesh listens: one TCP port for mutual TLS between nodes. Empty
	// means ":7443".
	PeerListen string `toml:"peer_listen"`
	// PeerAddress is the host:port other nodes dial. Empty means public_ip and the port of
	// PeerListen (PeerAddr). It is a hint: a session works from whichever side can connect.
	PeerAddress string `toml:"peer_address"`
	// Region is the node's region, a code from Regions. Empty means the top-level region.
	Region string `toml:"region"`
}

// Replicas is the [replicas] section. Cluster-scoped: every node uses the leader's values.
type Replicas struct {
	// Default is "off" (replicas exist when someone adds one) or "all" (every project that is not
	// a branch and not paused gets a replica on every other node). "all" needs S3-compatible
	// backup storage.
	Default string `toml:"default"`
	// Concurrency is how many replica setups run at once. 0 means 2.
	Concurrency int `toml:"concurrency"`
	// BootstrapMaxBackupAge: a replica is seeded from the project's newest base backup; when
	// that is older than this (a Go duration such as "24h") or missing, setup takes a new one
	// first. Empty means 24h.
	BootstrapMaxBackupAge string `toml:"bootstrap_max_backup_age"`
	// UnhealthyLagSeconds is the replication lag above which a replica is ACTIVE_UNHEALTHY.
	// 0 means 300.
	UnhealthyLagSeconds int `toml:"unhealthy_lag_seconds"`
	// LBMaxLagSeconds: the load balancer sends no read to a replica lagging more than this.
	// 0 means the balancer ignores lag.
	LBMaxLagSeconds int `toml:"lb_max_lag_seconds"`
	// SchemaReloadSeconds is how often the daemon asks a replica's PostgREST to reload its schema
	// cache, a safety net for a DDL change whose notification beat the replay of its WAL.
	// 0 means 30.
	SchemaReloadSeconds int `toml:"schema_reload_seconds"`
}

// Replicas.Default values.
const (
	ReplicasOff = "off"
	ReplicasAll = "all"
)

// Failover is the [failover] section. Cluster-scoped.
type Failover struct {
	// Mode is "manual" (an operator runs `supavise failover`), "project" (the leader fails over
	// one project whose primary stays unhealthy) or "server" (a follower takes over a dead leader).
	// The automatic modes need Fencing "aws".
	Mode string `toml:"mode"`
	// Fencing makes sure the old primary cannot write before the new one is promoted: "" (none:
	// the operator asserts it with --old-primary-is-down), "aws" (stop the instance and take the
	// Elastic IP through the EC2 API) or "command" (run FenceCommand).
	Fencing string `toml:"fencing"`
	// FenceCommand runs on the survivor with OLD_NODE, NEW_NODE and EPOCH in the environment and
	// must exit 0 before a promotion. Required when Fencing is "command".
	FenceCommand string `toml:"fence_command"`
	// TakeoverCommand runs after the fence with the same environment and moves the service
	// address (DNS or an IP) to the survivor. Optional.
	TakeoverCommand string `toml:"takeover_command"`
	// MaxLagSeconds: a replica lagging more than this is not promoted without --force. 0 means 30.
	MaxLagSeconds int `toml:"max_lag_seconds"`
	// GraceSeconds is how long the leader must be unreachable before server mode acts. 0 means 90.
	GraceSeconds int `toml:"grace_seconds"`
	// ProjectGraceSeconds is how long a project's primary must stay unhealthy before project
	// mode acts. 0 means 180.
	ProjectGraceSeconds int `toml:"project_grace_seconds"`
	// StopTimeoutSeconds bounds the wait for the fence to stop the old instance. 0 means 120.
	StopTimeoutSeconds int `toml:"stop_timeout_seconds"`
	// CooldownMinutes: no automatic failover within this long of the last one. 0 means 60.
	CooldownMinutes int `toml:"cooldown_minutes"`
	// KeepDivergedDays is how long `node rejoin` keeps the old data directories. 0 means 3.
	KeepDivergedDays int `toml:"keep_diverged_days"`
}

// Failover.Mode and Failover.Fencing values.
const (
	FailoverManual  = "manual"
	FailoverProject = "project"
	FailoverServer  = "server"

	FencingNone    = ""
	FencingAWS     = "aws"
	FencingCommand = "command"
)

// AWS is the [aws] section, written by `supavise upgrade --aws` into config.d/20-aws.toml.
// Node-local.
type AWS struct {
	// StackName is the CloudFormation stack that created this node.
	StackName string `toml:"stack_name"`
}

// DefaultReplicas and DefaultFailover are the built-in [replicas] and [failover] sections.
func DefaultReplicas() Replicas {
	return Replicas{Default: ReplicasOff, Concurrency: 2, BootstrapMaxBackupAge: "24h", UnhealthyLagSeconds: 300, SchemaReloadSeconds: 30}
}

func DefaultFailover() Failover {
	return Failover{Mode: FailoverManual, MaxLagSeconds: 30, GraceSeconds: 90, ProjectGraceSeconds: 180,
		StopTimeoutSeconds: 120, CooldownMinutes: 60, KeepDivergedDays: 3}
}

func secondsOr(n, def int) time.Duration {
	if n <= 0 {
		n = def
	}
	return time.Duration(n) * time.Second
}

// AllByDefault reports whether every project gets a replica on every other node.
func (r Replicas) AllByDefault() bool { return r.Default == ReplicasAll }

// SetupConcurrency is the number of replica setups in flight at once.
func (r Replicas) SetupConcurrency() int {
	if r.Concurrency <= 0 {
		return 2
	}
	return r.Concurrency
}

// BootstrapMaxAge is the age past which setup takes a new base backup. Validate has accepted the value.
func (r Replicas) BootstrapMaxAge() time.Duration {
	d, err := time.ParseDuration(r.BootstrapMaxBackupAge)
	if r.BootstrapMaxBackupAge == "" || err != nil || d < 0 {
		return 24 * time.Hour
	}
	return d
}

// UnhealthyLag is the lag above which a replica is unhealthy.
func (r Replicas) UnhealthyLag() time.Duration { return secondsOr(r.UnhealthyLagSeconds, 300) }

// LBMaxLag is the lag above which the load balancer skips a replica; zero means it does not look.
func (r Replicas) LBMaxLag() time.Duration {
	return time.Duration(max(r.LBMaxLagSeconds, 0)) * time.Second
}

// SchemaReload is the interval of the PostgREST schema reload on a replica.
func (r Replicas) SchemaReload() time.Duration { return secondsOr(r.SchemaReloadSeconds, 30) }

// Automatic reports whether the daemon may fail over by itself.
func (f Failover) Automatic() bool { return f.Mode == FailoverProject || f.Mode == FailoverServer }

func (f Failover) MaxLag() time.Duration       { return secondsOr(f.MaxLagSeconds, 30) }
func (f Failover) Grace() time.Duration        { return secondsOr(f.GraceSeconds, 90) }
func (f Failover) ProjectGrace() time.Duration { return secondsOr(f.ProjectGraceSeconds, 180) }
func (f Failover) StopTimeout() time.Duration  { return secondsOr(f.StopTimeoutSeconds, 120) }

// Cooldown is the pause after a failover during which an automatic one is not started.
func (f Failover) Cooldown() time.Duration {
	if f.CooldownMinutes <= 0 {
		return time.Hour
	}
	return time.Duration(f.CooldownMinutes) * time.Minute
}

// KeepDiverged is how long the data directories a rejoin set aside are kept.
func (f Failover) KeepDiverged() time.Duration {
	if f.KeepDivergedDays <= 0 {
		return 3 * 24 * time.Hour
	}
	return time.Duration(f.KeepDivergedDays) * 24 * time.Hour
}

var (
	nodeNameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	stackNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,127}$`)
)

// NodeName is the name this node has in the registry: [node] name, else the host name made to fit
// the registry's rule (lower case, anything else a hyphen, at most 41 characters).
func (c *Config) NodeName() string {
	if c.Node.Name != "" {
		return c.Node.Name
	}
	h, _ := os.Hostname()
	return SanitizeNodeName(h)
}

// SanitizeNodeName turns a host name into a valid node name; "node" when nothing is left.
func SanitizeNodeName(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 41 {
		s = strings.TrimRight(s[:41], "-")
	}
	if s == "" {
		return "node"
	}
	return s
}

// NodeRegion is the node's region: [node] region, else the top-level region.
func (c *Config) NodeRegion() string {
	if c.Node.Region != "" {
		return c.Node.Region
	}
	if c.Region != "" {
		return c.Region
	}
	return DefaultRegion
}

// PeerListen is the address the mesh listens on.
func (c *Config) PeerListen() string {
	if c.Node.PeerListen != "" {
		return c.Node.PeerListen
	}
	return ":" + strconv.Itoa(PortPeer)
}

// PeerAddr is the host:port other nodes dial: [node] peer_address, else public_ip and the port of
// PeerListen. Empty when neither names a host (the daemon then asks the cloud's metadata).
func (c *Config) PeerAddr() string {
	if c.Node.PeerAddress != "" {
		return c.Node.PeerAddress
	}
	if c.PublicIP == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(c.PeerListen())
	if err != nil {
		return ""
	}
	return net.JoinHostPort(c.PublicIP, port)
}

// FileBackup reports whether [backup] names a file:// backend, which a second server cannot read.
func (c *Config) FileBackup() bool { return strings.HasPrefix(c.Backup.Backend, "file://") }

// CheckReplicaPorts reports whether the replica port range fits: ReplicaBase must be an
// unprivileged port and MaxReplicaSeq at least 1, so that replica ports and project ports never
// meet. Validate calls it when the node uses replicas; the code that adds a replica calls it too.
func (c *Config) CheckReplicaPorts() error {
	if b := c.ReplicaBase(); b < 1024 || c.MaxReplicaSeq() < 1 {
		return fmt.Errorf("config: ports.replica_base %d out of range: replica ports run from it up to %d projects' worth (3 each) and must end below ports.project_base %d",
			b, max(c.MaxReplicaSeq(), 0), c.Ports.ProjectBase)
	}
	return nil
}

func (c *Config) validateCluster() error {
	n := c.Node
	if n.Name != "" && !nodeNameRe.MatchString(n.Name) {
		return fmt.Errorf("config: node.name %q must be lower case letters, digits and hyphens, at most 41 characters", n.Name)
	}
	if n.Region != "" && !ValidRegion(n.Region) {
		return fmt.Errorf("config: node.region %q is not one of the regions Studio knows (%s)", n.Region, strings.Join(Regions, ", "))
	}
	if n.PeerListen != "" {
		if err := checkHostPort("node.peer_listen", n.PeerListen, true); err != nil {
			return err
		}
	}
	if n.PeerAddress != "" {
		if err := checkHostPort("node.peer_address", n.PeerAddress, false); err != nil {
			return err
		}
	}
	if c.AWS.StackName != "" && !stackNameRe.MatchString(c.AWS.StackName) {
		return fmt.Errorf("config: aws.stack_name %q is not a CloudFormation stack name", c.AWS.StackName)
	}
	if s := c.Fleet.StorageS3CredentialsSecret; s != "" && !strings.HasPrefix(s, "arn:") {
		return fmt.Errorf("config: fleet.storage_s3_credentials_secret %q must be the ARN of an AWS Secrets Manager secret", s)
	}

	r := c.Replicas
	switch r.Default {
	case "", ReplicasOff, ReplicasAll:
	default:
		return fmt.Errorf("config: replicas.default %q: want %q or %q", r.Default, ReplicasOff, ReplicasAll)
	}
	if r.BootstrapMaxBackupAge != "" {
		if d, err := time.ParseDuration(r.BootstrapMaxBackupAge); err != nil || d < 0 {
			return fmt.Errorf("config: replicas.bootstrap_max_backup_age %q: want a duration such as 24h", r.BootstrapMaxBackupAge)
		}
	}
	for name, v := range map[string]int{
		"concurrency": r.Concurrency, "unhealthy_lag_seconds": r.UnhealthyLagSeconds,
		"lb_max_lag_seconds": r.LBMaxLagSeconds, "schema_reload_seconds": r.SchemaReloadSeconds,
	} {
		if v < 0 {
			return fmt.Errorf("config: replicas.%s must not be negative, not %d", name, v)
		}
	}

	f := c.Failover
	switch f.Mode {
	case "", FailoverManual, FailoverProject, FailoverServer:
	default:
		return fmt.Errorf("config: failover.mode %q: want manual, project or server", f.Mode)
	}
	switch f.Fencing {
	case FencingNone, FencingAWS, FencingCommand:
	default:
		return fmt.Errorf("config: failover.fencing %q: want aws, command or empty", f.Fencing)
	}
	if f.Fencing == FencingCommand && strings.TrimSpace(f.FenceCommand) == "" {
		return errors.New("config: failover.fencing = \"command\" needs failover.fence_command")
	}
	for name, v := range map[string]int{
		"max_lag_seconds": f.MaxLagSeconds, "grace_seconds": f.GraceSeconds, "project_grace_seconds": f.ProjectGraceSeconds,
		"stop_timeout_seconds": f.StopTimeoutSeconds, "cooldown_minutes": f.CooldownMinutes, "keep_diverged_days": f.KeepDivergedDays,
	} {
		if v < 0 {
			return fmt.Errorf("config: failover.%s must not be negative, not %d", name, v)
		}
	}
	if f.Automatic() && f.Fencing != FencingAWS {
		return fmt.Errorf("config: failover.mode %q needs failover.fencing = \"aws\": an automatic failover without a hard fence could leave two writers", f.Mode)
	}

	// A second server restores from the backup store and a promoted node archives into it, so
	// both need a store the other node can reach.
	if c.FileBackup() {
		if r.AllByDefault() {
			return errors.New("config: replicas.default = \"all\" needs S3-compatible backup storage; [backup] backend is a file:// path that a second server cannot read")
		}
		if f.Automatic() {
			return fmt.Errorf("config: failover.mode %q needs S3-compatible backup storage; [backup] backend is a file:// path that a second server cannot read", f.Mode)
		}
	}
	// The replica port range only matters once replicas can exist; a node that never uses them keeps
	// whatever ports.project_base it has always had.
	if r.AllByDefault() || f.Automatic() || c.Ports.ReplicaBase != PortReplicaBase {
		return c.CheckReplicaPorts()
	}
	return nil
}

// checkHostPort checks "host:port". With hostOptional an address such as ":7443" is fine.
func checkHostPort(key, v string, hostOptional bool) error {
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("config: %s %q: want host:port", key, v)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("config: %s %q: the port must be a number from 1 to 65535", key, v)
	}
	if host == "" && !hostOptional {
		return fmt.Errorf("config: %s %q: want host:port with a host", key, v)
	}
	return nil
}

// ---- config.d and the cluster file ----------------------------------------------------------

// Names of the files under the config directory (the directory of the config file, by default
// /etc/supavise), shared by the code that writes them and the loader that reads them.
const (
	ConfigDName = "config.d" // *.toml in it are merged over config.toml in lexical order
	// ClusterConfigFile holds the cluster-scoped keys (ClusterKeys). `supavise node join` writes
	// it and `supavise system converge` refreshes it from the leader.
	ClusterConfigFile = "10-cluster.toml"
	// AWSConfigFile holds [aws], written by `supavise upgrade --aws`.
	AWSConfigFile = "20-aws.toml"

	ClusterDirName = "cluster" // holds the node's key and certificate and the cluster CA
	NodeKeyFile    = "node.key"
	NodeCertFile   = "node.crt"
	ClusterCAFile  = "ca.crt"
)

// ResolvePath is the config file a command loads for path: path itself, else $SUPAVISE_CONFIG,
// else DefaultPath.
func ResolvePath(path string) string {
	if path == "" {
		path = os.Getenv(EnvConfigPath)
	}
	if path == "" {
		path = DefaultPath
	}
	return path
}

// ConfigDDir is the config.d directory next to the config file at path.
func ConfigDDir(path string) string {
	return filepath.Join(filepath.Dir(ResolvePath(path)), ConfigDName)
}

// ClusterDir is the directory next to the config file that holds the node's mesh key and
// certificate and the cluster CA (/etc/supavise/cluster).
func ClusterDir(path string) string {
	return filepath.Join(filepath.Dir(ResolvePath(path)), ClusterDirName)
}

// loadConfigD merges the *.toml files of the config.d directory next to path over c, in lexical
// order. A missing directory is not an error; a file that does not parse is.
func loadConfigD(c *Config, path string) error {
	dir := ConfigDDir(path)
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents { // sorted by file name
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".toml") || strings.HasPrefix(name, ".") {
			continue
		}
		p := filepath.Join(dir, name)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := DecodeTOML(b, c); err != nil {
			return fmt.Errorf("config %s: %w", p, err)
		}
	}
	return nil
}

// ClusterKeyPatterns name the keys of config.toml that are the same on every node of a cluster,
// as dotted TOML paths. A last element "*" matches everything below the table; in any other
// element "*" matches within that element. [node], [aws], [update], [listen], [ports], [health]
// and the rest are node-local.
var ClusterKeyPatterns = []string{"domain", "tls.*", "backup.*", "fleet.storage_*", "replicas.*", "failover.*"}

// IsClusterKey reports whether the dotted TOML path (for example "tls.mode" or
// "tls.credentials.api_token") is cluster-scoped.
func IsClusterKey(path string) bool {
	parts := strings.Split(path, ".")
	for _, pat := range ClusterKeyPatterns {
		pp := strings.Split(pat, ".")
		if last := len(pp) - 1; pp[last] == "*" {
			if len(parts) > last && matchParts(pp[:last], parts[:last]) {
				return true
			}
		} else if len(parts) == len(pp) && matchParts(pp, parts) {
			return true
		}
	}
	return false
}

func matchParts(pat, parts []string) bool {
	for i := range pat {
		if ok, _ := pathpkg.Match(pat[i], parts[i]); !ok {
			return false
		}
	}
	return true
}

// ClusterKeys returns the cluster-scoped part of c (ClusterKeyPatterns) as a nested table, ready
// for toml.Marshal. Every matching key is present, whether or not it differs from its default:
// a node that merges the table ends up with the leader's values, defaults included.
func ClusterKeys(c *Config) (map[string]any, error) {
	b, err := toml.Marshal(c)
	if err != nil {
		return nil, err
	}
	var all map[string]any
	if err := toml.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	return filterCluster(all, ""), nil
}

func filterCluster(m map[string]any, prefix string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		path := prefix + k
		if sub, ok := v.(map[string]any); ok {
			if kept := filterCluster(sub, path+"."); len(kept) > 0 {
				out[k] = kept
			}
			continue
		}
		if IsClusterKey(path) {
			out[k] = v
		}
	}
	return out
}

// MarshalCluster renders ClusterKeys as the text of config.d/10-cluster.toml. The file holds
// secrets (DNS provider tokens, backup keys), so whoever writes it keeps it 0600 or 0640.
func MarshalCluster(c *Config) ([]byte, error) {
	m, err := ClusterKeys(c)
	if err != nil {
		return nil, err
	}
	b, err := toml.Marshal(m)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Cluster settings, copied from the leader by `supavise node join` and refreshed by `supavise system converge`.\n# Change them on the leader; edits here are overwritten.\n"), b...), nil
}
