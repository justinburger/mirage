package anonymize

import (
	"bytes"
	"mime"
	"strings"
)

// maxMultipartDepth bounds how many multipart bodies may be nested inside one
// another (a form-data part that is itself multipart/mixed). Deeper nesting is
// handled like a malformed body.
const maxMultipartDepth = 3

// mpPart locates one part of a multipart body inside the body bytes. Nothing
// is copied: the delimiters, part headers and line endings stay exactly as
// they were captured, and only part bodies are ever rewritten.
type mpPart struct {
	lo     int // first byte owned by the part: the end of the previous part's body, or the first delimiter
	hdrLo  int // part headers start here (after the delimiter line)
	bodyLo int // part body starts here (after the blank line ending the headers)
	bodyHi int // part body ends here (before the line break that precedes the next delimiter)
}

const (
	delimNone  = iota // not a delimiter line
	delimPart         // "--boundary": another part follows
	delimClose        // "--boundary--": the last delimiter
)

// delimAt tests whether the delimiter line for dash ("--boundary") starts at
// b[i:] (the caller has checked that b[i:] begins with dash and that i is at
// the start of a line). It returns the kind and the index just after the
// line. Transport padding (spaces, tabs) after the delimiter is allowed.
func delimAt(b []byte, i int, dash []byte, truncated bool) (kind, next int) {
	j := i + len(dash)
	kind = delimPart
	if j+1 < len(b) && b[j] == '-' && b[j+1] == '-' {
		kind = delimClose
		j += 2
	}
	for j < len(b) && (b[j] == ' ' || b[j] == '\t') {
		j++
	}
	switch {
	case j == len(b):
		// A delimiter at the very end: fine for the closing one and for a cut
		// capture, anything else is not a delimiter line.
		if kind == delimClose || truncated {
			return kind, j
		}
	case b[j] == '\n':
		return kind, j + 1
	case b[j] == '\r' && j+1 < len(b) && b[j+1] == '\n':
		return kind, j + 2
	}
	return delimNone, 0
}

// findDelim returns the start of the first delimiter line for dash that
// begins at or after from, and its kind and end. A line only counts when it
// starts at 0 or right after a line feed.
func findDelim(b []byte, from int, dash []byte, truncated bool) (at, kind, next int) {
	for from <= len(b) {
		i := bytes.Index(b[from:], dash)
		if i < 0 {
			return -1, delimNone, 0
		}
		i += from
		if i == 0 || b[i-1] == '\n' {
			if k, n := delimAt(b, i, dash, truncated); k != delimNone {
				return i, k, n
			}
		}
		from = i + 1
	}
	return -1, delimNone, 0
}

// splitMultipart finds the parts of a multipart body. ok is false when b is
// not a well-formed multipart body for boundary: no opening delimiter, a part
// without its header terminator, or (unless truncated) no closing delimiter.
// A truncated capture may stop anywhere: the last part then runs to the end of
// the data.
func splitMultipart(b []byte, boundary string, truncated bool) (parts []mpPart, ok bool) {
	dash := []byte("--" + boundary)
	nlDash := []byte("\n--" + boundary)
	at, kind, pos := findDelim(b, 0, dash, truncated)
	if at < 0 {
		return nil, false
	}
	prevEnd := at
	for kind == delimPart {
		p := mpPart{lo: prevEnd, hdrLo: pos}
		// Part headers: lines up to the first blank one.
		i := pos
		for {
			nl := bytes.IndexByte(b[i:], '\n')
			if nl < 0 { // headers cut off or never terminated
				if !truncated {
					return nil, false
				}
				p.bodyLo, p.bodyHi = len(b), len(b)
				return append(parts, p), true
			}
			line := b[i : i+nl]
			i += nl + 1
			if len(line) == 0 || (len(line) == 1 && line[0] == '\r') {
				break
			}
		}
		p.bodyLo = i
		// Part body: up to the line break before the next delimiter. A
		// delimiter may follow the headers directly (empty body).
		hi, k, n := -1, delimNone, 0
		if k0, n0 := delimAtLineStart(b, i, dash, truncated); k0 != delimNone {
			hi, k, n = i, k0, n0
		} else {
			from := i
			for {
				j := bytes.Index(b[from:], nlDash)
				if j < 0 {
					break
				}
				j += from
				if k1, n1 := delimAt(b, j+1, dash, truncated); k1 != delimNone {
					hi, k, n = j, k1, n1
					if hi > i && b[hi-1] == '\r' {
						hi--
					}
					break
				}
				from = j + 1
			}
		}
		if hi < 0 { // no further delimiter
			if !truncated {
				return nil, false
			}
			p.bodyHi = len(b)
			return append(parts, p), true
		}
		p.bodyHi = hi
		parts = append(parts, p)
		prevEnd, kind, pos = hi, k, n
	}
	return parts, true
}

