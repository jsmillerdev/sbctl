package hostsetup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The CloudFormation template's first-boot script used to do this in bash: find the data volume,
// make a file system on it when it is blank, mount it with project quotas, bind /etc/supavise to
// it, and make the units wait for both mounts. A joiner's user data is a short stub that calls
// `supavise install --aws-first-boot`, and this is that code.

// IMDS is the part of the instance metadata service the first-boot code reads; *awsapi.IMDS
// implements it.
type IMDS interface {
	InstanceID(ctx context.Context) (string, error)
	Tags(ctx context.Context) (map[string]string, error)
}

// Accounts looks up and creates the users and groups first boot needs when the data volume comes
// from an earlier install. The default is the operating system's.
type Accounts interface {
	UserByName(name string) (uid, gid int, ok bool)
	UserNameByID(uid int) (name string, ok bool)
	GroupNameByID(gid int) (name string, ok bool)
	GroupIDByName(name string) (gid int, ok bool)
}

type osAccounts struct{}

func (osAccounts) UserByName(name string) (int, int, bool) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, false
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uid, gid, true
}

func (osAccounts) UserNameByID(uid int) (string, bool) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", false
	}
	return u.Username, true
}

func (osAccounts) GroupNameByID(gid int) (string, bool) {
	g, err := user.LookupGroupId(strconv.Itoa(gid))
	if err != nil {
		return "", false
	}
	return g.Name, true
}

func (osAccounts) GroupIDByName(name string) (int, bool) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, false
	}
	gid, _ := strconv.Atoi(g.Gid)
	return gid, true
}

// FirstBoot prepares an EC2 instance's data volume and the mounts that depend on it.
type FirstBoot struct {
	// StateDir and ConfigDir are the directories the volume holds (/var/lib/supavise, and
	// /etc/supavise as a bind mount of <StateDir>/etc). UnitDir is where the mount drop-ins go.
	StateDir, ConfigDir, UnitDir string
	// Device is the data volume's device node. Empty finds the one EBS volume that is not the
	// root volume.
	Device string
	// ByID is the directory of device links (/dev/disk/by-id) and Fstab the file system table
	// (/etc/fstab).
	ByID, Fstab string
	// User is the service account (supavise); a data volume that an earlier install owned gets
	// the account back with the ids that own its files.
	User string
	// Owner returns the ids that own a directory; nil stats it.
	Owner func(dir string) (uid, gid int, ok bool)

	IMDS     IMDS
	Runner   Runner
	Accounts Accounts
	// MountInfo reads /proc/self/mountinfo.
	MountInfo func() ([]byte, error)
	// Timeout bounds the wait for the volume to be attached (the instance exists before its data
	// volume does); Poll is the pause between looks.
	Timeout, Poll time.Duration
	// Sleep waits d or until ctx ends; nil is a real sleep.
	Sleep func(ctx context.Context, d time.Duration) error
	// Out receives a line for each thing done.
	Out io.Writer
}

// FirstBootResult says what first boot found out.
type FirstBootResult struct {
	InstanceID string
	Device     string
	// StackName is the CloudFormation stack, from the instance's supavise:stack-name tag; empty
	// when the instance has no tags.
	StackName string
	// Formatted is true when the volume was blank and first boot made the file system.
	Formatted bool
}

func (f *FirstBoot) say(format string, a ...any) {
	if f.Out != nil {
		fmt.Fprintf(f.Out, format+"\n", a...)
	}
}

func (f *FirstBoot) accounts() Accounts {
	if f.Accounts != nil {
		return f.Accounts
	}
	return osAccounts{}
}

