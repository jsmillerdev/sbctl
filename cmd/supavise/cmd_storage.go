package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/storagemigrate"
)

// storageS3DropIn is the file under config.d that holds the credentials of Storage's bucket.
// config.toml never gets them.
const storageS3DropIn = "30-storage-s3.toml"

func init() {
	storageCmd := &cobra.Command{
		Use:   "storage",
		Short: "Manage where Storage keeps its objects",
	}

	var to, bucket, credsFile, roleARN string
	var rate int
	var resume, status, rollback, cleanup bool
	migrate := &cobra.Command{
		Use:   "migrate --to s3",
		Short: "Move Storage's objects from files on this node to an S3 bucket",
		Long: `Copies every project's objects to the bucket while Storage keeps serving from files, runs
catch-up passes until they are short, checks that every object each project's database lists is in
the bucket, and then switches Storage to the bucket. At the switch Storage's writes get 503 with
Retry-After 5 while a copy of what changed runs and reads continue. Then supavise-storage is
stopped, and reads fail until it starts again on the bucket. That is more than a restart (5 to 15
seconds): it also covers a last pass over the files, which looks at every file once, and a read of
every running project's rows, so it grows with the number of files. The command prints how long it
lasted. The copy runs at --rate-limit; the passes of the switch run at full speed.

The bucket's endpoint, region and addressing come from the [fleet] storage_s3_* settings
(storage_s3_endpoint, storage_s3_region, storage_s3_force_path_style); --bucket overrides
storage_s3_bucket. Credentials come from a file or from an AWS role, never from a flag:

  --credentials-file F  a file with access_key_id=... and secret_access_key=... lines that only its owner
                        can read (mode 0600 or 0400), owned by you, by root or by the supavise user.
                        The key goes into /etc/supavise/config.d/` + storageS3DropIn + ` (0600), not config.toml.
  --role-arn ARN        an IAM role this node assumes with its own credentials (the instance role).
                        The daemon serves the role's credentials to supavise-storage and reads the role when
                        it starts, so a daemon that started before the role was in the configuration needs
                        sudo systemctl restart supavise.service before Storage can use the bucket.
  neither               the role [fleet] storage_s3_role_arn names, or the static key already in [fleet].

Server failover needs Storage on S3. The files stay under objects.migrated-<date>; --rollback copies
what changed in the bucket back to them and switches back (it fetches everything when they were
deleted), and --cleanup deletes them. --status shows where a run is. A run that stopped, for any
reason, continues with --resume.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if to != "s3" {
				return fmt.Errorf("--to %q: s3 is the only backend to move to", to)
			}
			if rate < 0 {
				return errors.New("--rate-limit must not be negative")
			}
			return runStorageMigrate(cmd, storageFlags{bucket: bucket, credsFile: credsFile, roleARN: roleARN, rate: rate,
				resume: resume, status: status, rollback: rollback, cleanup: cleanup})
		},
	}
	f := migrate.Flags()
	f.StringVar(&to, "to", "s3", "the backend to move to (s3)")
	f.StringVar(&bucket, "bucket", "", "the bucket (default: [fleet] storage_s3_bucket)")
	f.StringVar(&credsFile, "credentials-file", "", "file with access_key_id=... and secret_access_key=... lines that only its owner can read (mode 0600)")
	f.StringVar(&roleARN, "role-arn", "", "IAM role to assume with this node's credentials")
	f.IntVar(&rate, "rate-limit", storagemigrate.DefaultRateMiB, "MiB per second the copy may send to the bucket (0: no limit); the passes of the switch ignore it")
	f.BoolVar(&resume, "resume", false, "continue the run that stopped")
	f.BoolVar(&status, "status", false, "show where the run is")
	f.BoolVar(&rollback, "rollback", false, "switch back to files")
	f.BoolVar(&cleanup, "cleanup", false, "delete the files kept after a migration")
	migrate.MarkFlagsMutuallyExclusive("resume", "status", "rollback", "cleanup")
	migrate.MarkFlagsMutuallyExclusive("credentials-file", "role-arn")

	storageCmd.AddCommand(migrate)
	rootCmd.AddCommand(storageCmd)
}

type storageFlags struct {
	bucket, credsFile, roleARN        string
	rate                              int
	resume, status, rollback, cleanup bool
}

func runStorageMigrate(cmd *cobra.Command, fl storageFlags) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	deps := storagemigrate.Deps{Cfg: cfg, Log: newLogger(cfg), Out: out, Settings: storageSettings{path: selfUpdateConfigPath(), scratch: storagemigrate.RunDir(cfg.Paths())}}
	switch {
	case fl.status:
		st, err := storagemigrate.New(deps).Status()
		if err != nil {
			return err
		}
		if st == nil {
			fmt.Fprintf(out, "No Storage migration has run on this node. Storage keeps its objects in %s.\n", storageBackendName(cfg))
			return nil
		}
		fmt.Fprint(out, storagemigrate.Describe(st, time.Now()))
		return nil
	case fl.cleanup:
		return storagemigrate.New(deps).Cleanup(cmd.Context())
	}

	st, err := storagemigrate.New(deps).Status()
	if err != nil {
		return err
	}
	creds, file, err := storageCredentials(cfg, st, fl.credsFile, fl.roleARN)
	if err != nil {
		return err
	}
	n, err := openNode(cmd.Context())
	if err != nil {
		return err
	}
	defer n.Close()
	path := selfUpdateConfigPath()
	deps.Tenants = storagemigrate.NodeTenants(n)
	deps.Storage = storagemigrate.NodeService(n, path, deps.Log)
	deps.Reader = storagemigrate.NodeReader(n, path)
	eng := storagemigrate.New(deps)
	req := storagemigrate.Request{Bucket: fl.bucket, Credentials: creds, CredentialsFile: file, RateMiB: fl.rate}
	if fl.rate == 0 {
		req.RateMiB = -1
	}
	switch {
	case fl.resume:
		return eng.Resume(cmd.Context(), req)
	case fl.rollback:
		return eng.Rollback(cmd.Context(), req)
	}
	return eng.Migrate(cmd.Context(), req)
}

func storageBackendName(cfg *config.Config) string {
	if cfg.Fleet.StorageBackend == "s3" {
		return "the bucket " + cfg.Fleet.StorageS3Bucket
	}
	return "files on this node"
}

// storageCredentials decides how the run signs requests to the bucket: a credentials file or a role
// from the flags; else none for a rollback that no longer talks to the bucket; else the role the run
// in the state used; else the role [fleet] storage_s3_role_arn names; else the static key of the
// [fleet] settings (which holds the key a finished migration wrote to config.d); else the credentials
// file the run used. The second result is the path of the credentials file, for the state.
func storageCredentials(cfg *config.Config, st *storagemigrate.State, file, role string) (storagemigrate.Credentials, string, error) {
	switch {
	case file != "":
		c, err := readStorageCredentials(file)
		return c, file, err
	case role != "":
		return storagemigrate.Credentials{Source: storagemigrate.CredRole, RoleARN: role}, "", nil
	case st != nil && !st.NeedsBucket():
		// A rollback that has copied everything back only switches the configuration and starts
		// Storage; it never talks to the bucket again, so there is nothing to sign.
		return storagemigrate.Credentials{}, "", nil
	case st != nil && st.RoleARN != "":
		return storagemigrate.Credentials{Source: storagemigrate.CredRole, RoleARN: st.RoleARN}, "", nil
	case cfg.Fleet.StorageS3RoleARN != "":
		// The role the node was set up with (the AWS first boot writes it); the daemon serves it to
		// Storage already, and the run writes nothing of its own for it.
		return storagemigrate.Credentials{Source: storagemigrate.CredConfig, RoleARN: cfg.Fleet.StorageS3RoleARN}, "", nil
	case cfg.Fleet.StorageS3AccessKeyID != "" && cfg.Fleet.StorageS3SecretAccessKey != "":
		return storagemigrate.Credentials{Source: storagemigrate.CredConfig, AccessKeyID: cfg.Fleet.StorageS3AccessKeyID,
			SecretAccessKey: cfg.Fleet.StorageS3SecretAccessKey}, "", nil
	case st != nil && st.CredentialsFile != "":
		c, err := readStorageCredentials(st.CredentialsFile)
		if err != nil {
			return c, "", fmt.Errorf("the credentials file of the first run cannot be used (%w): give --credentials-file again", err)
		}
		return c, st.CredentialsFile, nil
	}
	return storagemigrate.Credentials{}, "", errors.New("no credentials for the bucket: give --credentials-file (access_key_id and secret_access_key lines, mode 0600) or --role-arn")
}

// readStorageCredentials reads an access key from a file that nobody else can read. The error
// never contains the key.
func readStorageCredentials(path string) (storagemigrate.Credentials, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return storagemigrate.Credentials{}, err
	}
	if !fi.Mode().IsRegular() {
		return storagemigrate.Credentials{}, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return storagemigrate.Credentials{}, fmt.Errorf("%s is mode %04o and holds a secret: run chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && !trustedOwner(int(st.Uid)) {
		return storagemigrate.Credentials{}, fmt.Errorf("%s belongs to another user (uid %d): it must be owned by root, by the supavise user or by you", path, st.Uid)
	}
	m, err := readKeyValueFile(path)
	if err != nil {
		return storagemigrate.Credentials{}, err
	}
	id, secret := firstOf(m, "access_key_id", "aws_access_key_id"), firstOf(m, "secret_access_key", "aws_secret_access_key")
	if id == "" || secret == "" {
		return storagemigrate.Credentials{}, fmt.Errorf("%s: want access_key_id=... and secret_access_key=... lines", path)
	}
	return storagemigrate.Credentials{Source: storagemigrate.CredFile, AccessKeyID: id, SecretAccessKey: secret}, nil
}

// trustedOwner says whether a secret file owned by uid may be read: root, the supavise user, or the
// user who runs the command.
func trustedOwner(uid int) bool {
	suid, _ := supaviseOwner()
	return uid == 0 || uid == os.Geteuid() || (suid >= 0 && uid == suid)
}

func firstOf(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := m[k]; v != "" {
			return v
		}
	}
	return ""
}

// storageS3Keys is the content of the drop-in. The role key belongs to the settings the Storage
// credential endpoint reads; a release that does not know it ignores it.
type storageS3Keys struct {
	Fleet storageS3Fleet `toml:"fleet"`
}

type storageS3Fleet struct {
	AccessKeyID     string `toml:"storage_s3_access_key_id,omitempty"`
	SecretAccessKey string `toml:"storage_s3_secret_access_key,omitempty"`
	RoleARN         string `toml:"storage_s3_role_arn,omitempty"`
}

// storageSettings writes the switch into the node's configuration: storage_backend and the bucket
// into config.toml through the same read and render the installer uses, the credentials into a
// file of their own under config.d.
type storageSettings struct {
	path string
	// scratch is the directory Preview makes its own in. Empty means the system's temporary
	// directory, which the tests use; the command gives the migration's directory in the state
	// directory, which the daemon's user owns.
	scratch string
}

func (s storageSettings) dropIn() string {
	return filepath.Join(config.ConfigDDir(s.path), storageS3DropIn)
}

// owner is the user and mode config.toml has, which its replacement keeps.
func (s storageSettings) owner() (uid, gid int, mode os.FileMode) {
	uid, gid = supaviseOwner()
	mode = 0o600
	if fi, err := os.Stat(s.path); err == nil {
		mode = fi.Mode().Perm()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}
	return uid, gid, mode
}

func (s storageSettings) edit(change func(*config.Config)) error {
	cfg, existed, err := readConfigFile(s.path)
	if err != nil {
		return err
	}
	if !existed {
		return fmt.Errorf("%s does not exist: this node is not installed with a configuration file to change", s.path)
	}
	change(cfg)
	body, err := renderConfig(cfg)
	if err != nil {
		return err
	}
	uid, gid, mode := s.owner()
	return writeFileAtomic(s.path, body, mode, uid, gid)
}

// dropInBody is the content of the credentials drop-in, and false when the credentials need none (a
// key already in [fleet]).
func dropInBody(c storagemigrate.Credentials) ([]byte, bool, error) {
	var keys storageS3Keys
	switch c.Source {
	case storagemigrate.CredFile:
		keys.Fleet.AccessKeyID, keys.Fleet.SecretAccessKey = c.AccessKeyID, c.SecretAccessKey
	case storagemigrate.CredRole:
		keys.Fleet.RoleARN = c.RoleARN
	}
	if keys.Fleet == (storageS3Fleet{}) {
		return nil, false, nil
	}
	b, err := toml.Marshal(keys)
	if err != nil {
		return nil, false, err
	}
	return append([]byte("# Written by `supavise storage migrate`: the credentials of Storage's bucket. Mode 0600.\n"), b...), true, nil
}

// UseBucket implements storagemigrate.Settings.
func (s storageSettings) UseBucket(_ context.Context, d storagemigrate.Destination, c storagemigrate.Credentials) (bool, error) {
	body, wrote, err := dropInBody(c)
	if err != nil {
		return false, err
	}
	if wrote {
		uid, gid, _ := s.owner()
		if err := writeFileAtomic(s.dropIn(), body, 0o600, uid, gid); err != nil {
			return false, err
		}
	}
	if err := s.edit(func(cfg *config.Config) {
		cfg.Fleet.StorageBackend = "s3"
		cfg.Fleet.StorageS3Bucket = d.Bucket
	}); err != nil {
		return wrote, err
	}
	return wrote, s.check("s3")
}

// previewPrefix names the scratch directories of Preview.
const previewPrefix = "supavise-storage-preview-"

// removeStalePreviews deletes the scratch directories an earlier Preview left behind when it was
// killed before it could remove them: they hold copies of the node's drop-ins, which can hold
// secrets. A directory younger than an hour may be a Preview that is running.
func removeStalePreviews(dir string) {
	if dir == "" {
		dir = os.TempDir()
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, en := range ents {
		if !en.IsDir() || !strings.HasPrefix(en.Name(), previewPrefix) {
			continue
		}
		if fi, err := en.Info(); err == nil && time.Since(fi.ModTime()) > time.Hour {
			_ = os.RemoveAll(filepath.Join(dir, en.Name()))
		}
	}
}

// Preview implements storagemigrate.Settings. It builds what UseBucket would write, config.toml and
// the credentials drop-in beside the node's other drop-ins, in a directory of its own (mode 0700,
// under the migration's directory in the state directory, removed on return) and loads it the way
// the daemon does. Nothing on the node changes.
func (s storageSettings) Preview(_ context.Context, d storagemigrate.Destination, c storagemigrate.Credentials) (*config.Config, error) {
	cfg, existed, err := readConfigFile(s.path)
	if err != nil {
		return nil, err
	}
	if !existed {
		return nil, fmt.Errorf("%s does not exist: this node is not installed with a configuration file to change", s.path)
	}
	cfg.Fleet.StorageBackend = "s3"
	cfg.Fleet.StorageS3Bucket = d.Bucket
	body, err := renderConfig(cfg)
	if err != nil {
		return nil, err
	}
	removeStalePreviews(s.scratch)
	scratch, err := os.MkdirTemp(s.scratch, previewPrefix)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	tmp := filepath.Join(scratch, "config.toml")
	dropins := config.ConfigDDir(tmp)
	if err := os.MkdirAll(dropins, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(config.ConfigDDir(s.path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, en := range ents {
		if en.IsDir() || en.Name() == storageS3DropIn {
			continue
		}
		b, err := os.ReadFile(filepath.Join(config.ConfigDDir(s.path), en.Name()))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dropins, en.Name()), b, 0o600); err != nil {
			return nil, err
		}
	}
	if b, ok, err := dropInBody(c); err != nil {
		return nil, err
	} else if ok {
		if err := os.WriteFile(filepath.Join(dropins, storageS3DropIn), b, 0o600); err != nil {
			return nil, err
		}
	}
	loaded, err := config.Load(tmp)
	if err != nil {
		return nil, err
	}
	if got := loaded.Fleet.StorageBackend; got != "s3" {
		return nil, fmt.Errorf("[fleet] storage_backend would be %q, not \"s3\": a SUPAVISE_FLEET_STORAGE_BACKEND variable or a file in %s overrides config.toml", got, config.ConfigDDir(s.path))
	}
	return loaded, nil
}

// UseFiles implements storagemigrate.Settings.
func (s storageSettings) UseFiles(_ context.Context, previous string, removeCredentials bool) error {
	if err := s.edit(func(cfg *config.Config) { cfg.Fleet.StorageBackend = previous }); err != nil {
		return err
	}
	if removeCredentials {
		if err := os.Remove(s.dropIn()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return s.check(previous)
}

// check loads the configuration the way the daemon does and says so when the backend is not what
// was just written, which an environment variable or another config.d file can cause.
func (s storageSettings) check(want string) error {
	cfg, err := config.Load(s.path)
	if err != nil {
		return err
	}
	if got := cfg.Fleet.StorageBackend; (got == "s3") != (want == "s3") {
		return fmt.Errorf("[fleet] storage_backend is %q after the change, not %q: a SUPAVISE_FLEET_STORAGE_BACKEND variable or a file in %s overrides config.toml", got, want, config.ConfigDDir(s.path))
	}
	return nil
}
