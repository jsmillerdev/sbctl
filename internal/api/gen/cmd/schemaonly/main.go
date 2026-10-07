// Command schemaonly copies an OpenAPI document with its paths emptied, so that
// oapi-codegen generates only the component schemas. Operation-level parameter
// types are not useful to a net/http server and some of them collide in the
// Supabase specs.
//
// It also disambiguates properties whose Go names would collide, such as
// "connectionString" next to "connection_string": the snake_case variant gets
// an x-go-name of "<Name>Snake".
//
// Usage: schemaonly <in.json> <out.json>
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: schemaonly <in.json> <out.json>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "schemaonly:", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	b, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	doc["paths"] = map[string]any{}
	fixCollisions(doc)
	b, err = json.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(out, b, 0o644)
}

// fixCollisions walks v and adds x-go-name to colliding property schemas.
func fixCollisions(v any) {
	switch t := v.(type) {
	case map[string]any:
		if props, ok := t["properties"].(map[string]any); ok {
			byNorm := map[string][]string{}
			for k := range props {
				byNorm[norm(k)] = append(byNorm[norm(k)], k)
			}
			for _, keys := range byNorm {
				if len(keys) < 2 {
					continue
				}
				for _, k := range keys {
					if !strings.ContainsAny(k, "_-") {
						continue
					}
					if sch, ok := props[k].(map[string]any); ok {
						sch["x-go-name"] = pascal(k) + "Snake"
					}
				}
			}
		}
		for _, c := range t {
			fixCollisions(c)
		}
	case []any:
		for _, c := range t {
			fixCollisions(c)
		}
	}
}

func norm(k string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(k))
}

func pascal(k string) string {
	var sb strings.Builder
	up := true
	for _, r := range k {
		if r == '_' || r == '-' || r == '.' {
			up = true
			continue
		}
		if up {
			r = unicode.ToUpper(r)
			up = false
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