func (f *FirstBoot) sleep(ctx context.Context, d time.Duration) error {
	if f.Sleep != nil {
		return f.Sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (f *FirstBoot) mountInfo() ([]byte, error) {
	if f.MountInfo != nil {
		return f.MountInfo()
	}
	return os.ReadFile("/proc/self/mountinfo")
}

func (f *FirstBoot) isMount(dir string) (bool, error) {
	b, err := f.mountInfo()
	if err != nil {
		return false, err
	}
	dir = filepath.Clean(dir)
	for _, m := range parseMountPoints(b) {
		if m == dir {
			return true, nil
		}
	}
	return false, nil
}

// Run does the work. Everything it does is repeatable: a second run finds the mounts, the fstab
// lines and the drop-ins in place and changes nothing.
func (f *FirstBoot) Run(ctx context.Context) (*FirstBootResult, error) {
	res := &FirstBootResult{}
	var err error
	// Only an instance that can say what it is has an EBS data volume of this kind; a laptop or a
	// VM of another cloud must not have a disk formatted by a flag meant for the template.
	if res.InstanceID, err = f.IMDS.InstanceID(ctx); err != nil {
		return nil, fmt.Errorf("--aws-first-boot is for an EC2 instance, and its metadata service did not answer: %w", err)
	}
	if tags, err := f.IMDS.Tags(ctx); err == nil {
		res.StackName = tags["supavise:stack-name"]
	}
	f.say("EC2 instance %s", res.InstanceID)

	f.say("finding the data volume")
	if res.Device, err = f.findDevice(ctx); err != nil {
		return nil, err
	}
	f.say("data volume %s", res.Device)
	if res.Formatted, err = f.prepareFilesystem(ctx, res.Device); err != nil {
		return nil, err
	}
	if err := f.mountData(ctx, res.Device); err != nil {
		return nil, err
	}
	if err := f.bindConfig(ctx); err != nil {
		return nil, err
	}
	if err := f.restoreAccount(ctx); err != nil {
		return nil, err
	}
	if err := f.dropIns(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

// findDevice returns the device node of the data volume: the EBS volume that is not the one the
// root file system is on. The volume is attached after the instance starts, so it waits.
func (f *FirstBoot) findDevice(ctx context.Context) (string, error) {
	if f.Device != "" {
		return f.Device, nil
	}
	root, err := f.rootDisk(ctx)
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(f.Timeout)
	for {
		cands, err := f.dataCandidates(root)
		switch {
		case err != nil:
			return "", err
		case len(cands) == 1:
			return cands[0], nil
		case len(cands) > 1:
			return "", fmt.Errorf("%d EBS volumes besides the root volume are attached (%s); name the data volume with --data-device", len(cands), strings.Join(cands, ", "))
		}
		if time.Now().After(deadline) {
			return "", errors.New("the data volume did not appear; is it attached to this instance?")
		}
		if err := f.sleep(ctx, f.Poll); err != nil {
			return "", err
		}
	}
}

// rootDisk is the name of the disk that holds the root file system ("nvme0n1"). It must be known:
// without it the root volume could be taken for the data volume.
func (f *FirstBoot) rootDisk(ctx context.Context) (string, error) {
	src, err := f.Runner.Run(ctx, nil, "findmnt", "-n", "-o", "SOURCE", "/")
	if err != nil {
		return "", fmt.Errorf("cannot tell which disk holds /: %w", err)
	}
	out, err := f.Runner.Run(ctx, nil, "lsblk", "-no", "PKNAME", strings.TrimSpace(string(src)))
	if err != nil {
		return "", fmt.Errorf("cannot tell which disk holds /: %w", err)
	}
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l, nil
		}
	}
	return "", errors.New("cannot tell which disk holds /: lsblk named no parent disk")
}

// dataCandidates lists the whole-disk EBS devices other than the root disk.
func (f *FirstBoot) dataCandidates(rootDisk string) ([]string, error) {
	ents, err := os.ReadDir(f.ByID)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "nvme-Amazon_Elastic_Block_Store_") || strings.Contains(n, "-part") {
			continue
		}
		dev, err := filepath.EvalSymlinks(filepath.Join(f.ByID, n))
		if err != nil {
			continue
		}
		if filepath.Base(dev) == rootDisk {
			continue
		}
		out = append(out, dev)
	}
	return out, nil
}

