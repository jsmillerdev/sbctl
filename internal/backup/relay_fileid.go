package backup

import (
	"os"
	"syscall"
)

// fileID identifies a file by device and inode, so the relay can tell its own socket
// from a file that replaced it.
type fileID struct{ dev, ino uint64 }

// socketID returns the identity of the socket file at path.
func socketID(path string) (fileID, bool) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return fileID{}, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, false
	}
	return fileID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true //nolint:unconvert // Dev is int32 on darwin
}
