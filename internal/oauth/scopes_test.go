package oauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// specPath is a vendored OpenAPI document of internal/api.
func specPath(name string) string {
	return filepath.Join("..", "api", "gen", "specs", name+".json")
}

func readSpec(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(specPath(name))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestScopesMatchSpec ties the vocabulary to the pinned specs: AllScopes is the enum of the platform
// spec's CreateOAuthAppBody, in its order, and every scope an operation of v1 or v2 asks for (x-oauth-scope)
// belongs to it.
func TestScopesMatchSpec(t *testing.T) {
	platform := readSpec(t, "platform")
	schemas := platform["components"].(map[string]any)["schemas"].(map[string]any)
	body := schemas["CreateOAuthAppBody"].(map[string]any)
	items := body["properties"].(map[string]any)["scopes"].(map[string]any)["items"].(map[string]any)
	var enum []string
	for _, v := range items["enum"].([]any) {
		enum = append(enum, v.(string))
	}
	if !slices.Equal(enum, AllScopes) {
		t.Fatalf("AllScopes differs from the platform spec's enum\n got %v\nwant %v", AllScopes, enum)
	}
	if len(AllScopes) != 24 {
		t.Fatalf("want 24 scopes, got %d", len(AllScopes))
	}
	used := map[string]bool{}
	for _, name := range []string{"v1", "v2"} {
		for path, item := range readSpec(t, name)["paths"].(map[string]any) {
			for method, op := range item.(map[string]any) {
				o, ok := op.(map[string]any)
				if !ok {
					continue
				}
				if sc, ok := o["x-oauth-scope"].(string); ok {
					used[sc] = true
					if !ValidScope(sc) {
						t.Errorf("%s %s %s asks for %q, which is not in AllScopes", name, method, path, sc)
					}
				}
			}
		}
	}
	if used[ScopeStorageWrite] || used[ScopeAnalyticsWrite] {
		t.Errorf("the plan says no operation is annotated storage:write or analytics:write; the specs changed: %v", used)
	}
}

func TestAdvertisedScopes(t *testing.T) {
	want := []string{
		"organizations:read", "projects:read", "projects:write", "database:read", "database:write",
		"analytics:read", "secrets:read", "edge_functions:read", "edge_functions:write",
		"environment:read", "environment:write", "storage:read", "storage:write",
	}
	if !slices.Equal(AdvertisedScopes, want) {
		t.Fatalf("AdvertisedScopes\n got %v\nwant %v", AdvertisedScopes, want)
	}
	for _, s := range AdvertisedScopes {
		if !ValidScope(s) || !IsAdvertised(s) {
			t.Errorf("%q is advertised but not valid", s)
		}
	}
	if IsAdvertised(ScopeAuthWrite) || !ValidScope(ScopeAuthWrite) {
		t.Error("auth:write is a valid scope that is not advertised")
	}
	if ValidScope("") || ValidScope("projects") || ValidScope("Projects:read") {
		t.Error("ValidScope accepted a scope outside the vocabulary")
	}
	sorted := slices.Clone(AllScopes)
	sort.Strings(sorted)
	if !slices.Equal(sorted, AllScopes) {
		t.Error("AllScopes is not in alphabetical order, as the spec's enum is")
	}
}

func TestScopeHelpers(t *testing.T) {
	t.Run("parse", func(t *testing.T) {
		cases := []struct {
			in   string
			want []string
		}{
			{"", nil},
			{"   ", nil},
			{"projects:read", []string{"projects:read"}},
			{"projects:read  database:read\tprojects:read\n", []string{"projects:read", "database:read"}},
		}
		for _, c := range cases {
			if got := ParseScopes(c.in); !slices.Equal(got, c.want) {
				t.Errorf("ParseScopes(%q) = %v, want %v", c.in, got, c.want)
			}
		}
	})
	t.Run("join", func(t *testing.T) {
		if got := JoinScopes([]string{"a", "b"}); got != "a b" {
			t.Error(got)
		}
		if JoinScopes(nil) != "" {
			t.Error("empty")
		}
	})
	t.Run("normalize", func(t *testing.T) {
		in := []string{"b", "a", "b"}
		if got := NormalizeScopes(in); !slices.Equal(got, []string{"a", "b"}) {
			t.Error(got)
		}
		if !slices.Equal(in, []string{"b", "a", "b"}) {
			t.Error("NormalizeScopes changed its argument")
		}
		if NormalizeScopes(nil) != nil {
			t.Error("nil in, nil out")
		}
	})
	t.Run("intersect", func(t *testing.T) {
		got := IntersectScopes([]string{"c", "a", "b", "a"}, []string{"b", "c", "d"})
		if !slices.Equal(got, []string{"c", "b"}) {
			t.Error(got)
		}
		if IntersectScopes([]string{"a"}, nil) != nil {
			t.Error("disjoint is nil")
		}
	})
	t.Run("union", func(t *testing.T) {
		got := UnionScopes([]string{"a", "b"}, []string{"b", "c", "c"})
		if !slices.Equal(got, []string{"a", "b", "c"}) {
			t.Error(got)
		}
		if UnionScopes(nil, nil) != nil {
			t.Error("empty union is nil")
		}
	})
	t.Run("subset", func(t *testing.T) {
		if !SubsetOf(nil, nil) || !SubsetOf(nil, []string{"a"}) || !SubsetOf([]string{"a"}, []string{"a", "b"}) {
			t.Error("subset refused")
		}
		if SubsetOf([]string{"a", "c"}, []string{"a", "b"}) || SubsetOf([]string{"a"}, nil) {
			t.Error("non-subset accepted")
		}
		// A write scope does not imply its read scope.
		if SubsetOf([]string{ScopeDatabaseRead}, []string{ScopeDatabaseWrite}) {
			t.Error("write implied read")
		}
	})
}
