package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/config"
)

const installUser = "supavise"

func init() {
	var o installOptions
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Set this server up as a Supavise node (run as root; deploy/install.sh calls it)",
		Long: `Turns a Linux server into a Supavise node. It checks the host (Ubuntu 24.04+ or Debian 12+,
glibc 2.35+, systemd), creates the supavise user, writes /etc/supavise/config.toml from the flags,
installs the systemd units and the polkit rule (` + "`supavise system converge`" + `), opens the firewall ports,
creates the system project (` + "`supavise system init`" + `), starts the shared services, enables and starts
supavise.service and prints the dashboard URL and the claim token that creates the first administrator.

It is idempotent. A flag you leave out keeps the value already in config.toml; the master key,
the registry and every project are never touched. Run it again with a flag to change that
setting (for example --email), or with no flags to repair a half-finished install.

Without --domain the node uses <public ip>.sslip.io, which works for a trial. With one, create
the DNS records the summary lists (a wildcard A record for *.api.<domain> is required).

--aws-first-boot is for an EC2 instance that a CloudFormation stack created with a data volume: before
anything else it finds the volume (the EBS disk that is not the root disk), makes an XFS file system on
it only when it is blank, mounts it at /var/lib/supavise with project quotas, binds /etc/supavise to
it, and makes the units wait for both mounts. It is safe to repeat.

--join-token-file F joins an existing cluster instead of creating a system project: the file holds the
token ` + "`supavise node token`" + ` printed on the leader. The server gets its settings, the master key and a
copy of the registry from the leader (` + "`supavise node join`" + `), and the dashboard stays on the leader.`,
		Args: cobra.NoArgs,
	}
	f := cmd.Flags()
	f.StringVar(&o.Domain, "domain", "", "base domain; empty uses <public ip>.sslip.io")
	f.StringVar(&o.PublicIP, "public-ip", "", "this server's public IP (default: detected)")
	f.StringVar(&o.Email, "email", "", "contact address for the ACME account (Let's Encrypt expiry notices)")
	f.StringVar(&o.TLS, "tls", "auto", "TLS mode: auto, dns01, http01 or off (off serves plain HTTP; for tests or behind a TLS terminator)")
	f.StringVar(&o.DNSProvider, "dns", "", "DNS provider for the wildcard certificate: route53, cloudflare, hetzner or digitalocean")
	f.StringArrayVar(&o.DNSCredentials, "dns-credential", nil, "provider credential KEY=VALUE, repeatable (api_token; route53: region, hosted_zone_id). Visible in the process list: prefer --dns-credentials-file")
	f.StringVar(&o.DNSCredFile, "dns-credentials-file", "", "file of KEY=VALUE provider credentials")
	f.StringVar(&o.Region, "region", "", "AWS region code shown for every project (default us-east-1)")
	f.StringVar(&o.S3Bucket, "s3-bucket", "", "keep backups in this S3 bucket instead of the local disk")
	f.StringVar(&o.S3Prefix, "s3-prefix", "", "key prefix inside the bucket")
	f.StringVar(&o.S3Region, "s3-region", "", "region of the bucket")
	f.StringVar(&o.S3Endpoint, "s3-endpoint", "", "S3-compatible endpoint (empty is AWS)")
	f.BoolVar(&o.S3PathStyle, "s3-path-style", false, "path-style S3 addressing (MinIO, Garage)")
	f.StringVar(&o.S3AccessKeyID, "s3-access-key-id", "", "static S3 access key id (default: the instance role or the AWS credential chain)")
	f.StringVar(&o.S3SecretAccessKey, "s3-secret-access-key", "", "static S3 secret access key; prefer --s3-credentials-file")
	f.StringVar(&o.S3CredFile, "s3-credentials-file", "", "file with access_key_id=... and secret_access_key=... lines")
	f.StringVar(&o.KeyPassphraseFile, "key-passphrase-file", "", "file with a passphrase (mode 0600, at least 12 characters): the installer then keeps an encrypted copy of the master key and config.toml in the backup backend")
	f.StringVar(&o.StudioURL, "studio-url", "", "download URL of the Studio build (tar.zst); install.sh sets it from the release")
	f.StringVar(&o.StudioSHA256, "studio-sha256", "", "SHA-256 of the Studio build")
	f.BoolVar(&o.NoStudio, "no-studio", false, "run without the dashboard")
	f.BoolVar(&o.NoFunctions, "no-functions", false, "run without Edge Functions (a new install turns them on)")
	f.BoolVar(&o.AutoUpgrade, "auto-upgrade", false, "install new releases by itself inside the maintenance window (update.mode = auto); the default only logs that a release exists. --auto-upgrade=false switches back")
	f.StringVar(&o.MaintenanceWindow, "maintenance-window", "", `weekly maintenance window in the node's time zone, for example "Sun 03:00-05:00" (default "Sun 04:00-06:00")`)
	f.BoolVar(&o.NoOSUpdates, "no-os-updates", false, "do not set up unattended OS security updates (a new install does, on Ubuntu and Debian)")
	f.StringVar(&o.OSReboot, "os-reboot", "", `reboot for OS patches: "window" inside the maintenance window (default) or "never"`)
	f.StringArrayVar(&o.Sets, "set", nil, "set any config.toml path, repeatable, for example ports.project_base=38000")
	f.DurationVar(&o.ClaimTTL, "claim-ttl", api.DefaultClaimTTL, "how long the claim token stays valid")
	f.StringVar(&o.ClaimTokenFile, "claim-token-file", "", "also write the claim token to this file (mode 0600)")
	f.StringVar(&o.Firewall, "firewall", "auto", "auto: open the ports in ufw when it is active and warn otherwise; ufw: install, enable and configure ufw (SSH stays open); none")
	f.BoolVar(&o.SkipOSCheck, "skip-os-check", false, "do not refuse an unlisted distribution (glibc 2.35+ is still required)")
	f.BoolVar(&o.AWSFirstBoot, "aws-first-boot", false, "first boot of an EC2 instance: find the data volume, make an XFS file system on it when it is blank, mount it with project quotas and bind /etc/supavise to it")
	f.StringVar(&o.DataDevice, "data-device", "", "with --aws-first-boot: the data volume's device node, when the instance has more than one EBS volume besides the root volume")
	f.StringVar(&o.JoinTokenFile, "join-token-file", "", "join the cluster whose leader printed the token in this file (mode 0600) instead of creating a system project")
	printCfg := f.Bool("print-config", false, "print the config.toml this run would write and exit; changes nothing")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := checkInstallFlags(cmd.Flags().Changed, o); err != nil {
			return err
		}
		if *printCfg {
			return printInstallConfig(cmd, o)
		}
		return runInstall(cmd, o)
	}
	rootCmd.AddCommand(cmd)
}