// delimAtLineStart is delimAt for b[i:] when it starts with the delimiter.
func delimAtLineStart(b []byte, i int, dash []byte, truncated bool) (kind, next int) {
	if !bytes.HasPrefix(b[i:], dash) {
		return delimNone, 0
	}
	return delimAt(b, i, dash, truncated)
}

// boundaryOf extracts the boundary parameter of a multipart Content-Type.
func boundaryOf(ct string) string {
	if _, params, err := mime.ParseMediaType(ct); err == nil {
		return params["boundary"]
	}
	// Lenient scan for values mime cannot parse (unquoted special characters).
	lower := strings.ToLower(ct)
	i := strings.Index(lower, "boundary=")
	if i < 0 {
		return ""
	}
	v := ct[i+len("boundary="):]
	if j := strings.IndexByte(v, ';'); j >= 0 {
		v = v[:j]
	}
	return strings.Trim(strings.TrimSpace(v), `"`)
}

// partInfo is what the anonymizer needs to know about a part's headers.
type partInfo struct {
	name        string // Content-Disposition name
	hasFilename bool   // Content-Disposition carries a filename: a file part
	contentType string
	encoded     bool // Content-Transfer-Encoding base64 or quoted-printable
}

// parsePartHeaders reads the headers of a part (raw bytes up to and including
// the blank line). Folded header lines are joined.
func parsePartHeaders(h []byte) partInfo {
	var pi partInfo
	var cur []byte
	flush := func() {
		if len(cur) == 0 {
			return
		}
		k, v, ok := bytes.Cut(cur, []byte{':'})
		cur = cur[:0]
		if !ok {
			return
		}
		key := string(bytes.TrimSpace(k))
		val := string(bytes.TrimSpace(v))
		switch {
		case strings.EqualFold(key, "Content-Disposition"):
			pi.name, pi.hasFilename = dispositionParams(val)
		case strings.EqualFold(key, "Content-Type"):
			pi.contentType = val
		case strings.EqualFold(key, "Content-Transfer-Encoding"):
			switch strings.ToLower(val) {
			case "base64", "quoted-printable":
				pi.encoded = true
			}
		}
	}
	for len(h) > 0 {
		var line []byte
		if nl := bytes.IndexByte(h, '\n'); nl >= 0 {
			line, h = h[:nl], h[nl+1:]
		} else {
			line, h = h, nil
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			break
		}
		if (line[0] == ' ' || line[0] == '\t') && len(cur) > 0 {
			cur = append(cur, ' ')
			cur = append(cur, bytes.TrimLeft(line, " \t")...)
			continue
		}
		flush()
		cur = append(cur, line...)
	}
	flush()
	return pi
}

// dispositionParams returns the name and whether a filename is present in a
// Content-Disposition value.
func dispositionParams(v string) (name string, hasFilename bool) {
	if _, params, err := mime.ParseMediaType(v); err == nil {
		_, hasFilename = params["filename"]
		return params["name"], hasFilename
	}
	// Lenient: split on ';' and read key=value pairs, quoted or not.
	for i, tok := range strings.Split(v, ";") {
		if i == 0 {
			continue // the disposition type
		}
		k, val, ok := strings.Cut(strings.TrimSpace(tok), "=")
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "name":
			name = val
		case "filename", "filename*":
			hasFilename = true
		}
	}
	return name, hasFilename
}

