package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/nodeupgrade"
	"github.com/supavise/supavise/internal/selfupdate"
	"github.com/supavise/supavise/internal/update"
)

// refusedBefore makes an error that stops `upgrade` or `rollback` before it has read the node a
// refusal (exit status 2): the contract has no other status for a command that changed nothing.
func refusedBefore(err error) error {
	return &nodeupgrade.Failure{Code: nodeupgrade.ExitRefused, Err: err}
}

// upgradeOptions are the options of `supavise upgrade`: the flags, the [upgrade] settings, and the
// hook that turns the run's events into alerts (upgradeNotifier). It is a function of its own so that
// a test can see the hook is there: a run without it raises no alert and says nothing about it.
func upgradeOptions(cfg *config.Config, out io.Writer, version string, check, plan, yes, unattended, includePostgres bool) nodeupgrade.Options {
	log := newLogger(cfg)
	return nodeupgrade.Options{
		Version: version, Check: check, Plan: plan, Yes: yes || unattended, Unattended: unattended, IncludePostgres: includePostgres,
		Canary: cfg.Upgrade.Canary(), Batch: cfg.Upgrade.Batch(), Keep: cfg.Upgrade.Keep(),
		Out: out, Log: log, Notify: upgradeNotifier(cfg, log),
	}
}

// rollbackOptions are the options of `supavise rollback`.
func rollbackOptions(cfg *config.Config, out io.Writer, yes bool) nodeupgrade.Options {
	log := newLogger(cfg)
	return nodeupgrade.Options{Yes: yes, Out: out, Log: log, Notify: upgradeNotifier(cfg, log)}
}

