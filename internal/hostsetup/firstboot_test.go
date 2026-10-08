package hostsetup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

// instance is a pretend EC2 instance: a directory of device links, an fstab, a mount table that
// the fake `mount` fills, and answers for lsblk and blkid.
type instance struct {
	t                    *testing.T
	root                 string
	byID, fstab          string
	state, config, units string
	mounts               []string
	out                  bytes.Buffer
	runner               *fakeRunner
	fb                   *FirstBoot
	fake                 *awsfake.Server

	// what the data volume holds: "" is blank, "xfs", "ext4", "gpt" (partitions)
	holds              string
	ownerUID, ownerGID int // who owns the state directory: root on a fresh volume
	appearAt           int // the volume shows up at this poll
	polls              int
	accounts           *fakeAccounts
}

type fakeAccounts struct {
	users  map[string][2]int
	uids   map[int]string
	gnames map[int]string
	gids   map[string]int
}

func (a *fakeAccounts) UserByName(n string) (int, int, bool) {
	v, ok := a.users[n]
	return v[0], v[1], ok
}
func (a *fakeAccounts) UserNameByID(uid int) (string, bool)  { n, ok := a.uids[uid]; return n, ok }
func (a *fakeAccounts) GroupNameByID(gid int) (string, bool) { n, ok := a.gnames[gid]; return n, ok }
func (a *fakeAccounts) GroupIDByName(n string) (int, bool)   { g, ok := a.gids[n]; return g, ok }

func newInstance(t *testing.T) *instance {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir()) // the device links are resolved, so the paths must be
	if err != nil {
		t.Fatal(err)
	}
	in := &instance{t: t, root: root, byID: filepath.Join(root, "dev/disk/by-id"), fstab: filepath.Join(root, "etc/fstab"),
		state: filepath.Join(root, "var/lib/supavise"), config: filepath.Join(root, "etc/supavise"), units: filepath.Join(root, "etc/systemd/system"),
		accounts: &fakeAccounts{users: map[string][2]int{}, uids: map[int]string{}, gnames: map[int]string{}, gids: map[string]int{}}}
	for _, d := range []string{in.byID, filepath.Join(root, "etc")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	in.disk("nvme0n1", "nvme-Amazon_Elastic_Block_Store_vol0root")
	if err := os.Symlink(filepath.Join(root, "dev/nvme0n1p1"), filepath.Join(in.byID, "nvme-Amazon_Elastic_Block_Store_vol0root-part1")); err != nil {
		t.Fatal(err)
	}
	in.disk("nvme1n1", "nvme-Amazon_Elastic_Block_Store_vol0data")

	in.fake = awsfake.New(t)
	d := in.fake.IMDS()
	d.InstanceID, d.Tags = "i-0joiner", map[string]string{"supavise:stack-name": "supavise-b"}
	in.fake.SetIMDS(d)

	in.runner = &fakeRunner{do: in.answer}
	in.fb = &FirstBoot{
		StateDir: in.state, ConfigDir: in.config, UnitDir: in.units, ByID: in.byID, Fstab: in.fstab, User: "supavise",
		IMDS: in.fake.Client().IMDS, Runner: in.runner, Accounts: in.accounts,
		Owner: func(string) (int, int, bool) { return in.ownerUID, in.ownerGID, true },
		MountInfo: func() ([]byte, error) {
			return []byte("20 1 8:1 / / rw - ext4 /dev/root rw\n" + strings.Join(in.mounts, "\n") + "\n"), nil
		},
		Timeout: time.Minute, Poll: time.Millisecond, Out: &in.out,
		Sleep: func(context.Context, time.Duration) error { in.polls++; return nil },
	}
	return in
}

// disk creates a device file and a by-id link to it.
func (in *instance) disk(name, link string) {
	dev := filepath.Join(in.root, "dev", name)
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		in.t.Fatal(err)
	}
	if err := os.Symlink(dev, filepath.Join(in.byID, link)); err != nil {
		in.t.Fatal(err)
	}
}

func (in *instance) dataDev() string { return filepath.Join(in.root, "dev/nvme1n1") }

