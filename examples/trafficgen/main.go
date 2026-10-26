// Command trafficgen generates realistic client traffic against the demo API
// (examples/demo-api), directly or through a Mirage service, to give record,
// anonymize and replay demos something to work with.
//
// Each simulated user runs sessions with its own cookie jar: it logs in
// (JSON or form), checks /api/me, browses products and searches, now and then
// looks at a user profile, and often places an order and reads it back. The
// users share one global request budget (-rps), so the arrival rate is
// steady however many users there are; with too few users or slow responses
// the achieved rate falls short, which the summary shows.
//
// Usage:
//
//	go run ./examples/trafficgen -target http://127.0.0.1:9001 -rps 20 -duration 30s -users 10
//
// One line is printed per second; a final summary follows (also on Ctrl-C).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---- latency histogram ----

// hist is a log-scale latency histogram with about 2% resolution, so the
// summary needs constant memory however long the run is.
type hist struct {
	counts [1200]int64
	n      int64
	max    time.Duration
}

const histBase = 1.02

func bucket(d time.Duration) int {
	us := float64(d) / float64(time.Microsecond)
	if us < 1 {
		return 0
	}
	return min(int(math.Log(us)/math.Log(histBase))+1, 1199)
}

func (h *hist) add(d time.Duration) {
	h.counts[bucket(d)]++
	h.n++
	h.max = max(h.max, d)
}

func (h *hist) quantile(q float64) time.Duration {
	if h.n == 0 {
		return 0
	}
	target := int64(math.Ceil(q * float64(h.n)))
	var seen int64
	for i, c := range h.counts {
		seen += c
		if seen >= target {
			if i == 0 {
				return time.Microsecond
			}
			return min(time.Duration(math.Pow(histBase, float64(i))*float64(time.Microsecond)), h.max)
		}
	}
	return h.max
}

// ---- statistics ----

type endpointStats struct {
	count, failed int64
	lat           hist
}

// stats collects results from every user.
type stats struct {
	mu       sync.Mutex
	second   hist  // latencies of the current reporting interval
	secOK    int64 // requests finished this interval
	secFail  int64
	total    hist
	statuses map[int]int64
	byLabel  map[string]*endpointStats
	ok, fail int64

	inflight atomic.Int64
	sessions atomic.Int64
	orders   atomic.Int64
}

func newStats() *stats {
	return &stats{statuses: map[int]int64{}, byLabel: map[string]*endpointStats{}}
}

func (s *stats) record(label string, status int, d time.Duration) {
	failed := status == 0 || status >= 500
	s.mu.Lock()
	defer s.mu.Unlock()
	s.second.add(d)
	s.total.add(d)
	s.statuses[status]++
	e := s.byLabel[label]
	if e == nil {
		e = &endpointStats{}
		s.byLabel[label] = e
	}
	e.count++
	e.lat.add(d)
	if failed {
		s.fail++
		s.secFail++
		e.failed++
	} else {
		s.ok++
		s.secOK++
	}
}

// ---- the simulated site ----

type site struct {
	target   *url.URL
	client   *http.Client // shared transport; per-user jars are set on copies
	tokens   <-chan struct{}
	timeout  time.Duration
	st       *stats
	emails   []string
	products []productInfo
	words    []string
}

type productInfo struct {
	ID       int
	Name     string
	Category string
}

// testCards are well-known test card numbers (Luhn valid).
var testCards = []string{"4242424242424242", "4111111111111111", "5555555555554444", "378282246310005", "6011111111111117"}

type simUser struct {
	site    *site
	rng     *rand.Rand
	client  *http.Client
	email   string
	useJSON bool
	token   string
}

