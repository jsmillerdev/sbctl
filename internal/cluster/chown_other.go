//go:build !unix

package cluster

func chownLike(path, ref string) {}
