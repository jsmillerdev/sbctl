package procutil

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPostmasterPID(t *testing.T) {
	dir := t.TempDir()
	if pid, ok := PostmasterPID(dir); pid != 0 || ok {
		t.Fatalf("missing file: %d %v", pid, ok)
	}
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "postmaster.pid"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(strconv.Itoa(os.Getpid()) + "\n/data\n")
	if pid, ok := PostmasterPID(dir); pid != os.Getpid() || !ok {
		t.Fatalf("own pid: %d %v", pid, ok)
	}
	write("0\n")
	if pid, ok := PostmasterPID(dir); pid != 0 || ok {
		t.Fatalf("pid 0: %d %v", pid, ok)
	}
	if Alive(-1) || Alive(0) {
		t.Fatal("non-positive pids are not alive")
	}
}
