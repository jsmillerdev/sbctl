package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/app"
	"github.com/supavise/supavise/internal/artifacts"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/notimpl"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/units"
	"github.com/supavise/supavise/internal/versions"
)

// seedSystemStandby builds this server's standby of the system cluster from the base backup the
// leader names and starts it (backup.SeedReplica, then the replica unit of lifecycle). The install
// command that joins a server (`supavise install --join-token-file`) and the join below both call the
// same function; the join itself (cluster.Join) does everything around it.
var seedSystemStandby cluster.SeedFunc = func(context.Context, peerapi.SystemBootstrap) error {
	return notimpl.For("seeding the system standby")
}

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
backend.

The first token also gives this server its cluster identity (a certificate signed by the CA that the
master key derives); the daemon restarts once to listen on the peer port.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, closeEnv, err := writableNodeEnv(cmd)
			if err != nil {
				return err
			}
			defer closeEnv()
			return runNodeToken(cmd.Context(), env, cluster.TokenOptions{Name: tokenName, Region: tokenRegion, TTL: tokenTTL})
		},
	}
	token.Flags().StringVar(&tokenName, "name", "", "the only node name this token admits")
	token.Flags().StringVar(&tokenRegion, "region", "", "the region the new node is in (default: the joining server's own)")
	token.Flags().DurationVar(&tokenTTL, "ttl", time.Hour, "how long the token works")

	var joinRegion, joinAddress, joinTokenFile, joinKeyFile, joinPassFile string
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
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			o := cluster.JoinOptions{Cfg: cfg, ConfigPath: config.ResolvePath(configPath), Resume: joinResume, Region: joinRegion, Address: joinAddress,
				Version: version, Log: newLogger(cfg), Seed: seedSystemStandby, DSNs: app.RegistryDSNs(cfg)}
			if !joinResume {
				raw, err := tokenInput(args, joinTokenFile, cmd.InOrStdin())
				if err != nil {
					return err
				}
				if o.Token, err = cluster.ParseToken(raw); err != nil {
					return err
				}
				if o.MasterKey, err = masterKeyInput(cmd.Context(), cfg, joinKeyFile, joinKeyFromEscrow, joinPassFile, cmd.InOrStdin()); err != nil {
					return err
				}
				if v, err := artifacts.ParseVersions(versions.VersionsYAML); err == nil {
					o.Pins = v.Pins()
				}
				o.PublicHost = cfg.PublicIP
				if id := app.AWSIdentity(cmd.Context(), cfg); id != nil {
					o.Provider = registry.NodeProvider{AWS: id}
				}
			}
			res, err := cluster.Join(cmd.Context(), o)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "this server joined the cluster as node %s; its system standby streams from the leader\n", res.NodeID)
			fmt.Fprintln(cmd.OutOrStdout(), "start the daemon: sudo systemctl enable --now supavise")
			return nil
		},
	}
	join.Flags().StringVar(&joinRegion, "region", "", "this server's region (default [node] region, else region)")
	join.Flags().StringVar(&joinAddress, "address", "", "host:port other nodes dial to reach this server (default public_ip and the mesh port)")
	join.Flags().StringVar(&joinTokenFile, "token-file", "", "read the token from this file")
	join.Flags().StringVar(&joinKeyFile, "master-key-file", "", "read the master key from this file")
	join.Flags().BoolVar(&joinKeyFromEscrow, "key-from-escrow", false, "fetch the master key from the backup store's key escrow (needs [backup] in config.toml and --passphrase-file)")
	join.Flags().StringVar(&joinPassFile, "passphrase-file", "", "file holding the escrow passphrase (mode 0600), or - for standard input")
	join.Flags().BoolVar(&joinResume, "resume", false, "continue a join that stopped after the certificate was issued")

	var lsDNS, lsJSON bool
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List the nodes of the cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, closeEnv, err := readOnlyNodeEnv(cmd)
			if err != nil {
				return err
			}
			defer closeEnv()
			return runNodeLs(cmd.Context(), env, lsDNS, lsJSON)
		},
	}
	ls.Flags().BoolVar(&lsDNS, "dns", false, "print the DNS records the cluster still needs")
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	var rmYes, rmForce bool
	var rmWait time.Duration
	rm := &cobra.Command{
		Use:   "rm <node>",
		Short: "Remove a node from the cluster (run on the leader)",
		Long: `Removes the node's replicas, marks it left and revokes its certificate at the next handshake.
The node wipes itself when it next reaches the leader.

The replica controller removes the replicas while this command waits; --force deletes their rows at
once, for a node that cannot be reached. A node that is the home of a project is refused: move the
projects first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, closeEnv, err := writableNodeEnv(cmd)
			if err != nil {
				return err
			}
			defer closeEnv()
			return runNodeRm(cmd.Context(), env, args[0], rmYes, cluster.RemoveOptions{Force: rmForce, Wait: rmWait})
		},
	}
	rm.Flags().BoolVarP(&rmYes, "yes", "y", false, "do not ask for confirmation")
	rm.Flags().BoolVar(&rmForce, "force", false, "delete the node's replica rows without waiting for the instances to be removed")
	rm.Flags().DurationVar(&rmWait, "timeout", 5*time.Minute, "how long to wait for the replicas to be removed")

	var rejoinLeader string
	rejoin := &cobra.Command{
		Use:   "rejoin",
		Short: "Bring a fenced node back into the cluster as a follower",
		Long: `A node that was replaced as leader while it was down refuses to start a primary. Rejoin
moves its stale data aside (kept for [failover] keep_diverged_days), rebuilds its system cluster and its
replicas from the current leader's archive, and keeps its identity.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			log := newLogger(cfg)
			sup, err := units.New(cfg, log)
			if err != nil {
				return fmt.Errorf("the units cannot be reached to stop them: %w", err)
			}
			o := cluster.RejoinOptions{Cfg: cfg, ConfigPath: config.ResolvePath(configPath), Leader: rejoinLeader, Version: version, Log: log,
				Seed: seedSystemStandby, DSNs: app.RegistryDSNs(cfg),
				StopLocal: func(ctx context.Context) error { _, err := cluster.FenceLocal(ctx, cfg, sup, log); return err }}
			if v, err := artifacts.ParseVersions(versions.VersionsYAML); err == nil {
				o.Pins = v.Pins()
			}
			res, err := cluster.Rejoin(cmd.Context(), o)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "node %s is back as a follower; %d data director(ies) were set aside\n", res.NodeID, len(res.Diverged))
			for _, d := range res.Diverged {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", d)
			}
			return nil
		},
	}
	rejoin.Flags().StringVar(&rejoinLeader, "leader", "", "host:port of the current leader (default: ask the peers)")

	nodeCmd.AddCommand(token, join, ls, rm, rejoin)
	rootCmd.AddCommand(nodeCmd)
}

