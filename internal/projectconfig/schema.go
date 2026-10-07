package projectconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Kind is the JSON type of a setting.
type Kind int

// The setting kinds. Int values are int64 in memory, Number values float64.
const (
	Bool Kind = iota
	Int
	Number
	String
	// Enum is a string from a fixed list.
	Enum
	// Object is a JSON object kept as given (Storage features, for example).
	Object
)

// Field describes one setting: its type and limits (the OpenAPI spec of the Management
// API is the source), whether it is secret, and its default, which is what the unit
// renders when the setting was never changed.
type Field struct {
	Name   string
	Kind   Kind
	Secret bool
	// Default is the value reported while the setting is unset; nil reports null.
	Default any
	// Min and Max bound Int and Number values when HasRange is set.
	Min, Max float64
	HasRange bool
	// Enum lists the allowed strings of an Enum; AllowEmpty also accepts "".
	Enum       []string
	AllowEmpty bool
	// Pattern, when set, must match String values.
	Pattern *regexp.Regexp
	// MaxLen bounds String values in bytes (default 4096).
	MaxLen int
	// Env is the environment variable the setting renders to; empty means the setting is
	// stored and reported but not rendered by the generic renderer.
	Env string
	// Check validates a value that passed the type checks.
	Check func(v any) error
	// Normalize rewrites a valid value into its stored form (trimmed lists, for example).
	Normalize func(v any) any
}

// Schema is the set of settings of one service.
type Schema struct {
	Service Service
	Fields  []Field
	byName  map[string]*Field
	// Fixup adjusts the settings after a patch was applied; touched names the settings the
	// patch addressed.
	Fixup func(set Values, touched map[string]bool)
	// Cross validates the settings as a whole: eff is the effective view, set the changed
	// settings only.
	Cross func(eff, set Values, cx CrossContext) error
}

// CrossContext carries what whole-settings validation needs from the project.
type CrossContext struct {
	// MemoryLimit is the project's memory limit in bytes (0: unknown).
	MemoryLimit int64
}

// NewSchema indexes fields. It panics on a duplicate name.
func NewSchema(svc Service, fields []Field) *Schema {
	s := &Schema{Service: svc, Fields: fields, byName: make(map[string]*Field, len(fields))}
	for i := range s.Fields {
		f := &s.Fields[i]
		if _, dup := s.byName[f.Name]; dup {
			panic("projectconfig: duplicate setting " + f.Name)
		}
		s.byName[f.Name] = f
	}
	return s
}

// Field returns the named field.
func (s *Schema) Field(name string) (*Field, bool) { f, ok := s.byName[name]; return f, ok }

// ValidationError is a rejected value; the API answers it as 400.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

const defaultMaxLen = 4096

// Coerce converts v (as decoded from JSON, or read back from the store) to the Go type
// of f.Kind and checks it against the field's limits. nil is never passed.
func (f *Field) Coerce(v any) (any, error) {
	switch f.Kind {
	case Bool:
		b, ok := v.(bool)
		if !ok {
			return nil, invalid("%s must be a boolean", f.Name)
		}
		return b, nil
	case Int, Number:
		var n float64
		switch x := v.(type) {
		case float64:
			n = x
		case int:
			n = float64(x)
		case int64:
			n = float64(x)
		default:
			return nil, invalid("%s must be a number", f.Name)
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, invalid("%s must be a finite number", f.Name)
		}
		if f.Kind == Int && n != math.Trunc(n) {
			return nil, invalid("%s must be an integer", f.Name)
		}
		if f.HasRange && (n < f.Min || n > f.Max) {
			return nil, invalid("%s must be between %v and %v", f.Name, f.Min, f.Max)
		}
		if f.Kind == Int {
			return int64(n), nil
		}
		return n, nil
	case String, Enum:
		s, ok := v.(string)
		if !ok {
			return nil, invalid("%s must be a string", f.Name)
		}
		if strings.ContainsRune(s, 0) {
			return nil, invalid("%s must not contain a NUL byte", f.Name)
		}
		max := f.MaxLen
		if max == 0 {
			max = defaultMaxLen
		}
		if len(s) > max {
			return nil, invalid("%s is longer than %d bytes", f.Name, max)
		}
		if f.Kind == Enum && !(s == "" && f.AllowEmpty) && !slices.Contains(f.Enum, s) {
			return nil, invalid("%s must be one of %s", f.Name, strings.Join(f.Enum, ", "))
		}
		if f.Pattern != nil && s != "" && !f.Pattern.MatchString(s) {
			return nil, invalid("%s has an invalid format", f.Name)
		}
		return s, nil
	case Object:
		m, ok := v.(map[string]any)
		if !ok {
			return nil, invalid("%s must be an object", f.Name)
		}
		return m, nil
	}
	return nil, invalid("%s has an unsupported type", f.Name)
}

// validate runs Coerce and the field's own check.
func (f *Field) validate(v any) (any, error) {
	c, err := f.Coerce(v)
	if err != nil {
		return nil, err
	}
	if f.Check != nil {
		if err := f.Check(c); err != nil {
			return nil, invalid("%s: %v", f.Name, err)
		}
	}
	if f.Normalize != nil {
		c = f.Normalize(c)
	}
	return c, nil
}

// Redact is how a secret is reported: the hex SHA-256 of its value, as hosted does, so a
// client can tell whether a secret is set and compare it with a local one without ever
// receiving it.
func Redact(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// Values are the effective settings of a service, secrets in plain text.
type Values map[string]any

// Keys returns the names present, sorted.
func (v Values) Keys() []string {
	out := make([]string, 0, len(v))
	for k := range v {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Str returns the string value of name, or "".
func (v Values) Str(name string) string { s, _ := v[name].(string); return s }

// Bool returns the bool value of name, or false.
func (v Values) Bool(name string) bool { b, _ := v[name].(bool); return b }

// Int returns the integer value of name and whether it is set.
func (v Values) Int(name string) (int64, bool) {
	switch x := v[name].(type) {
	case int64:
		return x, true
	case float64:
		return int64(x), true
	}
	return 0, false
}

// Float returns the numeric value of name and whether it is set.
func (v Values) Float(name string) (float64, bool) {
	switch x := v[name].(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}
