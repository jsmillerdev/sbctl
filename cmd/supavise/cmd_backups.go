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

	"github.com/supavise/supavise/internal/app"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// envRegistryDSN overrides where commands that run outside the daemon find the registry
// (a database "supavise" in the system cluster). Unset, they use lifecycle.RegistryDSN: the
// cluster's private unix socket as supabase_admin, which needs no password and works for
// the supavise user the nightly timers run as.
const envRegistryDSN = "SUPAVISE_REGISTRY_DSN"

// openRegistry connects to the registry for commands that do not need the lifecycle engine.
func openRegistry(ctx context.Context, cfg *config.Config) (registry.Registry, error) {
	warnEnvFileMode(os.Stderr, backup.EnvFile)
	var reg registry.Registry
	var err error
	dsn := os.Getenv(envRegistryDSN)
	standby := false
	if dsn == "" {
		if dsn, standby = localRegistry(ctx, cfg); dsn == "" {
			dsn = lifecycle.RegistryDSN(cfg)
		}
	}
	// A server that follows a leader reads a copy of the registry it cannot write, nor migrate.
	if standby {
		reg, err = registry.OpenReadOnly(ctx, dsn)
	} else {
		reg, err = registry.Open(ctx, dsn)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot reach the registry (is supavise-postgres@system running? run `supavise system init`; set %s to use another one): %w", envRegistryDSN, err)
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
	fmt.Fprintf(w, "warning: %s is mode %04o; it holds the registry password and must be 0600 and owned by the supavise user\n", path, fi.Mode().Perm())
}

// errRunAsRoot is returned by the wal and backups commands when run with euid 0. The
// file backend and the restored data directory are owned by the user who runs supavise; a
// root run would leave root-owned objects that the timer (User=supavise) can no longer
// read, prune or restore.
var errRunAsRoot = errors.New("supavise wal and backups commands must not run as root: they would create root-owned backup files that the supavise user cannot read; run `sudo -u supavise supavise ...`")

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
		fmt.Fprintf(w, "warning: %s is mode %04o and holds backup.s3_secret_access_key; it must be 0600 and owned by the supavise user\n", path, fi.Mode().Perm())
	}
}

// configFilePath is the file loadConfig reads.
func configFilePath() string {
	if configPath != "" {
		return configPath
	}
	if p := os.Getenv("SUPAVISE_CONFIG"); p != "" {
		return p
	}
	return config.DefaultPath
}

