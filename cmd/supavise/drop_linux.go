package main

import "syscall"

// becomeSupavise makes this process the supavise user, for good. The commands that build a cluster's
// data directory (`node join`, `node rejoin`) run as root when an operator starts them with sudo, and
// whatever root creates, the supavise user's units cannot use: Postgres refuses a data directory it
// does not own, and the artifacts and the relay's sockets belong to the user that runs the daemon.
// The installer runs them as that user already.
func becomeSupavise() error {
	cred, err := supaviseCredential()
	if err != nil {
		return err
	}
	return dropTo(cred, syscall.Setgroups, syscall.Setgid, syscall.Setuid)
}
