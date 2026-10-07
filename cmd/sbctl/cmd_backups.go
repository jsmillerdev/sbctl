package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/OWNER/sbctl/internal/app"
	"github.com/OWNER/sbctl/internal/backup"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// envRegistryDSN overrides where commands that run outside the daemon find the registry
// (a database "sbctl" in the system cluster). Unset, they use lifecycle.RegistryDSN: the
// cluster's private unix socket as supabase_admin, which needs no password and works for
// the sbctl user the nightly timers run as.
const envRegistryDSN = "SBCTL_REGISTRY_DSN"

// openRegistry connects to the registry for commands that do not need the lifecycle engine.
func openRegistry(ctx context.Context, cfg *config.Config) (registry.Registry, error) {
	warnEnvFileMode(os.Stderr, backup.EnvFile)
	dsn := os.Getenv(envRegistryDSN)
	if dsn == "" {
		dsn = lifecycle.RegistryDSN(cfg)
	}
	reg, err := registry.Open(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the registry (is sb-postgres@system running? run `sbctl system init`; set %s to use another one): %w", envRegistryDSN, err)
	}
	return reg, nil
}

// warnEnvFileMode warns when the env file that carries the registry DSN (with its
// password) is readable by group or others. A missing file is fine: the variable may
// come from the environment.
func warnEnvFileMode(w io.Writer, path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm()&0o077 == 0 {
		return
	}
	fmt.Fprintf(w, "warning: %s is mode %04o; it holds the registry password and must be 0600 and owned by the sbctl user\n", path, fi.Mode().Perm())
}

// errRunAsRoot is returned by the wal and backups commands when run with euid 0. The
// file backend and the restored data directory are owned by the user who runs sbctl; a
// root run would leave root-owned objects that the timer (User=sbctl) can no longer
// read, prune or restore.
var errRunAsRoot = errors.New("sbctl wal and backups commands must not run as root: they would create root-owned backup files that the sbctl user cannot read; run `sudo -u sbctl sbctl ...`")

// refuseRoot returns errRunAsRoot when euid is 0.
func refuseRoot(euid int) error {
	if euid == 0 {
		return errRunAsRoot
	}
	return nil
}

// warnConfigFileMode warns when config.toml holds an S3 secret key and is readable by
// group or others. Missing or unreadable files are not this check's business.
func warnConfigFileMode(w io.Writer, path string, cfg *config.Config) {
	if cfg.Backup.S3SecretAccessKey == "" {
		return
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(w, "warning: %s is mode %04o and holds backup.s3_secret_access_key; it must be 0600 and owned by the sbctl user\n", path, fi.Mode().Perm())
	}
}

// configFilePath is the file loadConfig reads.
func configFilePath() string {
	if configPath != "" {
		return configPath
	}
	if p := os.Getenv("SBCTL_CONFIG"); p != "" {
		return p
	}
	return config.DefaultPath
}

