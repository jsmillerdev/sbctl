//go:build !unix

package update

// tryLock has nothing to lock with off Unix: the update service belongs to a Linux install.
func tryLock(string) (release func(), held bool, err error) { return func() {}, false, nil }
