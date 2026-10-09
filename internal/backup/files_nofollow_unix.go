//go:build linux || darwin

package backup

import (
	"os"
	"syscall"
)

// openNoFollow opens path for reading and refuses a symbolic link.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
