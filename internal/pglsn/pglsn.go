// Package pglsn reads Postgres write-ahead log positions (pg_lsn) in their text form.
package pglsn

import (
	"fmt"
	"strconv"
	"strings"
)

// Parse reads the text form of a pg_lsn, two hexadecimal numbers around a slash ("0/3000060"), as
// the 64-bit position it names. Surrounding space is ignored.
func Parse(s string) (uint64, error) {
	hi, lo, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return 0, fmt.Errorf("%q is not an LSN", s)
	}
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("%q is not an LSN", s)
	}
	return h<<32 | l, nil
}

// Valid reports whether s is the text form of a pg_lsn.
func Valid(s string) bool {
	_, err := Parse(s)
	return err == nil
}