func init() {
	backups := &cobra.Command{
		Use:   "backups",
		Short: "Base backups, retention and point-in-time restore (bare: backend and master key status)",
		Args:  cobra.NoArgs,
		// Subcommands that define their own PersistentPreRunE must call refuseRoot too.
		PersistentPreRunE: func(*cobra.Command, []string) error { return refuseRoot(os.Geteuid()) },
	}

	var createReason string
	var createSkipFiles, createFilesOnly bool
	create := &cobra.Command{
		Use:   "create <ref>",
		Short: "Back up a project now: its database, then its Storage objects and Edge Functions",
		Long: "Takes a base backup of the project's database and then copies its Storage objects and\n" +
			"Edge Function deployments to the backup backend (<ref>/storage/ and <ref>/functions/).\n" +
			"The copy is incremental: only objects that are new or changed since the last run are\n" +
			"uploaded, and the snapshot it writes lists every object, so a restore matches the source.\n" +
			"The nightly timer runs this command. A node whose Storage uses its S3 backend keeps its\n" +
			"objects in your own bucket, which is not copied.\n\n" +
			"In a cluster a base backup reads the data directory of the project's home, so this command\n" +
			"takes the database of a project homed on this server only; for one homed on another server\n" +
			"the leader's daemon takes it in its nightly round, and --files-only works on the leader.\n\n" +
			"  --skip-files   the database only\n" +
			"  --files-only   Storage objects and Edge Functions only",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validReason(createReason) {
				return fmt.Errorf("--reason %q is not one of %s", createReason, strings.Join(reasons, ", "))
			}
			if createSkipFiles && createFilesOnly {
				return errors.New("--skip-files and --files-only exclude each other")
			}
			if !createFilesOnly {
				if err := requireHomeForBase(cmd.Context(), args[0]); err != nil {
					return err
				}
			}
			svc, closeFn, err := openBackupService(cmd.Context(), false, []string{args[0]})
			if err != nil {
				return err
			}
			defer closeFn()
			w := cmd.OutOrStdout()
			var errs []error
			// The files go first: the snapshot a "restore --to backup" pairs with a base backup
			// is the newest one that finished before the backup did, so it must be this run's.
			if !createSkipFiles {
				res, err := svc.BackupFiles(cmd.Context(), args[0], backup.FilesOptions{Reason: createReason})
				if res != nil {
					printFilesResult(w, args[0], res)
				}
				if err != nil {
					errs = append(errs, err)
				}
			}
			if !createFilesOnly {
				rec, err := svc.BaseBackupWith(cmd.Context(), args[0], backup.BackupOptions{Reason: createReason})
				if err != nil {
					errs = append(errs, err)
				} else {
					fmt.Fprintf(w, "backup %d of %s complete: %s, %s stored, WAL %s to %s (timeline %d)\n",
						rec.ID, rec.Ref, rec.Location, humanBytes(rec.SizeBytes), rec.StartLSN, rec.StopLSN, rec.Timeline)
				}
			}
			if err := errors.Join(errs...); err != nil {
				return err
			}
			if createReason == backup.ReasonManual {
				remindKeyEscrow(cmd.Context(), cmd.ErrOrStderr())
			}
			return nil
		},
	}
	create.Flags().StringVar(&createReason, "reason", backup.ReasonManual, "reason recorded in the manifest: "+strings.Join(reasons, ", "))
	create.Flags().BoolVar(&createSkipFiles, "skip-files", false, "back up the database only")
	create.Flags().BoolVar(&createFilesOnly, "files-only", false, "back up Storage objects and Edge Functions only")

	var listStore, listJSON, listFiles bool
	list := &cobra.Command{
		Use:   "list <ref>",
		Short: "List a project's base backups (or, with --files, its Storage and function snapshots)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService(cmd.Context(), false, nil)
			if err != nil {
				return err
			}
			defer closeFn()
			w := cmd.OutOrStdout()
			if listFiles {
				var all []backup.FilesSnapshot
				for _, kind := range []string{backup.KindStorage, backup.KindFunctions} {
					ms, err := svc.ListFilesSnapshots(cmd.Context(), args[0], kind)
					if err != nil {
						return err
					}
					all = append(all, ms...)
				}
				if listJSON {
					return json.NewEncoder(w).Encode(all)
				}
				tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tKIND\tREASON\tFINISHED\tFILES\tSIZE\tUPLOADED")
				for _, m := range all {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", m.ID, m.Kind, m.Reason, m.StopTime.UTC().Format(time.RFC3339),
						m.Files, humanBytes(m.Bytes), humanBytes(m.NewBytes))
				}
				return tw.Flush()
			}
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
	list.Flags().BoolVar(&listFiles, "files", false, "list the Storage object and Edge Function snapshots in the backend")
	list.Flags().BoolVar(&listJSON, "json", false, "print JSON")

	prune := &cobra.Command{
		Use:   "prune [<ref>]",
		Short: "Apply retention: delete old base backups and the WAL they no longer need",
		Long: "Keeps base backups that finished within backup.retention_days plus the newest one\n" +
			"before that window, and at least one in any case. Deletes WAL older than the oldest\n" +
			"kept backup's start. Without <ref> prunes every project.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService(cmd.Context(), false, nil)
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
	var restoreForce, restoreSkipFiles bool
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
			"--force; its old data directory is kept next to it as <dir>.pre-restore-<time>. The project's\n" +
			"read replicas cannot follow the restored cluster: once the restore has checked its target and\n" +
			"backup it removes them, and while one is still being removed (the leader's daemon finishes\n" +
			"it) the command stops and runs again later.\n\n" +
			"The project's Storage objects and Edge Functions come back too, from the newest nightly\n" +
			"backup at or before the target: objects return to that copy, not to the exact second (unlike\n" +
			"the database they have no log to replay), and a Storage object newer than the copy is\n" +
			"gone. A project with no such backup keeps its files as they are. --skip-files restores the\n" +
			"database only. When the files fail after the database came back, run `backups restore-files`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := backup.RestoreOptions{Force: restoreForce, BackupID: restoreBackup, SkipFiles: restoreSkipFiles,
				Progress: func(msg string) { fmt.Fprintln(cmd.ErrOrStderr(), msg) }}
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
			// A standby cannot follow a restored primary: the project's replicas go, once the restore
			// has checked what it needs and before it stops the project.
			if restoresInPlace(args[0], restoreAs) {
				opts.BeforeReplace = beforeInPlaceRestore
			}
			svc, closeFn, err := openBackupService(cmd.Context(), true, restoreRelayRefs(args[0], restoreAs))
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
	restore.Flags().BoolVar(&restoreSkipFiles, "skip-files", false, "restore the database only, not Storage objects and Edge Functions")
	_ = restore.MarkFlagRequired("to")

	var rfTo, rfInto, rfSnapshot string
	var rfForce bool
	restoreFiles := &cobra.Command{
		Use:   "restore-files <ref> --to <RFC3339 time | latest> [--into <ref>] [--force]",
		Short: "Restore a project's Storage objects and Edge Functions from the nightly backups",
		Long: "Brings back what `backups restore` brings back besides the database: the project's Storage\n" +
			"objects and Edge Function deployments, from the newest backup at or before --to. Use it\n" +
			"when the files of a restore failed, or to roll back files only.\n\n" +
			"Objects return to the last nightly copy, not to the exact second. Without --into the\n" +
			"project's own files are replaced, which needs --force: the old objects directory is kept as\n" +
			"<dir>.pre-restore-<time>, and the current deployments are snapshotted first (reason\n" +
			"pre-restore; --snapshot <id> brings one back). With --into <ref> the files go to that\n" +
			"existing project, which must hold no objects, Edge Functions or function secrets.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fo := backup.FilesRestoreOptions{SnapshotID: rfSnapshot, RequireProject: true,
				Progress: func(msg string) { fmt.Fprintln(cmd.OutOrStdout(), msg) }}
			switch {
			case rfSnapshot != "":
			case rfTo == "latest":
				fo.Latest = true
			case rfTo == "":
				return errors.New("pass --to <RFC3339 time | latest> or --snapshot <id>")
			default:
				t, err := time.Parse(time.RFC3339, rfTo)
				if err != nil {
					return fmt.Errorf("--to must be an RFC3339 timestamp such as 2026-10-06T14:30:00Z or \"latest\": %w", err)
				}
				fo.At = t
			}
			if (rfInto == "" || rfInto == args[0]) && !rfForce {
				return backup.ErrForceRequired
			}
			svc, closeFn, err := openBackupService(cmd.Context(), false, nil)
			if err != nil {
				return err
			}
			defer closeFn()
			res, err := svc.RestoreFiles(cmd.Context(), args[0], rfInto, fo)
			if err != nil {
				return err
			}
			if res.Aside != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "the replaced objects are kept in %s\n", res.Aside)
			}
			return nil
		},
	}
	restoreFiles.Flags().StringVar(&rfTo, "to", "", "restore target: an RFC3339 time or \"latest\"")
	restoreFiles.Flags().StringVar(&rfInto, "into", "", "ref of the existing project to restore into; empty replaces the project's own files")
	restoreFiles.Flags().StringVar(&rfSnapshot, "snapshot", "", "restore exactly this snapshot (see `backups list --files`)")
	restoreFiles.Flags().BoolVar(&rfForce, "force", false, "allow replacing the project's own files")

	finish := &cobra.Command{
		Use:   "finish-restore <ref>",
		Short: "Clear the recovery settings of a restored project once its recovery has finished",
		Long: "A restore leaves the recovery settings in place while the cluster is still replaying\n" +
			"WAL (the restore then reports restore.cleanup_pending). This waits for the cluster to\n" +
			"promote and removes them. It does nothing harmful on a project that is already done.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService(cmd.Context(), false, []string{args[0]})
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

	status := &cobra.Command{
		Use:   "status",
		Short: "Show where backups go and whether the master key is safe",
		Long: "Shows the backup backend and retention, and whether the node's master key has an encrypted\n" +
			"copy in the backend. The key unseals the passwords inside every backup, and it is in no\n" +
			"backup in the clear; without a copy, a lost server means passwords that cannot be opened.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return printBackupStatus(cmd) },
	}
	backups.RunE = func(cmd *cobra.Command, _ []string) error { return printBackupStatus(cmd) }

	backups.AddCommand(create, list, prune, restore, restoreFiles, finish, status)
	rootCmd.AddCommand(backups)
}

