package backup

import "testing"

func TestPromoteOKRoundTrip(t *testing.T) {
	b := FormatPromoteOK(12)
	if string(b) != "12\n" {
		t.Fatalf("FormatPromoteOK = %q", b)
	}
	if n, err := ParsePromoteOK(b); err != nil || n != 12 {
		t.Fatalf("ParsePromoteOK = %d, %v", n, err)
	}
	for _, bad := range []string{"", "x", "0", "-3", "1.5"} {
		if _, err := ParsePromoteOK([]byte(bad)); err == nil {
			t.Errorf("ParsePromoteOK(%q) succeeded", bad)
		}
	}
}