// ---- reading what the commands need ----

// nodeEnv is what the node commands work on: the registry, the master key and the live status the
// daemon writes. The commands are functions of it so that they run against a fake registry.
type nodeEnv struct {
	cfg        *config.Config
	configPath string
	reg        registry.Registry
	sec        cluster.Deriver
	status     *cluster.Status
	now        func() time.Time
	out, errw  io.Writer
	in         io.Reader
}

// readOnlyNodeEnv opens the registry the way `supavise status` may: on a leader or a follower,
// without migrating it.
func readOnlyNodeEnv(cmd *cobra.Command) (*nodeEnv, func(), error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	reg, err := openRegistryReadOnly(cmd.Context(), cfg)
	if err != nil {
		return nil, nil, err
	}
	st, _ := cluster.ReadStatus(cfg)
	return &nodeEnv{cfg: cfg, configPath: config.ResolvePath(configPath), reg: reg, status: st, now: time.Now,
		out: cmd.OutOrStdout(), errw: cmd.ErrOrStderr(), in: cmd.InOrStdin()}, reg.Close, nil
}

// writableNodeEnv opens the registry for writing, on the leader only.
func writableNodeEnv(cmd *cobra.Command) (*nodeEnv, func(), error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	if rec, err := cluster.ReadFenced(cfg); err == nil && rec != nil {
		return nil, nil, fmt.Errorf("this node is fenced (%s); it cannot run this command. Run `supavise node rejoin`", rec.Reason)
	}
	dsns := app.RegistryDSNs(cfg)
	if dsn := os.Getenv(envRegistryDSN); dsn != "" {
		dsns = []string{dsn}
	}
	var last error
	for _, dsn := range dsns {
		rec, err := cluster.InRecovery(cmd.Context(), dsn)
		if err != nil {
			last = err
			continue
		}
		if rec {
			return nil, nil, errors.New("this node is not the leader: its system cluster is a standby. Run this command on the leader")
		}
		reg, err := registry.Open(cmd.Context(), dsn)
		if err != nil {
			return nil, nil, err
		}
		kb, err := os.ReadFile(cfg.KeyPath)
		if err != nil {
			reg.Close()
			return nil, nil, fmt.Errorf("the master key: %w", err)
		}
		sec, err := secrets.Load(kb)
		if err != nil {
			reg.Close()
			return nil, nil, err
		}
		st, _ := cluster.ReadStatus(cfg)
		return &nodeEnv{cfg: cfg, configPath: config.ResolvePath(configPath), reg: reg, sec: sec, status: st, now: time.Now,
			out: cmd.OutOrStdout(), errw: cmd.ErrOrStderr(), in: cmd.InOrStdin()}, reg.Close, nil
	}
	return nil, nil, fmt.Errorf("cannot reach the registry (is supavise-postgres@system running? set %s to use another one): %w", envRegistryDSN, last)
}

