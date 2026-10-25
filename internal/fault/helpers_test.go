package fault

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// newReq builds a server-style request. Extra arguments are header
// name/value pairs.
func newReq(method, target string, headerKV ...string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(headerKV); i += 2 {
		r.Header.Add(headerKV[i], headerKV[i+1])
	}
	return r
}

// errRule returns an always-firing error rule, the simplest valid rule.
func errRule(id string, mods ...func(*model.FaultRule)) model.FaultRule {
	r := model.FaultRule{
		ID:          id,
		Name:        "rule " + id,
		Enabled:     true,
		Probability: 1,
		Type:        model.FaultError,
		Status:      503,
	}
	for _, m := range mods {
		m(&r)
	}
	return r
}

func latencyRule(id string, delay, jitter time.Duration, mods ...func(*model.FaultRule)) model.FaultRule {
	r := model.FaultRule{
		ID:          id,
		Name:        "rule " + id,
		Enabled:     true,
		Probability: 1,
		Type:        model.FaultLatency,
		Delay:       model.Duration(delay),
		Jitter:      model.Duration(jitter),
	}
	for _, m := range mods {
		m(&r)
	}
	return r
}

func throttleRule(id string, bps int64, mods ...func(*model.FaultRule)) model.FaultRule {
	r := model.FaultRule{
		ID:             id,
		Name:           "rule " + id,
		Enabled:        true,
		Probability:    1,
		Type:           model.FaultThrottle,
		BytesPerSecond: bps,
	}
	for _, m := range mods {
		m(&r)
	}
	return r
}

func abortRule(id string, mods ...func(*model.FaultRule)) model.FaultRule {
	r := model.FaultRule{
		ID:          id,
		Name:        "rule " + id,
		Enabled:     true,
		Probability: 1,
		Type:        model.FaultAbort,
	}
	for _, m := range mods {
		m(&r)
	}
	return r
}

func timeoutRule(id string, hold time.Duration, mods ...func(*model.FaultRule)) model.FaultRule {
	r := model.FaultRule{
		ID:          id,
		Name:        "rule " + id,
		Enabled:     true,
		Probability: 1,
		Type:        model.FaultTimeout,
		Delay:       model.Duration(hold),
	}
	for _, m := range mods {
		m(&r)
	}
	return r
}

// newInjector builds an injector and fails the test if the rules are invalid.
func newInjector(tb testing.TB, rules ...model.FaultRule) *Injector {
	tb.Helper()
	inj := NewInjector()
	if err := inj.SetRules(rules); err != nil {
		tb.Fatalf("SetRules: %v", err)
	}
	return inj
}

// lockedBuffer is a bytes.Buffer safe for concurrent use, for capturing an
// http.Server's error log.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
