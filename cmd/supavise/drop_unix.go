//go:build unix

package main

import (
	"fmt"
	"syscall"
)

// dropTo changes the process to cred with the three calls, in the order that works: after setuid the
// process may not change its groups or its gid, and a failure part-way leaves it as root, so the caller
// stops the command.
func dropTo(cred *syscall.Credential, setgroups func([]int) error, setgid, setuid func(int) error) error {
	groups := make([]int, len(cred.Groups))
	for i, g := range cred.Groups {
		groups[i] = int(g)
	}
	if err := setgroups(groups); err != nil {
		return fmt.Errorf("becoming the %s user: %w", installUser, err)
	}
	if err := setgid(int(cred.Gid)); err != nil {
		return fmt.Errorf("becoming the %s user: %w", installUser, err)
	}
	if err := setuid(int(cred.Uid)); err != nil {
		return fmt.Errorf("becoming the %s user: %w", installUser, err)
	}
	return nil
}
