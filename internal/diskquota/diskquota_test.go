package diskquota

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/deploy/systemd"
)

const mountinfo = `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,discard
30 22 8:16 / /var/lib/supavise rw,relatime shared:5 - xfs /dev/sdb rw,attr2,inode64,logbufs=8,logbsize=32k,prjquota
31 22 8:32 / /mnt/with\040space rw,relatime shared:6 - xfs /dev/sdc rw,attr2,inode64,pqnoenforce
32 22 8:48 / /var/lib/other rw,relatime shared:7 - xfs /dev/sdd rw,attr2,inode64
33 30 8:64 / /var/lib/supavise/backups rw,relatime shared:8 - ext4 /dev/sde rw
`

func TestParseMountInfoPicksTheInnermostMount(t *testing.T) {
	for dir, want := range map[string]struct {
		mount, fstype string
		enforceable   bool
	}{
		"/var/lib/supavise/projects/abc": {"/var/lib/supavise", "xfs", true},
		"/var/lib/supavise":              {"/var/lib/supavise", "xfs", true},
		"/var/lib/supavise/backups/x":    {"/var/lib/supavise/backups", "ext4", false},
		"/var/lib/supavise-other/x":      {"/", "ext4", false},
		"/mnt/with space/p":              {"/mnt/with space", "xfs", false}, // pqnoenforce: limits are not enforced
		"/var/lib/other/p":               {"/var/lib/other", "xfs", false},
		"/home/somebody":                 {"/", "ext4", false},
	} {
		m, fs, opts := parseMountInfo(strings.NewReader(mountinfo), dir)
		v := Volume{Mount: m, FSType: fs, Options: opts}
		if m != want.mount || fs != want.fstype || v.Enforceable() != want.enforceable {
			t.Errorf("%s: mount %q fstype %q enforceable %v (options %v), want %+v", dir, m, fs, v.Enforceable(), opts, want)
		}
	}
}

func TestInspectOfAMissingDirectoryUsesItsParent(t *testing.T) {
	dir := t.TempDir()
	v, err := Inspect(filepath.Join(dir, "not", "there", "yet"))
	if err != nil {
		t.Fatal(err)
	}
	if v.TotalBytes <= 0 || v.FreeBytes < 0 || v.FreeBytes > v.TotalBytes {
		t.Fatalf("volume = %+v", v)
	}
}

func TestUsedCountsRegularFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "a", "x"), make([]byte, 1000), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "a", "b", "y"), make([]byte, 234), 0o600)
	if got := Used(dir); got != 1234 {
		t.Fatalf("Used = %d", got)
	}
	if got := Used(filepath.Join(dir, "missing")); got != 0 {
		t.Fatalf("Used(missing) = %d", got)
	}
}

func TestSettingRoundTripAndValidation(t *testing.T) {
	path := SettingFile(t.TempDir())
	if _, ok, err := ReadSetting(path); ok || err != nil {
		t.Fatalf("no file: %v %v", ok, err)
	}
	want := Setting{ProjectID: ProjectID(7), SizeGB: 20}
	if err := WriteSetting(path, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadSetting(path)
	if err != nil || !ok || got != want {
		t.Fatalf("read back %+v %v %v", got, ok, err)
	}
	for _, bad := range []Setting{{ProjectID: 5, SizeGB: 20}, {ProjectID: ProjectID(1), SizeGB: 0}, {ProjectID: ProjectID(1), SizeGB: 1 << 20}} {
		if err := WriteSetting(path, bad); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	// A file someone edited by hand is judged on reading, since root acts on it.
	_ = os.WriteFile(path, []byte("project_id=1\nsize_gb=5\n"), 0o640)
	if _, _, err := ReadSetting(path); err == nil {
		t.Error("an out-of-range project id was accepted")
	}
	_ = os.WriteFile(path, []byte("project_id=100001\nsize_gb=5; rm -rf /\n"), 0o640)
	if _, _, err := ReadSetting(path); err == nil {
		t.Error("a non-numeric size was accepted")
	}
}

func TestApplyRunsXFSQuotaWithOnlyNumbersFromTheSetting(t *testing.T) {
	var calls []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	err := Apply(context.Background(), run, "/var/lib/supavise", "/var/lib/supavise/projects/abcdefghijklmnopqrst", Setting{ProjectID: ProjectID(3), SizeGB: 20})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"xfs_quota -x -c project -s -p /var/lib/supavise/projects/abcdefghijklmnopqrst 100003 /var/lib/supavise",
		"xfs_quota -x -c limit -p bhard=20g 100003 /var/lib/supavise",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q", calls)
	}
	failing := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("quota not enabled"), errors.New("exit 1")
	}
	if err := Apply(context.Background(), failing, "/m", "/m/p", Setting{ProjectID: ProjectID(3), SizeGB: 1}); err == nil || !strings.Contains(err.Error(), "quota not enabled") {
		t.Fatalf("a failing xfs_quota: %v", err)
	}
	if err := Apply(context.Background(), run, "/m", "/m/p", Setting{}); err == nil {
		t.Fatal("an invalid setting reached xfs_quota")
	}
}

// The root unit is narrow: it runs one command with the ref from its own name, keeps
// NoNewPrivileges, and its name is the one Unit builds (the polkit rule lets the supavise user
// start any supavise-* unit, so the unit file is what limits what it does).
func TestRootUnitIsNarrow(t *testing.T) {
	b, err := systemd.Read("supavise-diskquota@.service")
	if err != nil {
		t.Fatal(err)
	}
	u := string(b)
	for _, want := range []string{
		"Type=oneshot", "ExecStart=/usr/local/bin/supavise system set-disk-quota %i", "NoNewPrivileges=yes",
		"CapabilityBoundingSet=CAP_SYS_ADMIN", "PrivateNetwork=yes", "ProtectSystem=strict", "ReadWritePaths=/var/lib/supavise",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("supavise-diskquota@.service lacks %q", want)
		}
	}
	if strings.Contains(u, "\nUser=") {
		t.Error("the unit must run as root: xfs_quota needs it")
	}
	if got := Unit("%i"); got != "supavise-diskquota@%i.service" {
		t.Errorf("Unit = %q", got)
	}
}
