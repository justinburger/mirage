package anonymize

import (
	"fmt"
	"strconv"
	"strings"
)

// The JSONPath subset understood by json rules:
//
//	$                the document root
//	$.a.b            member access ($['a']['b'] and $["a"]["b"] also work)
//	$.a[0]           array element by non-negative index
//	$.a[*].b  $.*    wildcards over members and elements
//	$..key           recursive descent: `key` at any depth
//	$..*  $..[0]     recursive descent with a wildcard or an index
//
// Filters, slices, unions and negative indexes are rejected with a validation
// error rather than silently ignored, so a rule can never look like it covers
// data that it does not.

const (
	jpName = iota
	jpIndex
	jpWild
)

const maxJSONPathSteps = 64

type jpStep struct {
	// descend makes the step match at any depth below the previous step.
	descend bool
	kind    uint8
	name    string
	idx     int
}

// matches reports whether the step accepts a child of a container: an object
// member called key, or element idx of an array.
func (s *jpStep) matches(obj bool, key []byte, idx int) bool {
	switch s.kind {
	case jpWild:
		return true
	case jpName:
		return obj && string(key) == s.name
	default:
		return !obj && idx == s.idx
	}
}

type jsonPath struct {
	src   string
	steps []jpStep
}

func (p *jsonPath) String() string { return p.src }

// parseJSONPath compiles a selector. Errors are plain messages; callers wrap
// them in a model.ValidationError.
func parseJSONPath(sel string) (*jsonPath, error) {
	p := strings.TrimSpace(sel)
	if p == "" || p[0] != '$' {
		return nil, fmt.Errorf("JSONPath %q must start with $", sel)
	}
	jp := &jsonPath{src: sel}
	i := 1
	for i < len(p) {
		var st jpStep
		switch p[i] {
		case '.':
			if i+1 < len(p) && p[i+1] == '.' {
				st.descend = true
				i += 2
			} else {
				i++
			}
			if i >= len(p) {
				return nil, fmt.Errorf("JSONPath %q ends after a dot", sel)
			}
			switch {
			case p[i] == '[':
				if !st.descend {
					return nil, fmt.Errorf("JSONPath %q: unexpected '[' after '.'", sel)
				}
				n, err := parseBracket(p, i, &st)
				if err != nil {
					return nil, fmt.Errorf("JSONPath %q: %w", sel, err)
				}
				i = n
			case p[i] == '*':
				st.kind = jpWild
				i++
			default:
				j := i
				for j < len(p) && p[j] != '.' && p[j] != '[' {
					j++
				}
				name := p[i:j]
				if name == "" {
					return nil, fmt.Errorf("JSONPath %q has an empty member name", sel)
				}
				if strings.ContainsAny(name, "*()?@,:'\"] \t") {
					return nil, fmt.Errorf("JSONPath %q: unsupported member name %q (use ['...'] for special characters)", sel, name)
				}
				st.kind, st.name = jpName, name
				i = j
			}
		case '[':
			n, err := parseBracket(p, i, &st)
			if err != nil {
				return nil, fmt.Errorf("JSONPath %q: %w", sel, err)
			}
			i = n
		default:
			return nil, fmt.Errorf("JSONPath %q: unexpected character %q at offset %d", sel, p[i], i)
		}
		jp.steps = append(jp.steps, st)
		if len(jp.steps) > maxJSONPathSteps {
			return nil, fmt.Errorf("JSONPath %q has more than %d steps", sel, maxJSONPathSteps)
		}
	}
	return jp, nil
}

// parseBracket parses "[...]" starting at p[i] == '[' into st and returns the
// offset after the closing bracket.
func parseBracket(p string, i int, st *jpStep) (int, error) {
	i++ // '['
	for i < len(p) && p[i] == ' ' {
		i++
	}
	if i >= len(p) {
		return 0, fmt.Errorf("unterminated '['")
	}
	if q := p[i]; q == '\'' || q == '"' {
		var sb strings.Builder
		i++
		for {
			if i >= len(p) {
				return 0, fmt.Errorf("unterminated quoted name")
			}
			ch := p[i]
			if ch == '\\' && i+1 < len(p) {
				sb.WriteByte(p[i+1])
				i += 2
				continue
			}
			if ch == q {
				i++
				break
			}
			sb.WriteByte(ch)
			i++
		}
		for i < len(p) && p[i] == ' ' {
			i++
		}
		if i >= len(p) || p[i] != ']' {
			return 0, fmt.Errorf("expected ']' after quoted name")
		}
		st.kind, st.name = jpName, sb.String()
		return i + 1, nil
	}
	j := strings.IndexByte(p[i:], ']')
	if j < 0 {
		return 0, fmt.Errorf("unterminated '['")
	}
	body := strings.TrimSpace(p[i : i+j])
	switch {
	case body == "*":
		st.kind = jpWild
	case body == "":
		return 0, fmt.Errorf("empty brackets")
	case strings.ContainsAny(body, ",:?()@"):
		return 0, fmt.Errorf("unions, slices and filters like [%s] are not supported", body)
	case body[0] == '-':
		return 0, fmt.Errorf("negative index [%s] is not supported", body)
	default:
		n, err := strconv.Atoi(body)
		if err != nil || n < 0 || n > 1<<30 {
			return 0, fmt.Errorf("invalid index [%s]", body)
		}
		st.kind, st.idx = jpIndex, n
	}
	return i + j + 1, nil
}

// appendJSONPathKey appends the path segment for object member key to path in
// the form used by reports: ".name" for simple names and ['name'] otherwise.
func appendJSONPathKey(path []byte, key []byte) []byte {
	simple := len(key) > 0
	for _, c := range key {
		if !(isAlnum(c) || c == '_' || c == '$' || c >= 0x80) {
			simple = false
			break
		}
	}
	if simple {
		path = append(path, '.')
		return append(path, key...)
	}
	path = append(path, '[', '\'')
	for _, c := range key {
		if c == '\'' || c == '\\' {
			path = append(path, '\\')
		}
		path = append(path, c)
	}
	return append(path, '\'', ']')
}

func appendJSONPathIndex(path []byte, idx int) []byte {
	path = append(path, '[')
	path = strconv.AppendInt(path, int64(idx), 10)
	return append(path, ']')
}