func (in *instance) answer(name string, args []string) ([]byte, error) {
	cmd := name + " " + strings.Join(args, " ")
	switch {
	case cmd == "findmnt -n -o SOURCE /":
		return []byte("/dev/nvme0n1p1\n"), nil
	case cmd == "lsblk -no PKNAME /dev/nvme0n1p1":
		return []byte("nvme0n1\n"), nil
	case name == "blkid" && args[0] == "-p":
		if in.holds == "" {
			return nil, &ExitError{Command: cmd, Code: 2, Err: errors.New("exit status 2")}
		}
		return []byte("TYPE=" + in.holds), nil
	case name == "lsblk" && args[0] == "-nr":
		if in.holds == "gpt" {
			return []byte("nvme1n1\nnvme1n1p1\n"), nil
		}
		return []byte("nvme1n1\n"), nil
	case name == "blkid" && args[0] == "-o": // the file system type; a partition table has none
		if in.holds == "gpt" {
			return []byte("\n"), nil
		}
		return []byte(in.holds + "\n"), nil
	case name == "blkid" && args[0] == "-s":
		return []byte("1111-2222\n"), nil
	case name == "mkfs.xfs":
		in.holds = "xfs"
	case name == "mount" && len(args) == 1:
		in.mounts = append(in.mounts, "36 20 8:2 / "+args[0]+" rw - xfs "+in.dataDev()+" rw")
	case name == "mount" && args[0] == "--bind":
		in.mounts = append(in.mounts, "37 20 8:2 /etc "+args[2]+" rw - xfs "+in.dataDev()+" rw")
	}
	return nil, nil
}

func (in *instance) run() (*FirstBootResult, error) {
	in.t.Helper()
	// The volume is attached some polls after the instance starts.
	if in.appearAt > 0 {
		link := filepath.Join(in.byID, "nvme-Amazon_Elastic_Block_Store_vol0data")
		target, _ := os.Readlink(link)
		os.Remove(link)
		in.fb.Sleep = func(context.Context, time.Duration) error {
			in.polls++
			if in.polls == in.appearAt {
				_ = os.Symlink(target, link)
			}
			return nil
		}
	}
	return in.fb.Run(context.Background())
}

func TestFirstBootFormatsABlankVolumeAndMounts(t *testing.T) {
	in := newInstance(t)
	res, err := in.run()
	if err != nil {
		t.Fatalf("%v\n%s", err, in.out.String())
	}
	if res.InstanceID != "i-0joiner" || res.StackName != "supavise-b" || res.Device != in.dataDev() || !res.Formatted {
		t.Errorf("result = %+v", res)
	}
	if in.runner.ran("mkfs.xfs -L supavise "+in.dataDev()) != 1 {
		t.Errorf("calls: %v", in.runner.calls)
	}
	// The root volume is never touched.
	for _, c := range in.runner.calls {
		if strings.Contains(c, "nvme0n1") && (strings.HasPrefix(c, "mkfs") || strings.HasPrefix(c, "mount")) {
			t.Errorf("touched the root volume: %s", c)
		}
	}
	fstab, _ := os.ReadFile(in.fstab)
	for _, want := range []string{
		"UUID=1111-2222 " + in.state + " xfs defaults,nofail,prjquota 0 2",
		filepath.Join(in.state, "etc") + " " + in.config + " none bind,nofail,x-systemd.requires-mounts-for=" + in.state + " 0 0",
	} {
		if !strings.Contains(string(fstab), want) {
			t.Errorf("fstab lacks %q:\n%s", want, fstab)
		}
	}
	if in.runner.ran("mount "+in.state) != 1 || in.runner.ran("mount --bind "+filepath.Join(in.state, "etc")+" "+in.config) != 1 || in.runner.ran("xfs_growfs "+in.state) != 1 {
		t.Errorf("calls: %v", in.runner.calls)
	}
	for _, u := range []string{"supavise.service", "supavise-postgres@.service", "supavise-realtime.service"} {
		b, err := os.ReadFile(filepath.Join(in.units, u+".d", dataMountDropIn))
		if err != nil || string(b) != "[Unit]\nRequiresMountsFor="+in.state+"\n" {
			t.Errorf("%s: %q, %v", u, b, err)
		}
		if b, err := os.ReadFile(filepath.Join(in.units, u+".d", etcMountDropIn)); err != nil || string(b) != "[Unit]\nRequiresMountsFor="+in.config+"\n" {
			t.Errorf("%s etc: %q, %v", u, b, err)
		}
	}
	if in.runner.ran("systemctl daemon-reload") != 1 {
		t.Errorf("calls: %v", in.runner.calls)
	}
	for _, d := range []string{filepath.Join(in.state, "etc"), in.config} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
}

