//go:build linux

package hostsetup

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

// The first-boot tests in firstboot_test.go answer every command with a fake. This one runs the real
// tools (blkid, lsblk, findmnt, mkfs.xfs, mount, xfs_quota) on loop devices, as root, which is the
// only way to see what the fakes cannot: what blkid prints for a blank device, what udev's links
// look like, what the kernel reports for a mount. tests/linux/firstboot-e2e.sh runs it on a CI VM.
//
// It writes /etc/fstab (and puts it back), so it never runs by accident.
func TestFirstBootOnLoopDevices(t *testing.T) {
	if os.Getenv("SUPAVISE_FIRSTBOOT_E2E") != "1" {
		t.Skip("set SUPAVISE_FIRSTBOOT_E2E=1 to run first boot on loop devices (tests/linux/firstboot-e2e.sh)")
	}
	if os.Geteuid() != 0 {
		t.Fatal("run as root")
	}
	for _, tool := range []string{"blkid", "lsblk", "findmnt", "mkfs.xfs", "mkfs.ext4", "mount", "umount", "losetup", "xfs_growfs", "xfs_quota"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not installed", tool)
		}
	}

	fstab, err := os.ReadFile("/etc/fstab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile("/etc/fstab", fstab, 0o644)
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})

	dev := loopDevice(t, "xfs-data")
	other := loopDevice(t, "ext4-data")
	sh(t, "mkfs.ext4", "-q", "-F", other)

	// What udev gives an EBS disk on Ubuntu 24.04: two links per disk and two per partition. The
	// root disk is the runner's own, so that the code has something real to leave alone.
	root := rootDiskNode(t)
	byID := t.TempDir()
	for _, l := range []struct{ name, target string }{
		{"vol0root", root}, {"vol0root_1", root},
		{"vol0root-part1", root + "1"}, {"vol0root_1-part1", root + "1"},
		{"vol0data", dev}, {"vol0data_1", dev},
		{"vol0data-part1", dev + "p1"}, {"vol0data_1-part1", dev + "p1"},
	} {
		if err := os.Symlink(l.target, filepath.Join(byID, "nvme-Amazon_Elastic_Block_Store_"+l.name)); err != nil {
			t.Fatal(err)
		}
	}

	fake := awsfake.New(t)
	d := fake.IMDS()
	d.InstanceID, d.Tags = "i-0firstboot", map[string]string{"supavise:stack-name": "supavise-b"}
	fake.SetIMDS(d)

	base := t.TempDir()
	var out bytes.Buffer
	fb := &FirstBoot{
		StateDir: filepath.Join(base, "var/lib/supavise"), ConfigDir: filepath.Join(base, "etc/supavise"), UnitDir: filepath.Join(base, "etc/systemd/system"),
		ByID: byID, Fstab: "/etc/fstab", User: "supavise",
		IMDS: fake.Client().IMDS, Runner: ExecRunner{}, Timeout: 20 * time.Second, Poll: time.Second, Out: &out,
	}
	t.Cleanup(func() {
		// Registered after the directories, so it runs before they are removed.
		_ = exec.Command("umount", "-l", fb.ConfigDir).Run()
		_ = exec.Command("umount", "-l", fb.StateDir).Run()
	})

	res, err := fb.Run(context.Background())
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if res.Device != dev || !res.Formatted || res.StackName != "supavise-b" || res.InstanceID != "i-0firstboot" {
		t.Fatalf("result = %+v, want device %s formatted\n%s", res, dev, out.String())
	}

	// The data volume is an XFS file system with project quotas, mounted at the state directory.
	if got := sh(t, "findmnt", "-n", "-o", "FSTYPE,OPTIONS", "-M", fb.StateDir); !strings.HasPrefix(got, "xfs ") || !strings.Contains(got, "prjquota") {
		t.Errorf("state directory mount = %q, want xfs with prjquota", got)
	}
	if got := sh(t, "xfs_quota", "-x", "-c", "state -p", fb.StateDir); !strings.Contains(got, "Enforcement: ON") {
		t.Errorf("project quotas are not enforced:\n%s", got)
	}
	// The configuration directory is the volume's etc, bound.
	if got := sh(t, "findmnt", "-n", "-o", "SOURCE", "-M", fb.ConfigDir); !strings.HasSuffix(got, "[/etc]") {
		t.Errorf("config directory source = %q, want the volume's /etc", got)
	}
	if err := os.WriteFile(filepath.Join(fb.ConfigDir, "probe"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fb.StateDir, "etc", "probe")); err != nil {
		t.Errorf("a file written to the config directory is not on the volume: %v", err)
	}

	// fstab remounts both after a reboot, and the units wait for them.
	uuid := sh(t, "blkid", "-s", "UUID", "-o", "value", dev)
	tab, _ := os.ReadFile("/etc/fstab")
	for _, want := range []string{
		"UUID=" + uuid + " " + fb.StateDir + " xfs defaults,nofail,prjquota 0 2",
		filepath.Join(fb.StateDir, "etc") + " " + fb.ConfigDir + " none bind,nofail,x-systemd.requires-mounts-for=" + fb.StateDir + " 0 0",
	} {
		if !strings.Contains(string(tab), want) {
			t.Errorf("fstab lacks %q:\n%s", want, tab)
		}
	}
	for _, f := range []string{
		filepath.Join(fb.UnitDir, "supavise.service.d", dataMountDropIn),
		filepath.Join(fb.UnitDir, "supavise-postgres@.service.d", etcMountDropIn),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("no drop-in: %v", err)
		}
	}

	// A second run (a reboot, a repair) formats and mounts nothing again.
	again, err := fb.Run(context.Background())
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, out.String())
	}
	if again.Formatted || again.Device != dev {
		t.Errorf("second result = %+v", again)
	}
	if tab2, _ := os.ReadFile("/etc/fstab"); string(tab2) != string(tab) {
		t.Errorf("the second run changed fstab:\n%s", tab2)
	}
	if n := strings.Count(sh(t, "findmnt", "-n", "-o", "TARGET", "-M", fb.StateDir), "\n"); n > 0 {
		t.Errorf("the state directory is mounted more than once")
	}
	if got := sh(t, "blkid", "-s", "UUID", "-o", "value", dev); got != uuid {
		t.Errorf("the second run changed the file system: UUID %s -> %s", uuid, got)
	}

	// A volume that holds anything but XFS is left as it is.
	fb2 := *fb
	fb2.Device = other
	fb2.StateDir, fb2.ConfigDir = filepath.Join(base, "other/lib"), filepath.Join(base, "other/etc")
	tabBefore, _ := os.ReadFile("/etc/fstab")
	if _, err := fb2.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "holds ext4") {
		t.Errorf("a volume with ext4 on it: err = %v", err)
	}
	if got := sh(t, "blkid", "-s", "TYPE", "-o", "value", other); got != "ext4" {
		t.Errorf("the refused volume is now %q", got)
	}
	if tab3, _ := os.ReadFile("/etc/fstab"); string(tab3) != string(tabBefore) {
		t.Errorf("a refused volume changed fstab:\n%s", tab3)
	}
}

// loopDevice attaches a 512 MB sparse file to a loop device and detaches it when the test ends.
func loopDevice(t *testing.T, name string) string {
	t.Helper()
	img := filepath.Join(t.TempDir(), name+".img")
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img, 512<<20); err != nil {
		t.Fatal(err)
	}
	dev := sh(t, "losetup", "--find", "--show", img)
	t.Cleanup(func() { _ = exec.Command("losetup", "-d", dev).Run() })
	return dev
}

// rootDiskNode is the device node of the disk the root file system is on, found the way first boot
// finds it.
func rootDiskNode(t *testing.T) string {
	t.Helper()
	src := sh(t, "findmnt", "-n", "-o", "SOURCE", "/")
	disk := sh(t, "lsblk", "-no", "PKNAME", src)
	if disk = strings.TrimSpace(strings.SplitN(disk, "\n", 2)[0]); disk == "" {
		t.Fatalf("lsblk names no disk for %s", src)
	}
	return "/dev/" + disk
}

// sh runs a command and returns its trimmed output; a failure fails the test.
func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
