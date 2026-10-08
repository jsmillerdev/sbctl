//go:build unix

package cluster

import (
	"os"
	"syscall"
)

// chownLike gives path the owner and group of ref, best effort: only root can, and only root
// needs to.
func chownLike(path, ref string) {
	if os.Geteuid() != 0 {
		return
	}
	fi, err := os.Stat(ref)
	if err != nil {
		return
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = os.Chown(path, int(st.Uid), int(st.Gid))
	}
}