// call waits for a request token, performs the request and records it. A nil
// error with status 0 never happens: transport failures return an error.
func (u *simUser) call(ctx context.Context, label, method, path string, body []byte, contentType string) (int, []byte, error) {
	select {
	case <-u.site.tokens:
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
	rctx, cancel := context.WithTimeout(ctx, u.site.timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, u.site.target.String()+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "mirage-trafficgen/1.0")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	u.site.st.inflight.Add(1)
	start := time.Now()
	resp, err := u.client.Do(req)
	var status int
	var data []byte
	if err == nil {
		status = resp.StatusCode
		data, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
	}
	u.site.st.inflight.Add(-1)
	if ctx.Err() != nil { // shutting down: do not count the interrupted request
		return 0, nil, ctx.Err()
	}
	u.site.st.record(label, status, time.Since(start))
	return status, data, err
}

func (u *simUser) getJSON(ctx context.Context, label, path string) (int, []byte, error) {
	return u.call(ctx, label, http.MethodGet, path, nil, "")
}

// session is one visit: log in, browse, maybe order, leave.
func (u *simUser) session(ctx context.Context) error {
	s := u.site
	jar, _ := cookiejar.New(nil)
	u.client = &http.Client{Transport: s.client.Transport, Jar: jar, Timeout: s.timeout + time.Second}
	u.token = ""
	s.st.sessions.Add(1)

	if u.rng.Float64() < 0.7 { // many visits start on the shop front
		if _, _, err := u.getJSON(ctx, "GET /api/products", "/api/products?page=1"); ctx.Err() != nil {
			return err
		}
	}

	var status int
	var data []byte
	var err error
	if u.useJSON {
		body, _ := json.Marshal(map[string]string{"email": u.email, "password": "password123"})
		status, data, err = u.call(ctx, "POST /api/login", http.MethodPost, "/api/login", body, "application/json")
	} else {
		form := url.Values{"email": {u.email}, "password": {"password123"}}
		status, data, err = u.call(ctx, "POST /api/login", http.MethodPost, "/api/login", []byte(form.Encode()), "application/x-www-form-urlencoded")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || status != http.StatusOK {
		return nil // a failed login ends the visit
	}
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(data, &login)
	u.token = login.Token

	if _, _, err := u.getJSON(ctx, "GET /api/me", "/api/me"); ctx.Err() != nil {
		return err
	}

	for i, n := 0, 3+u.rng.IntN(6); i < n; i++ {
		if err := u.browse(ctx); err != nil {
			return err
		}
	}

	if u.rng.Float64() < 0.4 {
		return u.checkout(ctx)
	}
	return nil
}

// browse does one random read.
func (u *simUser) browse(ctx context.Context) error {
	s := u.site
	var err error
	switch r := u.rng.Float64(); {
	case r < 0.30:
		p := s.products[u.rng.IntN(len(s.products))]
		_, _, err = u.getJSON(ctx, "GET /api/products/{id}", "/api/products/"+strconv.Itoa(p.ID))
	case r < 0.50:
		path := "/api/products?page=" + strconv.Itoa(1+u.rng.IntN(3))
		if u.rng.Float64() < 0.4 {
			path += "&category=" + s.products[u.rng.IntN(len(s.products))].Category
		}
		_, _, err = u.getJSON(ctx, "GET /api/products", path)
	case r < 0.75:
		w := s.words[u.rng.IntN(len(s.words))]
		_, _, err = u.getJSON(ctx, "GET /api/search", "/api/search?q="+url.QueryEscape(w))
	case r < 0.85:
		_, _, err = u.getJSON(ctx, "GET /api/users/{id}", "/api/users/"+strconv.Itoa(1+u.rng.IntN(200)))
	case r < 0.92:
		_, _, err = u.getJSON(ctx, "GET /api/users", "/api/users?page="+strconv.Itoa(1+u.rng.IntN(10)))
	default:
		_, _, err = u.getJSON(ctx, "GET /health", "/health")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_ = err
	return nil
}

func (u *simUser) checkout(ctx context.Context) error {
	s := u.site
	type item struct {
		ProductID int `json:"product_id"`
		Quantity  int `json:"quantity"`
	}
	items := make([]item, 1+u.rng.IntN(3))
	for i := range items {
		items[i] = item{s.products[u.rng.IntN(len(s.products))].ID, 1 + u.rng.IntN(4)}
	}
	body, _ := json.Marshal(map[string]any{
		"email": u.email, "card_number": testCards[u.rng.IntN(len(testCards))], "items": items,
	})
	status, data, err := u.call(ctx, "POST /api/orders", http.MethodPost, "/api/orders", body, "application/json")
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || status != http.StatusCreated {
		return nil
	}
	s.st.orders.Add(1)
	var o struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &o) == nil && o.ID != "" {
		if _, _, err := u.getJSON(ctx, "GET /api/orders/{uuid}", "/api/orders/"+o.ID); ctx.Err() != nil {
			return err
		}
	}
	return nil
}

func (u *simUser) run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := u.session(ctx); err != nil {
			return
		}
	}
}

