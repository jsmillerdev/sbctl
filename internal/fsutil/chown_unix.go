//go:build unix

package fsutil

import (
	"os"
	"syscall"
)

// ChownLike gives path the owner and group of ref, best effort: only root can, and only root
// needs to.
func ChownLike(path, ref string) {
	if os.Geteuid() != 0 {
		return
	}
	if uid, gid, ok := OwnerOf(ref); ok {
		_ = os.Chown(path, uid, gid)
	}
}

// OwnerOf returns the owner and group of path.
func OwnerOf(path string) (uid, gid int, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
