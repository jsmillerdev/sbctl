package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
)

func tokenFile(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadJoinToken(t *testing.T) {
	tok, err := readJoinToken(tokenFile(t, "svj1.abc.def\n", 0o600))
	if err != nil || string(tok) != "svj1.abc.def" {
		t.Fatalf("%q, %v", tok, err)
	}
	for name, c := range map[string]struct {
		path string
		want string
	}{
		"too open":  {tokenFile(t, "svj1.x", 0o644), "chmod 600"},
		"empty":     {tokenFile(t, "\n", 0o600), "single token"},
		"two words": {tokenFile(t, "svj1.x svj1.y", 0o600), "single token"},
		"missing":   {filepath.Join(t.TempDir(), "nope"), "no such file"},
		"directory": {t.TempDir(), "not a file"},
		"huge":      {tokenFile(t, strings.Repeat("a", maxTokenBytes+1), 0o600), "larger than"},
	} {
		if _, err := readJoinToken(c.path); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckJoinOptions(t *testing.T) {
	good := tokenFile(t, "svj1.x", 0o600)
	if err := checkJoinOptions(installOptions{JoinTokenFile: good}); err != nil {
		t.Error(err)
	}
	err := checkJoinOptions(installOptions{JoinTokenFile: good, KeyPassphraseFile: "/x"})
	if err == nil || !strings.Contains(err.Error(), "--key-passphrase-file") {
		t.Errorf("with a passphrase file: %v", err)
	}
}

func TestCheckInstallFlags(t *testing.T) {
	changed := func(names ...string) func(string) bool {
		return func(n string) bool {
			for _, m := range names {
				if m == n {
					return true
				}
			}
			return false
		}
	}
	for name, c := range map[string]struct {
		o       installOptions
		changed func(string) bool
		want    string // "" is accepted
	}{
		"nothing":                        {installOptions{}, changed(), ""},
		"a token file":                   {installOptions{JoinTokenFile: "/root/token"}, changed("join-token-file"), ""},
		"an empty token file name":       {installOptions{}, changed("join-token-file"), "--join-token-file needs a path"},
		"a device":                       {installOptions{AWSFirstBoot: true, DataDevice: "/dev/nvme1n1"}, changed("data-device"), ""},
		"first boot without a device":    {installOptions{AWSFirstBoot: true}, changed(), ""},
		"a device without first boot":    {installOptions{DataDevice: "/dev/nvme1n1"}, changed("data-device"), "belongs to --aws-first-boot"},
		"a device that is a flag":        {installOptions{AWSFirstBoot: true, DataDevice: "-f"}, changed("data-device"), "not a device node"},
		"a device outside /dev":          {installOptions{AWSFirstBoot: true, DataDevice: "/var/lib/disk.img"}, changed("data-device"), "not a device node"},
		"a device that climbs out":       {installOptions{AWSFirstBoot: true, DataDevice: "/dev/../etc/passwd"}, changed("data-device"), "not a device node"},
		"a relative device":              {installOptions{AWSFirstBoot: true, DataDevice: "nvme1n1"}, changed("data-device"), "not a device node"},
		"a device with a double slash":   {installOptions{AWSFirstBoot: true, DataDevice: "/dev//nvme1n1"}, changed("data-device"), "not a device node"},
		"a flag value of /dev/ itself":   {installOptions{AWSFirstBoot: true, DataDevice: "/dev/"}, changed("data-device"), "not a device node"},
		"a loop device":                  {installOptions{AWSFirstBoot: true, DataDevice: "/dev/loop7"}, changed("data-device"), ""},
		"an empty name given explicitly": {installOptions{AWSFirstBoot: true}, changed("data-device"), ""},
	} {
		err := checkInstallFlags(c.changed, c.o)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

// The stack name comes from an instance tag. A name that is not a stack name stays out of config.d,
// where a stray quote or newline would make the file invalid TOML.
func TestWriteAWSConfigRefusesWhatIsNotAStackName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"", "a\"b", "x\ny = 1", "1stack", "has space", strings.Repeat("a", 129)} {
		if err := writeAWSConfig(dir, name); err == nil {
			t.Errorf("%q was written", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, config.ConfigDName)); err == nil {
		t.Error("a refused name created config.d")
	}
}

// The token reaches the joiner in a private file that the supavise user can read, and is removed
// when the join returns; the operator's file is left as it was.
func TestJoinStepStagesTheToken(t *testing.T) {
	src := tokenFile(t, "svj1.secret\n", 0o600)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	uid, gid := os.Getuid(), os.Getgid()
	var staged string
	j := fakeJoiner{do: func(path string, resume bool) error {
		staged = path
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("staged token: %v, %v", fi, err)
		}
		if d, _ := os.Stat(filepath.Dir(path)); d.Mode().Perm() != 0o700 {
			t.Errorf("staging directory mode %v", d.Mode())
		}
		if b, _ := os.ReadFile(path); string(b) != "svj1.secret\n" {
			t.Errorf("staged token = %q", b)
		}
		if resume {
			t.Error("a fresh join asked to resume")
		}
		return nil
	}}
	in := &installer{ctx: context.Background(), out: &bytes.Buffer{}}
	if err := in.joinStep(j, src, cfgPath, uid, gid); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staged); err == nil {
		t.Error("the staged token was left behind")
	}
	if b, _ := os.ReadFile(src); string(b) != "svj1.secret\n" {
		t.Errorf("the operator's file changed: %q", b)
	}

	// A failed join leaves nothing behind either, and the error is the joiner's.
	boom := errors.New("pin mismatch")
	j.do = func(path string, _ bool) error { staged = path; return boom }
	if err := in.joinStep(j, src, cfgPath, uid, gid); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(staged); err == nil {
		t.Error("the staged token was left behind after a failed join")
	}
}

// A server that holds its node certificate continues the join and needs no token.
func TestJoinStepResumesWhenTheCertificateExists(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.MkdirAll(config.ClusterDir(cfgPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.ClusterDir(cfgPath), config.NodeCertFile), []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	j := fakeJoiner{do: func(path string, resume bool) error {
		got = append(got, path, map[bool]string{true: "resume", false: "join"}[resume])
		return nil
	}}
	in := &installer{ctx: context.Background(), out: &bytes.Buffer{}}
	// The token file is gone: a re-run has no need of it.
	if err := in.joinStep(j, filepath.Join(dir, "gone"), cfgPath, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "|resume" {
		t.Errorf("joiner calls = %q", got)
	}
}

func TestJoinStepNeedsAUsableToken(t *testing.T) {
	in := &installer{ctx: context.Background(), out: &bytes.Buffer{}}
	called := false
	j := fakeJoiner{do: func(string, bool) error { called = true; return nil }}
	err := in.joinStep(j, filepath.Join(t.TempDir(), "gone"), filepath.Join(t.TempDir(), "config.toml"), os.Getuid(), os.Getgid())
	if err == nil || called {
		t.Errorf("err = %v, joiner called = %v", err, called)
	}
}

type fakeJoiner struct {
	do func(path string, resume bool) error
}

func (f fakeJoiner) Join(_ context.Context, tokenFile string, resume bool) error {
	return f.do(tokenFile, resume)
}

func TestJoinOptionsAreFlags(t *testing.T) {
	c, _, err := rootCmd.Find([]string{"install"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"aws-first-boot", "data-device", "join-token-file"} {
		if c.Flags().Lookup(f) == nil {
			t.Errorf("install has no --%s", f)
		}
	}
}

// The installer reads config.toml and nothing else: the cluster keys of config.d and the stack
// name are the daemon's view of the node, and are never written back into config.toml.
func TestInstallerNeverCopiesConfigDIntoConfigToml(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("public_ip = '203.0.113.9'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cd := filepath.Join(dir, config.ConfigDName)
	if err := os.MkdirAll(cd, 0o750); err != nil {
		t.Fatal(err)
	}
	cluster := "domain = 'cluster.example.com'\n[replicas]\nconcurrency = 3\n[failover]\ncooldown_minutes = 30\n[fleet]\nstorage_s3_bucket = 'objects'\n"
	if err := os.WriteFile(filepath.Join(cd, config.ClusterConfigFile), []byte(cluster), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cd, config.AWSConfigFile), []byte("[aws]\nstack_name = 'supavise-b'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The daemon's view has them...
	merged, err := config.Load(cfgPath)
	if err != nil || merged.Domain != "cluster.example.com" || merged.AWS.StackName != "supavise-b" || merged.Replicas.Concurrency != 3 {
		t.Fatalf("config.Load = %+v, %v", merged, err)
	}
	// ...the installer's does not, and what it writes back does not either.
	cfg, existed, err := readConfigFile(cfgPath)
	if err != nil || !existed {
		t.Fatal(err)
	}
	if cfg.Domain != "" || cfg.AWS.StackName != "" || cfg.Replicas.Concurrency != 2 {
		t.Fatalf("the installer read config.d: domain %q, stack %q, replicas %d", cfg.Domain, cfg.AWS.StackName, cfg.Replicas.Concurrency)
	}
	if err := applyInstall(cfg, installOptions{Fresh: false, JoinTokenFile: "x"}, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	body, err := renderConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"cluster.example.com", "supavise-b", "objects", "replicas", "failover", "stack_name"} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("config.toml would gain %q:\n%s", leaked, body)
		}
	}
	if !strings.Contains(string(body), "203.0.113.9") {
		t.Errorf("config.toml lost its own setting:\n%s", body)
	}
}

// A server whose system cluster is a standby follows a leader; a founder, or a follower that was
// promoted, does not. An installer that runs again on a follower keeps it, and one that finds a join
// that stopped after the certificate continues it.
func TestFollowsALeaderAndAnUnfinishedJoin(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	if followsALeader(cfg) {
		t.Error("a server with no system cluster follows a leader")
	}
	data := cfg.Paths().PostgresData(config.SystemRef)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if followsALeader(cfg) {
		t.Error("a founder's system cluster (no standby.signal) is a standby")
	}
	if err := os.WriteFile(filepath.Join(data, "standby.signal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !followsALeader(cfg) {
		t.Error("a standby system cluster is not a follower")
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if joinUnfinished(cfgPath) {
		t.Error("a join that never started is unfinished")
	}
	if err := os.MkdirAll(config.ClusterDir(cfgPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.ClusterDir(cfgPath), "join.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !joinUnfinished(cfgPath) {
		t.Error("a join state file is not an unfinished join")
	}
}

// The converge that follows the join is the one that has the node's cluster identity: it comes after the
// join, and never when the join failed.
func TestJoinAndConvergeConvergesAfterTheJoin(t *testing.T) {
	src := tokenFile(t, "svj1.secret\n", 0o600)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	var calls []string
	in := &installer{ctx: context.Background(), out: &bytes.Buffer{}, cfg: &config.Config{BinPath: "/usr/local/bin/supavise"},
		runCmd: func(name string, args ...string) error {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return nil
		}}
	j := fakeJoiner{do: func(string, bool) error { calls = append(calls, "join"); return nil }}
	if err := in.joinAndConverge(j, src, cfgPath, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	want := []string{"join", "/usr/local/bin/supavise --config " + config.DefaultPath + " system converge"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want %q", calls, want)
	}

	calls = nil
	boom := errors.New("pin mismatch")
	j.do = func(string, bool) error { return boom }
	if err := in.joinAndConverge(j, src, cfgPath, os.Getuid(), os.Getgid()); !errors.Is(err, boom) || len(calls) != 0 {
		t.Fatalf("a failed join: err = %v, converge calls = %q", err, calls)
	}

	// A failed converge fails the install: the host would not be what the installer promises.
	failing := errors.New("converge failed")
	in.runCmd = func(string, ...string) error { return failing }
	j.do = func(string, bool) error { return nil }
	if err := in.joinAndConverge(j, src, cfgPath, os.Getuid(), os.Getgid()); !errors.Is(err, failing) {
		t.Fatalf("err = %v", err)
	}
}

// First boot on AWS writes the stack's storage role from the instance tag, once, and not over what the
// operator set; a tag that is no role ARN is not written.
func TestApplyStorageRoleWritesTheTagsRoleToTheConfig(t *testing.T) {
	const arn = "arn:aws:iam::123456789012:role/supavise-StorageRole-1A2B3C"
	cfg := config.Default()
	note, err := applyStorageRole(cfg, arn)
	if err != nil || cfg.Fleet.StorageS3RoleARN != arn || !strings.Contains(note, arn) {
		t.Fatalf("role = %q, note %q, err %v", cfg.Fleet.StorageS3RoleARN, note, err)
	}
	// It lands in config.toml, and the file still loads: a role is valid on its own.
	body, err := renderConfig(cfg)
	if err != nil || !strings.Contains(string(body), "storage_s3_role_arn = '"+arn+"'") && !strings.Contains(string(body), `storage_s3_role_arn = "`+arn+`"`) {
		t.Fatalf("rendered:\n%s\n%v", body, err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if loaded, err := config.Load(path); err != nil || loaded.Fleet.StorageS3RoleARN != arn {
		t.Fatalf("the written config: %+v, %v", loaded, err)
	}

	// A role the operator set stays, and so does a static key, which excludes a role.
	own := config.Default()
	own.Fleet.StorageS3RoleARN = "arn:aws:iam::123456789012:role/mine"
	if note, err := applyStorageRole(own, arn); err != nil || note != "" || own.Fleet.StorageS3RoleARN != "arn:aws:iam::123456789012:role/mine" {
		t.Fatalf("over the operator's role: %q %q %v", own.Fleet.StorageS3RoleARN, note, err)
	}
	keyed := config.Default()
	keyed.Fleet.StorageS3AccessKeyID, keyed.Fleet.StorageS3SecretAccessKey = "AKIAEXAMPLE", "secret"
	if note, err := applyStorageRole(keyed, arn); err != nil || keyed.Fleet.StorageS3RoleARN != "" || !strings.Contains(note, "static key") {
		t.Fatalf("with a static key: role %q, note %q, err %v", keyed.Fleet.StorageS3RoleARN, note, err)
	}

	for _, bad := range []string{"", "not-an-arn", "arn:aws:iam::123:role/x", "arn:aws:iam::123456789012:user/x", "arn:aws:iam::123456789012:role/x\"\nstorage_backend = \"s3", "arn:aws:iam::123456789012:role/x y"} {
		c := config.Default()
		if _, err := applyStorageRole(c, bad); err == nil || c.Fleet.StorageS3RoleARN != "" {
			t.Errorf("%q was taken: role %q, err %v", bad, c.Fleet.StorageS3RoleARN, err)
		}
	}
}
