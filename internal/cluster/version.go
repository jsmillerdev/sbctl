package cluster

import (
	"fmt"
	"strconv"
	"strings"
)

// semver reads "v0.2.1", "0.2.1-rc1" and "v0.2.1+sha" into major, minor and patch; ok is false for
// anything else ("dev", "").
func semver(v string) (major, minor, patch int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return 0, 0, 0, false
		}
		n[i] = x
	}
	return n[0], n[1], n[2], true
}

// CheckVersionWindow says whether two nodes can be in one cluster: the same major version, and
// minors that differ by at most one (a binary one minor behind must run against the newer
// registry schema, invariant I6), and the same Postgres major version in their pins (a standby
// cannot replay another major's WAL). A version that is not a release ("dev") passes: it is a
// build made from a checkout.
func CheckVersionWindow(self, other string, selfPins, otherPins map[string]string) error {
	a1, a2, _, aok := semver(self)
	b1, b2, _, bok := semver(other)
	if aok && bok {
		if a1 != b1 || a2-b2 > 1 || b2-a2 > 1 {
			return fmt.Errorf("version skew: this node runs %s and the other runs %s; nodes of one cluster may differ by at most one minor version", self, other)
		}
	}
	if x, y := postgresMajor(selfPins), postgresMajor(otherPins); x != "" && y != "" && x != y {
		return fmt.Errorf("version skew: this node pins Postgres %s and the other pins %s", x, y)
	}
	return nil
}

// postgresMajor is the major version of the pinned Postgres ("postgres-17.11.0.004-r1" is "17"),
// or "" when the pins name none.
func postgresMajor(pins map[string]string) string {
	tag := strings.TrimPrefix(pins["postgres"], "postgres-")
	if tag == "" || tag == pins["postgres"] {
		return ""
	}
	major, _, _ := strings.Cut(tag, ".")
	return major
}
