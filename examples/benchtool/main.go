// Command benchtool is the load generator and trivial upstream behind
// scripts/bench.sh and docs/BENCHMARKS.md.
//
//	benchtool upstream -addr 127.0.0.1:7100
//	    a server that answers every GET with the same 256-byte JSON document
//
//	benchtool load -url http://127.0.0.1:9001 -c 64 -d 10s
//	    closed-loop load: -c connections, each sending the next request when
//	    the previous response was read. Reports throughput and latency.
//
//	benchtool load -url ... -c 8 -rate 5000 -d 10s
//	    paced load at a fixed total rate (for the latency of a lightly
//	    loaded system, and for capture without queue pressure)
//
// -paths N cycles through /item/1 ... /item/N so a recording made with it has
// N distinct request keys.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: benchtool upstream|load [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "upstream":
		upstream(os.Args[2:])
	case "load":
		load(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "benchtool: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

// ---------------------------------------------------------------- upstream

func upstream(args []string) {
	fs := flag.NewFlagSet("upstream", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7100", "listen address")
	fs.Parse(args)

	body := []byte(`{"id":1,"name":"benchmark item","sku":"SKU-00001","price_cents":1999,"stock":42,"tags":["a","b","c"],"description":"A trivial JSON document of about two hundred and fifty six bytes, the same for every request."}` + "\n")
	srv := &http.Server{
		Addr: *addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Type", "application/json")
			h.Set("Content-Length", fmt.Sprint(len(body)))
			w.Write(body)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchtool:", err)
		os.Exit(1)
	}
	go func() { <-ctx.Done(); srv.Close() }()
	fmt.Fprintf(os.Stderr, "benchtool upstream on %s (%d-byte body)\n", ln.Addr(), len(body))
	srv.Serve(ln)
}

// -------------------------------------------------------------------- load

type result struct {
	URL       string        `json:"url"`
	Conns     int           `json:"connections"`
	Rate      float64       `json:"target_rate,omitempty"`
	Duration  float64       `json:"duration_s"`
	Requests  int64         `json:"requests"`
	Errors    int64         `json:"errors"`
	RPS       float64       `json:"rps"`
	MeanUs    float64       `json:"mean_us"`
	P50Us     float64       `json:"p50_us"`
	P90Us     float64       `json:"p90_us"`
	P99Us     float64       `json:"p99_us"`
	P999Us    float64       `json:"p999_us"`
	MaxUs     float64       `json:"max_us"`
	StatusMix map[int]int64 `json:"status,omitempty"`
}

func load(args []string) {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	base := fs.String("url", "http://127.0.0.1:9001", "base URL")
	conns := fs.Int("c", 64, "concurrent connections")
	dur := fs.Duration("d", 10*time.Second, "measured duration")
	warm := fs.Duration("warmup", 2*time.Second, "warm-up before measuring")
	rate := fs.Float64("rate", 0, "total requests per second (0 = closed loop, as fast as possible)")
	paths := fs.Int("paths", 1, "cycle through /item/1 .. /item/N")
	asJSON := fs.Bool("json", false, "print the result as one JSON line")
	fs.Parse(args)

	tr := &http.Transport{
		MaxIdleConns: *conns * 2, MaxIdleConnsPerHost: *conns * 2, MaxConnsPerHost: *conns,
		DisableCompression: true, IdleConnTimeout: 30 * time.Second,
		DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	urls := make([]string, *paths)
	for i := range urls {
		urls[i] = strings.TrimRight(*base, "/") + fmt.Sprintf("/item/%d", i+1)
	}

	var (
		measuring atomic.Bool
		stop      atomic.Bool
		wg        sync.WaitGroup
		samples   = make([][]int64, *conns)
		errs      atomic.Int64
		statusMu  sync.Mutex
		status    = map[int]int64{}
	)
	interval := time.Duration(0)
	if *rate > 0 {
		interval = time.Duration(float64(time.Second) * float64(*conns) / *rate)
	}
	for w := 0; w < *conns; w++ {
		w := w
		samples[w] = make([]int64, 0, 1<<16)
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := map[int]int64{}
			next := time.Now().Add(time.Duration(w) * interval / time.Duration(*conns))
			for i := w; !stop.Load(); i++ {
				if interval > 0 {
					if d := time.Until(next); d > 0 {
						time.Sleep(d)
					}
					next = next.Add(interval)
				}
				req, _ := http.NewRequest(http.MethodGet, urls[i%len(urls)], nil)
				t0 := time.Now()
				resp, err := client.Do(req)
				code := 0
				if err == nil {
					_, err = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					code = resp.StatusCode
				}
				d := time.Since(t0)
				if !measuring.Load() {
					continue
				}
				local[code]++
				if err != nil || code >= 500 || code == 0 {
					errs.Add(1)
				}
				samples[w] = append(samples[w], int64(d))
			}
			statusMu.Lock()
			for k, v := range local {
				status[k] += v
			}
			statusMu.Unlock()
		}()
	}
	time.Sleep(*warm)
	t0 := time.Now()
	measuring.Store(true)
	time.Sleep(*dur)
	measuring.Store(false)
	elapsed := time.Since(t0)
	stop.Store(true)
	wg.Wait()
	tr.CloseIdleConnections()

	var all []int64
	for _, s := range samples {
		all = append(all, s...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	res := result{URL: *base, Conns: *conns, Rate: *rate, Duration: elapsed.Seconds(), Requests: int64(len(all)), Errors: errs.Load(), StatusMix: status}
	if len(all) > 0 {
		var sum float64
		for _, v := range all {
			sum += float64(v)
		}
		q := func(p float64) float64 { return float64(all[min(len(all)-1, int(p*float64(len(all))))]) / 1e3 }
		res.RPS = float64(len(all)) / elapsed.Seconds()
		res.MeanUs, res.P50Us, res.P90Us, res.P99Us, res.P999Us, res.MaxUs = sum/float64(len(all))/1e3, q(0.5), q(0.9), q(0.99), q(0.999), float64(all[len(all)-1])/1e3
	}
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(res)
		return
	}
	fmt.Printf("%s  c=%d  %.0f req/s  (%d requests, %d errors in %.1fs)\n", res.URL, res.Conns, res.RPS, res.Requests, res.Errors, res.Duration)
	fmt.Printf("latency  mean %.0fus  p50 %.0fus  p90 %.0fus  p99 %.0fus  p99.9 %.0fus  max %.0fus\n", res.MeanUs, res.P50Us, res.P90Us, res.P99Us, res.P999Us, res.MaxUs)
}
