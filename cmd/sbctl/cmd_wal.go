package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/OWNER/sbctl/internal/backup"
)

const (
	walPushTimeout  = 10 * time.Minute
	walFetchTimeout = 5 * time.Minute
)

func init() {
	var pushRef, fetchRef string

	wal := &cobra.Command{
		Use:   "wal",
		Short: "WAL archive commands used by PostgreSQL (archive_command and restore_command)",
		// Postgres runs these as its own (sbctl) user. Run by hand as root they would create
		// root-owned objects in a file backend.
		PersistentPreRunE: func(*cobra.Command, []string) error { return refuseRoot(os.Geteuid()) },
	}
	push := &cobra.Command{
		Use:   "push --ref <ref> <path>",
		Short: "Archive one WAL file (archive_command; %p)",
		Long: "Compresses the WAL file at <path> and stores it in the backup backend under <ref>.\n" +
			"Exits 0 only when the file is durable. Pushing a file identical to the archived one\n" +
			"succeeds; a different file under the same name fails.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := walService()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), walPushTimeout)
			defer cancel()
			return svc.PushWAL(ctx, pushRef, args[0])
		},
	}
	push.Flags().StringVar(&pushRef, "ref", "", "project ref the WAL belongs to")
	_ = push.MarkFlagRequired("ref")

	fetch := &cobra.Command{
		Use:   "fetch --ref <ref> <name> <dest>",
		Short: "Restore one WAL file from the archive (restore_command; %f %p)",
		Long: "Writes the archived WAL file <name> of <ref> to <dest>. A file that is not in the\n" +
			"archive exits 1; an archive that cannot be read exits 126, which aborts recovery.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := walService()
			if err != nil {
				exitFatal(err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), walFetchTimeout)
			defer cancel()
			err = svc.FetchWAL(ctx, fetchRef, args[0], args[1])
			if err != nil && !errors.Is(err, backup.ErrNoWAL) {
				cancel()
				exitFatal(err)
			}
			return err
		},
	}
	fetch.Flags().StringVar(&fetchRef, "ref", "", "project ref whose archive to read")
	_ = fetch.MarkFlagRequired("ref")

	wal.AddCommand(push, fetch)
	rootCmd.AddCommand(wal)
}

// walService builds a Service with just the backend: wal commands run once per WAL
// segment and must not open the registry or any database connection.
func walService() (*backup.Service, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := backup.OpenStore(ctx, cfg.Backup)
	if err != nil {
		return nil, err
	}
	return backup.New(backup.Options{Config: cfg, Store: store})
}

func exitFatal(err error) {
	fmt.Fprintln(os.Stderr, "sbctl:", err)
	os.Exit(backup.WALExitFatal)
}
