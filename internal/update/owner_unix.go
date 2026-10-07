//go:build unix

package update

import (
	"io/fs"
	"syscall"
)

// ownerOf returns the uid that owns the file.
func ownerOf(fi fs.FileInfo) (uid int, ok bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), true
	}
	return 0, false
}