// A second run, as after a reboot or a repair, formats nothing and mounts nothing again.
func TestFirstBootIsRepeatable(t *testing.T) {
	in := newInstance(t)
	if _, err := in.run(); err != nil {
		t.Fatal(err)
	}
	before := len(in.runner.calls)
	fstab, _ := os.ReadFile(in.fstab)
	res, err := in.run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Formatted {
		t.Error("a volume that holds XFS was formatted again")
	}
	for _, c := range in.runner.calls[before:] {
		if strings.HasPrefix(c, "mkfs") || strings.HasPrefix(c, "mount") || strings.HasPrefix(c, "systemctl") {
			t.Errorf("second run called %q", c)
		}
	}
	if again, _ := os.ReadFile(in.fstab); string(again) != string(fstab) {
		t.Errorf("fstab changed:\n%s\n--\n%s", fstab, again)
	}
}

// Only a blank device is formatted: a volume with data, with another file system or with
// partitions is left alone.
func TestFirstBootFormatsOnlyBlankDevices(t *testing.T) {
	for _, c := range []struct {
		holds   string
		wantErr string
		format  bool
	}{
		{"", "", true},
		{"xfs", "", false},
		{"ext4", "holds ext4", false},
		{"gpt", "partitions", false},
	} {
		in := newInstance(t)
		in.holds = c.holds
		_, err := in.run()
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%q: %v", c.holds, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%q: err = %v, want %q", c.holds, err, c.wantErr)
		}
		if got := in.runner.ran("mkfs") > 0; got != c.format {
			t.Errorf("%q: formatted = %v, want %v (calls %v)", c.holds, got, c.format, in.runner.calls)
		}
		if c.wantErr != "" && in.runner.ran("mount") > 0 {
			t.Errorf("%q: mounted a volume it refused: %v", c.holds, in.runner.calls)
		}
	}
}

// blkid failing for any reason but "nothing found" is not blank.
func TestFirstBootDoesNotFormatWhenBlkidFails(t *testing.T) {
	in := newInstance(t)
	in.runner.do = func(name string, args []string) ([]byte, error) {
		if name == "blkid" && args[0] == "-p" {
			return nil, &ExitError{Command: "blkid -p", Code: 4, Err: errors.New("exit status 4")}
		}
		return in.answer(name, args)
	}
	if _, err := in.run(); err == nil || !strings.Contains(err.Error(), "cannot tell whether") {
		t.Fatalf("err = %v", err)
	}
	if in.runner.ran("mkfs") != 0 {
		t.Errorf("formatted: %v", in.runner.calls)
	}
}

func TestFirstBootWaitsForTheVolume(t *testing.T) {
	in := newInstance(t)
	in.appearAt = 3
	res, err := in.run()
	if err != nil || res.Device != in.dataDev() {
		t.Fatalf("%+v, %v", res, err)
	}
	if in.polls != 3 {
		t.Errorf("polled %d times", in.polls)
	}

	in = newInstance(t)
	in.fb.Timeout = -time.Second
	link := filepath.Join(in.byID, "nvme-Amazon_Elastic_Block_Store_vol0data")
	os.Remove(link)
	if _, err := in.fb.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "did not appear") {
		t.Errorf("err = %v", err)
	}
}

func TestFirstBootNeedsAnEC2InstanceAndAKnownRootDisk(t *testing.T) {
	in := newInstance(t)
	in.fb.IMDS = failingIMDS{}
	if _, err := in.fb.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "EC2 instance") {
		t.Errorf("err = %v", err)
	}
	if len(in.runner.calls) != 0 {
		t.Errorf("ran %v on a host that is not EC2", in.runner.calls)
	}

	// Without knowing the root disk the root volume could be mistaken for the data volume.
	in = newInstance(t)
	in.runner.do = func(name string, args []string) ([]byte, error) {
		if name == "findmnt" {
			return nil, errors.New("findmnt: not found")
		}
		return in.answer(name, args)
	}
	if _, err := in.run(); err == nil || !strings.Contains(err.Error(), "which disk holds /") {
		t.Errorf("err = %v", err)
	}
	if in.runner.ran("mkfs") != 0 || in.runner.ran("mount") != 0 {
		t.Errorf("calls: %v", in.runner.calls)
	}
}