// reasons are the values `backups create --reason` accepts.
var reasons = []string{backup.ReasonManual, backup.ReasonScheduled, backup.ReasonFinal, backup.ReasonRestore, backup.ReasonUpgrade}

func validReason(r string) bool { return slices.Contains(reasons, r) }

// restoreRelayRefs lists the projects a restore needs WAL relayed for: the source (its base
// backup waits for archived WAL) and the clone, when there is one.
func restoreRelayRefs(source, as string) []string {
	if as == "" || as == source {
		return []string{source}
	}
	return []string{source, as}
}

// requireHomeForBase opens the registry for baseBackupHome.
func requireHomeForBase(ctx context.Context, ref string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	reg, err := openRegistry(ctx, cfg)
	if err != nil {
		return err
	}
	defer reg.Close()
	return baseBackupHome(ctx, reg, cfg, ref)
}

// baseBackupHome refuses a base backup of a project that is homed on another node. A base backup reads
// the data directory of the project's home, which this server does not have, and this command has no
// session with that node; the daemon of the cluster's leader takes those backups (placement.RoutedBackups)
// in its nightly round. Without it the command would fail on a missing data directory, or, worse, take
// the Storage objects and then fail.
func baseBackupHome(ctx context.Context, reg registry.Registry, cfg *config.Config, ref string) error {
	p, err := reg.GetProject(ctx, ref)
	if err != nil || p.NodeID == "" {
		return nil // a project that is not there is the backup service's to report
	}
	self, err := lifecycle.SelfNode(ctx, reg, cfg, false)
	if err != nil {
		return err
	}
	if p.NodeID == self {
		return nil
	}
	home := p.NodeID
	if n, err := reg.GetNode(ctx, p.NodeID); err == nil && n.Name != "" {
		home = n.Name
	}
	return fmt.Errorf("project %s is homed on node %s, and a base backup reads the data directory of its home: this command cannot take it from here. "+
		"The leader's daemon backs the project up in its nightly round; on the leader, --files-only backs up its Storage objects and Edge Functions", ref, home)
}

