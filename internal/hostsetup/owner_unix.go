//go:build unix

package hostsetup

import (
	"io/fs"
	"syscall"
)

// ownerOf returns the uid and gid that own the file.
func ownerOf(fi fs.FileInfo) (uid, gid int, ok bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid), true
	}
	return 0, 0, false
}
