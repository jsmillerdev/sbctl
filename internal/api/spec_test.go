package api

import "testing"

func TestOperationsLoad(t *testing.T) {
	ops, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, o := range ops {
		counts[o.Spec]++
	}
	// Spec sizes at pin time: v1 169 ops, v2 50, platform 394 (research/05 section 2.2).
	if counts["v1"] < 160 || counts["v2"] < 45 || counts["platform"] < 380 {
		t.Fatalf("operation counts look wrong: %v", counts)
	}
	byKey := map[string]*Operation{}
	for _, o := range ops {
		byKey[o.Key()] = o
	}
	list := byKey["GET /v1/projects"]
	if list == nil || list.Status != 200 || !list.JSON || list.Response == nil {
		t.Fatalf("GET /v1/projects: %+v", list)
	}
	if q := byKey["POST /v1/projects/{ref}/database/query"]; q == nil || q.Status != 201 || q.JSON {
		t.Fatalf("database/query: %+v", q)
	}
}