// prepareFilesystem makes an XFS file system on a blank device, and leaves a device that holds
// anything alone. A device with another file system or with partitions is an error: Supavise needs
// XFS (project quotas), and it never reformats a volume that has data.
func (f *FirstBoot) prepareFilesystem(ctx context.Context, dev string) (formatted bool, err error) {
	blank, err := f.isBlank(ctx, dev)
	if err != nil {
		return false, err
	}
	if blank {
		f.say("making an XFS file system on %s", dev)
		if _, err := f.Runner.Run(ctx, nil, "mkfs.xfs", "-L", "supavise", dev); err != nil {
			return false, err
		}
		return true, nil
	}
	out, _ := f.Runner.Run(ctx, nil, "blkid", "-o", "value", "-s", "TYPE", dev)
	if typ := strings.TrimSpace(string(out)); typ != "xfs" {
		if typ == "" {
			typ = "partitions or an unknown signature"
		}
		return false, fmt.Errorf("%s holds %s, not an XFS file system; Supavise will not reformat a volume that has data (use another volume, or wipe this one yourself)", dev, typ)
	}
	f.say("%s already holds an XFS file system", dev)
	return false, nil
}

// isBlank reports whether nothing on dev identifies a file system, a partition table or a
// partition. blkid exits with 2 when it finds nothing; any other failure is not an answer.
func (f *FirstBoot) isBlank(ctx context.Context, dev string) (bool, error) {
	_, err := f.Runner.Run(ctx, nil, "blkid", "-p", dev)
	switch exitCode(err) {
	case 0:
		return false, nil
	case 2:
	default:
		return false, fmt.Errorf("cannot tell whether %s is blank: %w", dev, err)
	}
	out, err := f.Runner.Run(ctx, nil, "lsblk", "-nr", "-o", "NAME", dev)
	if err != nil {
		return false, fmt.Errorf("cannot tell whether %s has partitions: %w", dev, err)
	}
	return len(nonEmptyLines(out)) <= 1, nil
}

func nonEmptyLines(b []byte) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// mountData mounts the volume at StateDir with project quotas on, and records it in fstab. Quotas
// can only be switched on when the file system is mounted, so a volume restored from a snapshot
// gets them here too.
func (f *FirstBoot) mountData(ctx context.Context, dev string) error {
	if err := os.MkdirAll(f.StateDir, 0o755); err != nil {
		return err
	}
	uuid, err := f.Runner.Run(ctx, nil, "blkid", "-s", "UUID", "-o", "value", dev)
	if err != nil {
		return fmt.Errorf("reading the UUID of %s: %w", dev, err)
	}
	line := fmt.Sprintf("UUID=%s %s xfs defaults,nofail,prjquota 0 2", strings.TrimSpace(string(uuid)), f.StateDir)
	if err := f.addFstab(f.StateDir, line); err != nil {
		return err
	}
	mounted, err := f.isMount(f.StateDir)
	if err != nil {
		return err
	}
	if !mounted {
		f.say("mounting %s", f.StateDir)
		if _, err := f.Runner.Run(ctx, nil, "mount", f.StateDir); err != nil {
			return err
		}
	}
	// A volume restored from a smaller snapshot comes up with the snapshot's size.
	if _, err := f.Runner.Run(ctx, nil, "xfs_growfs", f.StateDir); err != nil {
		f.say("xfs_growfs skipped: %v", err)
	}
	return nil
}

