package cluster

import (
	"fmt"
	"strings"

	"github.com/supavise/supavise/internal/selfupdate"
)

// CheckVersionWindow says whether two nodes can be in one cluster: the same major version, and
// minors that differ by at most one (a binary one minor behind must run against the newer
// registry schema, invariant I6), and the same Postgres major version in their pins (a standby
// cannot replay another major's WAL). A version that is not a release ("dev") passes: it is a
// build made from a checkout.
func CheckVersionWindow(self, other string, selfPins, otherPins map[string]string) error {
	a, aok := selfupdate.ParseVersion(self)
	b, bok := selfupdate.ParseVersion(other)
	if aok && bok {
		if a[0] != b[0] || a[1]-b[1] > 1 || b[1]-a[1] > 1 {
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
