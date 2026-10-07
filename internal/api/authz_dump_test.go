package api

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestRouteNeedsDump prints the requirement of every operation of the three specs
// (SUPAVISE_DUMP_NEEDS=1 go test ./internal/api -run RouteNeedsDump -v), for review.
func TestRouteNeedsDump(t *testing.T) {
	if os.Getenv("SUPAVISE_DUMP_NEEDS") == "" {
		t.Skip("set SUPAVISE_DUMP_NEEDS=1")
	}
	ops, _ := Operations()
	var lines []string
	for _, op := range ops {
		n := routeNeed(op.Method, op.Path)
		kind := map[needKind]string{needCheck: "check", needAny: "any", needSelf: "self", needOwner: "owner"}[n.kind]
		res := n.resource
		if n.resourceFn != nil {
			res = "<fn>"
		}
		lines = append(lines, fmt.Sprintf("%-6s %-90s %-5s %s %s", op.Method, op.Path, kind, n.action, res))
	}
	sort.Slice(lines, func(i, j int) bool { return strings.SplitN(lines[i], " ", 2)[1] < strings.SplitN(lines[j], " ", 2)[1] })
	fmt.Println(strings.Join(lines, "\n"))
}