// addFstab appends line unless fstab already has a line for the mount point.
func (f *FirstBoot) addFstab(mountPoint, line string) error {
	b, err := os.ReadFile(f.Fstab)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, l := range strings.Split(string(b), "\n") {
		fields := strings.Fields(l)
		if len(fields) >= 2 && !strings.HasPrefix(fields[0], "#") && fields[1] == mountPoint {
			return nil
		}
	}
	file, err := os.OpenFile(f.Fstab, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if len(b) > 0 && b[len(b)-1] != '\n' {
		line = "\n" + line
	}
	_, err = file.WriteString(line + "\n")
	return err
}

// bindConfig keeps the node's configuration on the data volume. /etc/supavise holds the master
// key, which unseals every secret in the registry, and config.toml; on the root volume they would
// be lost with the instance and the volume's secrets with them.
func (f *FirstBoot) bindConfig(ctx context.Context) error {
	src := filepath.Join(f.StateDir, "etc")
	if err := os.MkdirAll(src, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(f.ConfigDir, 0o755); err != nil {
		return err
	}
	mounted, err := f.isMount(f.ConfigDir)
	if err != nil {
		return err
	}
	if !mounted {
		f.say("binding %s to %s", f.ConfigDir, src)
		if _, err := f.Runner.Run(ctx, nil, "mount", "--bind", src, f.ConfigDir); err != nil {
			return fmt.Errorf("bind-mount %s: %w", f.ConfigDir, err)
		}
	}
	return f.addFstab(f.ConfigDir, fmt.Sprintf("%s %s none bind,nofail,x-systemd.requires-mounts-for=%s 0 0", src, f.ConfigDir, f.StateDir))
}

// restoreAccount gives a replacement instance the service account of the instance whose volume it
// now has. The files on the volume belong to numeric ids; the units run as supavise, so the account
// must have those ids. A volume of a fresh install is root's and needs nothing here: the
// installer creates the account.
func (f *FirstBoot) restoreAccount(ctx context.Context) error {
	owner := f.Owner
	if owner == nil {
		owner = func(dir string) (int, int, bool) {
			fi, err := os.Stat(dir)
			if err != nil {
				return 0, 0, false
			}
			return ownerOf(fi)
		}
	}
	uid, gid, ok := owner(f.StateDir)
	if !ok || uid == 0 {
		return nil
	}
	if _, _, exists := f.accounts().UserByName(f.User); exists {
		return nil
	}
	f.say("the data volume holds an earlier install (owner uid %d): repairing it, keeping the master key", uid)
	// The ids come from the old instance; a package on this image may have taken one of them. The
	// units run as User=supavise Group=supavise, so a different primary group or uid would leave
	// the volume unreadable.
	holder, taken := f.accounts().GroupNameByID(gid)
	if taken && holder != f.User {
		return fmt.Errorf("gid %d of the data volume is taken by group %s on this image; chown -R the volume to a free uid and gid, then run the installer again", gid, holder)
	}
	if !taken {
		if g, exists := f.accounts().GroupIDByName(f.User); exists {
			return fmt.Errorf("group %s exists with gid %d, not the volume's %d", f.User, g, gid)
		}
		if _, err := f.Runner.Run(ctx, nil, "groupadd", "--system", "--gid", strconv.Itoa(gid), f.User); err != nil {
			return fmt.Errorf("cannot create group %s with gid %d: %w", f.User, gid, err)
		}
	}
	if name, taken := f.accounts().UserNameByID(uid); taken {
		return fmt.Errorf("uid %d of the data volume is taken by user %s on this image; chown -R the volume to a free uid and gid, then run the installer again", uid, name)
	}
	if _, err := f.Runner.Run(ctx, nil, "useradd", "--system", "--uid", strconv.Itoa(uid), "--gid", strconv.Itoa(gid),
		"--home-dir", f.StateDir, "--no-create-home", "--shell", "/usr/sbin/nologin", f.User); err != nil {
		return fmt.Errorf("cannot recreate the %s user with uid %d: %w", f.User, uid, err)
	}
	return nil
}

// dropIns makes the units wait for the mounts: converge's mounts step, run now because the
// installer starts units soon after, and systemd must know the dependency by then.
func (f *FirstBoot) dropIns(ctx context.Context) error {
	step := mountsStep(Options{StateDir: f.StateDir, ConfigDir: f.ConfigDir, UnitDir: f.UnitDir, MountInfo: f.MountInfo})
	out, err := step.Apply(ctx)
	if err != nil {
		return err
	}
	for _, l := range out.Changed {
		f.say("%s", l)
	}
	if out.Reload {
		if _, err := f.Runner.Run(ctx, nil, "systemctl", "daemon-reload"); err != nil {
			// Converge reloads again once the units are installed.
			f.say("systemctl daemon-reload failed: %v", err)
		}
	}
	return nil
}
