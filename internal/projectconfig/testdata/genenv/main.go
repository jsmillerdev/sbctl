//go:build ignore

// genenv lists the environment variable names GoTrue reads, by walking the Go structs
// of supabase/auth internal/conf the way kelseyhightower/envconfig does (prefix
// "GOTRUE", field name or `envconfig` tag, `split_words`, embedded structs flattened,
// types with a Decode, Set, UnmarshalText or UnmarshalBinary method are leaves). The output is committed as
// gotrue-env-names.txt and pins the names the renderer may use.
//
//	go run testdata/genenv/main.go configuration.go saml.go rate.go > testdata/gotrue-env-names.txt
//
// The input files come from the pinned release, for example
// https://github.com/supabase/auth/tree/v2.195.0/internal/conf.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

var (
	gather  = regexp.MustCompile(`([^A-Z]+|[A-Z]+[^A-Z]+|[A-Z]+)`)
	acronym = regexp.MustCompile(`([A-Z]+)([A-Z][^A-Z]+)`)
)

func main() {
	structs := map[string]*ast.StructType{}
	decoders := map[string]bool{}
	fset := token.NewFileSet()
	for _, path := range os.Args[1:] {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, s := range d.Specs {
					if ts, ok := s.(*ast.TypeSpec); ok {
						if st, ok := ts.Type.(*ast.StructType); ok {
							structs[ts.Name.Name] = st
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv != nil && (d.Name.Name == "Decode" || d.Name.Name == "Set" || d.Name.Name == "UnmarshalText" || d.Name.Name == "UnmarshalBinary") && len(d.Recv.List) == 1 {
					decoders[recvName(d.Recv.List[0].Type)] = true
				}
			}
		}
	}
	out := map[string]bool{}
	var walk func(prefix, typ string)
	walk = func(prefix, typ string) {
		st := structs[typ]
		for _, fld := range st.Fields.List {
			tag := reflect.StructTag("")
			if fld.Tag != nil {
				tag = reflect.StructTag(strings.Trim(fld.Tag.Value, "`"))
			}
			tn := typeName(fld.Type)
			names := []string{""} // embedded
			if len(fld.Names) > 0 {
				names = []string{fld.Names[0].Name}
			}
			name := names[0]
			if name != "" && !ast.IsExported(name) {
				continue
			}
			if tag.Get("ignored") == "true" {
				continue
			}
			if name == "" { // embedded: same prefix
				if _, ok := structs[tn]; ok && !decoders[tn] {
					walk(prefix, tn)
				}
				continue
			}
			key := name
			if tag.Get("split_words") == "true" {
				var parts []string
				for _, w := range gather.FindAllStringSubmatch(name, -1) {
					if m := acronym.FindStringSubmatch(w[0]); len(m) == 3 {
						parts = append(parts, m[1], m[2])
					} else {
						parts = append(parts, w[0])
					}
				}
				if len(parts) > 0 {
					key = strings.Join(parts, "_")
				}
			}
			alt := strings.ToUpper(tag.Get("envconfig"))
			if alt != "" {
				key = alt
			}
			full := strings.ToUpper(prefix + "_" + key)
			if _, ok := structs[tn]; ok && !decoders[tn] {
				walk(full, tn)
				continue
			}
			out[full] = true
			if alt != "" {
				out[alt] = true
			}
		}
	}
	walk("GOTRUE", "GlobalConfiguration")
	names := make([]string, 0, len(out))
	for n := range out {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Println(strings.Join(names, "\n"))
}

func recvName(e ast.Expr) string {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return typeName(t.X)
	}
	return ""
}
