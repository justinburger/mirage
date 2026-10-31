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
//	$..key           recursive descent: `key` at any depth (member names
//	                 after ".." are compared ignoring case, "_" and "-":
//	                 $..api_key also selects apiKey, API-KEY and apikey)
//	$..*  $..[0]     recursive descent with a wildcard or an index
//	$..[?(@.a || @.b)].key
//	                 `key` at any depth, but only in objects that also have a
//	                 member `a` or `b` (the sibling filter; see below)
//
// Slices, unions, negative indexes and every other filter are rejected with a
// validation error rather than silently ignored, so a rule can never look like
// it covers data that it does not.
//
// The sibling filter exists for keys that are only sometimes personal data: a
// bare "name" is a person next to an "email" and a product next to a "sku".
// The one form `$..[?(@.a || @.b || ...)].key` (parentheses and the quoted
// form @['a'] are optional) is the whole syntax: a descendant filter on
// member existence followed by one member name. The rewriter decides it
// per object, looking at the whole object, so member order does not matter.

const (
	jpName = iota
	jpIndex
	jpWild
)

const maxJSONPathSteps = 64

type jpStep struct {
	// descend makes the step match at any depth below the previous step.
	descend bool
	// fold marks a recursive-descent name step, which matches member names
	// ignoring case, "_" and "-"; norm is name normalized (see normKey).
	fold bool
	kind uint8
	idx  int32 // parseBracket limits an index to 1<<30
	name string
	norm string
}

// matchFold is matches for a fold step.
//
//go:noinline
func (s *jpStep) matchFold(obj bool, key []byte) bool {
	return obj && normEqual(key, s.norm)
}

// matches reports whether the step accepts a child of a container: an object
// member called key, or element idx of an array. A recursive-descent name step
// (fold) is matched by matchFold instead: this method stays small, it is on
// the path of every state of every node.
func (s *jpStep) matches(obj bool, key []byte, idx int) bool {
	switch s.kind {
	case jpWild:
		return true
	case jpName:
		return obj && string(key) == s.name
	default:
		return !obj && idx == int(s.idx)
	}
}

type jsonPath struct {
	src   string
	steps []jpStep
	// siblings, when set, restricts the single step to members of objects
	// that also have one of these members (the sibling filter). sibNorm is
	// siblings normalized (see normKey): member names are compared ignoring
	// case, "_" and "-".
	siblings []string
	sibNorm  []string
}

func (p *jsonPath) String() string { return p.src }

// parseJSONPath compiles a selector. Errors are plain messages; callers wrap
// them in a model.ValidationError.
func parseJSONPath(sel string) (*jsonPath, error) {
	p := strings.TrimSpace(sel)
	if p == "" || p[0] != '$' {
		return nil, fmt.Errorf("JSONPath %q must start with $", sel)
	}
	if strings.HasPrefix(p, "$..[?") {
		return parseSiblingFilter(sel, p)
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
		if st.descend && st.kind == jpName {
			st.fold, st.norm = true, normKey(st.name)
		}
		jp.steps = append(jp.steps, st)
		if len(jp.steps) > maxJSONPathSteps {
			return nil, fmt.Errorf("JSONPath %q has more than %d steps", sel, maxJSONPathSteps)
		}
	}
	return jp, nil
}

// filterEnd returns the offset of the ']' that closes a filter whose text
// starts at s[0] (just after "[?"), skipping brackets inside quoted names, or
// -1.
func filterEnd(s string) int {
	var quote byte
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '[':
			depth++
		case c == ']':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

// maxSiblingNames bounds the members a sibling filter may list.
const maxSiblingNames = 64

// parseSiblingFilter parses `$..[?(@.a || @['b'])].key` (p, already trimmed;
// sel is the original text for messages).
func parseSiblingFilter(sel, p string) (*jsonPath, error) {
	bad := func(format string, args ...any) (*jsonPath, error) {
		return nil, fmt.Errorf("JSONPath %q: "+format+" (the only filter supported is $..[?(@.a || @.b)].key)", append([]any{sel}, args...)...)
	}
	i := len("$..[?")
	end := filterEnd(p[i:])
	if end < 0 {
		return bad("unterminated filter")
	}
	cond := strings.TrimSpace(p[i : i+end])
	rest := p[i+end+1:]
	if strings.HasPrefix(cond, "(") {
		if !strings.HasSuffix(cond, ")") {
			return bad("unbalanced parentheses in the filter")
		}
		cond = strings.TrimSpace(cond[1 : len(cond)-1])
	}
	var names []string
	for _, term := range strings.Split(cond, "||") {
		term = strings.TrimSpace(term)
		if !strings.HasPrefix(term, "@") {
			return bad("filter term %q must be @.member", term)
		}
		term = term[1:]
		var name string
		switch {
		case strings.HasPrefix(term, "['") && strings.HasSuffix(term, "']"), strings.HasPrefix(term, `["`) && strings.HasSuffix(term, `"]`):
			name = term[2 : len(term)-2]
		case strings.HasPrefix(term, "."):
			name = term[1:]
		}
		if name == "" || strings.ContainsAny(name, "*()?@,:'\"[]. \t&|=<>!") {
			return bad("filter term %q must be @.member or @['member'] with a plain member name", "@"+term)
		}
		names = append(names, name)
	}
	if len(names) == 0 || len(names) > maxSiblingNames {
		return bad("the filter must list between 1 and %d members", maxSiblingNames)
	}
	var st jpStep
	st.descend = true
	switch {
	case strings.HasPrefix(rest, "."):
		name := rest[1:]
		if name == "" || strings.ContainsAny(name, "*()?@,:'\"[]. \t") {
			return bad("the filter must be followed by one plain member name")
		}
		st.kind, st.name = jpName, name
	case strings.HasPrefix(rest, "['") || strings.HasPrefix(rest, `["`):
		var tmp jpStep
		n, err := parseBracket(rest, 0, &tmp)
		if err != nil || n != len(rest) || tmp.kind != jpName {
			return bad("the filter must be followed by one member name")
		}
		st.kind, st.name = jpName, tmp.name
	default:
		return bad("the filter must be followed by .member")
	}
	st.fold, st.norm = true, normKey(st.name)
	sibNorm := make([]string, len(names))
	for i, n := range names {
		sibNorm[i] = normKey(n)
	}
	return &jsonPath{src: sel, steps: []jpStep{st}, siblings: names, sibNorm: sibNorm}, nil
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
		st.kind, st.idx = jpIndex, int32(n)
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
