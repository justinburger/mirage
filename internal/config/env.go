package config

import (
	"bytes"
	"fmt"
	"strings"
)

// envError is a malformed or unresolvable ${VAR} reference.
type envError struct {
	line int
	msg  string
}

func (e *envError) Error() string { return fmt.Sprintf("line %d: %s", e.line, e.msg) }

// expandEnv replaces ${VAR} and ${VAR:-default} references in data.
//
//   - ${VAR} is the value of VAR; a variable that is not set at all is an
//     error (so a typo does not silently become an empty string), a variable
//     that is set but empty expands to "".
//   - ${VAR:-default} is default when VAR is unset or empty. The default runs
//     up to the first '}' and may be empty (${VAR:-}).
//   - $${ is an escape and yields a literal "${".
//   - A bare $VAR (no braces) or a lone '$' is left alone, so passwords and
//     regular expressions containing '$' survive.
//
// When skipComments is set (YAML), lines whose first non-blank character is
// '#' are copied unchanged, so a commented-out example such as
// "# token: ${ADMIN_TOKEN}" never fails the load.
func expandEnv(data []byte, skipComments bool, lookup func(string) (string, bool)) ([]byte, error) {
	if !bytes.Contains(data, []byte("${")) {
		return data, nil
	}
	out := make([]byte, 0, len(data)+64)
	for line := 1; len(data) > 0; line++ {
		var cur []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			cur, data = data[:i+1], data[i+1:]
		} else {
			cur, data = data, nil
		}
		if skipComments && isCommentLine(cur) {
			out = append(out, cur...)
			continue
		}
		var err error
		if out, err = expandLine(out, cur, line, lookup); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func isCommentLine(line []byte) bool {
	t := bytes.TrimLeft(line, " \t")
	return len(t) > 0 && t[0] == '#'
}

// expandLine appends the expansion of one line to out.
func expandLine(out, line []byte, lineNo int, lookup func(string) (string, bool)) ([]byte, error) {
	for len(line) > 0 {
		i := bytes.IndexByte(line, '$')
		if i < 0 {
			return append(out, line...), nil
		}
		out = append(out, line[:i]...)
		line = line[i:]
		switch {
		case bytes.HasPrefix(line, []byte("$${")):
			out = append(out, '$', '{')
			line = line[3:]
		case bytes.HasPrefix(line, []byte("${")):
			end := bytes.IndexByte(line, '}')
			if end < 0 {
				return nil, &envError{lineNo, "unterminated ${ reference (write $${ for a literal)"}
			}
			ref := string(line[2:end])
			name, def, hasDef := ref, "", false
			if k := strings.Index(ref, ":-"); k >= 0 {
				name, def, hasDef = ref[:k], ref[k+2:], true
			}
			if !validEnvName(name) {
				return nil, &envError{lineNo, fmt.Sprintf("invalid variable reference ${%s} (write $${ for a literal)", ref)}
			}
			val, set := lookup(name)
			switch {
			case hasDef && (!set || val == ""):
				val = def
			case !set:
				return nil, &envError{lineNo, fmt.Sprintf("environment variable %s is not set (use ${%s:-default} to make it optional)", name, name)}
			}
			out = append(out, val...)
			line = line[end+1:]
		default:
			out = append(out, '$')
			line = line[1:]
		}
	}
	return out, nil
}

func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