// openRegistryReadOnly connects to the registry of this server, a leader's or a follower's.
func openRegistryReadOnly(ctx context.Context, cfg *config.Config) (registry.Registry, error) {
	dsns := app.RegistryDSNs(cfg)
	if dsn := os.Getenv(envRegistryDSN); dsn != "" {
		dsns = []string{dsn}
	}
	var last error
	for _, dsn := range dsns {
		reg, err := registry.OpenReadOnly(ctx, dsn)
		if err == nil {
			return reg, nil
		}
		last = err
	}
	return nil, fmt.Errorf("cannot reach the registry (is supavise-postgres@system running? set %s to use another one): %w", envRegistryDSN, last)
}

// tokenInput returns the token from the argument, --token-file or standard input ("-").
func tokenInput(args []string, file string, stdin io.Reader) (string, error) {
	switch {
	case file != "" && len(args) > 0:
		return "", errors.New("give the token as an argument or with --token-file, not both")
	case file == "-" || (len(args) == 1 && args[0] == "-"):
		b, err := io.ReadAll(io.LimitReader(stdin, 8192))
		return string(b), err
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("token file: %w", err)
		}
		return string(b), nil
	case len(args) == 1:
		return args[0], nil
	}
	return "", errors.New("a join needs a token: pass it as an argument, or with --token-file (a file keeps it out of the process list)")
}

// masterKeyInput returns the master key when the joiner supplies it, in the form the key file holds
// it, or nil to receive it from the leader.
func masterKeyInput(ctx context.Context, cfg *config.Config, file string, fromEscrow bool, passFile string, stdin io.Reader) ([]byte, error) {
	switch {
	case file != "" && fromEscrow:
		return nil, errors.New("give the master key with --master-key-file or --key-from-escrow, not both")
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("master key file: %w", err)
		}
		if _, err := secrets.Load(b); err != nil {
			return nil, err
		}
		return b, nil
	case fromEscrow:
		if passFile == "" {
			return nil, errors.New("--key-from-escrow needs --passphrase-file")
		}
		pass, err := readPassphrase(passFile, stdin)
		if err != nil {
			return nil, err
		}
		st, err := backup.OpenStore(ctx, cfg.Backup)
		if err != nil {
			return nil, fmt.Errorf("the key escrow is read from the backup store, and [backup] in config.toml does not open: %w", err)
		}
		c, err := backup.GetKeyEscrow(ctx, st, pass, "")
		if err != nil {
			return nil, err
		}
		return []byte(c.MasterKey), nil
	}
	return nil, nil
}

// ---- the commands ----

// runNodeToken prints a token and gives the founding server its cluster identity if it has none.
// The token comes first: a refusal (a file:// backend, no address to dial, a taken name) must not
// leave the server with an identity that restarts its daemon into cluster mode.
func runNodeToken(ctx context.Context, env *nodeEnv, o cluster.TokenOptions) error {
	ca, err := cluster.NewCA(env.sec)
	if err != nil {
		return err
	}
	self, err := env.reg.GetNode(ctx, registry.FounderNodeID)
	if err != nil {
		return err
	}
	a := &cluster.Authority{Reg: env.reg, CA: ca, Secrets: env.sec, Cfg: env.cfg, Topology: cluster.Solo(*self), Now: env.now}
	tok, err := a.IssueToken(ctx, o)
	if err != nil {
		return err
	}
	created, err := cluster.EnsureFounder(ctx, env.reg, ca, env.cfg, env.configPath, version, env.now())
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintln(env.errw, "this server now has a cluster identity; supavise.service restarts once to listen on the peer port")
	}
	fmt.Fprintln(env.errw, "On the new server run (the token works once):")
	fmt.Fprintln(env.errw, "  sudo supavise node join --token-file <file holding the token>")
	fmt.Fprintf(env.errw, "The peer port (%s) must be reachable from the new server.\n", env.cfg.PeerListen())
	fmt.Fprintln(env.out, tok)
	return nil
}

// nodeRow is a node in the output of `node ls`.
type nodeRow struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Role       string  `json:"role"`
	State      string  `json:"state"`
	Region     string  `json:"region,omitempty"`
	Version    string  `json:"version,omitempty"`
	PeerAddr   string  `json:"peer_addr,omitempty"`
	PublicHost string  `json:"public_host,omitempty"`
	Self       bool    `json:"self,omitempty"`
	Connected  *bool   `json:"connected,omitempty"`
	RTTMillis  float64 `json:"rtt_ms,omitempty"`
	Projects   int     `json:"projects"`
	Replicas   int     `json:"replicas"`
	CertSerial string  `json:"cert_serial,omitempty"`
}

