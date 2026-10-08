package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A config.toml from before clusters loads with the code defaults of every new key.
func TestClusterKeysHaveCodeDefaults(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.toml"), "domain = \"example.com\"\n[backup]\nbackend = \"s3://b/p\"\n")
	c, err := Load(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Node.PeerListen != ":7443" || c.PeerListen() != ":7443" || c.Node.Name != "" || c.Node.PeerAddress != "" {
		t.Errorf("node: %+v", c.Node)
	}
	if c.Ports.ReplicaBase != 10000 || c.ReplicaBase() != 10000 {
		t.Errorf("replica_base = %d", c.Ports.ReplicaBase)
	}
	if want := (Replicas{Default: "off", Concurrency: 2, BootstrapMaxBackupAge: "24h", UnhealthyLagSeconds: 300, SchemaReloadSeconds: 10}); c.Replicas != want {
		t.Errorf("replicas: %+v", c.Replicas)
	}
	if want := (Failover{Mode: "manual", MaxLagSeconds: 30, GraceSeconds: 90, ProjectGraceSeconds: 180, StopTimeoutSeconds: 120, CooldownMinutes: 60, KeepDivergedDays: 3}); c.Failover != want {
		t.Errorf("failover: %+v", c.Failover)
	}
	if c.AWS.StackName != "" || c.Fleet.StorageS3RoleARN != "" || c.Fleet.StorageCredentials() != DefaultStorageCredentialsPort {
		t.Errorf("aws %+v fleet %q", c.AWS, c.Fleet.StorageS3RoleARN)
	}
	r, f := c.Replicas, c.Failover
	if !(r.SetupConcurrency() == 2 && r.BootstrapMaxAge() == 24*time.Hour && r.UnhealthyLag() == 5*time.Minute && r.LBMaxLag() == 0 && r.SchemaReload() == 10*time.Second && !r.AllByDefault()) {
		t.Errorf("replica accessors: %+v", r)
	}
	if !(f.MaxLag() == 30*time.Second && f.Grace() == 90*time.Second && f.ProjectGrace() == 3*time.Minute && f.StopTimeout() == 2*time.Minute &&
		f.Cooldown() == time.Hour && f.KeepDiverged() == 72*time.Hour && !f.Automatic()) {
		t.Errorf("failover accessors: %+v", f)
	}
	// A configuration built in code, with every field zero, gets the same answers.
	var z Config
	if z.ReplicaBase() != 10000 || z.PeerListen() != ":7443" || z.Replicas.SetupConcurrency() != 2 || z.Failover.Cooldown() != time.Hour || z.Replicas.BootstrapMaxAge() != 24*time.Hour {
		t.Error("the zero Config does not fall back to the defaults")
	}
}

func TestConfigDMergesInLexicalOrder(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	writeFile(t, cfg, `
domain = "base.example"
[backup]
backend = "s3://b/p"
retention_days = 3
s3_region = "eu-west-1"
[tls.credentials]
api_token = "from-config"
keep = "yes"
`)
	writeFile(t, filepath.Join(dir, "config.d", "20-aws.toml"), "[aws]\nstack_name = \"supavise\"\n[backup]\nretention_days = 7\n")
	writeFile(t, filepath.Join(dir, "config.d", "10-cluster.toml"), "domain = \"cluster.example\"\n[backup]\nretention_days = 5\n[tls.credentials]\napi_token = \"from-cluster\"\n")
	writeFile(t, filepath.Join(dir, "config.d", "05-first.toml"), "[backup]\nretention_days = 1\n[replicas]\nconcurrency = 4\n")
	writeFile(t, filepath.Join(dir, "config.d", "notes.txt"), "[backup]\nretention_days = 99\n")
	writeFile(t, filepath.Join(dir, "config.d", ".hidden.toml"), "[backup]\nretention_days = 98\n")
	writeFile(t, filepath.Join(dir, "config.d", "sub", "30.toml"), "[backup]\nretention_days = 97\n")

	c, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.Backup.RetentionDays != 7 {
		t.Errorf("retention_days = %d, want 7 (config.toml 3, then 05: 1, 10: 5, 20: 7)", c.Backup.RetentionDays)
	}
	if c.Domain != "cluster.example" || c.Backup.S3Region != "eu-west-1" || c.Replicas.Concurrency != 4 || c.AWS.StackName != "supavise" {
		t.Errorf("merged config: domain %q region %q concurrency %d stack %q", c.Domain, c.Backup.S3Region, c.Replicas.Concurrency, c.AWS.StackName)
	}
	if c.TLS.Credentials["api_token"] != "from-cluster" || c.TLS.Credentials["keep"] != "yes" {
		t.Errorf("credentials: %v", c.TLS.Credentials)
	}
	// The environment wins over every file.
	t.Setenv("SUPAVISE_BACKUP_RETENTION_DAYS", "11")
	t.Setenv("SUPAVISE_REPLICAS_CONCURRENCY", "6")
	c, err = Load(cfg)
	if err != nil || c.Backup.RetentionDays != 11 || c.Replicas.Concurrency != 6 {
		t.Fatalf("environment: %v %+v %+v", err, c.Backup, c.Replicas)
	}
	// $SUPAVISE_CONFIG finds the same directory.
	t.Setenv(EnvConfigPath, cfg)
	if c, err = Load(""); err != nil || c.Domain != "cluster.example" {
		t.Fatalf("via the environment: %v %q", err, c.Domain)
	}
	if got := ConfigDDir(cfg); got != filepath.Join(dir, "config.d") {
		t.Errorf("ConfigDDir = %s", got)
	}
	if got := ClusterDir(cfg); got != filepath.Join(dir, "cluster") {
		t.Errorf("ClusterDir = %s", got)
	}
}

func TestConfigDErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	writeFile(t, cfg, "")
	writeFile(t, filepath.Join(dir, "config.d", "10-bad.toml"), "[backup\nretention_days = 5\n")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "10-bad.toml") {
		t.Fatalf("a config.d file that does not parse: %v", err)
	}
	// A value that validates only in the merged result is judged there.
	if err := os.Remove(filepath.Join(dir, "config.d", "10-bad.toml")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "config.d", "10-ok.toml"), "[replicas]\ndefault = \"all\"\n")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "replicas.default") {
		t.Fatalf("replicas.default = all on a file:// backend: %v", err)
	}
	// No config.d at all, and a config.d that is a plain file, are fine.
	if _, err := Load(filepath.Join(t.TempDir(), "absent.toml")); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "config.toml"), "")
	writeFile(t, filepath.Join(other, "config.d"), "not a directory")
	if _, err := Load(filepath.Join(other, "config.toml")); err != nil {
		t.Fatalf("config.d is a file: %v", err)
	}
}

func TestClusterValidation(t *testing.T) {
	s3 := func(c *Config) { c.Backup.Backend = "s3://bucket/prefix" }
	for _, tc := range []struct {
		name string
		edit func(c *Config)
		want string // substring of the error; empty means valid
	}{
		{"defaults", func(c *Config) {}, ""},
		{"replicas all on s3", func(c *Config) { s3(c); c.Replicas.Default = "all" }, ""},
		{"replicas all on files", func(c *Config) { c.Replicas.Default = "all" }, "replicas.default"},
		{"replicas unknown", func(c *Config) { c.Replicas.Default = "some" }, "replicas.default"},
		{"manual failover on files", func(c *Config) { c.Failover.Mode = "manual" }, ""},
		{"project failover needs aws fencing", func(c *Config) { s3(c); c.Failover.Mode = "project" }, "fencing"},
		{"server failover with command fencing", func(c *Config) {
			s3(c)
			c.Failover.Mode, c.Failover.Fencing, c.Failover.FenceCommand = "server", "command", "/bin/true"
		}, "fencing"},
		{"server failover with aws fencing", func(c *Config) { s3(c); c.Failover.Mode, c.Failover.Fencing = "server", "aws" }, ""},
		{"server failover on files", func(c *Config) { c.Failover.Mode, c.Failover.Fencing = "server", "aws" }, "S3-compatible"},
		{"unknown mode", func(c *Config) { c.Failover.Mode = "auto" }, "failover.mode"},
		{"unknown fencing", func(c *Config) { c.Failover.Fencing = "ssh" }, "failover.fencing"},
		{"command fencing without a command", func(c *Config) { c.Failover.Fencing = "command" }, "fence_command"},
		{"command fencing with a command", func(c *Config) { c.Failover.Fencing, c.Failover.FenceCommand = "command", "/bin/true" }, ""},
		{"negative number", func(c *Config) { c.Failover.GraceSeconds = -1 }, "grace_seconds"},
		{"negative replica number", func(c *Config) { c.Replicas.Concurrency = -1 }, "concurrency"},
		{"bad age", func(c *Config) { c.Replicas.BootstrapMaxBackupAge = "yesterday" }, "bootstrap_max_backup_age"},
		{"zero age takes a fresh backup", func(c *Config) { c.Replicas.BootstrapMaxBackupAge = "0s" }, ""},
		{"node name", func(c *Config) { c.Node.Name = "Replica One" }, "node.name"},
		{"node name ok", func(c *Config) { c.Node.Name = "replica-1" }, ""},
		{"node region", func(c *Config) { c.Node.Region = "mars-1" }, "node.region"},
		{"node region ok", func(c *Config) { c.Node.Region = "eu-west-1" }, ""},
		{"peer listen", func(c *Config) { c.Node.PeerListen = "7443" }, "node.peer_listen"},
		{"peer listen port", func(c *Config) { c.Node.PeerListen = ":99999" }, "node.peer_listen"},
		{"peer listen ok", func(c *Config) { c.Node.PeerListen = "10.0.0.5:7443" }, ""},
		{"peer address needs a host", func(c *Config) { c.Node.PeerAddress = ":7443" }, "node.peer_address"},
		{"peer address ok", func(c *Config) { c.Node.PeerAddress = "203.0.113.5:7443" }, ""},
		{"stack name", func(c *Config) { c.AWS.StackName = "1 bad" }, "aws.stack_name"},
		{"stack name ok", func(c *Config) { c.AWS.StackName = "supavise-prod" }, ""},
		{"storage role", func(c *Config) { c.Fleet.StorageS3RoleARN = "my-role" }, "storage_s3_role_arn"},
		{"storage role ok", func(c *Config) { c.Fleet.StorageS3RoleARN = "arn:aws:iam::123456789012:role/supavise-storage" }, ""},
		{"storage role and a static key", func(c *Config) {
			c.Fleet.StorageS3RoleARN = "arn:aws:iam::123456789012:role/supavise-storage"
			c.Fleet.StorageS3AccessKeyID = "AKIAEXAMPLE"
		}, "alternatives"},
		{"storage credentials port", func(c *Config) { c.Fleet.StorageCredentialsPort = 70000 }, "storage_credentials_port"},
		{"storage credentials port ok", func(c *Config) { c.Fleet.StorageCredentialsPort = 4011 }, ""},
		// The endpoint's port is a fixed port of the node: nothing else may bind it.
		{"storage credentials port is supavisor's", func(c *Config) { c.Fleet.StorageCredentialsPort = 4001 }, "fleet.supavisor_api_port"},
		{"storage credentials port is supavisor's, moved", func(c *Config) { c.Fleet.SupavisorAPIPort = 4999; c.Fleet.StorageCredentialsPort = 4999 }, "fleet.supavisor_api_port"},
		{"storage credentials port is studio's", func(c *Config) { c.Fleet.StorageCredentialsPort = c.Ports.Studio }, "ports.studio"},
		{"storage credentials port is the admin listener's", func(c *Config) { c.Fleet.StorageCredentialsPort = 7000 }, "listen.admin"},
		{"storage credentials port is the peer port", func(c *Config) { c.Fleet.StorageCredentialsPort = 7443 }, "node.peer_listen"},
		{"storage credentials port in the project range", func(c *Config) { c.Fleet.StorageCredentialsPort = c.Ports.ProjectBase + 3 }, "project port range"},
		{"role ARN with the default port", func(c *Config) { c.Fleet.StorageS3RoleARN = "arn:aws:iam::123456789012:role/supavise-storage" }, ""},
		{"role ARN and supavisor on the default port", func(c *Config) {
			c.Fleet.StorageS3RoleARN = "arn:aws:iam::123456789012:role/supavise-storage"
			c.Fleet.SupavisorAPIPort = DefaultStorageCredentialsPort
		}, "fleet.supavisor_api_port"},
		// Not served, not judged: a port nobody binds collides with nothing.
		{"unused default port is supavisor's", func(c *Config) { c.Fleet.SupavisorAPIPort = DefaultStorageCredentialsPort }, ""},
		// The replica port range is judged only once replicas are in use, so a node with a small
		// project_base that never uses them still starts.
		{"small project_base, no replicas", func(c *Config) { c.Ports.ProjectBase = 5000 }, ""},
		{"small project_base, replicas on", func(c *Config) { s3(c); c.Ports.ProjectBase = 5000; c.Replicas.Default = "all" }, "replica_base"},
		{"replica_base set but overlapping", func(c *Config) { c.Ports.ReplicaBase = 21000 }, "replica_base"},
		{"replica_base privileged", func(c *Config) { c.Ports.ReplicaBase = 80 }, "replica_base"},
		{"replica_base ok", func(c *Config) { c.Ports.ReplicaBase = 12000 }, ""},
	} {
		c := Default()
		tc.edit(c)
		err := c.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.want)
		}
	}
}

