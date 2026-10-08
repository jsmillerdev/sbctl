package main

import (
	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/notimpl"
)

func init() {
	storageCmd := &cobra.Command{
		Use:   "storage",
		Short: "Manage where Storage keeps its objects",
	}

	var to, bucket, keyID, secret, secretARN string
	var resume, status, rollback, cleanup bool
	migrate := &cobra.Command{
		Use:   "migrate --to s3",
		Short: "Move Storage's objects from files on this node to an S3 bucket",
		Long: `Copies every project's objects to the bucket while Storage keeps serving from files, runs
catch-up passes until the difference is small, checks object counts and sizes against each project's
database, and then switches Storage to the bucket. Writes pause for the length of one Storage restart
(5 to 15 seconds) at the switch; reads continue.

Server failover needs Storage on S3. The files stay under objects.migrated-<date> for 14 days;
--rollback copies what changed back and switches back, and --cleanup deletes the files. State is kept so
that --resume continues a run that stopped.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return notimpl.For("supavise storage migrate") },
	}
	f := migrate.Flags()
	f.StringVar(&to, "to", "s3", "the backend to move to (s3)")
	f.StringVar(&bucket, "bucket", "", "the bucket (default: [fleet] storage_s3_bucket)")
	f.StringVar(&keyID, "access-key-id", "", "static access key id for the bucket")
	f.StringVar(&secret, "secret-access-key", "", "static secret access key for the bucket")
	f.StringVar(&secretARN, "secret-arn", "", "AWS Secrets Manager secret that holds the bucket and its credentials")
	f.BoolVar(&resume, "resume", false, "continue the run that stopped")
	f.BoolVar(&status, "status", false, "show where the run is")
	f.BoolVar(&rollback, "rollback", false, "switch back to files")
	f.BoolVar(&cleanup, "cleanup", false, "delete the files kept after a migration")
	migrate.MarkFlagsMutuallyExclusive("resume", "status", "rollback", "cleanup")
	migrate.MarkFlagsMutuallyExclusive("secret-arn", "access-key-id")
	migrate.MarkFlagsMutuallyExclusive("secret-arn", "secret-access-key")
	migrate.MarkFlagsRequiredTogether("access-key-id", "secret-access-key")

	storageCmd.AddCommand(migrate)
	rootCmd.AddCommand(storageCmd)
}
