package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The loader parses YAML and JSON into one small document tree that keeps
// line numbers. The tree is checked against the Go types of Config (see
// check.go), which is what lets every error name the offending field and
// line, and is then re-encoded as JSON for encoding/json to decode.

type nodeKind uint8

const (
	kindNull nodeKind = iota
	kindBool
	kindNumber
	kindString
	kindArray
	kindObject
)

func (k nodeKind) String() string {
	switch k {
	case kindNull:
		return "null"
	case kindBool:
		return "bool"
	case kindNumber:
		return "number"
	case kindString:
		return "string"
	case kindArray:
		return "list"
	}
	return "object"
}

// node is one value of the document tree.
type node struct {
	kind nodeKind
	line int
	// text is the JSON literal of a bool or number and the value of a string.
	text string
	// src is the source text of a plain YAML bool or number. A string field
	// accepts such a scalar as written (Retry-After: 1), which JSON could not.
	src string

	items []*node // kindArray

	keys     []string // kindObject, in document order
	keyLines []int
	vals     []*node
}

// get returns the value stored under key.
func (n *node) get(key string) (*node, bool) {
	for i, k := range n.keys {
		if k == key {
			return n.vals[i], true
		}
	}
	return nil, false
}

// add appends key unless it exists; it reports whether it was added.
func (n *node) add(key string, line int, v *node) bool {
	if _, dup := n.get(key); dup {
		return false
	}
	n.keys = append(n.keys, key)
	n.keyLines = append(n.keyLines, line)
	n.vals = append(n.vals, v)
	return true
}

// appendJSON encodes the tree as JSON.
func (n *node) appendJSON(b []byte) []byte {
	switch n.kind {
	case kindNull:
		return append(b, "null"...)
	case kindBool, kindNumber:
		return append(b, n.text...)
	case kindString:
		return appendJSONString(b, n.text)
	case kindArray:
		b = append(b, '[')
		for i, it := range n.items {
			if i > 0 {
				b = append(b, ',')
			}
			b = it.appendJSON(b)
		}
		return append(b, ']')
	}
	b = append(b, '{')
	for i, k := range n.keys {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendJSONString(b, k)
		b = append(b, ':')
		b = n.vals[i].appendJSON(b)
	}
	return append(b, '}')
}

func appendJSONString(b []byte, s string) []byte {
	enc, _ := json.Marshal(s) // cannot fail for a string
	return append(b, enc...)
}

// parseError is a syntax or structure error at a line (0 = unknown).
type parseError struct {
	line int
	msg  string
}

func (e *parseError) Error() string { return fmt.Sprintf("line %d: %s", e.line, e.msg) }

const (
	maxNodes = 1 << 18 // bounds alias expansion ("billion laughs")
	maxDepth = 128
)

// builder tracks the limits shared by one tree construction.
type builder struct {
	nodes int
}

func (b *builder) newNode(kind nodeKind, line int) (*node, error) {
	if b.nodes++; b.nodes > maxNodes {
		return nil, &parseError{line, "document expands to too many values (alias loop or alias bomb?)"}
	}
	return &node{kind: kind, line: line}, nil
}

// ---- YAML ----

// parseYAML builds the tree of the first (and only) YAML document of data.
// An empty document yields an empty object.
func parseYAML(data []byte) (*node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return &node{kind: kindObject, line: 1}, nil
		}
		return nil, yamlError(err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, &parseError{extra.Line, "multiple YAML documents are not supported; use one document"}
	} else if !errors.Is(err, io.EOF) {
		return nil, yamlError(err)
	}
	b := &builder{}
	return b.fromYAML(&doc, 0)
}

// yamlErrorRE splits a yaml.v3 error ("yaml: line 3: did not find ...").
var yamlErrorRE = regexp.MustCompile(`(?s)^(?:yaml: )?(?:line (\d+): )?(.*)$`)

// yamlError converts a yaml.v3 syntax error to a parseError carrying the line.
func yamlError(err error) error {
	m := yamlErrorRE.FindStringSubmatch(err.Error())
	line, _ := strconv.Atoi(m[1])
	return &parseError{line, strings.TrimSpace(m[2])}
}

func (b *builder) fromYAML(y *yaml.Node, depth int) (*node, error) {
	if depth > maxDepth {
		return nil, &parseError{y.Line, "document is nested too deeply"}
	}
	switch y.Kind {
	case yaml.DocumentNode:
		if len(y.Content) == 0 {
			return b.newNode(kindNull, y.Line)
		}
		return b.fromYAML(y.Content[0], depth+1)
	case yaml.AliasNode:
		return b.fromYAML(y.Alias, depth+1)
	case yaml.SequenceNode:
		n, err := b.newNode(kindArray, y.Line)
		if err != nil {
			return nil, err
		}
		for _, c := range y.Content {
			item, err := b.fromYAML(c, depth+1)
			if err != nil {
				return nil, err
			}
			n.items = append(n.items, item)
		}
		return n, nil
	case yaml.MappingNode:
		return b.yamlMapping(y, depth)
	case yaml.ScalarNode:
		return b.yamlScalar(y)
	}
	return nil, &parseError{y.Line, "unsupported YAML node"}
}

