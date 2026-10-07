package backup

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const (
	soPeerPidfd       = 77  // SO_PEERPIDFD (Linux 6.5): a pidfd of the process that connected
	sysPidfdSendSgnal = 424 // pidfd_send_signal(2): the same number on every architecture supavise builds for
)

// checkRelayPeer accepts a connection to ref's socket only from a process whose systemd unit
// may use it (relayPeerUnitAllowed).
//
// SO_PEERCRED gives a pid, and the unit is read from /proc/<pid>/cgroup afterwards, so the
// pid must still be the connecting process when the file is read. A process in another unit
// could connect from a child that exits at once and make the target project's postmaster
// fork until the pid is reused inside the target's unit. On Linux 6.5 and later SO_PEERPIDFD
// returns a pidfd for the connecting process itself: a pid that was reused names a different
// struct pid, so a pidfd_send_signal(0) after the cgroup read tells whether the process (or
// its zombie, whose pid cannot be reused) was still there. Older kernels (Debian 12 has
// 6.1) have no such option; there the check falls back to comparing the process's start time
// before and after the read, which closes the window of the read only.
func checkRelayPeer(c net.Conn, ref string) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("not a unix connection")
	}
	sc, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var cerr error
	pidfd := -1
	if err := sc.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if cerr == nil {
			if v, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, soPeerPidfd); err == nil {
				pidfd = v
			}
		}
	}); err != nil {
		return err
	}
	if pidfd >= 0 {
		defer syscall.Close(pidfd)
	}
	if cerr != nil {
		return cerr
	}
	if cred.Pid <= 0 {
		return fmt.Errorf("the peer's pid is not visible")
	}
	before := uint64(0)
	if pidfd < 0 {
		if before, err = procStartTime(int(cred.Pid)); err != nil {
			return fmt.Errorf("peer %d: %w", cred.Pid, err)
		}
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", cred.Pid))
	if err != nil {
		return fmt.Errorf("peer %d: %w", cred.Pid, err)
	}
	if pidfd >= 0 {
		if !pidfdAlive(pidfd) {
			return fmt.Errorf("peer %d is gone: its pid may have been reused", cred.Pid)
		}
	} else if after, err := procStartTime(int(cred.Pid)); err != nil || after != before {
		return fmt.Errorf("peer %d changed while its cgroup was read", cred.Pid)
	}
	unit, err := peerUnit(string(b))
	if err != nil {
		return fmt.Errorf("peer %d: %w", cred.Pid, err)
	}
	if !relayPeerUnitAllowed(unit, ref) {
		return fmt.Errorf("peer %d in %s may not use the relay of %s", cred.Pid, unit, ref)
	}
	return nil
}

// pidfdAlive reports whether the process behind pidfd still exists (a zombie counts: its pid
// cannot be reused until it is reaped). EPERM means it exists and the caller may not signal it.
func pidfdAlive(pidfd int) bool {
	_, _, errno := syscall.Syscall6(sysPidfdSendSgnal, uintptr(pidfd), 0, 0, 0, 0, 0)
	return errno == 0 || errno == syscall.EPERM
}

// procStartTime returns the start time (clock ticks since boot) of pid, which identifies a
// process among those that had the same pid.
func procStartTime(pid int) (uint64, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	return parseStatStartTime(string(b))
}

// parseStatStartTime reads field 22 of a /proc/<pid>/stat line. The command name (field 2) is
// in parentheses and may hold spaces and parentheses, so the fields count from the last ')'.
func parseStatStartTime(stat string) (uint64, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, errors.New("malformed /proc stat")
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 20 { // field 3 is f[0], so field 22 is f[19]
		return 0, errors.New("malformed /proc stat")
	}
	return strconv.ParseUint(f[19], 10, 64)
}
