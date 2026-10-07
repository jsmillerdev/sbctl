//go:build linux

package branching

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const cloneMethodName = MethodReflink

// cloneFile makes dst a copy-on-write clone of the regular file src with the FICLONE
// ioctl (XFS with reflink=1, btrfs, bcachefs, OpenZFS 2.2 and later). dst must not exist.
func cloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		out.Close()
		os.Remove(dst)
		if errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EINVAL) ||
			errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.ENOSYS) {
			return errNoClone
		}
		return err
	}
	return out.Close()
}

// Magic numbers of statfs(2) f_type.
var fsMagic = map[int64]string{
	0x58465342: "xfs", 0x9123683e: "btrfs", 0xef53: "ext4", 0x2fc12fc1: "zfs", 0x01021994: "tmpfs",
	0x794c7630: "overlayfs", 0xf2f52010: "f2fs", 0xca451a4e: "bcachefs", 0x6969: "nfs", 0x65735546: "fuse",
}

// fsName is the name of the filesystem that holds path.
func fsName(path string) string {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "unknown"
	}
	if n, ok := fsMagic[int64(st.Type)]; ok {
		return n
	}
	return "unknown"
}
