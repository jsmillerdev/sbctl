package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/nodeupgrade"
	"github.com/supavise/supavise/internal/selfupdate"
	"github.com/supavise/supavise/internal/update"
)

func init() {
	var (
		tag       string
		repo      string
		check     bool
		force     bool
		noRestart bool
		noUnits   bool
		wait      time.Duration
		apiBase   string
		keyFiles  []string
	)
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Replace this binary with the latest release (or --version), after verifying its signature",
		Long: `Fetches the latest release of the Supavise repository on GitHub (or the one named by
--version), verifies the ed25519 signature of its SHA256SUMS against the public key built
into this binary (the current one, or the next one while a key rotation is under way), checks
the signed release manifest and the binary against their checksums, replaces /usr/local/bin/supavise
atomically (the previous binary stays beside it as supavise.prev), brings the host to what the
new binary expects (` + "`supavise system converge`" + `: its systemd units and the rest of its host layer;
` + "`system install-units`" + ` for a release from before the host layer) and restarts
supavise.service. Project units are not restarted: they belong to systemd and keep running. If the
restarted daemon does not answer on its admin listener within --wait (default 5 minutes; the installer's readiness check, because systemd
calls a daemon active the moment it forks), the previous binary is put back, the units are
rendered again with it and the service is restarted.

Needs root (the binary's directory is root's). Artifacts upgrade through versions.yaml,
not through this command. A release states the oldest version it upgrades from
(min_upgrade_from in its manifest); self-update refuses to skip past it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return errors.New("self-update replaces the binary of a Linux server install; build from source elsewhere")
			}
			// Resolve the path before the update: afterwards /proc/self/exe names the
			// replaced file as "(deleted)".
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return err
			}
			o := selfupdate.Options{ExecPath: exe, Repo: repo, APIBase: apiBase, Tag: tag, Current: version, Platform: "linux-" + runtime.GOARCH, Force: force, Out: cmd.OutOrStdout()}
			for _, f := range keyFiles {
				b, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				k, err := selfupdate.ParsePublicKey(b)
				if err != nil {
					return err
				}
				o.Keys = append(o.Keys, k)
			}
			if check {
				rel, err := selfupdate.Latest(cmd.Context(), o)
				if err != nil {
					return err
				}
				state := "up to date"
				if selfupdate.Newer(rel.Tag, version) {
					state = "update available: run `sudo supavise self-update`"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "installed %s, latest %s: %s\n", version, rel.Tag, state)
				return nil
			}
			if os.Geteuid() != 0 {
				return errors.New("run as root: sudo supavise self-update")
			}
			// Read the config before anything changes: a file the installer cannot read
			// must not leave a swapped binary beside the old daemon.
			cfg, _, err := readConfigFile(selfUpdateConfigPath())
			if err != nil {
				return err
			}
			// The OS reboot in the maintenance window waits on this lock, so it never lands
			// between the swap and the verified restart.
			unlock, err := update.LockHost(update.HostLockPath)
			if err != nil {
				return err
			}
			defer unlock()
			res, err := selfupdate.Update(cmd.Context(), o)
			if err != nil {
				return err
			}
			if !res.Replaced {
				return nil
			}
			if !noUnits {
				// The new binary carries the units and the host layer of its release; a node that is
				// already converged changes nothing. A failure is a warning here: the daemon raises
				// host_not_converged while the host is behind, and `sudo supavise system converge`
				// repeats the step. (`supavise upgrade` treats it as a failure and rolls back.)
				c := exec.CommandContext(cmd.Context(), exe, "system", "converge")
				c.Stdout, c.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
				if err := c.Run(); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: supavise system converge failed: %v\n", err)
				}
			}
			if noRestart || !serviceInstalled(cmd.Context()) {
				fmt.Fprintln(cmd.OutOrStdout(), "supavise.service was not restarted; run `systemctl restart supavise.service` to use the new binary")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "restarting supavise.service (a restart waits for running operations, up to 10 minutes)")
			if err := restartAndWait(cmd.Context(), cfg, wait); err != nil {
				if res.Previous != "" {
					return rollback(cmd, exe, res.Previous, cfg, wait, err, noUnits)
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Supavise %s is running\n", res.Tag)
			return nil
		},
	}
	cmd.Flags().StringVar(&tag, "version", "", "install this release tag (default: the latest release)")
	cmd.Flags().StringVar(&repo, "repo", "", "GitHub repository to update from, owner/name (default "+selfupdate.DefaultRepo+")")
	cmd.Flags().BoolVar(&check, "check", false, "only say whether a newer release exists")
	cmd.Flags().BoolVar(&force, "force", false, "install even when the release is not newer")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "do not restart supavise.service")
	cmd.Flags().BoolVar(&noUnits, "no-units", false, "do not refresh the systemd units")
	cmd.Flags().DurationVar(&wait, "wait", 5*time.Minute, "how long the restarted daemon has to answer before the update is rolled back")
	// For tests against a local release server and a throwaway key; a release build
	// verifies against the key compiled into the binary.
	cmd.Flags().StringVar(&apiBase, "api-base", "", "GitHub API root (tests)")
	cmd.Flags().StringArrayVar(&keyFiles, "public-key-file", nil, "verify against this PEM public key instead of the built-in release keys; repeat for a second key (tests)")
	_ = cmd.Flags().MarkHidden("api-base")
	_ = cmd.Flags().MarkHidden("public-key-file")
	rootCmd.AddCommand(cmd)
}

// hostLayerCommand is the `system` subcommand of the binary at exe that brings the host to what its
// release expects: converge for a release that has a host layer, and install-units, which renders
// the units as it always did, for one that does not (--force can install an older release, which has
// no converge and would only print the help of `system`).
func hostLayerCommand(ctx context.Context, exe string) string {
	if info, err := nodeupgrade.ProbeInfo(ctx, exe); err == nil && info.ConvergeRevision > 0 {
		return "converge"
	}
	return "install-units"
}

func serviceInstalled(ctx context.Context) bool {
	return exec.CommandContext(ctx, "systemctl", "cat", "supavise.service").Run() == nil
}

// selfUpdateConfigPath is the config file of the node: --config, SUPAVISE_CONFIG, or the default.
func selfUpdateConfigPath() string {
	if p := configPath; p != "" {
		return p
	}
	if p := os.Getenv(config.EnvConfigPath); p != "" {
		return p
	}
	return config.DefaultPath
}

// restartAndWait restarts supavise.service and waits until the daemon answers on its admin
// listener. Type=simple reports "active" at fork, and a daemon that dies later (the registry
// wait, a listener that cannot bind) is restarted by systemd in a loop, so is-active proves
// nothing; the installer's readiness probe does.
func restartAndWait(ctx context.Context, cfg *config.Config, wait time.Duration) error {
	if out, err := exec.CommandContext(ctx, "systemctl", "restart", "supavise.service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return waitDaemon(ctx, cfg, wait)
}

// rollback puts the previous binary back, re-renders the units with it (the new binary just
// installed its own), restarts the service and checks that the old daemon answers.
func rollback(cmd *cobra.Command, exe, prev string, cfg *config.Config, wait time.Duration, cause error, noUnits bool) error {
	ctx := cmd.Context()
	fmt.Fprintf(cmd.ErrOrStderr(), "supavise.service did not come back (%v); restoring the previous binary\n", cause)
	if rerr := os.Rename(prev, exe); rerr != nil {
		return fmt.Errorf("%w; and restoring %s failed: %v", cause, prev, rerr)
	}
	if !noUnits {
		c := exec.CommandContext(ctx, exe, "system", "install-units")
		c.Stdout, c.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
		if err := c.Run(); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: supavise system install-units with the restored binary failed: %v\n", err)
		}
	}
	_ = exec.CommandContext(ctx, "systemctl", "reset-failed", "supavise.service").Run()
	if err := restartAndWait(ctx, cfg, wait); err != nil {
		return fmt.Errorf("%w; the previous binary is back at %s but the service still does not answer: %v", cause, exe, err)
	}
	return fmt.Errorf("%w; rolled back to the previous binary, which is running", cause)
}
