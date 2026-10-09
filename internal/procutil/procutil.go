// Package procutil answers questions about local processes by pid.
package procutil

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Alive reports whether a process with this pid exists. A process another user owns exists too
// (kill answers EPERM).
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// PostmasterPID reads the pid on the first line of dataDir's postmaster.pid and reports whether
// that process exists. It returns 0, false when the file is missing or unreadable.
func PostmasterPID(dataDir string) (pid int, alive bool) {
	b, err := os.ReadFile(filepath.Join(dataDir, "postmaster.pid"))
	if err != nil {
		return 0, false
	}
	first, _, _ := strings.Cut(string(b), "\n")
	pid, err = strconv.Atoi(strings.TrimSpace(first))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, Alive(pid)
}
