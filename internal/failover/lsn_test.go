package failover

import "testing"

func TestParseLSN(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want uint64
		bad  bool
	}{
		{"0/0", 0, false},
		{"0/3000060", 0x3000060, false},
		{"1/0", 1 << 32, false},
		{"A/FFFFFFFF", 0xA<<32 | 0xFFFFFFFF, false},
		{" 0/10 ", 0x10, false},
		{"", 0, true},
		{"3000060", 0, true},
		{"0/zz", 0, true},
		{"100000000/0", 0, true},
	} {
		got, err := ParseLSN(tc.in)
		if (err != nil) != tc.bad || got != tc.want {
			t.Errorf("ParseLSN(%q) = %#x, %v", tc.in, got, err)
		}
	}
}

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
