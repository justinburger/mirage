package fault

import (
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// kind is a precomputed fault type, so the hot path switches on an integer
// rather than comparing strings.
type kind uint8

const (
	kindLatency kind = iota + 1
	kindError
	kindAbort
	kindTimeout
	kindThrottle
)

// maxClamp bounds delays and jitter so that sums and ± jitter arithmetic can
// never overflow a time.Duration (about 73 years).
const maxClamp = time.Duration(math.MaxInt64 / 4)

// headerCond is one exact header match.
type headerCond struct {
	name  string // canonical header name
	value string
	host  bool // name is Host: compared against r.Host, case-insensitively
}

func (h *headerCond) matches(r *http.Request) bool {
	if h.host {
		return strings.EqualFold(r.Host, h.value)
	}
	for _, v := range r.Header[h.name] {
		if v == h.value {
			return true
		}
	}
	return false
}

// compiledRule is an immutable, pre-processed fault rule. Only the fired
// counter changes, and it is shared by pointer between successive rule sets
// that contain the same rule ID.
type compiledRule struct {
	rule  model.FaultRule // defaulted deep copy
	kind  kind
	fired *atomic.Int64

	terminal bool // error, abort or timeout
	prob     float64

	serviceID string
	methods   []string
	hasPath   bool
	glob      pathGlob
	re        *regexp.Regexp // non-nil for "re:" patterns
	headers   []headerCond

	delay  time.Duration // latency
	jitter time.Duration // latency
	bps    int64         // throttle
	resp   *errorResponse
}

// matches reports whether the request is in scope for the rule: service,
// method, path and headers. It does not consider probability and never
// allocates.
func (c *compiledRule) matches(serviceID string, r *http.Request) bool {
	if c.serviceID != "" && c.serviceID != serviceID {
		return false
	}
	if len(c.methods) > 0 && !c.methodMatches(r.Method) {
		return false
	}
	if c.hasPath {
		path := ""
		if r.URL != nil {
			path = r.URL.Path
		}
		if c.re != nil {
			if !c.re.MatchString(path) {
				return false
			}
		} else if !c.glob.match(path) {
			return false
		}
	}
	for i := range c.headers {
		if !c.headers[i].matches(r) {
			return false
		}
	}
	return true
}

func (c *compiledRule) methodMatches(method string) bool {
	if method == "" {
		method = http.MethodGet // http.Request: empty means GET
	}
	for _, m := range c.methods {
		if m == method || strings.EqualFold(m, method) {
			return true
		}
	}
	return false
}

// roll decides whether a matching request gets the fault. math/rand/v2's
// top-level functions use a per-thread generator, so this is lock-free.
func (c *compiledRule) roll() bool {
	switch {
	case c.prob >= 1:
		return true
	case c.prob <= 0:
		return false
	}
	return rand.Float64() < c.prob
}

// sampleDelay returns delay ± uniform jitter, never negative.
func (c *compiledRule) sampleDelay() time.Duration {
	d := c.delay
	if j := c.jitter; j > 0 {
		d += time.Duration(rand.Int64N(2*int64(j)+1)) - j
	}
	if d < 0 {
		return 0
	}
	return d
}

// addDelay adds two non-negative durations, saturating instead of overflowing.
func addDelay(a, b time.Duration) time.Duration {
	if s := a + b; s >= a {
		return s
	}
	return time.Duration(math.MaxInt64)
}

func clampDuration(d time.Duration) time.Duration {
	if d > maxClamp {
		return maxClamp
	}
	return d
}
