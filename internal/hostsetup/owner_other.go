//go:build !unix

package hostsetup

import "io/fs"

func ownerOf(fs.FileInfo) (int, int, bool) { return 0, 0, false }