// ---- pacing and bootstrap ----

// pace emits request tokens at rps per second until ctx ends. Credit is
// capped at a tenth of a second so a stall does not cause a burst.
func pace(ctx context.Context, rps float64) <-chan struct{} {
	ch := make(chan struct{}, max(1, int(rps/20)))
	go func() {
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		creditCap := math.Max(1, rps/10)
		var credit float64
		last := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				credit = math.Min(credit+now.Sub(last).Seconds()*rps, creditCap)
				last = now
				credit = emit(ch, credit)
			}
		}
	}()
	return ch
}

// emit sends whole tokens for the credit until the channel is full and
// returns the credit left.
func emit(ch chan<- struct{}, credit float64) float64 {
	for credit >= 1 {
		select {
		case ch <- struct{}{}:
			credit--
		default:
			return credit
		}
	}
	return credit
}

// bootstrap learns user emails and products from the API, the way a client
// would, with a few retries because the API injects errors.
func bootstrap(ctx context.Context, s *site, wantUsers int) error {
	get := func(path string, out any) error {
		var last error
		for try := 0; try < 5; try++ {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.target.String()+path, nil)
			resp, err := s.client.Do(req)
			if err == nil {
				body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
				resp.Body.Close()
				switch {
				case rerr != nil:
					err = rerr
				case resp.StatusCode != http.StatusOK:
					err = fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
				default:
					return json.Unmarshal(body, out)
				}
			}
			last = err
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		return last
	}

	seenEmail := map[string]bool{}
	for page := 1; len(s.emails) < wantUsers && page <= 10; page++ {
		var out struct {
			Users []struct {
				Email string `json:"email"`
			} `json:"users"`
		}
		if err := get("/api/users?page="+strconv.Itoa(page), &out); err != nil {
			return fmt.Errorf("cannot load users from %s: %w", s.target, err)
		}
		if len(out.Users) == 0 {
			break
		}
		for _, u := range out.Users {
			if !seenEmail[u.Email] && len(s.emails) < max(wantUsers, 1) {
				seenEmail[u.Email] = true
				s.emails = append(s.emails, u.Email)
			}
		}
	}
	if len(s.emails) == 0 {
		return errors.New("the API returned no users")
	}

	wordSet := map[string]bool{}
	for page := 1; page <= 5; page++ {
		var out struct {
			Products []struct {
				ID       int    `json:"id"`
				Name     string `json:"name"`
				Category string `json:"category"`
			} `json:"products"`
		}
		if err := get("/api/products?page="+strconv.Itoa(page), &out); err != nil {
			return fmt.Errorf("cannot load products from %s: %w", s.target, err)
		}
		if len(out.Products) == 0 {
			break
		}
		for _, p := range out.Products {
			s.products = append(s.products, productInfo{p.ID, p.Name, p.Category})
			for _, w := range strings.Fields(strings.ToLower(p.Name)) {
				if len(w) > 3 && !wordSet[w] {
					wordSet[w] = true
					s.words = append(s.words, w)
				}
			}
		}
	}
	if len(s.products) == 0 {
		return errors.New("the API returned no products")
	}
	sort.Strings(s.words)
	return nil
}

// ---- reporting ----

