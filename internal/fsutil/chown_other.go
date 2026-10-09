//go:build !unix

package fsutil

// ChownLike does nothing where files have no Unix owner.
func ChownLike(path, ref string) {}

// OwnerOf reports no owner where files have no Unix owner.
func OwnerOf(path string) (uid, gid int, ok bool) { return 0, 0, false }