func TestReplicaPorts(t *testing.T) {
	c := Default()
	if got := c.ReplicaPorts("abcdefghijklmnopqrst", 2); got != (ProjectPorts{Postgres: 10006, PostgREST: 10008}) {
		t.Errorf("replica ports of seq 2: %+v", got)
	}
	if got := c.ReplicaPorts(SystemRef, 0); got != (ProjectPorts{Postgres: 10000}) {
		t.Errorf("replica ports of system: %+v", got)
	}
	// 10000 + 3*3332 + 2 = 19998, the last replica below project_base 20000.
	if got := c.MaxReplicaSeq(); got != 3332 {
		t.Errorf("MaxReplicaSeq = %d, want 3332", got)
	}
	if last := c.ReplicaPorts("x", c.MaxReplicaSeq()); last.PostgREST >= c.Ports.ProjectBase || c.ReplicaPorts("x", c.MaxReplicaSeq()+1).PostgREST < c.Ports.ProjectBase {
		t.Errorf("the replica range does not end just below project_base: %+v", last)
	}
	// No replica port is a project port, for any sequence both can have.
	for seq := 1; seq <= c.MaxReplicaSeq(); seq += 97 {
		r, p := c.ReplicaPorts("x", seq), c.PortsFor("x", seq)
		if r.Postgres == p.Postgres || r.PostgREST == p.PostgREST || r.PostgREST >= c.Ports.ProjectBase {
			t.Fatalf("seq %d: replica %+v project %+v", seq, r, p)
		}
	}
	c.Ports.ProjectBase, c.Ports.ReplicaBase = 34000, 30000
	if c.MaxReplicaSeq() != 1332 || c.ReplicaPorts("x", 1).Postgres != 30003 {
		t.Errorf("moved bases: max %d, ports %+v", c.MaxReplicaSeq(), c.ReplicaPorts("x", 1))
	}
	t.Setenv("SUPAVISE_PORTS_REPLICA_BASE", "12000")
	l, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil || l.ReplicaPorts("x", 1).Postgres != 12003 {
		t.Errorf("env replica_base: %v %+v", err, l.ReplicaPorts("x", 1))
	}
}