func printInstallConfig(cmd *cobra.Command, o installOptions) error {
	cfg, existed, err := readConfigFile(config.DefaultPath)
	if err != nil {
		return err
	}
	o.Fresh = !existed
	if err := applyInstall(cfg, o, cmd.Flags().Changed); err != nil {
		return err
	}
	b, err := renderConfig(cfg)
	if err != nil {
		return err
	}
	_, err = cmd.OutOrStdout().Write(b)
	return err
}

// installer carries the output streams and helpers of one run.
type installer struct {
	ctx      context.Context
	out, err io.Writer
	cfg      *config.Config
}

func (in *installer) step(format string, a ...any) { fmt.Fprintf(in.out, "==> "+format+"\n", a...) }
func (in *installer) warn(format string, a ...any) {
	fmt.Fprintf(in.out, "WARNING: "+format+"\n", a...)
}

// run runs a command, streaming its output.
func (in *installer) run(name string, args ...string) error {
	c := exec.CommandContext(in.ctx, name, args...)
	c.Stdout, c.Stderr = in.out, in.err
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// asSupavise runs the installed binary as the supavise user (the owner of the state directory
// and the only user the polkit rule lets drive the units).
func (in *installer) asSupavise(stdout io.Writer, args ...string) error {
	return in.asSupaviseIn(nil, stdout, args...)
}

// asSupaviseIn is asSupavise with standard input: a secret goes to the command that way, not
// through the process list.
func (in *installer) asSupaviseIn(stdin io.Reader, stdout io.Writer, args ...string) error {
	if stdout == nil {
		stdout = in.out
	}
	full := append([]string{"-u", installUser, "--", "env", "HOME=" + in.cfg.StateDir, in.cfg.BinPath, "--config", config.DefaultPath}, args...)
	c := exec.CommandContext(in.ctx, "runuser", full...)
	c.Stdin = stdin
	c.Stdout, c.Stderr = stdout, in.err
	if err := c.Run(); err != nil {
		return fmt.Errorf("supavise %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func runInstall(cmd *cobra.Command, o installOptions) error {
	if runtime.GOOS != "linux" {
		return errors.New("supavise install sets up a Linux server; use the Supabase CLI for local development")
	}
	if os.Geteuid() != 0 {
		return errors.New("run as root: sudo supavise install ...")
	}
	in := &installer{ctx: cmd.Context(), out: cmd.OutOrStdout(), err: cmd.ErrOrStderr()}
	changed := cmd.Flags().Changed

	in.step("checking the host")
	if err := preflight(o.SkipOSCheck); err != nil {
		return err
	}
	// A bad token file fails here, before the install changes anything.
	if o.JoinTokenFile != "" {
		if err := checkJoinOptions(o); err != nil {
			return err
		}
	}
	// The data volume and the bind mount of /etc/supavise come before anything reads or writes
	// either: a replacement instance finds config.toml and the master key on the volume.
	var stackName string
	if o.AWSFirstBoot {
		in.step("preparing the EC2 data volume")
		res, err := firstBootAWS(in.ctx, in.out, o)
		if err != nil {
			return err
		}
		stackName = res.StackName
	}
	// A bad passphrase file fails here, before the install changes anything.
	var keyPass []byte
	if o.KeyPassphraseFile != "" {
		var err error
		if keyPass, err = readPassphrase(o.KeyPassphraseFile, cmd.InOrStdin()); err != nil {
			return fmt.Errorf("--key-passphrase-file: %w", err)
		}
	}

	cfg, existed, err := readConfigFile(config.DefaultPath)
	if err != nil {
		return err
	}
	o.Fresh = !existed
	if err := applyInstall(cfg, o, changed); err != nil {
		return err
	}
	in.cfg = cfg
	publicIP := cfg.PublicIP
	if publicIP == "" {
		ip, derr := detectPublicIP(in.ctx)
		switch {
		case derr == nil:
			publicIP = ip
		case cfg.Domain == "":
			return derr
		default:
			in.warn("%v", derr)
		}
		if cfg.Domain == "" {
			cfg.PublicIP = publicIP
		}
	}
	// A joining server takes the domain and the TLS settings from the leader.
	if o.JoinTokenFile == "" {
		if cfg.TLS.Mode != "off" && cfg.TLS.Email == "" {
			in.warn("no --email: Let's Encrypt cannot send expiry notices")
		}
		if cfg.Domain != "" && cfg.TLS.DNSProvider == "" && cfg.TLS.Mode != "off" {
			in.warn("no --dns provider: certificates are requested per host on first use (HTTP-01), which needs every name below to resolve here first")
		}
		if cfg.Domain == "" {
			in.warn("no --domain: using %s (sslip.io), which is for trials; Let's Encrypt rate limits apply to the shared sslip.io domain", cfg.BaseDomain())
		}
	}

	in.step("creating the %s user and its directories", installUser)
	uid, gid, err := ensureUser()
	if err != nil {
		return err
	}
	for _, d := range []string{cfg.StateDir, filepath.Dir(config.DefaultPath)} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
		if err := os.Chown(d, uid, gid); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o750); err != nil {
			return err
		}
	}

	in.step("installing the binary at %s", cfg.BinPath)
	if err := ensureBinary(cfg.BinPath); err != nil {
		return err
	}
	if err := in.ensurePolkit(); err != nil {
		return err
	}

	in.step("writing %s", config.DefaultPath)
	body, err := renderConfig(cfg)
	if err != nil {
		return err
	}
	prev, _ := os.ReadFile(config.DefaultPath)
	configChanged := !bytes.Equal(prev, body)
	if configChanged {
		if err := writeFileAtomic(config.DefaultPath, body, 0o600, uid, gid); err != nil {
			return err
		}
	} else if existed {
		fmt.Fprintln(in.out, "unchanged")
	}
	// The daemon and the units read the file as supavise; a mode left by an older install
	// or by hand is put right.
	if err := os.Chown(config.DefaultPath, uid, gid); err != nil {
		return err
	}
	if err := os.Chmod(config.DefaultPath, 0o600); err != nil {
		return err
	}

	in.step("converging the host (systemd units, directories, mount protection, firewall rule for the mesh port)")
	if err := in.run(cfg.BinPath, "--config", config.DefaultPath, "system", "converge"); err != nil {
		return err
	}
	if stackName != "" {
		// The tags of the instance name its stack; the node remembers it for `supavise upgrade --aws`.
		if err := writeAWSConfig(filepath.Dir(config.DefaultPath), stackName); err != nil {
			in.warn("could not record the stack name in config.d: %v", err)
		}
	}
	if err := in.firewall(o.Firewall, existed); err != nil {
		return err
	}
	if o.Firewall != "none" {
		// ufw may have been switched on just now; converge opens the mesh port in it.
		if err := in.run(cfg.BinPath, "--config", config.DefaultPath, "system", "converge"); err != nil {
			return err
		}
	}
	in.osUpdates()

	// A join that stopped after the certificate is continued, with or without the token; a server that
	// already follows a leader is kept as it is. Neither creates a system project, and nothing above
	// created a project directory before the join, which seeds the system standby into an empty one.
	switch {
	case o.JoinTokenFile != "", joinUnfinished(config.DefaultPath):
		return in.joinCluster(o, configChanged, existed, uid, gid)
	case followsALeader(cfg):
		return in.keepFollower(configChanged, existed)
	}

	in.step("creating the system project (first run downloads Postgres and the auth service)")
	if err := in.asSupavise(nil, "system", "init"); err != nil {
		return err
	}
	in.step("starting the shared services (pooler, realtime, storage, postgres-meta, dashboard)")
	if err := in.asSupavise(nil, "fleet", "start"); err != nil {
		return err
	}

	if err := in.startDaemon(configChanged, existed); err != nil {
		return err
	}
	if err := waitDaemon(in.ctx, cfg, 5*time.Minute); err != nil {
		_ = in.run("journalctl", "-u", "supavise.service", "-n", "40", "--no-pager")
		return err
	}

	if keyPass != nil {
		in.step("keeping an encrypted copy of the master key in the backup backend")
		if err := in.asSupaviseIn(bytes.NewReader(keyPass), nil, "system", "escrow-key", "--passphrase-file", "-"); err != nil {
			return err
		}
	}
	// The summary reminds the operator while the backend has no copy of the key. A backend
	// that cannot be asked counts as having none: the reminder costs a line, silence could
	// cost the key.
	if st, err := escrowState(in.ctx, cfg); err == nil && st.covered() {
		o.KeyEscrowed = true
	}

	token, claimed, err := in.claimToken(o)
	if err != nil {
		return err
	}
	printSummary(in.out, cfg, publicIP, token, claimed, o)
	return nil
}

// startDaemon enables supavise.service and starts it, or restarts it when it runs with settings or a
// binary that are not the ones just installed.
func (in *installer) startDaemon(configChanged, existed bool) error {
	in.step("starting supavise.service")
	if err := in.run("systemctl", "enable", "supavise.service"); err != nil {
		return err
	}
	verb := "start"
	switch {
	case configChanged && existed:
		verb = "restart" // the running daemon holds the old settings
	case in.daemonStale():
		// install.sh swapped the binary file under a daemon that is still running the old
		// one, and the units were just re-rendered by the new one.
		in.step("the running daemon is not the installed binary: restarting it")
		verb = "restart"
	}
	if err := in.run("systemctl", verb, "supavise.service"); err != nil {
		_ = in.run("journalctl", "-u", "supavise.service", "-n", "40", "--no-pager")
		return err
	}
	return nil
}

// osUpdates sets up (or, after --no-os-updates, takes down) unattended OS security updates. A
// failure is a warning: the node works without them, and a re-run tries again.
func (in *installer) osUpdates() {
	if !in.cfg.Update.OSSecurityUpdates {
		// The operator opted out, or the node predates the setting. Take down Supavise's apt
		// configuration if an earlier run wrote it; the needrestart setting stays, where
		// needrestart is installed, so that a manual apt upgrade cannot restart a project.
		if err := applyOSUpdates(in.ctx, io.Discard, in.err, false); err != nil {
			in.warn("could not remove the unattended-upgrades configuration: %v", err)
		}
		return
	}
	in.step("setting up unattended OS security updates (--no-os-updates skips this)")
	if err := applyOSUpdates(in.ctx, in.out, in.err, true); err != nil {
		in.warn("unattended security updates are not set up: %v. Patch the OS yourself, or fix this and re-run the installer", err)
	}
}

// daemonStale reports whether supavise.service runs a binary other than the one at BinPath.
// Replacing the file by rename leaves the daemon on the old inode until it restarts.
func (in *installer) daemonStale() bool {
	out, err := exec.CommandContext(in.ctx, "systemctl", "show", "-p", "MainPID", "--value", "supavise.service").Output()
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return false // not running
	}
	return exeStale(filepath.Join("/proc", strconv.Itoa(pid), "exe"), in.cfg.BinPath)
}

