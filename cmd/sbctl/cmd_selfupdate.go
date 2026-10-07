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

	"github.com/OWNER/sbctl/internal/selfupdate"
)

func init() {
	var (
		tag       string
		repo      string
		check     bool
		force     bool
		noRestart bool
		noUnits   bool
		apiBase   string
		keyFile   string
	)
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Replace this binary with the latest release (or --version), after verifying its signature",
		Long: `Fetches the latest release of the sbctl repository on GitHub (or the one named by
--version), verifies the ed25519 signature of its SHA256SUMS against the public key built
into this binary, checks the binary against its checksum, replaces /usr/local/bin/sbctl
atomically (the previous binary stays beside it as sbctl.prev), refreshes the systemd units
with the new binary and restarts sbctl.service. Project units are not restarted: they
belong to systemd and keep running. If sbctl.service does not come back within 30 seconds
the previous binary is put back.

Needs root (the binary's directory is root's). Artifacts upgrade through versions.yaml,
not through this command.`,
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
			if keyFile != "" {
				b, err := os.ReadFile(keyFile)
				if err != nil {
					return err
				}
				if o.Key, err = selfupdate.ParsePublicKey(b); err != nil {
					return err
				}
			}
			if check {
				rel, err := selfupdate.Latest(cmd.Context(), o)
				if err != nil {
					return err
				}
				state := "up to date"
				if selfupdate.Newer(rel.Tag, version) {
					state = "update available: run `sudo sbctl self-update`"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "installed %s, latest %s: %s\n", version, rel.Tag, state)
				return nil
			}
			if os.Geteuid() != 0 {
				return errors.New("run as root: sudo sbctl self-update")
			}
			res, err := selfupdate.Update(cmd.Context(), o)
			if err != nil {
				return err
			}
			if !res.Replaced {
				return nil
			}
			if !noUnits {
				// The new binary carries the new units; an unchanged set is a no-op.
				c := exec.CommandContext(cmd.Context(), exe, "system", "install-units")
				c.Stdout, c.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
				if err := c.Run(); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: sbctl system install-units failed: %v\n", err)
				}
			}
			if noRestart || !serviceInstalled(cmd.Context()) {
				fmt.Fprintln(cmd.OutOrStdout(), "sbctl.service was not restarted; run `systemctl restart sbctl.service` to use the new binary")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "restarting sbctl.service (a restart waits for running operations, up to 10 minutes)")
			if err := restartAndWait(cmd.Context()); err != nil {
				if res.Previous != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "sbctl.service did not come back (%v); restoring the previous binary\n", err)
					if rerr := os.Rename(res.Previous, exe); rerr != nil {
						return fmt.Errorf("%w; and restoring %s failed: %v", err, res.Previous, rerr)
					}
					_ = exec.CommandContext(cmd.Context(), "systemctl", "restart", "sbctl.service").Run()
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "sbctl %s is running\n", res.Tag)
			return nil
		},
	}
	cmd.Flags().StringVar(&tag, "version", "", "install this release tag (default: the latest release)")
	cmd.Flags().StringVar(&repo, "repo", "", "GitHub repository to update from, owner/name (default "+selfupdate.DefaultRepo+")")
	cmd.Flags().BoolVar(&check, "check", false, "only say whether a newer release exists")
	cmd.Flags().BoolVar(&force, "force", false, "install even when the release is not newer")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "do not restart sbctl.service")
	cmd.Flags().BoolVar(&noUnits, "no-units", false, "do not refresh the systemd units")
	// For tests against a local release server and a throwaway key; a release build
	// verifies against the key compiled into the binary.
	cmd.Flags().StringVar(&apiBase, "api-base", "", "GitHub API root (tests)")
	cmd.Flags().StringVar(&keyFile, "public-key-file", "", "verify against this PEM public key instead of the built-in release key (tests)")
	_ = cmd.Flags().MarkHidden("api-base")
	_ = cmd.Flags().MarkHidden("public-key-file")
	rootCmd.AddCommand(cmd)
}

func serviceInstalled(ctx context.Context) bool {
	return exec.CommandContext(ctx, "systemctl", "cat", "sbctl.service").Run() == nil
}

// restartAndWait restarts sbctl.service and waits up to 30 seconds for it to be active
// and stay so for two seconds (a crash loop passes through "active" briefly).
func restartAndWait(ctx context.Context) error {
	if out, err := exec.CommandContext(ctx, "systemctl", "restart", "sbctl.service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart: %v: %s", err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(30 * time.Second)
	stable := 0
	for time.Now().Before(deadline) {
		out, _ := exec.CommandContext(ctx, "systemctl", "is-active", "sbctl.service").Output()
		if strings.TrimSpace(string(out)) == "active" {
			if stable++; stable >= 2 {
				return nil
			}
		} else {
			stable = 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("sbctl.service is not active 30 seconds after the restart")
}
