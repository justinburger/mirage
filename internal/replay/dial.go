package replay

import (
	"context"
	"net"
)

// maxConcurrentDials bounds how many connections a job opens at the same
// time. A job with hundreds of workers otherwise dials them all in the first
// instant, and a target whose accept queue is short (128 on macOS, often 128
// to 4096 elsewhere) answers the overflow with resets that show up as failed
// requests although nothing is wrong with the target. Dials past the limit
// wait their turn, so the pool fills in a few milliseconds instead.
const maxConcurrentDials = 32

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// limitDials wraps dial so at most n calls run at once.
func limitDials(dial dialFunc, n int) dialFunc {
	slots := make(chan struct{}, n)
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return dial(ctx, network, addr)
	}
}
