package api

import (
	"fmt"
	"strings"
	"testing"
)

func findNulls(v any, path string, out *[]string) {
	switch x := v.(type) {
	case nil:
		*out = append(*out, path)
	case map[string]any:
		for k, e := range x {
			findNulls(e, path+"."+k, out)
		}
	case []any:
		for i, e := range x {
			findNulls(e, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

// Studio's pages crash on a null where the schema has an array (docs/research/08 section 9,
// finding 2: upgrade/eligibility validation_errors). No stub may contain a null at all:
// arrays are [] and every other type has a neutral value.
func TestStubsContainNoNulls(t *testing.T) {
	ops, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, o := range ops {
		if !o.JSON {
			continue
		}
		checked++
		var nulls []string
		findNulls(StubValue(o), "$", &nulls)
		if len(nulls) > 0 {
			t.Errorf("%s: stub has null at %v", o.Key(), nulls)
		}
	}
	if checked < 100 {
		t.Fatalf("only %d operations checked", checked)
	}
}

// Studio's project home shows "Failed to load project usage" for a bare {} from the analytics
// endpoints and renders zero counts for {"result": []} (docs/research/08 section 9). The stubs
// of every analytics endpoint must be the latter.
func TestAnalyticsStubsWrapEmptyRows(t *testing.T) {
	ops, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range ops {
		if !o.JSON || !strings.Contains(o.Path, "/analytics/endpoints/") {
			continue
		}
		n++
		m, ok := StubValue(o).(map[string]any)
		rows, isArr := m["result"].([]any)
		if !ok || !isArr || len(rows) != 0 {
			t.Errorf("%s: stub is %v, want {\"result\": []}", o.Key(), StubValue(o))
		}
	}
	if n < 10 {
		t.Fatalf("only %d analytics operations found", n)
	}
}
