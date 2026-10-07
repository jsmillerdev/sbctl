package api

import (
	"flag"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryImplementedRouteIsExercised fails when a hand-written route is never
// called by any other test of the package. It runs last (file name) and only on a
// full run.
func TestEveryImplementedRouteIsExercised(t *testing.T) {
	if flag.Lookup("test.run").Value.String() != "" {
		t.Skip("coverage of routes is only meaningful on a full run")
	}
	f := newFixture(t)
	var calls []string
	calledRoutes.Range(func(k, _ any) bool { calls = append(calls, k.(string)); return true })
	param := regexp.MustCompile(`\\\{[^}]+\\\}`)
	var missing []string
	for key := range f.srv.implemented() {
		method, tmpl, _ := strings.Cut(key, " ")
		re := regexp.MustCompile("^" + method + " " + param.ReplaceAllString(regexp.QuoteMeta(tmpl), "[^/]+") + "$")
		hit := false
		for _, c := range calls {
			if re.MatchString(c) {
				hit = true
				break
			}
		}
		if !hit {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("routes never exercised by a test:\n  %s", strings.Join(missing, "\n  "))
	}
}
