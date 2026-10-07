//go:build !unix

package update

import "io/fs"

func ownerOf(fs.FileInfo) (int, bool) { return 0, false }
