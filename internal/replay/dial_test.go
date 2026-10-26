package replay

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimitDialsBoundsConcurrency(t *testing.T) {
	var cur, peak atomic.Int64
	inner := func(ctx context.Context, network, addr string) (net.Conn, error) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		cur.Add(-1)
		return nil, errors.New("no connection in this test")
	}
	dial := limitDials(inner, 4)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = dial(context.Background(), "tcp", "x:1")
		}()
	}
	wg.Wait()
	if p := peak.Load(); p > 4 || p < 2 {
		t.Fatalf("peak concurrent dials = %d, want between 2 and 4", p)
	}
}

func TestLimitDialsGivesUpWhenTheContextEnds(t *testing.T) {
	release := make(chan struct{})
	inner := func(ctx context.Context, network, addr string) (net.Conn, error) {
		<-release
		return nil, errors.New("released")
	}
	dial := limitDials(inner, 1)
	go func() { _, _ = dial(context.Background(), "tcp", "x:1") }() // holds the only slot
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := dial(ctx, "tcp", "x:1")
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("waiting for a slot ignored the context for %v", time.Since(start))
	}
}
