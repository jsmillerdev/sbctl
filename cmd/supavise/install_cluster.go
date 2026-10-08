package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/hostsetup"
)

// firstBootAWS is `supavise install --aws-first-boot`: the data volume and its mounts, on an EC2
// instance, before the installer reads or writes anything under /etc/supavise.
func firstBootAWS(ctx context.Context, out io.Writer, o installOptions) (*hostsetup.FirstBootResult, error) {
	c, err := awsapi.New(awsapi.Config{})
	if err != nil {
		return nil, err
	}
	fb := &hostsetup.FirstBoot{
		StateDir: config.DefaultStateDir, ConfigDir: filepath.Dir(config.DefaultPath), UnitDir: defaultUnitDir,
		Device: o.DataDevice, ByID: "/dev/disk/by-id", Fstab: "/etc/fstab", User: installUser,
		IMDS: c.IMDS, Runner: hostsetup.ExecRunner{}, Timeout: 10 * time.Minute, Poll: 5 * time.Second, Out: out,
	}
	return fb.Run(ctx)
}

// checkInstallFlags refuses the first-boot and join flags that cannot be meant: an empty
// --join-token-file (an unset shell variable would turn a join into a founding install), and a
// --data-device that is not a path under /dev (mkfs.xfs, blkid and mount take it as an argument) or
// that goes without --aws-first-boot. It runs before anything changes.
func checkInstallFlags(changed func(string) bool, o installOptions) error {
	if changed("join-token-file") && o.JoinTokenFile == "" {
		return errors.New("--join-token-file needs a path")
	}
	if o.DataDevice != "" {
		if !o.AWSFirstBoot {
			return errors.New("--data-device belongs to --aws-first-boot")
		}
		if !strings.HasPrefix(o.DataDevice, "/dev/") || filepath.Clean(o.DataDevice) != o.DataDevice {
			return fmt.Errorf("--data-device %q is not a device node under /dev", o.DataDevice)
		}
	}
	return nil
}

// maxTokenBytes bounds a join token file: a token is a short line.
const maxTokenBytes = 4096

// checkJoinOptions refuses what cannot go with --join-token-file, and a token file that is not
// usable, before the install changes anything.
func checkJoinOptions(o installOptions) error {
	if o.KeyPassphraseFile != "" {
		return errors.New("--key-passphrase-file does not go with --join-token-file: the leader holds the master key and its encrypted copy")
	}
	_, err := readJoinToken(o.JoinTokenFile)
	return err
}

// readJoinToken reads the token file: owner-only, one line, not empty.
func readJoinToken(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("--join-token-file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("--join-token-file: %s is not a file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("--join-token-file: %s is mode %04o; the token file must be readable by its owner only (chmod 600 %s)", path, fi.Mode().Perm(), path)
	}
	if fi.Size() > maxTokenBytes {
		return nil, fmt.Errorf("--join-token-file: %s is larger than %d bytes, which no token is", path, maxTokenBytes)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--join-token-file: %w", err)
	}
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 || strings.ContainsAny(string(b), " \t\r\n") {
		return nil, fmt.Errorf("--join-token-file: %s does not hold a single token", path)
	}
	return b, nil
}

// clusterJoiner joins this server to a cluster. The default runs `supavise node join` as the
// supavise user, which owns the files the join writes; a test replaces it.
type clusterJoiner interface {
	// Join joins with the token in tokenFile, or continues a join that stopped after the
	// certificate was issued when resume is set (no token is needed then).
	Join(ctx context.Context, tokenFile string, resume bool) error
}

type cliJoiner struct{ in *installer }

func (j cliJoiner) Join(_ context.Context, tokenFile string, resume bool) error {
	args := []string{"node", "join"}
	if resume {
		args = append(args, "--resume")
	} else {
		args = append(args, "--token-file", tokenFile)
	}
	return j.in.asSupavise(nil, args...)
}

// joined reports whether this server already holds a node certificate: an earlier join got that
// far, and a re-run continues it instead of asking for a token again.
func joined(configPath string) bool {
	_, err := os.Stat(filepath.Join(config.ClusterDir(configPath), config.NodeCertFile))
	return err == nil
}

// stageToken copies the token to a file the supavise user can read in a directory only it can
// enter, so that the token never goes on a command line or into a world-readable place, and returns
// the path with the function that removes it. The operator's own file stays as it was.
func stageToken(token []byte, uid, gid int) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "supavise-join-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	if err := os.Chown(dir, uid, gid); err != nil {
		cleanup()
		return "", nil, err
	}
	path = filepath.Join(dir, "token")
	if err := writeFileAtomic(path, append(token, '\n'), 0o600, uid, gid); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

// joinCluster is the part of the install that replaces the system project: the server joins the
// leader the token names, then its daemon runs.
func (in *installer) joinCluster(o installOptions, configChanged, existed bool, uid, gid int) error {
	if err := in.joinStep(cliJoiner{in}, o.JoinTokenFile, config.DefaultPath, uid, gid); err != nil {
		return err
	}
	if err := in.startDaemon(configChanged, existed); err != nil {
		return err
	}
	if err := in.waitActive(30 * time.Second); err != nil {
		_ = in.run("journalctl", "-u", "supavise.service", "-n", "40", "--no-pager")
		return err
	}
	printJoinSummary(in.out)
	return nil
}

// joinStep joins with j: it continues a join that got as far as a certificate, and otherwise hands
// the token to j in a private file that is removed when the join returns.
func (in *installer) joinStep(j clusterJoiner, tokenFile, configPath string, uid, gid int) error {
	if joined(configPath) {
		in.step("this server has joined the cluster before: continuing the join")
		return j.Join(in.ctx, "", true)
	}
	in.step("joining the cluster")
	token, err := readJoinToken(tokenFile)
	if err != nil {
		return err
	}
	path, cleanup, err := stageToken(token, uid, gid)
	if err != nil {
		return err
	}
	defer cleanup()
	return j.Join(in.ctx, path, false)
}

// waitActive waits until supavise.service has been active for a few seconds in a row: a daemon that
// starts and dies is restarted by systemd and reads as activating in between. The joiner's admin
// port is the leader's, so the readiness probe of a founder cannot tell.
func (in *installer) waitActive(d time.Duration) error {
	deadline := time.Now().Add(d)
	stable := 0
	for time.Now().Before(deadline) {
		out, _ := exec.CommandContext(in.ctx, "systemctl", "is-active", "supavise.service").Output()
		if strings.TrimSpace(string(out)) == "active" {
			if stable++; stable >= 3 {
				return nil
			}
		} else {
			stable = 0
		}
		select {
		case <-in.ctx.Done():
			return in.ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("supavise.service is not running after %s", d)
}

func printJoinSummary(w io.Writer) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "This server asked to join the cluster, and supavise.service is running.")
	fmt.Fprintln(w, "  `supavise node ls` shows it as joining until its copy of the registry streams, then as active.")
	fmt.Fprintln(w, "  The dashboard and the Management API stay on the leader; add read replicas there.")
}