func TestPathsForPromotion(t *testing.T) {
	p := Default().Paths()
	if got := p.PromoteOK("abc"); got != "/var/lib/supavise/projects/abc/promote.ok" {
		t.Error(got)
	}
	if got := p.FailoverState(); got != "/var/lib/supavise/failover.json" {
		t.Error(got)
	}
}

func TestNodeIdentityDefaults(t *testing.T) {
	c := Default()
	c.Node.Name = "replica-1"
	if c.NodeName() != "replica-1" {
		t.Error(c.NodeName())
	}
	c.Node.Name = ""
	if n := c.NodeName(); !nodeNameRe.MatchString(n) {
		t.Errorf("the host name %q does not fit the registry's rule", n)
	}
	for in, want := range map[string]string{
		"ip-10-0-0-5.ec2.internal": "ip-10-0-0-5-ec2-internal", "MacBook Pro": "macbook-pro", "...": "node", "": "node",
		strings.Repeat("a", 60):                      strings.Repeat("a", 41),
		strings.Repeat("a", 40) + "." + "b" + "cccc": strings.Repeat("a", 40),
	} {
		if got := SanitizeNodeName(in); got != want {
			t.Errorf("SanitizeNodeName(%q) = %q, want %q", in, got, want)
		}
	}
	c.Region = "eu-west-2"
	if c.NodeRegion() != "eu-west-2" {
		t.Error(c.NodeRegion())
	}
	c.Node.Region = "us-west-2"
	if c.NodeRegion() != "us-west-2" {
		t.Error(c.NodeRegion())
	}
	if c.PeerAddr() != "" {
		t.Error("no public ip, no peer address")
	}
	c.PublicIP = "203.0.113.9"
	if c.PeerAddr() != "203.0.113.9:7443" {
		t.Error(c.PeerAddr())
	}
	c.Node.PeerListen = ":9443"
	if c.PeerAddr() != "203.0.113.9:9443" {
		t.Error(c.PeerAddr())
	}
	c.Node.PeerAddress = "peer.example:7443"
	if c.PeerAddr() != "peer.example:7443" {
		t.Error(c.PeerAddr())
	}
	c.PublicIP = "2001:db8::1"
	c.Node.PeerAddress = ""
	if c.PeerAddr() != "[2001:db8::1]:9443" {
		t.Error(c.PeerAddr())
	}
}

