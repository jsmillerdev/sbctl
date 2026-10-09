//go:build linux

package storageattr

import "golang.org/x/sys/unix"

// errNoAttr is what a missing extended attribute reports.
var errNoAttr = unix.ENODATA