// exeStale reports whether the process executable at procExe (a /proc/<pid>/exe link) holds
// different bytes from the file at path. Comparing contents rather than inodes matters:
// install.sh replaces the binary by rename on every run, and a re-run with the same release
// must not restart the daemon. It answers false when either cannot be read.
func exeStale(procExe, path string) bool {
	running, err := os.Stat(procExe)
	if err != nil {
		return false
	}
	installed, err := os.Stat(path)
	if err != nil {
		return false
	}
	if os.SameFile(running, installed) {
		return false
	}
	if running.Size() != installed.Size() {
		return true
	}
	a, err := fileSHA256(procExe)
	if err != nil {
		return false
	}
	b, err := fileSHA256(path)
	if err != nil {
		return false
	}
	return a != b
}

func fileSHA256(path string) ([32]byte, error) {
	var sum [32]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// preflight checks the platform before anything is changed.
func preflight(skipOS bool) error {
	switch runtime.GOARCH {
	case "amd64", "arm64":
	default:
		return fmt.Errorf("%s is not supported: amd64 or arm64 is required", runtime.GOARCH)
	}
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return fmt.Errorf("cannot read /etc/os-release: %w", err)
	}
	if err := checkOS(parseOSRelease(b)); err != nil && !skipOS {
		return err
	}
	out, err := exec.Command("ldd", "--version").CombinedOutput()
	if err != nil && len(out) == 0 {
		return fmt.Errorf("cannot run ldd to read the glibc version: %w", err)
	}
	if err := checkGlibc(string(out)); err != nil {
		return err
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return errors.New("systemd is not the init system of this host")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return errors.New("cgroup v2 is required (the memory limits of the project units need it)")
	}
	return nil
}