func ms(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.2fs", d.Seconds())
	case d >= 10*time.Millisecond:
		return fmt.Sprintf("%.0fms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
}

// tick prints and resets the per-interval counters.
func (s *stats) tick(elapsed time.Duration, interval time.Duration) {
	s.mu.Lock()
	h, ok, fail := s.second, s.secOK, s.secFail
	s.second, s.secOK, s.secFail = hist{}, 0, 0
	s.mu.Unlock()
	n := ok + fail
	rate := float64(n) / interval.Seconds()
	errPct := 0.0
	if n > 0 {
		errPct = 100 * float64(fail) / float64(n)
	}
	fmt.Printf("[%4ds] rps=%6.1f ok=%-5d err=%-4d (%4.1f%%) p50=%-7s p95=%-7s max=%-7s inflight=%d\n",
		int(elapsed.Round(time.Second).Seconds()), rate, ok, fail, errPct,
		ms(h.quantile(0.5)), ms(h.quantile(0.95)), ms(h.max), s.inflight.Load())
}

func (s *stats) summary(elapsed time.Duration, target float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := s.ok + s.fail
	fmt.Println()
	fmt.Println("=== summary ===")
	fmt.Printf("duration   %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("requests   %d  (%d ok, %d failed = %.2f%%)\n", total, s.ok, s.fail, pct(s.fail, total))
	fmt.Printf("rate       %.1f req/s achieved, %.1f requested\n", float64(total)/elapsed.Seconds(), target)
	fmt.Printf("sessions   %d started, %d orders placed\n", s.sessions.Load(), s.orders.Load())
	fmt.Printf("latency    p50=%s p90=%s p95=%s p99=%s max=%s\n",
		ms(s.total.quantile(0.5)), ms(s.total.quantile(0.9)), ms(s.total.quantile(0.95)), ms(s.total.quantile(0.99)), ms(s.total.max))

	codes := make([]int, 0, len(s.statuses))
	for c := range s.statuses {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	var parts []string
	for _, c := range codes {
		name := strconv.Itoa(c)
		if c == 0 {
			name = "net-err"
		}
		parts = append(parts, fmt.Sprintf("%s=%d", name, s.statuses[c]))
	}
	fmt.Printf("status     %s\n\n", strings.Join(parts, " "))

	labels := make([]string, 0, len(s.byLabel))
	for l := range s.byLabel {
		labels = append(labels, l)
	}
	sort.Slice(labels, func(i, j int) bool { return s.byLabel[labels[i]].count > s.byLabel[labels[j]].count })
	fmt.Printf("%-26s %8s %7s %9s %9s\n", "endpoint", "count", "failed", "p50", "p95")
	for _, l := range labels {
		e := s.byLabel[l]
		fmt.Printf("%-26s %8d %7d %9s %9s\n", l, e.count, e.failed, ms(e.lat.quantile(0.5)), ms(e.lat.quantile(0.95)))
	}
}

func pct(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func main() {
	target := flag.String("target", "http://127.0.0.1:9001", "base URL of the API (the demo API or a Mirage service)")
	rps := flag.Float64("rps", 20, "total request rate across all users")
	duration := flag.Duration("duration", 30*time.Second, "how long to run; 0 runs until interrupted")
	users := flag.Int("users", 10, "number of simulated users (each with its own session and cookies)")
	timeout := flag.Duration("timeout", 10*time.Second, "per-request timeout")
	seed := flag.Uint64("seed", uint64(time.Now().UnixNano()), "random seed (default: time based)")
	flag.Parse()

	u, err := url.Parse(strings.TrimRight(*target, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fmt.Fprintf(os.Stderr, "trafficgen: -target %q must be an http(s) URL\n", *target)
		os.Exit(2)
	}
	if *rps <= 0 || *users < 1 || *duration < 0 || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "trafficgen: -rps and -timeout must be positive, -users at least 1, -duration not negative")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        *users * 2,
		MaxIdleConnsPerHost: *users * 2,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	sx := &site{target: u, client: &http.Client{Transport: transport, Timeout: *timeout}, timeout: *timeout, st: newStats()}
	fmt.Printf("trafficgen: bootstrapping from %s\n", u)
	if err := bootstrap(ctx, sx, *users); err != nil {
		fmt.Fprintln(os.Stderr, "trafficgen:", err)
		os.Exit(1)
	}

	runCtx := ctx
	if *duration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	sx.tokens = pace(runCtx, *rps)

	fmt.Printf("trafficgen: %d users, %.1f req/s, %s against %s\n", *users, *rps, durationLabel(*duration), u)
	var wg sync.WaitGroup
	for i := 0; i < *users; i++ {
		su := &simUser{
			site:    sx,
			rng:     rand.New(rand.NewPCG(*seed, uint64(i)+1)),
			email:   sx.emails[i%len(sx.emails)],
			useJSON: i%3 != 2,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			su.run(runCtx)
		}()
	}

	start := time.Now()
	const interval = time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
loop:
	for {
		select {
		case <-ticker.C:
			sx.st.tick(time.Since(start), interval)
		case <-done:
			break loop
		}
	}
	sx.st.summary(time.Since(start), *rps)
	transport.CloseIdleConnections()
}

func durationLabel(d time.Duration) string {
	if d == 0 {
		return "until interrupted"
	}
	return "for " + d.String()
}