func init() {
	backups := &cobra.Command{
		Use:   "backups",
		Short: "Base backups, retention and point-in-time restore",
		// Subcommands that define their own PersistentPreRunE must call refuseRoot too.
		PersistentPreRunE: func(*cobra.Command, []string) error { return refuseRoot(os.Geteuid()) },
	}

	var createReason string
	create := &cobra.Command{
		Use:   "create <ref>",
		Short: "Take a base backup of a project now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validReason(createReason) {
				return fmt.Errorf("--reason %q is not one of %s", createReason, strings.Join(reasons, ", "))
			}
			svc, closeFn, err := openBackupService(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			rec, err := svc.BaseBackupWith(cmd.Context(), args[0], backup.BackupOptions{Reason: createReason})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "backup %d of %s complete: %s, %s stored, WAL %s to %s (timeline %d)\n",
				rec.ID, rec.Ref, rec.Location, humanBytes(rec.SizeBytes), rec.StartLSN, rec.StopLSN, rec.Timeline)
			return nil
		},
	}
	create.Flags().StringVar(&createReason, "reason", backup.ReasonManual, "reason recorded in the manifest: "+strings.Join(reasons, ", "))

	var listStore, listJSON bool
	list := &cobra.Command{
		Use:   "list <ref>",
		Short: "List a project's base backups",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			w := cmd.OutOrStdout()
			if listStore {
				ms, err := svc.ListBackups(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if listJSON {
					return json.NewEncoder(w).Encode(ms)
				}
				tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tREASON\tSTARTED\tSTOPPED\tTIMELINE\tSTART LSN\tSTORED")
				for _, m := range ms {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", m.ID, m.Reason, m.StartTime.UTC().Format(time.RFC3339),
						m.StopTime.UTC().Format(time.RFC3339), m.Timeline, m.StartLSN, humanBytes(m.StoredBytes))
				}
				return tw.Flush()
			}
			reg, err := svc.RegistryRows(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if listJSON {
				return json.NewEncoder(w).Encode(reg)
			}
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATUS\tSTARTED\tTIMELINE\tSTART LSN\tSTOP LSN\tSTORED\tLOCATION")
			for _, b := range reg {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", b.ID, b.Status, b.StartedAt.UTC().Format(time.RFC3339),
					b.Timeline, b.StartLSN, b.StopLSN, humanBytes(b.SizeBytes), b.Location)
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&listStore, "store", false, "read the manifests in the backend instead of the registry")
	list.Flags().BoolVar(&listJSON, "json", false, "print JSON")

	prune := &cobra.Command{
		Use:   "prune [<ref>]",
		Short: "Apply retention: delete old base backups and the WAL they no longer need",
		Long: "Keeps base backups that finished within backup.retention_days plus the newest one\n" +
			"before that window, and at least one in any case. Deletes WAL older than the oldest\n" +
			"kept backup's start. Without <ref> prunes every project.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			var results []*backup.PruneResult
			if len(args) == 1 {
				r, err := svc.Prune(cmd.Context(), args[0])
				if r != nil {
					results = append(results, r)
				}
				if err != nil {
					return err
				}
			} else if results, err = svc.PruneAll(cmd.Context()); err != nil {
				printPrune(cmd, results)
				return err
			}
			printPrune(cmd, results)
			return nil
		},
	}

	var restoreTo, restoreAs, restoreBackup string
	var restoreForce bool
	restore := &cobra.Command{
		Use:   "restore <ref> --to <RFC3339 time | latest | backup> [--as <newref>] [--force]",
		Short: "Restore a project to a point in time, to the latest archived state, or to a base backup",
		Long: "Builds a cluster from the newest base backup before --to and replays archived WAL.\n\n" +
			"  --to <RFC3339 time>  stop at that time. PostgreSQL stops at the first commit after\n" +
			"                       the time, so some transaction must have committed after it\n" +
			"                       in the archive (a running source first archives its newest WAL).\n" +
			"                       If there is none, recovery fails and the restore reports an error.\n" +
			"  --to latest          replay the whole archive (a running source first archives its\n" +
			"                       newest WAL). Use this to restore a deleted project's last state.\n" +
			"  --to backup          the exact state of a base backup (--backup-id, default newest).\n\n" +
			"With --as <newref> the result is a new project (the source is untouched; <newref> must\n" +
			"have no archive of its own). Without --as the project itself is replaced, which needs\n" +
			"--force; its old data directory is kept next to it as <dir>.pre-restore-<time>.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := backup.RestoreOptions{Force: restoreForce, BackupID: restoreBackup}
			var target time.Time
			switch restoreTo {
			case "latest":
				opts.Latest = true
			case "backup":
				opts.ToBackup = true
			default:
				var err error
				if target, err = time.Parse(time.RFC3339, restoreTo); err != nil {
					return fmt.Errorf("--to must be an RFC3339 timestamp such as 2026-10-06T14:30:00Z, \"latest\" or \"backup\": %w", err)
				}
			}
			if (restoreAs == "" || restoreAs == args[0]) && !restoreForce {
				return backup.ErrForceRequired
			}
			svc, closeFn, err := openBackupService(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			p, err := svc.RestoreWith(cmd.Context(), args[0], target, restoreAs, opts)
			if err != nil {
				return err
			}
			what := target.UTC().Format(time.RFC3339)
			if restoreTo == "latest" || restoreTo == "backup" {
				what = restoreTo
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restored %s to %s as project %s (%s)\n", args[0], what, p.Ref, p.Status)
			return nil
		},
	}
	restore.Flags().StringVar(&restoreTo, "to", "", "restore target: an RFC3339 time, \"latest\" (end of the archive) or \"backup\" (a base backup)")
	restore.Flags().StringVar(&restoreAs, "as", "", "ref of the new project; empty restores in place")
	restore.Flags().BoolVar(&restoreForce, "force", false, "allow replacing the project's own data (in-place restore)")
	restore.Flags().StringVar(&restoreBackup, "backup-id", "", "use this base backup instead of the newest one before --to")
	_ = restore.MarkFlagRequired("to")

	finish := &cobra.Command{
		Use:   "finish-restore <ref>",
		Short: "Clear the recovery settings of a restored project once its recovery has finished",
		Long: "A restore leaves the recovery settings in place while the cluster is still replaying\n" +
			"WAL (the restore then reports restore.cleanup_pending). This waits for the cluster to\n" +
			"promote and removes them. It does nothing harmful on a project that is already done.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			if err := svc.FinishRestore(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: recovery finished and recovery settings cleared\n", args[0])
			return nil
		},
	}

	backups.AddCommand(create, list, prune, restore, finish)
	rootCmd.AddCommand(backups)
}