// ensureUser creates the supavise system user when it is missing and returns its ids.
func ensureUser() (uid, gid int, err error) {
	u, err := user.Lookup(installUser)
	if err != nil {
		out, uerr := exec.Command("useradd", "--system", "--home-dir", config.DefaultStateDir, "--no-create-home",
			"--shell", "/usr/sbin/nologin", installUser).CombinedOutput()
		if uerr != nil {
			return 0, 0, fmt.Errorf("useradd %s: %v: %s", installUser, uerr, strings.TrimSpace(string(out)))
		}
		if u, err = user.Lookup(installUser); err != nil {
			return 0, 0, err
		}
	}
	uid, _ = strconv.Atoi(u.Uid)
	gid, _ = strconv.Atoi(u.Gid)
	return uid, gid, nil
}

// ensureBinary puts the running binary at dst unless it is already there.
func ensureBinary(dst string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	if cur, err := filepath.EvalSymlinks(dst); err == nil && cur == self {
		return nil
	}
	b, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, b, 0o755, 0, 0)
}

// writeFileAtomic writes to a temp file next to path and renames it into place.
func writeFileAtomic(path string, b []byte, mode os.FileMode, uid, gid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chown(uid, gid); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// writeSecretFile writes b to path with mode 0600 even when the file already exists with a wider
// mode: os.WriteFile applies its permission only when it creates the file, so the mode is set
// on the open descriptor before the secret goes in.
func writeSecretFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ensurePolkit installs polkit when the host lacks it: the supavise user drives systemd over
// D-Bus, which needs polkit and the rule `system install-units` installs.
func (in *installer) ensurePolkit() error {
	if _, err := exec.LookPath("pkaction"); err != nil {
		if err := in.installPolkit(); err != nil {
			return err
		}
	}
	out, err := exec.CommandContext(in.ctx, "pkaction", "--version").Output()
	if err != nil {
		return fmt.Errorf("pkaction --version: %w", err)
	}
	return checkPolkitVersion(string(out))
}

var polkitVersionRe = regexp.MustCompile(`(\d+)(?:\.(\d+))?\s*$`)

// checkPolkitVersion requires polkit 121 or later. The rule that lets the supavise user manage
// its units is JavaScript; polkit 0.105 (Ubuntu 22.04) reads only .pkla files, so the rule
// would never load and every unit call from supavise would fail with access denied.
func checkPolkitVersion(out string) error {
	m := polkitVersionRe.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return fmt.Errorf("cannot read the polkit version from %q", strings.TrimSpace(out))
	}
	major, _ := strconv.Atoi(m[1])
	if major < 121 {
		v := m[1]
		if m[2] != "" {
			v += "." + m[2]
		}
		return fmt.Errorf("polkit %s is too old: 121 or later is required to load the JavaScript rule Supavise installs (Ubuntu 24.04 and Debian 12 have it)", v)
	}
	return nil
}

