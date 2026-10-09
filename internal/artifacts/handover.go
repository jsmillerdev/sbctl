package artifacts

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/supavise/supavise/internal/fsutil"
)

// handOver gives a freshly unpacked tree to the owner of the state directory when root
// fetched it. The Postgres launcher chmods a script inside its artifact on first boot,
// which only the file's owner may do, and the services run as that owner (the supavise
// user), not as root. A fetch by the supavise user itself needs nothing.
func handOver(tree, stateDir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	uid, gid, ok := fsutil.OwnerOf(stateDir) // not ok: no state directory to take an owner from
	if !ok || uid == 0 {
		return nil
	}
	return filepath.WalkDir(tree, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}
