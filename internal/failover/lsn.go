package failover

import "github.com/supavise/supavise/internal/pglsn"

// lsnPast reports whether position have is beyond position want. The stop position of a cluster is
// pg_controldata's latest checkpoint location, the start of its shutdown checkpoint record, and a
// standby's replay position is the end of the last record it replayed. A standby that has replayed
// the shutdown checkpoint is therefore strictly past the stop position; one that is at it is one
// record short, and promoted there it forks before the old primary's end. A position that does not
// parse is not past anything.
func lsnPast(have, want string) bool {
	h, err := pglsn.Parse(have)
	if err != nil {
		return false
	}
	w, err := pglsn.Parse(want)
	if err != nil {
		return false
	}
	return h > w
}
