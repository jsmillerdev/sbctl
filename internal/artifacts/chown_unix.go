//go:build unix

package artifacts

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// handOver gives a freshly unpacked tree to the owner of the state directory when root
// fetched it. The Postgres launcher chmods a script inside its artifact on first boot,
// which only the file's owner may do, and the services run as that owner (the supavise
// user), not as root. A fetch by the supavise user itself needs nothing.
func handOver(tree, stateDir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	fi, err := os.Stat(stateDir)
	if err != nil {
		return nil // no state directory to take an owner from
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return nil
	}
	return filepath.WalkDir(tree, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, int(st.Uid), int(st.Gid))
	})
}
