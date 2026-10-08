package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/app"
	"github.com/supavise/supavise/internal/artifacts"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/units"
	"github.com/supavise/supavise/internal/versions"
)

// stopLocalFn and leadsHere are what the join and the rejoin use to stop the units of this server and to
// ask whether it leads its cluster; the tests of the commands replace them.
var (
	stopLocalFn = stopLocal
	leadsHere   = cluster.LeadsHere
)

// hostReady refuses a command that turns the cluster features on while the host layer is behind this
// binary (design 2.15.1): the peer port, the units' rights on the cluster directory and the rest are
// what `supavise system converge` brings. A node whose marker cannot be read is not called behind.
func hostReady(cfg *config.Config) error {
	if st := hostsetup.StatusOf(cfg.StateDir); st.Behind() {
		return fmt.Errorf("the host is at converge revision %d and this release needs %d; run `sudo supavise system converge` first, because the cluster features stay off until it has run", st.Have, st.Want)
	}
	return nil
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
once and expires after --ttl. Refused on a node that is not the leader, when [backup] is a file://
backend and while the host is behind this release (run "supavise system converge" first).

The first token also gives this server its cluster identity (a certificate signed by the CA that the
master key derives); the daemon restarts once to listen on the peer port.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The first token gives this server its cluster identity and restarts the daemon to listen on
			// the peer port, so it waits for a converged host like the join does, before the registry is
			// opened.
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := hostReady(cfg); err != nil {
				return err
			}
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
	var joinResume, joinReset, joinKeyFromEscrow, joinYes bool
	join := &cobra.Command{
		Use:   "join [token]",
		Short: "Join this server to a cluster with a token from the leader",
		Long: `Connects to the leader named in the token, checks that it presents the cluster CA the token
pins, proves it holds the token's secret, and receives a node certificate, the master key, the cluster's
settings and the first copy of the system cluster. The node is "joining" until its copy streams, then
"active".

A join that stopped after the certificate was issued continues with --resume, which needs no token.
A server that holds the identity of a join that was given up on (the leader removes a node that is still
joining after an hour), of a node that cannot rejoin, or of a node that was removed while it was down,
starts over with --reset and a new token: it stops what runs, sets its data aside and joins as a new node;
run "supavise node rm" on the leader first when the old node is still listed there. --reset checks the
token and the master key, lists the data it will set aside and asks for confirmation; --yes answers it,
and is required on a server that leads its cluster. It then looks at the leader, stops what runs and sets
the data aside before it asks the leader to admit the node, so a refusal that only the leader can make
(the token was used already, the name is taken, the release is outside the window) leaves this server
stopped with its data set aside; the join then runs again with a new token. Without a connection to the
key's holder, --master-key-file or --key-from-escrow supply the master key; the token still authenticates
the node. The token may be read from a file (--token-file) so that it never appears in a process listing.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := hostReady(cfg); err != nil {
				return err
			}
			log := newLogger(cfg)
			standby := openStandby(log, true)
			defer standby.Close()
			o := cluster.JoinOptions{Cfg: cfg, ConfigPath: config.ResolvePath(configPath), Resume: joinResume, Reset: joinReset, Region: joinRegion, Address: joinAddress,
				Version: version, Log: log, Seed: standby.Seed, Preflight: standby.Preflight, DSNs: app.RegistryDSNs(cfg)}
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
			// The inputs that only root could read are read. The seeding builds data that the supavise
			// user's units run on, so a command that sudo started becomes that user from here on.
			if err := runAsSupavise(); err != nil {
				return err
			}
			// A reset throws away the identity this server holds, so it is asked before anything is
			// stopped or moved: after the inputs are checked, with the data listed, and refused on a
			// leader without --yes.
			if joinReset && !joinResume && cluster.Joined(config.ClusterDir(o.ConfigPath)) {
				removed, err := confirmJoinReset(cmd, &o, joinYes, joinTokenFile == "-" || (len(args) == 1 && args[0] == "-"))
				if err != nil {
					return err
				}
				if !removed {
					stop, closeUnits, err := stopLocalFn(cfg, o.Log)
					if err != nil {
						return err
					}
					defer closeUnits()
					o.StopLocal = stop
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
	join.Flags().BoolVar(&joinReset, "reset", false, "give up this server's place in the cluster it joined before (its data is set aside) and join as a new node")
	join.Flags().BoolVarP(&joinYes, "yes", "y", false, "with --reset: do not ask for confirmation, and reset a server that is the leader")

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
		Long: `Marks the node left, which revokes its certificate at the next handshake, and removes its replicas.
The node is marked first: while it is active the replica controller would make a replica again for each
one that goes. A node that is up retires itself when its copy of the registry shows the change: it stops
what runs, sets its data aside and waits to be joined again. A node that was down or cut off from the
leader when this ran does not learn it; once it is reachable again its peers refuse it, and it is reset
on that server with "supavise node join --reset" and a new token.

The replica controller removes the replicas while this command waits; if the wait ends first, the node
is removed already and the controller finishes. --force deletes their rows at once, for a node that
cannot be reached. A node that is the home of a project is refused: move the projects first.`,
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
			if err := hostReady(cfg); err != nil {
				return err
			}
			// The seeding builds data that the supavise user's units run on: a command that sudo started
			// becomes that user before it stops anything.
			if err := runAsSupavise(); err != nil {
				return err
			}
			log := newLogger(cfg)
			stop, closeUnits, err := stopLocalFn(cfg, log)
			if err != nil {
				return err
			}
			defer closeUnits()
			standby := openStandby(log, false)
			defer standby.Close()
			o := cluster.RejoinOptions{Cfg: cfg, ConfigPath: config.ResolvePath(configPath), Leader: rejoinLeader, Version: version, Log: log,
				Seed: standby.Seed, Preflight: standby.Preflight, DSNs: app.RegistryDSNs(cfg), StopLocal: stop}
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
	rejoin.Flags().StringVar(&rejoinLeader, "leader", "", "host:port of the current leader (default: the leader's address this node recorded when it was fenced)")

	nodeCmd.AddCommand(token, join, ls, rm, rejoin)
	rootCmd.AddCommand(nodeCmd)
}

// confirmJoinReset is the check that comes before a `node join --reset` stops anything: the token and the
// master key are good (cluster.JoinOptions.CheckInputs), then the operator is told what the reset does to
// this server and asked, and a server that leads its cluster is refused unless yes. It reports removed
// when the server holds the identity of a node that was removed from its cluster: that identity is
// revoked, the join deletes it without more ado, and there is nothing to ask or to stop.
func confirmJoinReset(cmd *cobra.Command, o *cluster.JoinOptions, yes, tokenFromStdin bool) (removed bool, err error) {
	rec, _ := cluster.ReadFenced(o.Cfg)
	if rec != nil && rec.Removed {
		return true, nil
	}
	if err := o.CheckInputs(); err != nil {
		return false, err
	}
	// A fenced node does not lead, and its database is stopped, which would leave the question open.
	lead := cluster.LeadsNo
	if rec == nil {
		lead = leadsHere(cmd.Context(), o.Cfg, o.ConfigPath, app.RegistryDSNs(o.Cfg))
	}
	err = confirmReset(cmd.OutOrStdout(), cmd.InOrStdin(), lead, cluster.LocalData(o.Cfg), o.Cfg.Failover.KeepDiverged(), yes)
	if err != nil && tokenFromStdin && !yes {
		err = fmt.Errorf("%w (the token was read from standard input, which leaves nothing to answer with: pass --yes)", err)
	}
	return false, err
}

// confirmReset says what `node join --reset` does to this server and asks the operator to agree; yes
// agrees in advance. A server that leads its cluster is refused without yes, whatever is typed: its
// projects stop, and the cluster has no leader until another node is promoted. So is a server whose
// leadership could not be checked, because a leader whose database is down is still a leader.
func confirmReset(w io.Writer, in io.Reader, lead cluster.Leadership, data []string, keep time.Duration, yes bool) error {
	fmt.Fprintf(w, "--reset gives up this server's place in its cluster: it stops everything that runs here, sets the data below aside (kept %d days, then removed), deletes the node's cluster identity and joins as a new node.\n", int(keep.Hours()/24))
	for _, d := range data {
		fmt.Fprintf(w, "  %s\n", d)
	}
	if len(data) == 0 {
		fmt.Fprintln(w, "  (no project data on this server)")
	}
	switch lead {
	case cluster.LeadsYes:
		fmt.Fprintln(w, "This server is the LEADER of its cluster: its projects stop and the cluster has no leader until another node is promoted.")
		if !yes {
			return errors.New("not reset: this server leads its cluster; promote another node first, or pass --yes")
		}
		return nil
	case cluster.LeadsUnknown:
		fmt.Fprintln(w, "Whether this server is the LEADER of its cluster could not be checked: its system cluster, or the registry in it, did not answer. If it is, its projects stop and the cluster has no leader until another node is promoted.")
		if !yes {
			return errors.New("not reset: it could not be checked whether this server leads its cluster; start the system cluster and try again, or pass --yes")
		}
		return nil
	}
	if yes {
		return nil
	}
	fmt.Fprint(w, "Go ahead? [y/N] ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("not reset")
	}
	return nil
}

// stopLocal returns the function that stops everything on this server that could write as a primary
// (cluster.FenceLocal), and the function that lets go of the supervisor it uses.
func stopLocal(cfg *config.Config, log *slog.Logger) (func(context.Context) error, func(), error) {
	sup, err := units.New(cfg, log)
	if err != nil {
		return nil, nil, fmt.Errorf("the units cannot be reached to stop them: %w", err)
	}
	closeUnits := func() {
		if c, ok := sup.(interface{ Close() }); ok {
			c.Close()
		}
	}
	return func(ctx context.Context) error { _, err := cluster.FenceLocal(ctx, cfg, sup, log); return err }, closeUnits, nil
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