type failingIMDS struct{}

func (failingIMDS) InstanceID(context.Context) (string, error) { return "", errors.New("no route") }
func (failingIMDS) Tags(context.Context) (map[string]string, error) {
	return nil, errors.New("no route")
}

func TestFirstBootRefusesSeveralCandidates(t *testing.T) {
	in := newInstance(t)
	in.disk("nvme2n1", "nvme-Amazon_Elastic_Block_Store_vol0other")
	if _, err := in.run(); err == nil || !strings.Contains(err.Error(), "--data-device") {
		t.Fatalf("err = %v", err)
	}
	if in.runner.ran("mkfs") != 0 {
		t.Errorf("formatted one of two candidates")
	}
	in.fb.Device = in.dataDev()
	if _, err := in.run(); err != nil {
		t.Errorf("with --data-device: %v", err)
	}
}

// An explicit --data-device is not trusted blindly: the root disk, a partition of it and the device
// the root file system is mounted from are refused, and nothing is formatted or mounted.
func TestFirstBootRefusesTheRootVolumeAsTheNamedDevice(t *testing.T) {
	for name, dev := range map[string]string{
		"the root disk by its node":         "nvme0n1",
		"the device the root is mounted on": "/dev/nvme0n1p1",
		"another partition of the root":     "nvme0n1p2",
	} {
		t.Run(name, func(t *testing.T) {
			in := newInstance(t)
			if !strings.HasPrefix(dev, "/") {
				dev = filepath.Join(in.root, "dev", dev)
				if err := os.WriteFile(dev, nil, 0o600); err != nil && !os.IsExist(err) {
					t.Fatal(err)
				}
			}
			wrapped := in.answer
			in.runner.do = func(n string, a []string) ([]byte, error) {
				if n+" "+strings.Join(a, " ") == "lsblk -no PKNAME "+dev && strings.Contains(dev, "nvme0n1p") {
					return []byte("nvme0n1\n"), nil
				}
				return wrapped(n, a)
			}
			in.fb.Device = dev
			if _, err := in.run(); err == nil || !strings.Contains(err.Error(), "root volume") {
				t.Fatalf("err = %v", err)
			}
			if in.runner.ran("mkfs") != 0 || in.runner.ran("mount") != 0 {
				t.Errorf("the root volume was formatted or mounted: %v", in.runner.calls)
			}
		})
	}
	// The data volume named explicitly is fine, and so is a root that cannot be told only when no
	// device was named (the next test): here the check needs the root and fails without it.
	in := newInstance(t)
	in.fb.Device = in.dataDev()
	if _, err := in.run(); err != nil {
		t.Errorf("the data volume named by hand: %v", err)
	}
	in = newInstance(t)
	in.fb.Device = in.dataDev()
	in.runner.do = func(n string, a []string) ([]byte, error) {
		if n == "findmnt" {
			return nil, errors.New("findmnt: not found")
		}
		return in.answer(n, a)
	}
	if _, err := in.run(); err == nil || !strings.Contains(err.Error(), "which disk holds /") {
		t.Errorf("a named device with an unknown root: %v", err)
	}
}

