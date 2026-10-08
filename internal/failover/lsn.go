package failover

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseLSN reads the text form of a pg_lsn ("0/3000060") as the 64-bit position it names.
func ParseLSN(s string) (uint64, error) {
	hi, lo, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return 0, fmt.Errorf("failover: %q is not an LSN", s)
	}
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("failover: %q is not an LSN", s)
	}
	return h<<32 | l, nil
}

// lsnPast reports whether position have is beyond position want. The stop position of a cluster is
// pg_controldata's latest checkpoint location, the start of its shutdown checkpoint record, and a
// standby's replay position is the end of the last record it replayed. A standby that has replayed
// the shutdown checkpoint is therefore strictly past the stop position; one that is at it is one
// record short, and promoted there it forks before the old primary's end. A position that does not
// parse is not past anything.
func lsnPast(have, want string) bool {
	h, err := ParseLSN(have)
	if err != nil {
		return false
	}
	w, err := ParseLSN(want)
	if err != nil {
		return false
	}
	return h > w
}