func (b *builder) yamlMapping(y *yaml.Node, depth int) (*node, error) {
	n, err := b.newNode(kindObject, y.Line)
	if err != nil {
		return nil, err
	}
	var merges []*node // "<<: *anchor" sources, applied after explicit keys
	for i := 0; i+1 < len(y.Content); i += 2 {
		k, v := y.Content[i], y.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			return nil, &parseError{k.Line, "object keys must be plain strings"}
		}
		val, err := b.fromYAML(v, depth+1)
		if err != nil {
			return nil, err
		}
		if k.ShortTag() == "!!merge" {
			switch val.kind {
			case kindObject:
				merges = append(merges, val)
			case kindArray:
				for _, it := range val.items {
					if it.kind != kindObject {
						return nil, &parseError{v.Line, "a merge (<<) needs a mapping or a list of mappings"}
					}
					merges = append(merges, it)
				}
			default:
				return nil, &parseError{v.Line, "a merge (<<) needs a mapping or a list of mappings"}
			}
			continue
		}
		if !n.add(k.Value, k.Line, val) {
			first, _ := n.get(k.Value)
			return nil, &parseError{k.Line, fmt.Sprintf("duplicate key %q (first used on line %d)", k.Value, firstKeyLine(n, k.Value, first))}
		}
	}
	for _, m := range merges {
		for i, key := range m.keys {
			n.add(key, m.keyLines[i], m.vals[i]) // explicit keys win; earlier merges win
		}
	}
	return n, nil
}

func firstKeyLine(n *node, key string, fallback *node) int {
	for i, k := range n.keys {
		if k == key {
			return n.keyLines[i]
		}
	}
	return fallback.line
}

func (b *builder) yamlScalar(y *yaml.Node) (*node, error) {
	switch y.ShortTag() {
	case "!!null":
		return b.newNode(kindNull, y.Line)
	case "!!str", "!!timestamp":
		n, err := b.newNode(kindString, y.Line)
		if err == nil {
			n.text = y.Value
		}
		return n, err
	case "!!bool", "!!int", "!!float":
		var v any
		if err := y.Decode(&v); err != nil {
			return nil, &parseError{y.Line, fmt.Sprintf("invalid value %q", y.Value)}
		}
		enc, err := json.Marshal(v)
		if err != nil { // .inf, .nan
			return nil, &parseError{y.Line, fmt.Sprintf("%q is not a finite number", y.Value)}
		}
		kind := kindNumber
		if y.ShortTag() == "!!bool" {
			kind = kindBool
		}
		n, err := b.newNode(kind, y.Line)
		if err == nil {
			n.text, n.src = string(enc), y.Value
		}
		return n, err
	}
	return nil, &parseError{y.Line, fmt.Sprintf("unsupported YAML tag %s on %q", y.ShortTag(), y.Value)}
}

// ---- JSON ----

// parseJSON builds the tree of one JSON document. Unlike encoding/json it
// rejects duplicate keys and keeps line numbers.
func parseJSON(data []byte) (*node, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return &node{kind: kindObject, line: 1}, nil
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	p := &jsonParser{dec: json.NewDecoder(bytes.NewReader(data)), data: data}
	p.dec.UseNumber()
	n, err := p.value(0)
	if err != nil {
		return nil, p.wrap(err)
	}
	if _, err := p.dec.Token(); !errors.Is(err, io.EOF) {
		line := p.line()
		if err != nil {
			return nil, p.wrap(err)
		}
		return nil, &parseError{line, "unexpected data after the top-level value"}
	}
	return n, nil
}

type jsonParser struct {
	dec  *json.Decoder
	data []byte
	b    builder
}

// line is the 1-based line of the decoder's current position.
func (p *jsonParser) line() int {
	off := int(p.dec.InputOffset())
	if off > len(p.data) {
		off = len(p.data)
	}
	return 1 + bytes.Count(p.data[:off], []byte{'\n'})
}

// wrap turns a decoder error into a parseError with a line.
func (p *jsonParser) wrap(err error) error {
	var pe *parseError
	if errors.As(err, &pe) {
		return err
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		off := int(se.Offset)
		if off > len(p.data) {
			off = len(p.data)
		}
		return &parseError{1 + bytes.Count(p.data[:off], []byte{'\n'}), se.Error()}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &parseError{p.line(), "unexpected end of JSON input"}
	}
	return &parseError{p.line(), err.Error()}
}

func (p *jsonParser) value(depth int) (*node, error) {
	if depth > maxDepth {
		return nil, &parseError{p.line(), "document is nested too deeply"}
	}
	tok, err := p.dec.Token()
	if err != nil {
		return nil, err
	}
	line := p.line()
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			return p.array(line, depth)
		}
		return p.object(line, depth)
	case string:
		n, err := p.b.newNode(kindString, line)
		if err == nil {
			n.text = t
		}
		return n, err
	case json.Number:
		n, err := p.b.newNode(kindNumber, line)
		if err == nil {
			n.text = t.String()
		}
		return n, err
	case bool:
		n, err := p.b.newNode(kindBool, line)
		if err == nil {
			n.text = strconv.FormatBool(t)
		}
		return n, err
	}
	return p.b.newNode(kindNull, line)
}

func (p *jsonParser) array(line, depth int) (*node, error) {
	n, err := p.b.newNode(kindArray, line)
	if err != nil {
		return nil, err
	}
	for p.dec.More() {
		item, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		n.items = append(n.items, item)
	}
	if _, err := p.dec.Token(); err != nil { // ]
		return nil, err
	}
	return n, nil
}

func (p *jsonParser) object(line, depth int) (*node, error) {
	n, err := p.b.newNode(kindObject, line)
	if err != nil {
		return nil, err
	}
	for p.dec.More() {
		kt, err := p.dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, &parseError{p.line(), "object keys must be strings"}
		}
		keyLine := p.line()
		val, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		if !n.add(key, keyLine, val) {
			return nil, &parseError{keyLine, fmt.Sprintf("duplicate key %q (first used on line %d)", key, firstKeyLine(n, key, val))}
		}
	}
	if _, err := p.dec.Token(); err != nil { // }
		return nil, err
	}
	return n, nil
}
