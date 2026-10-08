package main

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/notimpl"
)

func init() {
	nodeCmd := &cobra.Command{
		Use:   "node",
		Short: "Join this server to a cluster and manage the cluster's nodes",
		Long: `A cluster is the servers that share one registry. The leader is the node whose system
cluster is a primary; every other node follows it. A second server hosts read replicas and takes over
when the leader fails.

  supavise node token       on the leader: create a one-time join token
  supavise node join        on the new server: join with that token
  supavise node ls          list the nodes
  supavise node rm          remove a node
  supavise node rejoin      bring a fenced node back as a follower

Nodes talk over one mutually authenticated TLS port ([node] peer_listen, 7443). Joining needs
S3-compatible backup storage, which the new server reads its first copy from.`,
	}

	var tokenName, tokenRegion string
	var tokenTTL time.Duration
	token := &cobra.Command{
		Use:   "token",
		Short: "Create a one-time join token (run on the leader)",
		Long: `Creates a token that lets one server join, and prints it. The token holds the leader's
address, the fingerprint of the cluster CA and a secret; the leader keeps only the secret's hash. It works
once and expires after --ttl. Refused on a node that is not the leader and when [backup] is a file://
backend.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return notimpl.For("supavise node token") },
	}
	token.Flags().StringVar(&tokenName, "name", "", "the only node name this token admits")
	token.Flags().StringVar(&tokenRegion, "region", "", "the region the new node is in (default: the joining server's own)")
	token.Flags().DurationVar(&tokenTTL, "ttl", time.Hour, "how long the token works")

	var joinRegion, joinAddress, joinTokenFile, joinKeyFile string
	var joinResume, joinKeyFromEscrow bool
	join := &cobra.Command{
		Use:   "join [token]",
		Short: "Join this server to a cluster with a token from the leader",
		Long: `Connects to the leader named in the token, checks that it presents the cluster CA the token
pins, proves it holds the token's secret, and receives a node certificate, the master key, the cluster's
settings and the first copy of the system cluster. The node is "joining" until its copy streams, then
"active".

A join that stopped after the certificate was issued continues with --resume, which needs no token.
Without a connection to the key's holder, --master-key-file or --key-from-escrow supply the master key;
the token still authenticates the node. The token may be read from a file (--token-file) so that it never
appears in a process listing.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(*cobra.Command, []string) error { return notimpl.For("supavise node join") },
	}
	join.Flags().StringVar(&joinRegion, "region", "", "this server's region (default [node] region, else region)")
	join.Flags().StringVar(&joinAddress, "address", "", "host:port other nodes dial to reach this server (default public_ip and the mesh port)")
	join.Flags().StringVar(&joinTokenFile, "token-file", "", "read the token from this file")
	join.Flags().StringVar(&joinKeyFile, "master-key-file", "", "read the master key from this file")
	join.Flags().BoolVar(&joinKeyFromEscrow, "key-from-escrow", false, "fetch the master key from the backup store's key escrow")
	join.Flags().BoolVar(&joinResume, "resume", false, "continue a join that stopped after the certificate was issued")

	var lsDNS, lsJSON bool
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List the nodes of the cluster",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return notimpl.For("supavise node ls") },
	}
	ls.Flags().BoolVar(&lsDNS, "dns", false, "print the DNS records the cluster still needs")
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	var rmYes bool
	rm := &cobra.Command{
		Use:   "rm <node>",
		Short: "Remove a node from the cluster (run on the leader)",
		Long: `Removes the node's replicas, marks it left and revokes its certificate at the next handshake.
The node wipes itself when it next reaches the leader.`,
		Args: cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error { return notimpl.For("supavise node rm") },
	}
	rm.Flags().BoolVarP(&rmYes, "yes", "y", false, "do not ask for confirmation")

	var rejoinLeader string
	rejoin := &cobra.Command{
		Use:   "rejoin",
		Short: "Bring a fenced node back into the cluster as a follower",
		Long: `A node that was replaced as leader while it was down refuses to start a primary. Rejoin
moves its stale data aside (kept for [failover] keep_diverged_days), rebuilds its system cluster and its
replicas from the current leader's archive, and keeps its identity.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return notimpl.For("supavise node rejoin") },
	}
	rejoin.Flags().StringVar(&rejoinLeader, "leader", "", "host:port of the current leader (default: ask the peers)")

	nodeCmd.AddCommand(token, join, ls, rm, rejoin)
	rootCmd.AddCommand(nodeCmd)
}
