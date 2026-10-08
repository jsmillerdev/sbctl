package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/storagemigrate"
)

func TestStorageMigrateFlags(t *testing.T) {
	help, err := run(t, "storage", "migrate", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--credentials-file", "--role-arn", "--bucket", "--rate-limit", "--resume", "--status", "--rollback", "--cleanup"} {
		if !strings.Contains(help, flag) {
			t.Errorf("--help does not list %s", flag)
		}
	}
	// A secret never travels on the command line.
	for _, flag := range []string{"--access-key-id", "--secret-access-key", "--secret-arn"} {
		if strings.Contains(help, flag) {
			t.Errorf("--help lists %s", flag)
		}
	}
	for _, args := range [][]string{
		{"storage", "migrate", "--resume", "--rollback"},
		{"storage", "migrate", "--status", "--cleanup"},
		{"storage", "migrate", "--credentials-file", "/x", "--role-arn", "arn:aws:iam::1:role/x"},
		{"storage", "migrate", "--access-key-id", "a"},
		{"storage", "migrate", "--to", "gcs"},
		{"storage", "migrate", "--rate-limit", "-3"},
		{"storage", "migrate", "extra"},
	} {
		if _, err := run(t, args...); err == nil {
			t.Errorf("supavise %s worked", strings.Join(args, " "))
		}
	}
}

// storageNode is a config file in a temporary state directory.
func storageNode(t *testing.T, body string) (cfgPath string, cfg *config.Config) {
	t.Helper()
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "etc", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body = "state_dir = " + `"` + filepath.Join(dir, "state") + `"` + "\n" + body
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath = cfgPath
	t.Cleanup(func() { configPath = "" })
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfgPath, cfg
}

func TestStorageMigrateStatusAndCleanupWithoutARun(t *testing.T) {
	storageNode(t, "")
	out, err := run(t, "storage", "migrate", "--status")
	if err != nil || !strings.Contains(out, "No Storage migration has run") || !strings.Contains(out, "files on this node") {
		t.Errorf("--status = %q, %v", out, err)
	}
	if _, err := run(t, "storage", "migrate", "--cleanup"); err == nil || !strings.Contains(err.Error(), "no finished Storage migration") {
		t.Errorf("--cleanup = %v", err)
	}
}

func TestReadStorageCredentials(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c, err := readStorageCredentials(write("ok", "# garage\naccess_key_id = GK123\nsecret_access_key=\"s3cr3t\"\n", 0o600))
	if err != nil || c.AccessKeyID != "GK123" || c.SecretAccessKey != "s3cr3t" || c.Source != storagemigrate.CredFile {
		t.Errorf("credentials: %v, %v", c, err)
	}
	if c, err := readStorageCredentials(write("aws", "AWS_ACCESS_KEY_ID=A\nAWS_SECRET_ACCESS_KEY=B\n", 0o400)); err != nil || c.AccessKeyID != "A" || c.SecretAccessKey != "B" {
		t.Errorf("AWS spelling: %v, %v", c, err)
	}
	for name, tc := range map[string]struct {
		path string
		want string
	}{
		"open to the group": {write("wide", "access_key_id=a\nsecret_access_key=topsecret\n", 0o640), "chmod 600"},
		"missing a line":    {write("half", "access_key_id=a\n", 0o600), "want access_key_id"},
		"not there":         {filepath.Join(dir, "nope"), "no such file"},
		"a directory":       {dir, "not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readStorageCredentials(tc.path)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "topsecret") {
				t.Errorf("error = %v, want %q and no secret", err, tc.want)
			}
		})
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "ok"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := readStorageCredentials(link); err == nil {
		t.Error("a link was followed")
	}
}

