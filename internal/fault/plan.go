package fault

import (
	"context"
	"net/http"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// Plan is the set of faults chosen for one request. A nil *Plan is valid and
// means "no faults": Apply returns false and WrapWriter returns its argument.
type Plan struct {
	Delay          time.Duration    // total added latency
	BytesPerSecond int64            // 0 = no throttle
	Terminal       *model.FaultRule // error, abort or timeout rule; nil if none
	Fired          []string         // names of the rules that fired, in order

	// resp is the pre-rendered response for an error Terminal chosen by
	// Decide. Apply falls back to rendering Terminal on the fly when it is
	// absent or no longer belongs to Terminal (a hand-built Plan).
	resp *errorResponse
}

// Apply performs the delay and any terminal fault. It returns true if the
// request has been fully handled (or aborted) and the caller must stop.
//
// Apply first waits for Delay. If the request context ends during the wait
// (the client went away) Apply returns true immediately and writes nothing.
// Then, depending on Terminal:
//
//   - error: writes the rule's ResponseHeader, Status and Body. The
//     Content-Type is application/json when Body is a JSON object or array and
//     text/plain otherwise, unless the rule sets its own Content-Type
//     response header. "X-Mirage-Fault: <rule name>" is always set. Apply
//     returns true.
//   - abort: panics with http.ErrAbortHandler.
//   - timeout: holds the request open for Terminal.Delay (60 s if unset) or
//     until the client goes away, then panics with http.ErrAbortHandler.
//
// Callers and any middleware around them must let the http.ErrAbortHandler
// panic propagate so that net/http can reset the connection; see the package
// documentation. With no Terminal, Apply returns false once the delay has
// elapsed and the caller continues normally.
func (p *Plan) Apply(w http.ResponseWriter, r *http.Request) (handled bool) {
	if p == nil {
		return false
	}
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	if p.Delay > 0 {
		if sleepCtx(ctx, p.Delay) != nil {
			return true
		}
	}
	t := p.Terminal
	if t == nil {
		return false
	}
	switch t.Type {
	case model.FaultError:
		resp := p.resp
		if resp == nil || resp.rule != t {
			resp = newErrorResponse(t)
		}
		resp.write(w)
		return true
	case model.FaultAbort:
		panic(http.ErrAbortHandler)
	case model.FaultTimeout:
		hold := t.Delay.D()
		if hold <= 0 {
			hold = 60 * time.Second
		}
		_ = sleepCtx(ctx, hold)
		panic(http.ErrAbortHandler)
	}
	return false
}

// WrapWriter applies the throttle, if any, to w. The returned writer paces
// Write calls to BytesPerSecond (see WrapWriterContext). With no throttle, or
// on a nil Plan, it returns w unchanged.
//
// WrapWriter has no request context, so the wrapper cannot notice a client
// that went away until a write fails; prefer WrapWriterContext.
func (p *Plan) WrapWriter(w http.ResponseWriter) http.ResponseWriter {
	return p.WrapWriterContext(context.Background(), w)
}

// WrapWriterContext is WrapWriter with a context, normally r.Context(). Once
// the context is done, in-progress and later Write calls stop pacing and
// return the context's error, so a throttled transfer to a client that
// disconnected ends promptly instead of trickling to nowhere.
//
// The wrapper implements http.Flusher and Unwrap() http.ResponseWriter, so
// http.NewResponseController keeps working through it.
func (p *Plan) WrapWriterContext(ctx context.Context, w http.ResponseWriter) http.ResponseWriter {
	if p == nil || p.BytesPerSecond <= 0 {
		return w
	}
	return newThrottleWriter(ctx, w, p.BytesPerSecond)
}

// sleepCtx waits for d or until ctx is done, returning ctx's error in the
// latter case. It does not allocate a timer for d <= 0.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
