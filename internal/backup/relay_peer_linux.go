package backup

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// checkRelayPeer accepts a connection to ref's socket only from a process whose systemd unit
// may use it (relayPeerUnitAllowed).
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
	if err := sc.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if cerr != nil {
		return cerr
	}
	if cred.Pid <= 0 {
		return fmt.Errorf("the peer's pid is not visible")
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", cred.Pid))
	if err != nil {
		return fmt.Errorf("peer %d: %w", cred.Pid, err)
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