// Ubuntu 24.04 (systemd 255) gives every EBS disk two links, with and without a "_1" suffix, and its
// partitions two each. One data volume is one candidate, whatever its links.
func TestFirstBootCountsADiskOnceWhateverItsLinks(t *testing.T) {
	in := newInstance(t)
	link := func(name, target string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(in.byID, name)); err != nil {
			t.Fatal(err)
		}
	}
	link("nvme-Amazon_Elastic_Block_Store_vol0root_1", filepath.Join(in.root, "dev/nvme0n1"))
	link("nvme-Amazon_Elastic_Block_Store_vol0root_1-part1", filepath.Join(in.root, "dev/nvme0n1p1"))
	link("nvme-Amazon_Elastic_Block_Store_vol0data_1", in.dataDev())
	link("nvme-Amazon_Elastic_Block_Store_vol0data-part1", filepath.Join(in.root, "dev/nvme1n1p1"))
	link("nvme-Amazon_Elastic_Block_Store_vol0data_1-part1", filepath.Join(in.root, "dev/nvme1n1p1"))
	res, err := in.run()
	if err != nil || res.Device != in.dataDev() {
		t.Fatalf("%+v, %v", res, err)
	}

	// A second disk is still a second candidate.
	in.disk("nvme2n1", "nvme-Amazon_Elastic_Block_Store_vol0other")
	link("nvme-Amazon_Elastic_Block_Store_vol0other_1", filepath.Join(in.root, "dev/nvme2n1"))
	if _, err := in.run(); err == nil || !strings.Contains(err.Error(), "2 EBS volumes") {
		t.Errorf("err = %v", err)
	}
}

// What blkid prints goes into fstab, so it has to be a UUID.
func TestFirstBootWritesOnlyAUUIDToFstab(t *testing.T) {
	for name, out := range map[string]string{
		"a warning": "blkid: error: cannot open /dev/nvme1n1\n",
		"nothing":   "\n",
		"two lines": "1111-2222\nsomething else\n",
	} {
		in := newInstance(t)
		answer := in.runner.do
		in.runner.do = func(name string, args []string) ([]byte, error) {
			if name == "blkid" && args[0] == "-s" {
				return []byte(out), nil
			}
			return answer(name, args)
		}
		if _, err := in.run(); err == nil || !strings.Contains(err.Error(), "did not print a UUID") {
			t.Errorf("%s: err = %v", name, err)
		}
		if b, _ := os.ReadFile(in.fstab); len(b) != 0 {
			t.Errorf("%s: fstab = %q", name, b)
		}
		if in.runner.ran("mount") != 0 {
			t.Errorf("%s: mounted: %v", name, in.runner.calls)
		}
	}
}

// An instance that takes over a volume an earlier install used gets the account back with the
// ids that own the files.
func TestFirstBootRecreatesTheAccountOfAnEarlierInstall(t *testing.T) {
	uid, gid := 1001, 1002
	setup := func() *instance {
		in := newInstance(t)
		in.holds = "xfs"
		in.ownerUID, in.ownerGID = uid, gid
		if err := os.MkdirAll(in.state, 0o750); err != nil {
			t.Fatal(err)
		}
		return in
	}

	in := setup()
	if _, err := in.run(); err != nil {
		t.Fatalf("%v\n%s", err, in.out.String())
	}
	if in.runner.ran("groupadd --system --gid "+strconv.Itoa(gid)+" supavise") != 1 || in.runner.ran("useradd --system --uid "+strconv.Itoa(uid)+" --gid "+strconv.Itoa(gid)+" --home-dir "+in.state) != 1 {
		t.Errorf("calls: %v", in.runner.calls)
	}

	// The account exists already: nothing to do.
	in = setup()
	in.accounts.users["supavise"] = [2]int{uid, gid}
	if _, err := in.run(); err != nil || in.runner.ran("useradd") != 0 {
		t.Errorf("existing account: %v, %v", err, in.runner.calls)
	}

	// Another group or user holds the id: the repair stops with the way out.
	in = setup()
	in.accounts.gnames[gid] = "staff"
	if _, err := in.run(); err == nil || !strings.Contains(err.Error(), "taken by group staff") {
		t.Errorf("group taken: %v", err)
	}
	in = setup()
	in.accounts.gnames[gid] = "supavise"
	in.accounts.uids[uid] = "ubuntu"
	if _, err := in.run(); err == nil || !strings.Contains(err.Error(), "taken by user ubuntu") {
		t.Errorf("uid taken: %v", err)
	}
	if in.runner.ran("useradd") != 0 {
		t.Error("useradd ran with the uid taken")
	}
	in = setup()
	in.accounts.gids["supavise"] = 999
	if _, err := in.run(); err == nil || !strings.Contains(err.Error(), "group supavise exists with gid 999") {
		t.Errorf("group exists: %v", err)
	}
}