func TestStorageCredentialsComeFromTheFlagsFirst(t *testing.T) {
	_, cfg := storageNode(t, "")
	file := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(file, []byte("access_key_id=F\nsecret_access_key=S\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, path, err := storageCredentials(cfg, nil, file, "")
	if err != nil || c.Source != storagemigrate.CredFile || path != file {
		t.Errorf("file: %v %q %v", c, path, err)
	}
	if c, _, err := storageCredentials(cfg, nil, "", "arn:aws:iam::1:role/s"); err != nil || c.Source != storagemigrate.CredRole || c.RoleARN != "arn:aws:iam::1:role/s" {
		t.Errorf("role: %v %v", c, err)
	}
	if _, _, err := storageCredentials(cfg, nil, "", ""); err == nil || !strings.Contains(err.Error(), "--credentials-file") {
		t.Errorf("neither: %v", err)
	}
	// What a run used is used again by --resume.
	st := &storagemigrate.State{CredentialsFile: file}
	if c, p, err := storageCredentials(cfg, st, "", ""); err != nil || c.AccessKeyID != "F" || p != file {
		t.Errorf("resume with the file: %v %q %v", c, p, err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storageCredentials(cfg, st, "", ""); err == nil || !strings.Contains(err.Error(), "give --credentials-file again") {
		t.Errorf("resume without the file: %v", err)
	}
	if c, _, err := storageCredentials(cfg, &storagemigrate.State{RoleARN: "arn:r"}, "", ""); err != nil || c.RoleARN != "arn:r" {
		t.Errorf("resume with a role: %v %v", c, err)
	}
	// The static key a finished migration left in config.d serves --rollback.
	cfg.Fleet.StorageS3AccessKeyID, cfg.Fleet.StorageS3SecretAccessKey = "K", "V"
	if c, _, err := storageCredentials(cfg, st, "", ""); err != nil || c.Source != storagemigrate.CredConfig || c.AccessKeyID != "K" {
		t.Errorf("config: %v %v", c, err)
	}
	// A run that used a role goes on with it, whatever key is in [fleet].
	if c, _, err := storageCredentials(cfg, &storagemigrate.State{RoleARN: "arn:r"}, "", ""); err != nil || c.Source != storagemigrate.CredRole || c.RoleARN != "arn:r" {
		t.Errorf("resume with a role and a key in [fleet]: %v %v", c, err)
	}
}

func TestStorageSettingsKeepSecretsOutOfConfigToml(t *testing.T) {
	path, _ := storageNode(t, "domain = \"example.test\"\n[fleet]\nstorage_s3_endpoint = \"http://127.0.0.1:3900\"\n")
	s := storageSettings{path: path}
	d := storagemigrate.Destination{Bucket: "objects", Endpoint: "http://127.0.0.1:3900", Region: "us-east-1", PathStyle: true}

	wrote, err := s.UseBucket(context.Background(), d, storagemigrate.Credentials{Source: storagemigrate.CredFile, AccessKeyID: "GKID", SecretAccessKey: "topsecret"})
	if err != nil || !wrote {
		t.Fatalf("UseBucket: %v %v", wrote, err)
	}
	raw, _ := os.ReadFile(path)
	toml := strings.ReplaceAll(string(raw), "'", `"`) // the encoder quotes with either
	if !strings.Contains(toml, `storage_backend = "s3"`) || !strings.Contains(toml, `storage_s3_bucket = "objects"`) ||
		!strings.Contains(toml, "example.test") || strings.Contains(toml, "topsecret") || strings.Contains(toml, "GKID") {
		t.Errorf("config.toml:\n%s", raw)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config.toml: %v, %v", fi, err)
	}
	drop := filepath.Join(config.ConfigDDir(path), storageS3DropIn)
	if fi, err := os.Stat(drop); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("drop-in: %v, %v", fi, err)
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.Fleet.StorageBackend != "s3" || cfg.Fleet.StorageS3AccessKeyID != "GKID" || cfg.Fleet.StorageS3SecretAccessKey != "topsecret" || cfg.Fleet.StorageS3Bucket != "objects" {
		t.Errorf("the daemon's view: %+v, %v", cfg.Fleet, err)
	}
	if cfg.Domain != "example.test" {
		t.Errorf("config.toml lost its other settings: %q", cfg.Domain)
	}

	if err := s.UseFiles(context.Background(), "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(drop); !os.IsNotExist(err) {
		t.Errorf("the credentials file is still there: %v", err)
	}
	cfg, err = config.Load(path)
	if err != nil || cfg.Fleet.StorageBackend != "" || cfg.Fleet.StorageS3AccessKeyID != "" {
		t.Errorf("after going back: %+v, %v", cfg.Fleet, err)
	}

	// A role is written as a setting of the drop-in, not as a key; a key already in [fleet] needs no file.
	if wrote, err := s.UseBucket(context.Background(), d, storagemigrate.Credentials{Source: storagemigrate.CredRole, RoleARN: "arn:aws:iam::1:role/storage"}); err != nil || !wrote {
		t.Errorf("role: %v %v", wrote, err)
	}
	if b, _ := os.ReadFile(drop); !strings.Contains(string(b), `storage_s3_role_arn = 'arn:aws:iam::1:role/storage'`) && !strings.Contains(string(b), `storage_s3_role_arn = "arn:aws:iam::1:role/storage"`) {
		t.Errorf("drop-in:\n%s", b)
	}
	if wrote, err := s.UseBucket(context.Background(), d, storagemigrate.Credentials{Source: storagemigrate.CredConfig}); err != nil || wrote {
		t.Errorf("config key: %v %v", wrote, err)
	}
}

func TestStorageSettingsNoticeAnOverride(t *testing.T) {
	path, _ := storageNode(t, "")
	t.Setenv("SUPAVISE_FLEET_STORAGE_BACKEND", "s3")
	err := storageSettings{path: path}.UseFiles(context.Background(), "", false)
	if err == nil || !strings.Contains(err.Error(), "overrides config.toml") {
		t.Errorf("UseFiles = %v", err)
	}
}

func TestStorageSettingsNeedAConfigFile(t *testing.T) {
	err := storageSettings{path: filepath.Join(t.TempDir(), "none.toml")}.UseFiles(context.Background(), "", false)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("UseFiles = %v", err)
	}
}

func TestStorageSettingsPreviewChangesNothing(t *testing.T) {
	path, _ := storageNode(t, "domain = \"example.test\"\n")
	s := storageSettings{path: path}
	d := storagemigrate.Destination{Bucket: "objects", Region: "us-east-1"}
	creds := storagemigrate.Credentials{Source: storagemigrate.CredFile, AccessKeyID: "GKID", SecretAccessKey: "topsecret"}
	before, _ := os.ReadFile(path)

	cfg, err := s.Preview(context.Background(), d, creds)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fleet.StorageBackend != "s3" || cfg.Fleet.StorageS3Bucket != "objects" || cfg.Fleet.StorageS3AccessKeyID != "GKID" || cfg.Domain != "example.test" {
		t.Errorf("the configuration it would load: %+v", cfg.Fleet)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("config.toml changed:\n%s", after)
	}
	if _, err := os.Stat(config.ConfigDDir(path)); !os.IsNotExist(err) {
		t.Errorf("config.d appeared: %v", err)
	}

	// Another drop-in that sets the backend, or the environment, wins over config.toml: the switch
	// would not take effect, and the preview says so while Storage still runs.
	dir := config.ConfigDDir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "50-other.toml")
	if err := os.WriteFile(other, []byte("[fleet]\nstorage_backend = \"file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Preview(context.Background(), d, creds); err == nil || !strings.Contains(err.Error(), "overrides config.toml") {
		t.Errorf("a drop-in that sets the backend: %v", err)
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	// The drop-in UseBucket writes is replaced, not read.
	if err := os.WriteFile(filepath.Join(dir, storageS3DropIn), []byte("[fleet]\nstorage_backend = \"file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Preview(context.Background(), d, creds); err != nil {
		t.Errorf("an old credentials drop-in: %v", err)
	}
	t.Setenv("SUPAVISE_FLEET_STORAGE_BACKEND", "file")
	if _, err := s.Preview(context.Background(), d, creds); err == nil || !strings.Contains(err.Error(), "overrides config.toml") {
		t.Errorf("an environment variable: %v", err)
	}
	if _, err := (storageSettings{path: filepath.Join(t.TempDir(), "none.toml")}).Preview(context.Background(), d, creds); err == nil {
		t.Error("a node without a configuration file")
	}
}

// Preview copies the node's other drop-ins, which can hold secrets, into a directory of its own. It
// lives under the migration's directory when the command says so, and a directory an earlier
// Preview left behind when it was killed is removed, a younger one (a Preview that is running) is not.
func TestStorageSettingsPreviewScratchIsCleanedUp(t *testing.T) {
	path, _ := storageNode(t, "domain = \"example.test\"\n")
	scratch := t.TempDir()
	stale := filepath.Join(scratch, previewPrefix+"old")
	fresh := filepath.Join(scratch, previewPrefix+"fresh")
	other := filepath.Join(scratch, "state.json.d")
	for _, d := range []string{stale, fresh, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatal(err)
	}
	s := storageSettings{path: path, scratch: scratch}
	d := storagemigrate.Destination{Bucket: "objects", Region: "us-east-1"}
	if _, err := s.Preview(context.Background(), d, storagemigrate.Credentials{Source: storagemigrate.CredFile, AccessKeyID: "K", SecretAccessKey: "S"}); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(scratch)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "state.json.d,"+previewPrefix+"fresh" {
		t.Errorf("left in the scratch directory: %v", names)
	}
}

// The role [fleet] storage_s3_role_arn names serves a run that was given nothing else, and a
// rollback that only has Storage left to start needs no credentials at all.
func TestStorageCredentialsFromTheConfiguredRoleAndForALateRollback(t *testing.T) {
	_, cfg := storageNode(t, "")
	cfg.Fleet.StorageS3RoleARN = "arn:aws:iam::1:role/storage"
	c, file, err := storageCredentials(cfg, nil, "", "")
	if err != nil || c.Source != storagemigrate.CredConfig || c.RoleARN != cfg.Fleet.StorageS3RoleARN || file != "" {
		t.Fatalf("configured role: %+v %q %v", c, file, err)
	}
	// A key beside it does not take its place: Storage uses the role when both are set.
	cfg.Fleet.StorageS3AccessKeyID, cfg.Fleet.StorageS3SecretAccessKey = "K", "V"
	if c, _, err := storageCredentials(cfg, nil, "", ""); err != nil || c.RoleARN == "" || c.AccessKeyID != "" {
		t.Fatalf("role and key: %+v %v", c, err)
	}
	// The role of a run in the state still wins over the configuration's.
	if c, _, err := storageCredentials(cfg, &storagemigrate.State{RoleARN: "arn:r"}, "", ""); err != nil || c.Source != storagemigrate.CredRole || c.RoleARN != "arn:r" {
		t.Fatalf("the run's role: %+v %v", c, err)
	}

	cfg.Fleet = config.Fleet{}
	late := &storagemigrate.State{Phase: storagemigrate.PhaseRollingBack, Step: "switched", CredentialsFile: filepath.Join(t.TempDir(), "gone")}
	if c, _, err := storageCredentials(cfg, late, "", ""); err != nil || c.Source != "" {
		t.Fatalf("a rollback past the copy needs none: %+v %v", c, err)
	}
	early := &storagemigrate.State{Phase: storagemigrate.PhaseRollingBack, Step: "final", CredentialsFile: late.CredentialsFile}
	if _, _, err := storageCredentials(cfg, early, "", ""); err == nil {
		t.Error("a rollback that still copies went on without credentials")
	}
}
