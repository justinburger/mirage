package config

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// maxProblems caps how many problems one load reports.
const maxProblems = 20

// Problem is one thing wrong with a configuration file.
type Problem struct {
	// Line is the 1-based line in the file, 0 when unknown.
	Line int `json:"line,omitempty"`
	// Field is the path of the offending field, e.g. "services[0].listen_addr".
	Field string `json:"field,omitempty"`
	// Msg says what is wrong.
	Msg string `json:"msg"`
}

func (p Problem) String() string {
	var b strings.Builder
	if p.Line > 0 {
		fmt.Fprintf(&b, "line %d: ", p.Line)
	}
	if p.Field != "" {
		b.WriteString(p.Field)
		b.WriteString(": ")
	}
	b.WriteString(p.Msg)
	return b.String()
}

var (
	unmarshalerType     = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// checker validates a document tree against a Go type the way encoding/json
// would decode it, but reports every problem with its field path and line,
// rejects unknown fields (with a suggestion) and lets plain YAML numbers and
// booleans fill string fields.
type checker struct {
	problems []Problem
}

func (c *checker) add(line int, path, format string, args ...any) {
	if len(c.problems) < maxProblems {
		c.problems = append(c.problems, Problem{Line: line, Field: path, Msg: fmt.Sprintf(format, args...)})
	}
}

func (c *checker) check(n *node, t reflect.Type, path string) {
	if n.kind == kindNull {
		return // null decodes to the zero value of anything
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if isCustom(t) {
		c.leaf(n, t, path)
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		c.checkStruct(n, t, path)
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 { // []byte is a base64 string
			c.leaf(n, t, path)
			return
		}
		if n.kind != kindArray {
			c.add(n.line, path, "expected a list, got %s", describe(n))
			return
		}
		for i, it := range n.items {
			c.check(it, t.Elem(), fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			c.leaf(n, t, path)
			return
		}
		if n.kind != kindObject {
			c.add(n.line, path, "expected an object, got %s", describe(n))
			return
		}
		for i, k := range n.keys {
			c.check(n.vals[i], t.Elem(), join(path, k))
		}
	case reflect.String:
		if (n.kind == kindNumber || n.kind == kindBool) && n.src != "" {
			n.kind, n.text = kindString, n.src // Retry-After: 1
		}
		c.leaf(n, t, path)
	case reflect.Interface:
		// any: accepts every value.
	default:
		c.leaf(n, t, path)
	}
}

func (c *checker) checkStruct(n *node, t reflect.Type, path string) {
	if n.kind != kindObject {
		c.add(n.line, path, "expected an object, got %s", describe(n))
		return
	}
	fields := jsonFields(t)
	for i, key := range n.keys {
		f, ok := fields.byName[key]
		if !ok {
			msg := fmt.Sprintf("unknown field %q", key)
			if s := suggest(key, fields.names); s != "" {
				msg += fmt.Sprintf(" (did you mean %q?)", s)
			}
			c.add(n.keyLines[i], join(path, key), "%s", msg)
			continue
		}
		c.check(n.vals[i], f, join(path, key))
	}
}

// leaf validates a scalar (or any value with a custom decoder) by decoding it.
func (c *checker) leaf(n *node, t reflect.Type, path string) {
	err := json.Unmarshal(n.appendJSON(nil), reflect.New(t).Interface())
	if err == nil {
		return
	}
	var ute *json.UnmarshalTypeError
	switch {
	case errors.As(err, &ute):
		c.add(n.line, path, "expected %s, got %s", typeName(t), describe(n))
	default:
		c.add(n.line, path, "%s", strings.TrimPrefix(err.Error(), "json: "))
	}
}

func isCustom(t reflect.Type) bool {
	pt := reflect.PointerTo(t)
	return pt.Implements(unmarshalerType) || pt.Implements(textUnmarshalerType)
}

// describe renders a value for "got ..." messages.
func describe(n *node) string {
	switch n.kind {
	case kindString:
		return "string " + strconv.Quote(truncate(n.text, 40))
	case kindNumber, kindBool:
		return n.kind.String() + " " + n.text
	}
	return n.kind.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func typeName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.String:
		return "a string"
	case reflect.Slice, reflect.Array:
		return "a list"
	}
	return "an object"
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// fieldSet is the JSON schema of one struct type.
type fieldSet struct {
	names  []string // in declaration order
	byName map[string]reflect.Type
}

// jsonFields lists the JSON-visible fields of struct type t, flattening
// embedded structs the way encoding/json does.
func jsonFields(t reflect.Type) fieldSet {
	fs := fieldSet{byName: map[string]reflect.Type{}}
	collectFields(t, &fs)
	return fs
}

func collectFields(t reflect.Type, fs *fieldSet) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collectFields(ft, fs)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if _, dup := fs.byName[name]; !dup {
			fs.names = append(fs.names, name)
			fs.byName[name] = f.Type
		}
	}
}

// suggest returns the known name closest to key, or "" when none is close.
func suggest(key string, names []string) string {
	best, bestDist := "", len(key)/3+2
	lower := strings.ToLower(key)
	for _, n := range names {
		d := editDistance(lower, n)
		if len(lower) >= 3 && strings.HasPrefix(n, lower) {
			d = min(d, 1) // an abbreviation ("prob")
		}
		if d < bestDist {
			best, bestDist = n, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
