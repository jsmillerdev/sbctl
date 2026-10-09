package failover

import "testing"

func TestLSNPast(t *testing.T) {
	for _, tc := range []struct {
		have, want string
		ok         bool
	}{
		{"0/3000060", "0/3000060", false}, // at the checkpoint record's start: it is not replayed yet
		{"0/30000D0", "0/3000060", true},  // past the record
		{"0/2FFFFFF", "0/3000060", false},
		{"1/0", "0/FFFFFFFF", true},
		{"", "0/1", false},
		{"0/1", "bad", false},
	} {
		if got := lsnPast(tc.have, tc.want); got != tc.ok {
			t.Errorf("lsnPast(%q, %q) = %v", tc.have, tc.want, got)
		}
	}
}
