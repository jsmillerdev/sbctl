package backup

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestParseStatStartTime(t *testing.T) {
	// Field 2 holds spaces and parentheses; start time is field 22.
	line := "4242 (a (weird) name) S 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000000 50 18446744073709551615 0 0 0\n"
	got, err := parseStatStartTime(line)
	if err != nil || got != 987654 {
		t.Fatalf("start time = %d, %v", got, err)
	}
	for _, bad := range []string{"", "1 (x", "1 (x) S 1 2"} {
		if _, err := parseStatStartTime(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	if _, err := procStartTime(os.Getpid()); err != nil {
		t.Fatalf("own start time: %v", err)
	}
}

func TestPidfdAlive(t *testing.T) {
	// pidfd_open(2) of this process: alive. (Syscall 434 on every architecture.)
	fd, _, errno := syscall.Syscall(434, uintptr(os.Getpid()), 0, 0)
	if errno != 0 {
		t.Skipf("pidfd_open: %v", errno)
	}
	defer syscall.Close(int(fd))
	if !pidfdAlive(int(fd)) {
		t.Fatal("a pidfd of a running process is not alive")
	}
}

// The peer of a connection from this test process is in no sb-* unit, so it is accepted,
// whichever of the pidfd and start-time paths the kernel offers.
func TestCheckRelayPeerAcceptsAnOrdinaryProcess(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "sbp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		done <- checkRelayPeer(c, testRef)
	}()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