func (in *installer) installPolkit() error {
	in.step("installing polkit")
	env := append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	apt := func(args ...string) error {
		c := exec.CommandContext(in.ctx, "apt-get", args...)
		c.Env, c.Stdout, c.Stderr = env, in.out, in.err
		return c.Run()
	}
	if err := apt("update", "-qq"); err != nil {
		in.warn("apt-get update failed: %v", err)
	}
	if err := apt("install", "-y", "polkitd", "pkexec"); err != nil {
		if err := apt("install", "-y", "policykit-1"); err != nil {
			return fmt.Errorf("polkit is required and could not be installed: %w", err)
		}
	}
	return nil
}

// publicPorts are the TCP ports that must be reachable from outside.
func publicPorts(cfg *config.Config) []int {
	ports := []int{cfg.Ports.SupavisorSession, cfg.Ports.SupavisorTransaction}
	for _, l := range []string{cfg.Listen.HTTP, cfg.Listen.HTTPS} {
		if _, p, ok := strings.Cut(l, ":"); ok {
			if n, err := strconv.Atoi(p); err == nil {
				ports = append(ports, n)
			}
		}
	}
	return ports
}

// firewall opens the public ports in ufw. The shared services listen on more interfaces
// than they should (fleet README); a host firewall or security group that admits only
// these ports is part of the install.
func (in *installer) firewall(mode string, rerun bool) error {
	ports := publicPorts(in.cfg)
	list := make([]string, len(ports))
	for i, p := range ports {
		list[i] = strconv.Itoa(p)
	}
	switch mode {
	case "none":
		in.warn("firewall left alone: allow TCP %s only, and nothing else, from outside", strings.Join(list, ", "))
		return nil
	case "auto", "ufw":
	default:
		return fmt.Errorf("--firewall %q: want auto, ufw or none", mode)
	}
	_, err := exec.LookPath("ufw")
	if err != nil && mode == "ufw" {
		in.step("installing ufw")
		c := exec.CommandContext(in.ctx, "apt-get", "install", "-y", "ufw")
		c.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		c.Stdout, c.Stderr = in.out, in.err
		if err := c.Run(); err != nil {
			return fmt.Errorf("install ufw: %w", err)
		}
	} else if err != nil {
		in.warn("ufw is not installed: allow TCP %s only, and nothing else, in your firewall or security group", strings.Join(list, ", "))
		return nil
	}
	st, _ := exec.CommandContext(in.ctx, "ufw", "status").Output()
	active := strings.Contains(string(st), "Status: active")
	if !active && mode == "auto" && rerun {
		// A re-run of a node that is already installed: the operator made this choice at the
		// first install (nothing stores it), so remind instead of refusing every re-run.
		in.warn("ufw is installed but inactive, so no host firewall is configured: the shared services listen on more ports than the public ones. Allow TCP %s only in your security group or firewall, or re-run with --firewall ufw", strings.Join(list, ", "))
		return nil
	}
	if !active && mode == "auto" {
		// Cloud images often ship ufw installed and off. Carrying on would leave Supavisor's
		// API and its shard listeners (ephemeral ports) open on every interface, so the
		// operator decides, in the open.
		return fmt.Errorf("ufw is installed but inactive, so no host firewall is configured, and the shared services listen on more ports than the public ones (Supavisor's API and its shard listeners). Re-run with --firewall ufw (the installer enables ufw with SSH and TCP %s open) or with --firewall none (you then allow TCP %s only, from outside, in a security group or firewall of your own)", strings.Join(list, ", "), strings.Join(list, ", "))
	}
	in.step("opening TCP %s in ufw", strings.Join(list, ", "))
	if !active {
		// Enabling ufw must not lock the administrator out: SSH first.
		sshPorts := sshPorts(in.ctx)
		for _, p := range sshPorts {
			if err := in.run("ufw", "allow", strconv.Itoa(p)+"/tcp"); err != nil {
				return err
			}
		}
	}
	for _, p := range list {
		if err := in.run("ufw", "allow", p+"/tcp"); err != nil {
			return err
		}
	}
	if !active {
		return in.run("ufw", "--force", "enable")
	}
	return nil
}

