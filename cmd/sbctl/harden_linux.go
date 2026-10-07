package main

import (
	"fmt"
	"os"
	"syscall"
)

// prSetDumpable is PR_SET_DUMPABLE from <linux/prctl.h>.
const prSetDumpable = 4

// hardenProcess marks the process non-dumpable. The kernel then refuses a same-uid
// process without CAP_SYS_PTRACE access to /proc/<pid>/root, /proc/<pid>/environ and
// /proc/<pid>/mem (ptrace_may_access, PTRACE_MODE_READ). Every sb-* unit runs as the same
// user as the CLI an operator starts with `sudo -u sbctl sbctl ...`, and that CLI sees the
// host view of the filesystem (/etc/sbctl/master.key, the backend credentials); without
// this, code running in a tenant unit could read it through /proc while the command runs.
// The daemon and the backup units are also protected by the capability they hold
// (deploy/systemd/README.md); this covers everything else. Exec resets the flag, so
// the children a command starts (psql, pg_basebackup, systemctl) are unaffected.
func hardenProcess() {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		fmt.Fprintln(os.Stderr, "sbctl: warning: cannot mark the process non-dumpable:", errno)
	}
}
