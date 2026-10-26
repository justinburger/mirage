package replay

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
)

// maxPreloadBytes is how much prepared request data a job may hold in memory.
const maxPreloadBytes = 1 << 30

// itemOverhead approximates the memory of an item struct, its slice slot and
// the allocator's rounding, for the preload cap.
const itemOverhead = 160

var (
	// ErrRecordingTooLarge is wrapped by the error Run returns when the
	// prepared requests would exceed 1 GiB.
	ErrRecordingTooLarge = errors.New("recording too large to preload")
	// ErrEmptyRecording is wrapped by the error Run returns when the recording
	// has no request left to replay (empty, or everything filtered out).
	ErrEmptyRecording = errors.New("nothing to replay")
)

// Item flags.
const (
	flagErr  uint8 = 1 << iota // the recorded response was an error
	flagGzip                   // body is plain and must be gzipped when sent (anonymized path)
)

// item is one prepared request. Items are immutable once loading finishes and
// shared by every worker.
type item struct {
	// off is the request's StartedAt in Unix nanoseconds while loading and its
	// offset from the earliest request (after MaxGap) once finish has run.
	off  time.Duration
	uri  string // escaped target path prefix + recorded path, then "?" + raw query
	body []byte
	hdr  *hdrSet
	// method and endpoint are interned.
	method   string
	endpoint string
	status   int32 // recorded response status
	flags    uint8
}

func (it *item) isErr() bool { return it.flags&flagErr != 0 }

// splitURI returns the recorded path and raw query of the item, with the
// target prefix (prefixLen bytes) removed.
func (it *item) splitURI(prefixLen int) (path, rawQuery string) {
	uri := it.uri[prefixLen:]
	for i := 0; i < len(uri); i++ {
		if uri[i] == '?' {
			return uri[:i], uri[i+1:]
		}
	}
	return uri, ""
}

// loader turns streamed exchanges into prepared items.
type loader struct {
	tgt     target
	plain   bool // headers are final at load time (no anonymization on the fly)
	remove  map[string]struct{}
	ov      overlay
	include []*regexp.Regexp
	exclude []*regexp.Regexp
	limit   int64

	items   []item
	bytes   int64
	seen    int64
	skipped int64

	strs map[string]string
	hdrs *interner
	gz   *gzipper
}

func newLoader(cfg model.ReplayConfig, tgt target, ov overlay, plain bool, limit int64) (*loader, error) {
	l := &loader{
		tgt: tgt, plain: plain, ov: ov, limit: limit,
		remove: removeSet(cfg.RemoveHeaders),
		strs:   map[string]string{},
		hdrs:   newInterner(),
	}
	var err error
	if l.include, err = compileAll(cfg.Include); err != nil {
		return nil, err
	}
	if l.exclude, err = compileAll(cfg.Exclude); err != nil {
		return nil, err
	}
	return l, nil
}

func compileAll(exprs []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, e := range exprs {
		re, err := regexp.Compile(e)
		if err != nil {
			return nil, fmt.Errorf("filter %q: %w", e, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func (l *loader) intern(s string) string {
	if v, ok := l.strs[s]; ok {
		return v
	}
	l.strs[s] = s
	return s
}

// keep applies the include and exclude filters to "METHOD /path?query".
func (l *loader) keep(method string, r *model.Request) bool {
	if len(l.include) == 0 && len(l.exclude) == 0 {
		return true
	}
	line := method + " " + r.URI()
	if len(l.include) > 0 && !slices.ContainsFunc(l.include, func(re *regexp.Regexp) bool { return re.MatchString(line) }) {
		return false
	}
	return !slices.ContainsFunc(l.exclude, func(re *regexp.Regexp) bool { return re.MatchString(line) })
}

// add prepares one exchange. It takes ownership of the exchange's request
// body and header values.
func (l *loader) add(ex *model.Exchange) error {
	l.seen++
	req := &ex.Request
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	if !l.keep(method, req) {
		l.skipped++
		return nil
	}

	path := req.Path
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	uri := l.tgt.prefix + path
	if req.RawQuery != "" {
		uri += "?" + req.RawQuery
	}

	body := req.Body
	hdr := cleanHeaders(req.Header, l.remove)
	var flags uint8
	if wantsGzip(req) {
		if l.plain {
			body = l.gzip().compress(body)
			hdr["Content-Encoding"] = []string{"gzip"}
		} else {
			flags |= flagGzip
		}
	}
	if l.plain {
		l.ov.apply(hdr)
	}
	set, added := l.hdrs.intern(hdr)
	if added {
		l.bytes += set.size
	}

	endpoint := ex.Endpoint
	if endpoint == "" {
		endpoint = pathnorm.Default.Endpoint(method, req.Path)
	}
	if ex.IsError() {
		flags |= flagErr
	}
	l.items = append(l.items, item{
		off:      time.Duration(ex.StartedAt.UnixNano()),
		uri:      uri,
		body:     body,
		hdr:      set,
		method:   l.intern(method),
		endpoint: l.intern(endpoint),
		status:   int32(ex.Response.Status),
		flags:    flags,
	})
	l.bytes += itemOverhead + int64(len(uri)) + int64(len(body))
	if l.bytes > l.limit {
		return fmt.Errorf("%w (%d items, %d bytes); narrow it with include/exclude",
			ErrRecordingTooLarge, len(l.items), l.bytes)
	}
	return nil
}

func (l *loader) gzip() *gzipper {
	if l.gz == nil {
		l.gz = newGzipper(-1)
	}
	return l.gz
}

// prepared is the loaded, time-ordered request list.
type prepared struct {
	items []item
	// span is the offset of the last item; period is the length of one pass
	// of the recording as the timeline replays it (see passPeriod).
	span, period time.Duration
	seen         int64
	skipped      int64
	bytes        int64
}

// finish sorts the items by StartedAt (stable, so ties keep the store's Seq
// order), rebases the offsets to the earliest item and applies maxGap.
func (l *loader) finish(maxGap time.Duration) (*prepared, error) {
	if len(l.items) == 0 {
		if l.seen > 0 {
			return nil, fmt.Errorf("%w: all %d requests were removed by include/exclude", ErrEmptyRecording, l.seen)
		}
		return nil, fmt.Errorf("%w: the recording is empty", ErrEmptyRecording)
	}
	items := l.items
	byOff := func(a, b item) int { return cmp.Compare(a.off, b.off) }
	if !slices.IsSortedFunc(items, byOff) {
		slices.SortStableFunc(items, byOff)
	}
	prev, cum := items[0].off, time.Duration(0)
	for i := range items {
		gap := items[i].off - prev
		prev = items[i].off
		if maxGap > 0 && gap > maxGap {
			gap = maxGap
		}
		cum += gap
		items[i].off = cum
	}
	span := items[len(items)-1].off
	return &prepared{items: items, span: span, period: passPeriod(len(items), span),
		seen: l.seen, skipped: l.skipped, bytes: l.bytes}, nil
}

// passPeriod is the length of one pass of a recording of n requests spanning
// span: the span plus the mean gap, so that looping continues at the recorded
// rate instead of bunching the last request of one pass with the first of the
// next. A recording with no time extent (one request, or identical
// timestamps) has no recorded rate; a pass then takes one second.
func passPeriod(n int, span time.Duration) time.Duration {
	mean := time.Duration(0)
	if n > 1 {
		mean = span / time.Duration(n-1)
	}
	if p := span + mean; p > 0 {
		return p
	}
	return time.Second
}