// openBackupService wires a Service from config, the registry and the master key.
// withManager also builds the lifecycle engine for restore: it opens the whole node
// (supervisor, artifacts, registry) as the daemon does.
func openBackupService(ctx context.Context, withManager bool, relayRefs []string) (*backup.Service, func(), error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	warnConfigFileMode(os.Stderr, configFilePath(), cfg)
	opts := appOptions(cfg)
	// A base backup waits until its WAL is archived, and archiving goes through the
	// daemon's relay: with the daemon down (or between restarts) this process serves
	// the sockets nobody answers while it runs.
	// Commands that only read the archive (list, prune) pass no refs and start no relay;
	// the others serve the projects they work on and no others.
	relay, stopRelay := (*backup.Relay)(nil), func() {}
	if relayRefs != nil {
		relay, stopRelay = app.StartWALRelay(ctx, cfg, opts.Log, true, relayRefs...)
	}
	if relay != nil {
		// A restored clone starts in recovery and fetches WAL through its own socket at once:
		// serve it before the cluster starts, not at the next sweep.
		opts.ArchiveReady = func(ref string) { _ = relay.Ensure(ref) }
	}
	if withManager {
		// A restore creates a project, so the Engine registers it with the shared services.
		node, err := openLifecycle(ctx, cfg, app.LifecycleOptions(cfg, opts))
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
		fmt.Fprintf(cmd.OutOrStdout(), "%s: kept %d base backup(s), deleted %d backup(s), %d WAL file(s), %d abandoned upload(s)",
			r.Ref, len(r.KeptBackups), len(r.DeletedBackups), r.DeletedWAL, r.DeletedOrphans)
		if r.DeletedFileSnapshots > 0 || r.DeletedBlobs > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "; files: deleted %d snapshot(s), %d stored file(s)", r.DeletedFileSnapshots, r.DeletedBlobs)
		}
		fmt.Fprintln(cmd.OutOrStdout())
	}
}

// printFilesResult prints what BackupFiles did, one line per kind.
func printFilesResult(w io.Writer, ref string, res *backup.FilesResult) {
	line := func(what string, m *backup.FilesSnapshot) {
		if m == nil {
			return
		}
		fmt.Fprintf(w, "%s of %s backed up: %d files, %s; %d new or changed (%s stored); snapshot %s\n",
			what, ref, m.Files, humanBytes(m.Bytes), m.NewFiles, humanBytes(m.NewBytes), m.ID)
	}
	line("objects", res.Storage)
	line("functions", res.Functions)
	for _, n := range res.Notes {
		fmt.Fprintln(w, n)
	}
}

