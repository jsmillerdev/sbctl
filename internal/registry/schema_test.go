package registry

import (
	"strings"
	"testing"
)

func TestSchemaVersionIsTheNewestEmbeddedMigration(t *testing.T) {
	names, err := Migrations()
	if err != nil || len(names) == 0 {
		t.Fatalf("Migrations() = %v, %v", names, err)
	}
	got := SchemaVersion()
	if got == "" || strings.Contains(got, "/") || "migrations/"+got != names[len(names)-1] {
		t.Fatalf("SchemaVersion() = %q, newest migration %q", got, names[len(names)-1])
	}
	// Names order as numbers, which is what a comparison of two schema versions relies on.
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("migrations out of order: %s, %s", names[i-1], names[i])
		}
	}
}
