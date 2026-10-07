//go:build darwin

package branching

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// cloneMethodName is what a successful cloneFile is called on this platform.
const cloneMethodName = MethodClonefile

// cloneFile makes dst a copy-on-write clone of the regular file src with clonefile(2)
// (APFS). dst must not exist.
func cloneFile(src, dst string) error {
	err := unix.Clonefile(src, dst, 0)
	if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EINVAL) {
		return errNoClone
	}
	return err
}

// fsName is the name of the filesystem that holds path ("apfs", "hfs", ...).
func fsName(path string) string {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "unknown"
	}
	b := make([]byte, 0, len(st.Fstypename))
	for _, v := range st.Fstypename {
		if v == 0 {
			break
		}
		b = append(b, byte(v))
	}
	return string(b)
}
