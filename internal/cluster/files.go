package cluster

import (
	"os"

	"github.com/supavise/supavise/internal/fsutil"
)

// writeOpts is how this package writes its files: through a temporary file in the same directory,
// synced before the rename, into a directory that is created with mode 0750 when it is missing. The
// owner of the directory goes to the file and to the directories made: the commands that write these
// files run as root during an install and the daemon reads them as the supavise user.
var writeOpts = fsutil.Options{Sync: true, MkdirMode: 0o750, OwnerLike: fsutil.Dir}

func writeFile(path string, data []byte, mode os.FileMode) error {
	return fsutil.WriteFile(path, data, mode, writeOpts)
}
