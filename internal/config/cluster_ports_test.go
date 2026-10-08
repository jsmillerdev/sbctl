package config

import (
	"strings"
	"testing"
)

func TestCheckReplicaPortsRefusesOverlapWithFixedPorts(t *testing.T) {
	c := Default()
	if err := c.CheckReplicaPorts(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	// The default fixed ports are all below the replica range, so a node that never set them is fine
	// with the default replica_base too.
	for _, tc := range []struct {
		name string
		edit func(c *Config)
		want string
	}{
		{"studio inside the range", func(c *Config) { c.Ports.Studio = 10300 }, "ports.studio 10300"},
		{"the system standby's own port", func(c *Config) { c.Ports.Realtime = c.ReplicaBase() }, "ports.realtime"},
		{"the top of the range", func(c *Config) { c.Ports.PGMeta = c.ReplicaBase() + 3*c.MaxReplicaSeq() + 2 }, "ports.pgmeta"},
		{"the admin listener", func(c *Config) { c.Listen.Admin = "127.0.0.1:10500" }, "listen.admin 10500"},
		{"the peer listener", func(c *Config) { c.Node.PeerListen = ":10600" }, "node.peer_listen 10600"},
		{"supavisor", func(c *Config) { c.Ports.SupavisorSession = 11000 }, "ports.supavisor_session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.edit(c)
			err := c.CheckReplicaPorts()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckReplicaPorts() = %v, want an error naming %q", err, tc.want)
			}
		})
	}
	// Just outside the range on both sides is fine.
	c = Default()
	c.Ports.Studio = c.ReplicaBase() - 1
	c.Ports.Realtime = c.ReplicaBase() + 3*c.MaxReplicaSeq() + 3
	if err := c.CheckReplicaPorts(); err != nil {
		t.Fatalf("ports next to the range: %v", err)
	}
	// An unset port is not a port.
	c = Default()
	c.Ports.EdgeRuntime = 0
	if err := c.CheckReplicaPorts(); err != nil {
		t.Fatalf("unset port: %v", err)
	}
}

func TestValidateRunsThePortCheckWhenReplicasAreInUse(t *testing.T) {
	c := Default()
	c.Ports.Studio = 10300
	if err := c.Validate(); err != nil {
		t.Fatalf("a node that does not use replicas keeps its ports: %v", err)
	}
	c.Replicas.Default = ReplicasAll
	c.Backup.Backend = "s3://bucket"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "ports.studio") {
		t.Fatalf("Validate() = %v, want the port overlap refused once replicas are in use", err)
	}
}