// waitDaemon waits until the daemon's admin listener answers.
func waitDaemon(ctx context.Context, cfg *config.Config, d time.Duration) error {
	addr := cfg.Listen.Admin
	deadline := time.Now().Add(d)
	hc := &http.Client{Timeout: 3 * time.Second}
	var last error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/claim", nil)
		resp, err := hc.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("answered %s", resp.Status)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("supavise.service did not answer on %s within %s: %v", addr, d, last)
}

// claimToken issues the first-administrator token while nobody has claimed yet.
func (in *installer) claimToken(o installOptions) (token string, claimed bool, err error) {
	var st bytes.Buffer
	if err := in.asSupavise(&st, "claim", "status"); err != nil {
		return "", false, err
	}
	if strings.TrimSpace(st.String()) == "claimed" {
		return "", true, nil
	}
	var tok bytes.Buffer
	// --if-none: a re-run before anyone has claimed must not replace a live token. The value
	// an earlier run stored elsewhere (the CloudFormation stack's secret) would stop working.
	if err := in.asSupavise(&tok, "claim", "token", "--if-none", "--ttl", o.ClaimTTL.String()); err != nil {
		return "", false, err
	}
	token = strings.TrimSpace(tok.String())
	if token == "" {
		return "", false, nil
	}
	if o.ClaimTokenFile != "" {
		if err := writeSecretFile(o.ClaimTokenFile, []byte(token+"\n")); err != nil {
			return "", false, err
		}
	}
	return token, false, nil
}