// keyReminder is what the operator is told while the master key has no encrypted copy.
const keyReminder = "The master key is not in your backups. It unseals the passwords inside them, so a lost server cannot be rebuilt without it.\n" +
	"Run `sudo -u supavise supavise system export-key` and keep the output offline, or\n" +
	"`sudo -u supavise supavise system escrow-key --passphrase-file <file>` to keep a copy in the backup backend, encrypted with a passphrase only you know."

// escrowStatus is what the backend holds in the way of encrypted copies of master keys.
type escrowStatus struct {
	All []backup.EscrowInfo
	// NodeKeyID is the key id of this node's key, or empty when the key file cannot be read
	// by the current user.
	NodeKeyID string
	// Mine is the copy of this node's key, if the backend has one.
	Mine *backup.EscrowInfo
}

// covered reports whether the node's key has an encrypted copy in the backend. When the key
// cannot be read (so the copies cannot be compared with it), any copy counts.
func (e *escrowStatus) covered() bool {
	if e.NodeKeyID == "" {
		return len(e.All) > 0
	}
	return e.Mine != nil
}

// escrowState looks for the master key's encrypted copy in the backend. It returns an error
// when the backend could not be asked.
func escrowState(ctx context.Context, cfg *config.Config) (*escrowStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	st, err := backup.OpenStore(ctx, cfg.Backup)
	if err != nil {
		return nil, err
	}
	all, err := backup.ListKeyEscrows(ctx, st)
	if err != nil {
		return nil, err
	}
	e := &escrowStatus{All: all}
	if b, rerr := os.ReadFile(cfg.KeyPath); rerr == nil {
		e.NodeKeyID = backup.KeyID(string(b))
		for i := range all {
			if all[i].KeyID == e.NodeKeyID {
				e.Mine = &all[i]
			}
		}
	}
	return e, nil
}

// remindKeyEscrow prints keyReminder to w when the backend has no copy of the node's key. It
// says nothing when the backend cannot be asked: the backup that just ran did not need the
// answer.
func remindKeyEscrow(ctx context.Context, w io.Writer) {
	cfg, err := loadConfig()
	if err != nil {
		return
	}
	if st, err := escrowState(ctx, cfg); err == nil && !st.covered() {
		fmt.Fprintln(w, keyReminder)
	}
}

func printBackupStatus(cmd *cobra.Command) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "backend:    %s\n", cfg.Backup.Backend)
	if cfg.Backup.RetentionDays > 0 {
		fmt.Fprintf(w, "retention:  %d days\n", cfg.Backup.RetentionDays)
	} else {
		fmt.Fprintln(w, "retention:  off (nothing is pruned)")
	}
	if strings.HasPrefix(cfg.Backup.Backend, "file://") {
		fmt.Fprintln(w, "            the backend is a directory on this server: it does not survive losing the server's disk (use --s3-bucket)")
	}
	if cfg.Fleet.StorageBackend == "s3" {
		fmt.Fprintln(w, "storage:    objects are in your own S3 bucket (Storage's S3 backend); they are not copied into the backup backend")
	} else {
		fmt.Fprintln(w, "storage:    objects and Edge Functions are copied to the backend with each nightly backup")
	}
	st, err := escrowState(cmd.Context(), cfg)
	switch {
	case err != nil:
		fmt.Fprintf(w, "master key: could not check the backend for an encrypted copy: %v\n", err)
	case st.Mine != nil:
		fmt.Fprintf(w, "master key: encrypted copy in the backend, made %s, key id %s: this node's key\n", st.Mine.Created.UTC().Format(time.RFC3339), st.Mine.KeyID)
	case st.NodeKeyID == "" && len(st.All) > 0:
		fmt.Fprintf(w, "master key: %d encrypted copy(ies) in the backend; this user cannot read key_path, so they cannot be compared with this node's key\n", len(st.All))
	default:
		fmt.Fprintf(w, "master key: NOT backed up\n%s\n", keyReminder)
	}
	if err == nil {
		for _, o := range st.All {
			if o.KeyID != st.NodeKeyID && st.NodeKeyID != "" {
				fmt.Fprintf(w, "            the backend also holds the copy of another master key (key id %s, made %s); `system restore-key --key-id %s` opens it\n",
					o.KeyID, o.Created.UTC().Format(time.RFC3339), o.KeyID)
			}
		}
	}
	return nil
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