func TestIsClusterKey(t *testing.T) {
	for k, want := range map[string]bool{
		"domain": true, "tls.mode": true, "tls.credentials.api_token": true, "tls": false, "backup.backend": true,
		"backup.s3_secret_access_key": true, "fleet.storage_backend": true, "fleet.storage_s3_bucket": true,
		"fleet.supavisor_api_port": false, "fleet.pooler_max_client_conn": false, "replicas.default": true,
		"failover.fence_command": true, "update.mode": false, "node.name": false, "aws.stack_name": false,
		"ports.replica_base": false, "public_ip": false, "region": false, "alerts.email_to": false, "domains": false,
	} {
		if got := IsClusterKey(k); got != want {
			t.Errorf("IsClusterKey(%q) = %v, want %v", k, got, want)
		}
	}
}

// ClusterKeys carries exactly the cluster-scoped settings, defaults included, and what
// MarshalCluster renders loads back to the same values on a node with different local ones.
func TestClusterKeysRoundTrip(t *testing.T) {
	leader := Default()
	leader.Domain = "example.com"
	leader.PublicIP = "203.0.113.1"
	leader.Region = "eu-west-1"
	leader.Node.Name = "leader"
	leader.AWS.StackName = "leader-stack"
	leader.Update.Mode = "auto"
	leader.TLS.Mode, leader.TLS.DNSProvider = "dns01", "cloudflare"
	leader.TLS.Credentials = map[string]string{"api_token": "tok"}
	leader.Backup.Backend, leader.Backup.S3Region, leader.Backup.S3SecretAccessKey = "s3://b/p", "eu-west-1", "shh"
	leader.Fleet.StorageBackend, leader.Fleet.StorageS3Bucket, leader.Fleet.SupavisorAPIPort = "s3", "objects", 4999
	leader.Replicas.Default, leader.Replicas.Concurrency = "all", 3
	leader.Failover.Mode, leader.Failover.Fencing, leader.Failover.CooldownMinutes = "project", "aws", 30

	m, err := ClusterKeys(leader)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"public_ip", "region", "node", "aws", "update", "ports", "listen"} {
		if _, ok := m[absent]; ok {
			t.Errorf("node-local %q is in the cluster keys", absent)
		}
	}
	if m["domain"] != "example.com" {
		t.Errorf("domain: %v", m["domain"])
	}
	fleet, _ := m["fleet"].(map[string]any)
	if fleet["storage_s3_bucket"] != "objects" || fleet["supavisor_api_port"] != nil {
		t.Errorf("fleet keys: %v", fleet)
	}
	if rep, _ := m["replicas"].(map[string]any); rep["unhealthy_lag_seconds"] != int64(300) {
		t.Errorf("a default is missing from the cluster keys: %v", rep)
	}

	b, err := MarshalCluster(leader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "# Cluster settings") {
		t.Errorf("no header:\n%s", b)
	}
	follower := Default()
	follower.Domain = "stale.example"
	follower.Replicas.Concurrency = 9 // set locally; the cluster file overrides it
	follower.PublicIP = "203.0.113.2"
	follower.Node.Name = "follower"
	if err := DecodeTOML(b, follower); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if follower.Domain != "example.com" || follower.Replicas.Concurrency != 3 || follower.Failover.Mode != "project" ||
		follower.TLS.Credentials["api_token"] != "tok" || follower.Backup.S3SecretAccessKey != "shh" || follower.Fleet.StorageS3Bucket != "objects" {
		t.Errorf("follower after the cluster file: %+v", follower)
	}
	if follower.PublicIP != "203.0.113.2" || follower.Node.Name != "follower" || follower.Fleet.SupavisorAPIPort != 0 || follower.Update.Mode != "notify" {
		t.Errorf("node-local settings changed: %+v", follower)
	}
	// The same table through the loader.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.toml"), "public_ip = \"203.0.113.2\"\n")
	writeFile(t, filepath.Join(dir, "config.d", ClusterConfigFile), string(b))
	loaded, err := Load(filepath.Join(dir, "config.toml"))
	if err != nil || loaded.Replicas.Default != "all" || loaded.PublicIP != "203.0.113.2" {
		t.Fatalf("load: %v %+v", err, loaded.Replicas)
	}
	var back map[string]any
	if err := toml.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, m) {
		t.Errorf("marshal does not match the table:\n%v\n%v (%v)", back, m, err)
	}
}