func printSummary(w io.Writer, cfg *config.Config, ip, token string, claimed bool, o installOptions) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Supavise is running.")
	fmt.Fprintf(w, "  Dashboard   %s\n", cfg.DashboardURL())
	fmt.Fprintf(w, "  API         %s\n", cfg.APIURL())
	fmt.Fprintf(w, "  Pooler      %s (ports %d and %d)\n", cfg.PoolerHost(), cfg.Ports.SupavisorSession, cfg.Ports.SupavisorTransaction)
	if recs := dnsRecords(cfg, ip); len(recs) > 0 {
		fmt.Fprintln(w, "\nDNS records this install needs (one wildcard record for the projects):")
		for _, r := range recs {
			fmt.Fprintln(w, "  "+r)
		}
	}
	fmt.Fprintln(w)
	switch {
	case token == "" && !claimed:
		fmt.Fprintf(w, "A claim token from an earlier run is still valid, so none was issued: it is not shown again (only its hash is kept). Create the first administrator at %s/claim with it.\n", cfg.APIURL())
		fmt.Fprintln(w, "Lost it? sudo -u supavise supavise claim token   (revokes the old one)")
		fmt.Fprintln(w)
	case claimed:
		fmt.Fprintln(w, "The first administrator already exists. Invite more users with: sudo -u supavise supavise users invite <email> --role developer")
	default:
		if o.ClaimTokenFile != "" {
			// An unattended install (cloud-init) logs everything it prints; the token goes only
			// to the file, which the caller moves to a secret store.
			fmt.Fprintf(w, "Create the first administrator at %s/claim with the token in %s (works once, expires in %s).\n\n", cfg.APIURL(), o.ClaimTokenFile, o.ClaimTTL)
		} else {
			fmt.Fprintf(w, "Create the first administrator at %s/claim with this token (works once, expires in %s):\n\n  %s\n\n", cfg.APIURL(), o.ClaimTTL, token)
		}
		fmt.Fprintln(w, "Need another token later? sudo -u supavise supavise claim token")
	}
	if !o.KeyEscrowed {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "IMPORTANT: %s\n", keyReminder)
	}
}