// multipartBody anonymizes a multipart body part by part. Text parts (no
// filename, a textual or missing Content-Type) are processed like the rest of
// the engine would process a value or body of that type: a form rule whose
// selector is the part name wins, otherwise the part is handled by its content
// type (JSON, form, text with regex rules and detectors). File parts and
// binary parts are opaque: kept, or filled when OpaqueBodies is "fill". The
// boundary, delimiters and part headers are written back byte for byte; only
// part bodies change, and a part removed by a `remove` form rule disappears
// with its delimiter and headers. A malformed multipart body is handled by
// malformedMultipart.
func (r *run) multipartBody(b []byte, ct string, truncated bool, loc *locator) ([]byte, bool) {
	boundary := boundaryOf(ct)
	if boundary == "" || r.mpDepth >= maxMultipartDepth {
		return r.malformedMultipart(b, truncated, loc)
	}
	parts, ok := splitMultipart(b, boundary, truncated)
	if !ok {
		return r.malformedMultipart(b, truncated, loc)
	}
	r.mpDepth++
	defer func() { r.mpDepth-- }()
	dash := []byte("--" + boundary)

	var out []byte // built lazily, on the first change
	copied := 0    // b[:copied] is already represented in out
	kept := false  // an earlier part stays in the output
	for i := range parts {
		p := &parts[i]
		body := b[p.bodyLo:p.bodyHi]
		if len(body) == 0 {
			kept = true
			continue
		}
		info := parsePartHeaders(b[p.hdrLo:p.bodyLo])
		l := *loc
		if r.rep != nil {
			seg := "multipart." + info.name
			if info.name == "" {
				seg = "multipart[" + itoa(i+1) + "]"
			}
			if l.part != "" {
				seg = l.part + "/" + seg
			}
			l.part = seg
		}
		// Only the last part of a cut capture can be cut itself.
		cut := truncated && i == len(parts)-1 && p.bodyHi == len(b)

		var nb []byte
		var changed, drop bool
		switch {
		case info.hasFilename || info.encoded:
			nb, changed = r.opaque(body, &l)
		default:
			sel := r.sd.form[info.name]
			switch rl := removes(sel); {
			case rl != nil:
				if r.rep != nil {
					r.record(&l, rl.source, string(body), "")
				}
				drop = true
			case len(sel) > 0:
				l.name = info.name
				s := string(body)
				if ns := r.value(sel, s, &l); ns != s {
					nb, changed = []byte(ns), true
				}
			default:
				nb, changed = r.inspect(body, info.contentType, cut, &l)
			}
		}
		if changed {
			scrubDelimiter(nb, dash)
		}
		switch {
		case drop:
			if out == nil {
				out = make([]byte, 0, len(b))
			}
			if copied < p.lo {
				out = append(out, b[copied:p.lo]...)
			}
			copied = p.bodyHi
			if !kept {
				// Nothing before this part stays, so the line break that
				// ends it would now precede the first delimiter: drop it.
				switch {
				case bytes.HasPrefix(b[copied:], []byte("\r\n")):
					copied += 2
				case bytes.HasPrefix(b[copied:], []byte("\n")):
					copied++
				}
			}
			continue
		case changed:
			if out == nil {
				out = make([]byte, 0, len(b)+64)
			}
			out = append(out, b[copied:p.bodyLo]...)
			out = append(out, nb...)
			copied = p.bodyHi
		}
		kept = true
	}
	if out == nil {
		return nil, false
	}
	out = append(out, b[copied:]...)
	return out, true
}

// scrubDelimiter makes sure a new part body cannot be mistaken for the end of
// its part: filler is arbitrary bytes, and a short boundary could appear in
// it by chance. Each occurrence of a line starting with the delimiter is
// broken by changing the byte before it (or its first byte, at the start of
// the body).
func scrubDelimiter(body, dash []byte) {
	if bytes.HasPrefix(body, dash) {
		body[0] ^= 0x20
	}
	for from := 0; ; {
		i := bytes.Index(body[from:], dash)
		if i < 0 {
			return
		}
		i += from
		if i > 0 && body[i-1] == '\n' {
			body[i-1] = ' '
		}
		from = i + 1
	}
}

// malformedMultipart handles a multipart body that cannot be split into parts
// (no boundary, no delimiters, an unterminated part): like any other body the
// engine cannot inspect it is kept, or filled when OpaqueBodies is "fill". A
// textual body is still scanned as text when it is kept, as it would be for an
// unknown content type, so a damaged upload does not hide PII from the
// detectors.
func (r *run) malformedMultipart(b []byte, truncated bool, loc *locator) ([]byte, bool) {
	if r.c.fill || looksBinary(b, truncated) {
		return r.opaque(b, loc)
	}
	return r.textBody(b, loc)
}