// nodeRows lists the nodes with their role, what lives on them and, when the daemon's status is fresh,
// whether this node has a session with each.
func nodeRows(ctx context.Context, env *nodeEnv) ([]nodeRow, *registry.Cluster, error) {
	cl, err := env.reg.GetCluster(ctx)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := env.reg.ListNodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	ps, err := env.reg.ListProjects(ctx)
	if err != nil {
		return nil, nil, err
	}
	rs, err := env.reg.ListReplicas(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	self := ""
	var peers map[string]cluster.PeerStatus
	if st := env.status; st != nil && env.now().Sub(st.At) < cluster.StatusStale {
		self = st.Node
		peers = map[string]cluster.PeerStatus{}
		for _, p := range st.Peers {
			peers[p.Node] = p
		}
	}
	var rows []nodeRow
	for _, n := range nodes {
		r := nodeRow{ID: n.ID, Name: n.Name, State: string(n.State), Region: n.Region, Version: n.Version, PeerAddr: n.PeerAddr, PublicHost: n.PublicHost,
			Self: n.ID == self, CertSerial: n.CertSerial, Role: "follower"}
		if n.ID == cl.Leader {
			r.Role = "leader"
		}
		if n.State != registry.NodeActive {
			r.Role = "-"
		}
		for _, p := range ps {
			if p.NodeID == n.ID {
				r.Projects++
			}
		}
		for _, x := range rs {
			if x.NodeID == n.ID {
				r.Replicas++
			}
		}
		if p, ok := peers[n.ID]; ok {
			c := p.Connected
			r.Connected, r.RTTMillis = &c, p.RTTMillis
		}
		rows = append(rows, r)
	}
	return rows, cl, nil
}

// runNodeLs prints the nodes, or the DNS records the cluster needs, or JSON.
func runNodeLs(ctx context.Context, env *nodeEnv, dns, asJSON bool) error {
	rows, cl, err := nodeRows(ctx, env)
	if err != nil {
		return err
	}
	if dns {
		nodes, _ := env.reg.ListNodes(ctx)
		recs := cluster.DNSRecords(env.cfg, cl, nodes)
		if asJSON {
			return printJSON(env.out, recs)
		}
		if len(recs) == 0 {
			fmt.Fprintln(env.out, "no DNS records are needed beyond the ones for the service names; set domain in config.toml")
			return nil
		}
		t := newTable(env.out)
		fmt.Fprintln(t, "NAME\tTYPE\tVALUE\tFOR")
		for _, r := range recs {
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", r.Name, r.Type, r.Value, r.Reason)
		}
		return t.Flush()
	}
	if asJSON {
		return printJSON(env.out, map[string]any{"cluster": cl.Name, "epoch": cl.Epoch, "leader": cl.Leader, "nodes": rows})
	}
	t := newTable(env.out)
	fmt.Fprintln(t, "ID\tNAME\tROLE\tSTATE\tREGION\tVERSION\tADDRESS\tPROJECTS\tREPLICAS\tSESSION")
	for _, r := range rows {
		mark := ""
		if r.Self {
			mark = " (this node)"
		}
		session := "-"
		if r.Self {
			session = "this node"
		} else if r.Connected != nil && *r.Connected {
			session = fmt.Sprintf("up %.0f ms", r.RTTMillis)
		} else if r.Connected != nil {
			session = "down"
		}
		fmt.Fprintf(t, "%s\t%s%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n", r.ID, r.Name, mark, r.Role, r.State, orDash(r.Region), orDash(r.Version), orDash(r.PeerAddr), r.Projects, r.Replicas, session)
	}
	return t.Flush()
}

// runNodeRm removes a node, by id or by name, after a confirmation.
func runNodeRm(ctx context.Context, env *nodeEnv, which string, yes bool, o cluster.RemoveOptions) error {
	n, err := env.reg.GetNode(ctx, which)
	if errors.Is(err, registry.ErrNotFound) {
		n, err = env.reg.GetNodeByName(ctx, which)
	}
	if errors.Is(err, registry.ErrNotFound) {
		return fmt.Errorf("there is no node %q (see `supavise node ls`)", which)
	}
	if err != nil {
		return err
	}
	if !yes {
		fmt.Fprintf(env.errw, "Remove node %s (%s) from the cluster? Its replicas are removed and its certificate stops working. [y/N] ", n.ID, n.Name)
		line, _ := bufio.NewReader(env.in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("not removed")
		}
	}
	o.Log = func(format string, a ...any) { fmt.Fprintf(env.errw, format+"\n", a...) }
	if err := cluster.RemoveNode(ctx, env.reg, n.ID, o); err != nil {
		return err
	}
	fmt.Fprintf(env.out, "node %s (%s) was removed; it retires itself when it next reaches the leader\n", n.ID, n.Name)
	return nil
}
