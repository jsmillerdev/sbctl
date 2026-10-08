//go:build darwin

package storagemigrate

import "golang.org/x/sys/unix"

// errNoAttr is what a missing extended attribute reports.
var errNoAttr = unix.ENOATTR