func init() {
	var (
		check, plan, yes, unattended, includePostgres bool
		target, repo, apiBase                         string
		keyFiles                                      []string
		wait                                          time.Duration
	)
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Move the node to a newer Supavise release: the binary, the shared services and the projects",
		Long: `A Supavise release is a tested bundle: a binary and the Supabase service versions pinned in
it. This command moves the whole node onto one, in these steps:

  check    finds the newest release (or --version), verifies the signature of its checksum list and
           its manifest, and refuses a jump the manifest does not allow
  plan     prints the binary change, every service version change, what restarts and the impact
  prepare  downloads and verifies the new binary and every new artifact, checks the disk, and takes a
           fresh base backup of every running project and of the system project; if any of it fails
           nothing is changed
  apply    installs the binary and restarts the daemon (a few seconds of HTTPS interruption), rolls
           the shared services one at a time, each waited for, then upgrades the projects' GoTrue
           and PostgREST: [upgrade] canary_projects first, then [upgrade] batch_size at a time, and
           stops at the first failure
  verify   waits for the node to be no worse than it was

If the new release does not come up, or a project fails, the projects the run moved go back to the
releases they ran, the previous binary is put back, and the node is checked again. A project's
PostgreSQL release moves only with --include-postgres: on hosted Supabase the owner of a project
decides when its Postgres is upgraded, and each move restarts PostgreSQL.

--check only looks for a release (it reads the installed version and nothing else, so any account can run it);
--plan also prints the plan and stops. Neither changes anything, and neither needs root. The upgrade needs root (the binary is root's); it runs everything that
touches the node's data as the supavise user.

--unattended is for a timer. It implies --yes, never asks, and refuses (exit status 2) unless
` + "`supavise status`" + ` says healthy, every running project has a backup newer than 24 hours (fresh ones
are taken anyway) and the master key has a copy in the backup backend.

Exit status: 0 upgraded, or nothing to do; 2 refused by a check, nothing changed; 3 failed and
rolled back; 4 failed and the node needs the operator. While it runs, the state is in
<state_dir>/system/upgrade.json, which ` + "`supavise status`" + ` shows.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return &nodeupgrade.Failure{Code: nodeupgrade.ExitRefused, Err: errors.New("supavise upgrade works on a Linux server install")}
			}
			apply := !check && !plan
			if apply && os.Geteuid() != 0 {
				return &nodeupgrade.Failure{Code: nodeupgrade.ExitRefused, Err: errors.New("run as root: sudo supavise upgrade")}
			}
			// Whatever stops the command before it has looked at the node is a refusal: nothing changed.
			cfg, err := loadConfig()
			if err != nil {
				return refusedBefore(err)
			}
			so := selfupdate.Options{Repo: repo, APIBase: apiBase}
			for _, f := range keyFiles {
				b, err := os.ReadFile(f)
				if err != nil {
					return refusedBefore(err)
				}
				k, err := selfupdate.ParsePublicKey(b)
				if err != nil {
					return refusedBefore(err)
				}
				so.Keys = append(so.Keys, k)
			}
			h, err := newNodeHost(cobraIO{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr(), In: cmd.InOrStdin()}, cfg, wait, so)
			if err != nil {
				return refusedBefore(err)
			}
			if apply {
				// The OS reboot in the maintenance window waits on this lock, so it never lands
				// between the swap and the verified restart; a second upgrade does not start either.
				unlock, err := update.LockHost(update.HostLockPath)
				if err != nil {
					return &nodeupgrade.Failure{Code: nodeupgrade.ExitRefused, Err: err}
				}
				defer unlock()
				// An upgrade must outlive the terminal that started it; Ctrl-C and SIGTERM still stop it,
				// and a stop after the binary was swapped rolls back.
				signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
				defer signal.Reset(syscall.SIGHUP, syscall.SIGPIPE)
			}
			return nodeupgrade.Run(cmd.Context(), h, upgradeOptions(cfg, cmd.OutOrStdout(), target, check, plan, yes, unattended, includePostgres))
		},
	}
	f := cmd.Flags()
	f.BoolVar(&check, "check", false, "only look for a newer release")
	f.BoolVar(&plan, "plan", false, "print what the upgrade would do and stop")
	f.BoolVar(&yes, "yes", false, "do not ask for confirmation")
	f.BoolVar(&unattended, "unattended", false, "for a timer: implies --yes, and refuses unless the node is healthy, backed up and its key escrowed")
	f.StringVar(&target, "version", "", "upgrade to this release tag (default: the newest release)")
	f.BoolVar(&includePostgres, "include-postgres", false, "also move the projects' PostgreSQL release (each restarts PostgreSQL)")
	f.DurationVar(&wait, "wait", 5*time.Minute, "how long the restarted daemon has to answer before the upgrade is rolled back")
	// For tests against a local release server and a throwaway key.
	f.StringVar(&repo, "repo", "", "GitHub repository to read releases from, owner/name (default "+selfupdate.DefaultRepo+")")
	f.StringVar(&apiBase, "api-base", "", "GitHub API root (tests)")
	f.StringArrayVar(&keyFiles, "public-key-file", nil, "verify against this PEM public key instead of the built-in release keys; repeat for a second key (tests)")
	_ = f.MarkHidden("api-base")
	_ = f.MarkHidden("public-key-file")
	rootCmd.AddCommand(cmd)

	var rollYes bool
	var rollWait time.Duration
	roll := &cobra.Command{
		Use:   "rollback",
		Short: "Go back to the previous kept Supavise release",
		Long: `Puts the node back on the release it ran before the last upgrade: that release's binary and
units, its shared-service releases, and the projects the upgrade moved on the releases they ran
(the older GoTrue runs on the database schema the newer one migrated; the base backup of the
upgrade is the way back for the data). The last [upgrade] keep_releases (default 3) binaries are
kept, with the releases they pin.

Registry migrations only go forward. A binary older than the registry's schema may not run on it, so
the rollback refuses (exit status 2) until the registry has been put back: restore the system
cluster from its pre-upgrade base backup by hand, with the control plane stopped (internal/backup/README.md,
"Disaster recovery of the system cluster"); the same command then goes through. A rollback that fails after it started exits with
status 4. Needs root.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return &nodeupgrade.Failure{Code: nodeupgrade.ExitRefused, Err: errors.New("supavise rollback works on a Linux server install")}
			}
			if os.Geteuid() != 0 {
				return &nodeupgrade.Failure{Code: nodeupgrade.ExitRefused, Err: errors.New("run as root: sudo supavise rollback")}
			}
			cfg, err := loadConfig()
			if err != nil {
				return refusedBefore(err)
			}
			h, err := newNodeHost(cobraIO{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr(), In: cmd.InOrStdin()}, cfg, rollWait, selfupdate.Options{})
			if err != nil {
				return refusedBefore(err)
			}
			unlock, err := update.LockHost(update.HostLockPath)
			if err != nil {
				return &nodeupgrade.Failure{Code: nodeupgrade.ExitRefused, Err: err}
			}
			defer unlock()
			signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
			defer signal.Reset(syscall.SIGHUP, syscall.SIGPIPE)
			return nodeupgrade.Rollback(cmd.Context(), h, rollbackOptions(cfg, cmd.OutOrStdout(), rollYes))
		},
	}
	roll.Flags().BoolVar(&rollYes, "yes", false, "do not ask for confirmation")
	roll.Flags().DurationVar(&rollWait, "wait", 5*time.Minute, "how long the restored daemon has to answer")
	rootCmd.AddCommand(roll)

	var infoJSON bool
	info := &cobra.Command{
		Use:   "release-info",
		Short: "Print the release this binary is: its version, the service versions it pins and its registry schema",
		Long: `The same facts the release carries in its versions.yaml and its migrations, as the binary itself
reports them. ` + "`supavise upgrade`" + ` runs the new binary with this to plan the upgrade.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			i, err := nodeupgrade.OwnInfo(version)
			if err != nil {
				return err
			}
			if infoJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(i)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "version          %s\nplatform         %s\nregistry schema  %s\n", i.Version, i.Platform, i.RegistrySchema)
			t := newTable(w)
			fmt.Fprintln(t, "SERVICE\tRELEASE")
			for _, m := range nodeupgrade.DiffPins(nil, i.Pins) {
				fmt.Fprintf(t, "%s\t%s\n", m.Service, m.To)
			}
			return t.Flush()
		},
	}
	info.Flags().BoolVar(&infoJSON, "json", false, "print JSON")
	rootCmd.AddCommand(info)
}
