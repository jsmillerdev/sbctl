package pglsn

import "testing"

func TestParse(t *testing.T) {
	for s, want := range map[string]uint64{"0/0": 0, "0/3000060": 0x3000060, " 1/A \n": 1<<32 | 0xa, "FFFFFFFF/FFFFFFFF": 1<<64 - 1, "A/FFFFFFFF": 0xA<<32 | 0xFFFFFFFF} {
		if got, err := Parse(s); err != nil || got != want {
			t.Errorf("Parse(%q) = %x, %v; want %x", s, got, err, want)
		}
	}
	for _, s := range []string{"", "0", "0/", "/0", "x/0", "1/100000000", "-1/0", "+1/0", "100000000/0"} {
		if Valid(s) {
			t.Errorf("Valid(%q)", s)
		}
	}
}