// reasons are the values `backups create --reason` accepts.
var reasons = []string{backup.ReasonManual, backup.ReasonScheduled, backup.ReasonFinal, backup.ReasonRestore}

func validReason(r string) bool { return slices.Contains(reasons, r) }

// openBackupService wires a Service from config, the registry and the master key.
// withManager also builds the lifecycle engine for restore: it opens the whole node
// (supervisor, artifacts, registry) as the daemon does.
func openBackupService(ctx context.Context, withManager bool) (*backup.Service, func(), error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	warnConfigFileMode(os.Stderr, configFilePath(), cfg)
	opts := appOptions(cfg)
	// A base backup waits until its WAL is archived, and archiving goes through the
	// daemon's relay: with the daemon down (or between restarts) this process serves
	// the sockets nobody answers while it runs.
	relay, stopRelay := app.StartWALRelay(ctx, cfg, opts.Log, true)
	if relay != nil {
		// A restored clone starts in recovery and fetches WAL through its own socket at once:
		// serve it before the cluster starts, not at the next sweep.
		opts.ArchiveReady = func(ref string) { _ = relay.Ensure(ref) }
	}
	if withManager {
		// A restore creates a project, so the Engine registers it with the shared services.
		node, err := lifecycle.Open(ctx, cfg, app.LifecycleOptions(cfg, opts))
		if err != nil {
			stopRelay()
			return nil, nil, err
		}
		opts.BindFleet(node.Registry, node.Secrets)
		svc, err := app.NewBackupService(ctx, cfg, node.Registry, node.Secrets, opts)
		if err != nil {
			node.Close()
			stopRelay()
			return nil, nil, err
		}
		svc.SetManager(node.Engine)
		return svc, func() { node.Close(); stopRelay() }, nil
	}
	reg, err := openRegistry(ctx, cfg)
	if err != nil {
		stopRelay()
		return nil, nil, err
	}
	// Load, never LoadOrCreate: a missing master key must not be replaced by a new one.
	body, err := os.ReadFile(cfg.KeyPath)
	if err != nil {
		reg.Close()
		stopRelay()
		return nil, nil, fmt.Errorf("master key: %w", err)
	}
	sec, err := secrets.Load(body)
	if err != nil {
		reg.Close()
		stopRelay()
		return nil, nil, err
	}
	svc, err := app.NewBackupService(ctx, cfg, reg, sec, opts)
	if err != nil {
		reg.Close()
		stopRelay()
		return nil, nil, err
	}
	return svc, func() { reg.Close(); stopRelay() }, nil
}

func printPrune(cmd *cobra.Command, rs []*backup.PruneResult) {
	for _, r := range rs {
		fmt.Fprintf(cmd.OutOrStdout(), "%s: kept %d base backup(s), deleted %d backup(s), %d WAL file(s), %d abandoned upload(s)\n",
			r.Ref, len(r.KeptBackups), len(r.DeletedBackups), r.DeletedWAL, r.DeletedOrphans)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
