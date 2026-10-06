package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/OWNER/sbctl/internal/backup"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// envRegistryDSN names the DSN of the registry database ("sbctl" in the system
// cluster) for commands that run outside the sbctl daemon.
const envRegistryDSN = "SBCTL_REGISTRY_DSN"

// openRegistry connects to the registry. The default reads SBCTL_REGISTRY_DSN; the
// integration step may replace it with whatever the daemon uses to find the system cluster.
var openRegistry = func(ctx context.Context, _ *config.Config) (registry.Registry, error) {
	dsn := os.Getenv(envRegistryDSN)
	if dsn == "" {
		return nil, fmt.Errorf("registry DSN unknown: set %s", envRegistryDSN)
	}
	return registry.Open(ctx, dsn)
}

// newLifecycleManager builds the lifecycle.Manager that `backups restore` needs. It
// is nil until the lifecycle workstream registers one from its own cmd file.
var newLifecycleManager func(ctx context.Context, cfg *config.Config, reg registry.Registry, sec secrets.Secrets) (lifecycle.Manager, error)

func init() {
	backups := &cobra.Command{Use: "backups", Short: "Base backups, retention and point-in-time restore"}

	var createReason string
	create := &cobra.Command{
		Use:   "create <ref>",
		Short: "Take a base backup of a project now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
	create.Flags().StringVar(&createReason, "reason", backup.ReasonManual, "reason recorded in the manifest: manual, scheduled or final")

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
		Use:   "restore <ref> --to <RFC3339 time> [--as <newref>] [--force]",
		Short: "Restore a project to a point in time",
		Long: "Builds a cluster from the newest base backup before --to and replays archived WAL up\n" +
			"to --to. With --as <newref> the result is a new project (the source is untouched).\n" +
			"Without --as the project itself is replaced, which needs --force; its old data\n" +
			"directory is kept next to it as <dir>.pre-restore-<time>.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := time.Parse(time.RFC3339, restoreTo)
			if err != nil {
				return fmt.Errorf("--to must be an RFC3339 timestamp such as 2026-10-06T14:30:00Z: %w", err)
			}
			if (restoreAs == "" || restoreAs == args[0]) && !restoreForce {
				return backup.ErrForceRequired
			}
			svc, closeFn, err := openBackupService(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			p, err := svc.RestoreWith(cmd.Context(), args[0], target, restoreAs, backup.RestoreOptions{Force: restoreForce, BackupID: restoreBackup})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restored %s to %s as project %s (%s)\n", args[0], target.UTC().Format(time.RFC3339), p.Ref, p.Status)
			return nil
		},
	}
	restore.Flags().StringVar(&restoreTo, "to", "", "point in time to restore to (RFC3339)")
	restore.Flags().StringVar(&restoreAs, "as", "", "ref of the new project; empty restores in place")
	restore.Flags().BoolVar(&restoreForce, "force", false, "allow replacing the project's own data (in-place restore)")
	restore.Flags().StringVar(&restoreBackup, "backup-id", "", "use this base backup instead of the newest one before --to")
	_ = restore.MarkFlagRequired("to")

	backups.AddCommand(create, list, prune, restore)
	rootCmd.AddCommand(backups)
}

// openBackupService wires a Service from config, the registry and the master key.
// withManager also builds the lifecycle.Manager (restore only).
func openBackupService(ctx context.Context, withManager bool) (*backup.Service, func(), error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	store, err := backup.OpenStore(ctx, cfg.Backup)
	if err != nil {
		return nil, nil, err
	}
	reg, err := openRegistry(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	// Load, never LoadOrCreate: a missing master key must not be replaced by a new one.
	body, err := os.ReadFile(cfg.KeyPath)
	if err != nil {
		reg.Close()
		return nil, nil, fmt.Errorf("master key: %w", err)
	}
	sec, err := secrets.Load(body)
	if err != nil {
		reg.Close()
		return nil, nil, err
	}
	opt := backup.Options{
		Config: cfg, Registry: reg, Store: store, Secrets: sec,
		Access: backup.AccessFromRegistry(cfg, reg, sec), ConfigPath: configPath, Version: version,
	}
	if withManager {
		if newLifecycleManager == nil {
			reg.Close()
			return nil, nil, errors.New("restore needs the lifecycle manager, which is not wired into this build")
		}
		if opt.Manager, err = newLifecycleManager(ctx, cfg, reg, sec); err != nil {
			reg.Close()
			return nil, nil, err
		}
	}
	svc, err := backup.New(opt)
	if err != nil {
		reg.Close()
		return nil, nil, err
	}
	return svc, reg.Close, nil
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
