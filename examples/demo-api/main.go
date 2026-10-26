// Command demo-api is a small fake e-commerce JSON API to record traffic
// against. It has deterministic seeded data, a realistic long-tailed latency
// distribution, random 5xx errors, JWT-shaped tokens, session cookies and
// PII-looking fields (names, emails, phones, addresses, card numbers), which
// makes it a good target for demonstrating Mirage's record, anonymize,
// virtualize and replay features.
//
// Usage:
//
//	go run ./examples/demo-api -addr :7001 -latency 25ms -error-rate 0.02 -v
//
// Endpoints (every demo user's password is "password123"):
//
//	GET  /health
//	GET  /api/users?page=N            paged list of users
//	GET  /api/users/{id}              one user (name, email, phone, address, last4, created_at)
//	GET  /api/products?category=&page=N
//	GET  /api/products/{id}
//	GET  /api/search?q=text           product search
//	POST /api/login                   JSON {"email","password"} or a form; returns a token and sets a session cookie
//	GET  /api/me                      the logged-in user (Authorization: Bearer <token>)
//	POST /api/orders                  JSON {"email","card_number","items":[{"product_id","quantity"}]}
//	GET  /api/orders/{uuid}
package main

import (
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	numUsers    = 200
	numProducts = 60
	pageSize    = 20
	password    = "password123"
	maxOrders   = 50000
)

// ---- data ----

type address struct {
	Street string `json:"street"`
	City   string `json:"city"`
	State  string `json:"state"`
	Zip    string `json:"zip"`
}

type user struct {
	ID        int     `json:"id"`
	Name      string  `json:"name"`
	Email     string  `json:"email"`
	Phone     string  `json:"phone"`
	Address   address `json:"address"`
	Last4     string  `json:"last4"`
	CreatedAt string  `json:"created_at"`
}

type product struct {
	ID         int     `json:"id"`
	SKU        string  `json:"sku"`
	Name       string  `json:"name"`
	Category   string  `json:"category"`
	PriceCents int     `json:"price_cents"`
	Stock      int     `json:"stock"`
	Rating     float64 `json:"rating"`
}

type orderItem struct {
	ProductID      int    `json:"product_id"`
	Name           string `json:"name"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int    `json:"unit_price_cents"`
	LineTotalCents int    `json:"line_total_cents"`
}

type order struct {
	ID            string      `json:"id"`
	Status        string      `json:"status"`
	Email         string      `json:"email"`
	CardLast4     string      `json:"card_last4"`
	Items         []orderItem `json:"items"`
	SubtotalCents int         `json:"subtotal_cents"`
	TaxCents      int         `json:"tax_cents"`
	ShippingCents int         `json:"shipping_cents"`
	TotalCents    int         `json:"total_cents"`
	Currency      string      `json:"currency"`
	CreatedAt     string      `json:"created_at"`
}

var (
	firstNames = []string{"Olivia", "Liam", "Emma", "Noah", "Ava", "Elijah", "Sophia", "Lucas", "Mia", "Mason", "Amelia", "Ethan", "Harper", "Logan", "Evelyn", "Jackson", "Abigail", "Levi", "Ella", "Mateo", "Scarlett", "Henry", "Grace", "Owen", "Chloe", "Wyatt", "Nora", "Caleb", "Hazel", "Isaac"}
	lastNames  = []string{"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis", "Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez", "Wilson", "Anderson", "Thomas", "Taylor", "Moore", "Jackson", "Martin", "Lee", "Perez", "Thompson", "White", "Harris", "Sanchez", "Clark", "Ramirez", "Lewis", "Robinson"}
	streets    = []string{"Maple St", "Oak Ave", "Pine Rd", "Cedar Ln", "Elm St", "Willow Way", "Birch Blvd", "Orchard Dr", "Meadow Ct", "Ridge Rd", "Lakeview Dr", "Sunset Ave"}
	cities     = []struct{ city, state, zip string }{
		{"Portland", "OR", "972"}, {"Vancouver", "WA", "986"}, {"Seattle", "WA", "981"}, {"Salem", "OR", "973"},
		{"Boise", "ID", "837"}, {"Spokane", "WA", "992"}, {"Eugene", "OR", "974"}, {"Tacoma", "WA", "984"},
	}
	adjectives = []string{"Organic", "Rustic", "Heritage", "Wild", "Smoked", "Cold-Pressed", "Hand-Picked", "Small-Batch", "Raw", "Pasture-Raised"}
	nouns      = []struct{ name, category string }{
		{"Honey", "pantry"}, {"Maple Syrup", "pantry"}, {"Chili Oil", "pantry"}, {"Strawberry Jam", "pantry"},
		{"Goat Cheese", "dairy"}, {"Butter", "dairy"}, {"Yogurt", "dairy"}, {"Cream", "dairy"},
		{"Sourdough Loaf", "bakery"}, {"Rye Bread", "bakery"}, {"Cinnamon Rolls", "bakery"},
		{"Heirloom Tomatoes", "produce"}, {"Kale", "produce"}, {"Carrots", "produce"}, {"Apples", "produce"},
		{"Apple Cider", "drinks"}, {"Cold Brew", "drinks"}, {"Kombucha", "drinks"},
	}
)

// catalog is the seeded, read-only data plus the orders placed so far.
type catalog struct {
	users    []user
	products []product
	byEmail  map[string]*user
	secret   []byte

	mu        sync.Mutex
	orders    map[string]*order
	orderIDs  []string // insertion order, for eviction
	idRNG     *rand.Rand
	startTime time.Time
}

func newCatalog(seed uint64) *catalog {
	rng := rand.New(rand.NewPCG(seed, 0x6d697261676521))
	c := &catalog{
		byEmail:   map[string]*user{},
		orders:    map[string]*order{},
		idRNG:     rand.New(rand.NewPCG(seed, 0x6f72646572)),
		startTime: time.Now(),
	}
	secret := sha256.Sum256([]byte("demo-api-secret-" + strconv.FormatUint(seed, 10)))
	c.secret = secret[:]

	base := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	c.users = make([]user, numUsers)
	for i := range c.users {
		first, last := pick(rng, firstNames), pick(rng, lastNames)
		city := cities[rng.IntN(len(cities))]
		u := user{
			ID:    i + 1,
			Name:  first + " " + last,
			Email: fmt.Sprintf("%s.%s%d@example.com", strings.ToLower(first), strings.ToLower(last), 10+rng.IntN(90)),
			Phone: fmt.Sprintf("(%d) 555-%04d", 200+rng.IntN(800), rng.IntN(10000)),
			Address: address{
				Street: fmt.Sprintf("%d %s", 10+rng.IntN(9900), pick(rng, streets)),
				City:   city.city, State: city.state, Zip: fmt.Sprintf("%s%02d", city.zip, rng.IntN(100)),
			},
			Last4:     fmt.Sprintf("%04d", rng.IntN(10000)),
			CreatedAt: base.Add(time.Duration(rng.IntN(900*24)) * time.Hour).Format(time.RFC3339),
		}
		for { // keep emails unique
			if _, dup := c.byEmail[u.Email]; !dup {
				break
			}
			u.Email = fmt.Sprintf("%s%d@example.com", strings.TrimSuffix(u.Email, "@example.com"), rng.IntN(10))
		}
		c.users[i] = u
	}
	for i := range c.users {
		c.byEmail[c.users[i].Email] = &c.users[i]
	}

	c.products = make([]product, numProducts)
	for i := range c.products {
		n := nouns[rng.IntN(len(nouns))]
		c.products[i] = product{
			ID:         i + 1,
			SKU:        fmt.Sprintf("SKU-%05d", 10000+rng.IntN(90000)),
			Name:       pick(rng, adjectives) + " " + n.name,
			Category:   n.category,
			PriceCents: 199 + rng.IntN(4800),
			Stock:      rng.IntN(500),
			Rating:     math.Round((3+rng.Float64()*2)*10) / 10,
		}
	}
	return c
}

func pick(rng *rand.Rand, s []string) string { return s[rng.IntN(len(s))] }

// newUUID returns a random version 4 UUID from the seeded generator.
func (c *catalog) newUUID() string {
	var b [16]byte
	c.mu.Lock()
	for i := range b {
		b[i] = byte(c.idRNG.Uint32())
	}
	c.mu.Unlock()
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// ---- tokens ----

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// issueToken returns a JWT-shaped (HS256) token for the user.
func (c *catalog) issueToken(u *user, now time.Time) string {
	head := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"sub": "u_" + strconv.Itoa(u.ID), "email": u.Email, "name": u.Name,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	signing := head + "." + b64(claims)
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(signing))
	return signing + "." + b64(mac.Sum(nil))
}

// userFromToken verifies a bearer token and returns its user.
func (c *catalog) userFromToken(tok string, now time.Time) (*user, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, false
	}
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal([]byte(b64(mac.Sum(nil))), []byte(parts[2])) {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims struct {
		Email string `json:"email"`
		Exp   int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp < now.Unix() {
		return nil, false
	}
	u, ok := c.byEmail[claims.Email]
	return u, ok
}

// ---- handlers ----

type api struct {
	c *catalog
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"encode failed"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)+1))
	w.WriteHeader(status)
	w.Write(append(body, '\n'))
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "status": status})
}

func page(r *http.Request) (int, bool) {
	s := r.URL.Query().Get("page")
	if s == "" {
		return 1, true
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 1
}

// paged slices a list for a 1-based page.
func paged[T any](all []T, n int) []T {
	lo := (n - 1) * pageSize
	if lo >= len(all) {
		return []T{}
	}
	return all[lo:min(lo+pageSize, len(all))]
}

func (a *api) listUsers(w http.ResponseWriter, r *http.Request) {
	n, ok := page(r)
	if !ok {
		apiError(w, http.StatusBadRequest, "page must be a positive integer")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"page": n, "per_page": pageSize, "total": len(a.c.users), "users": paged(a.c.users, n),
	})
}

func (a *api) getUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 || id > len(a.c.users) {
		apiError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, a.c.users[id-1])
}

func (a *api) listProducts(w http.ResponseWriter, r *http.Request) {
	n, ok := page(r)
	if !ok {
		apiError(w, http.StatusBadRequest, "page must be a positive integer")
		return
	}
	list := a.c.products
	if cat := r.URL.Query().Get("category"); cat != "" {
		list = nil
		for _, p := range a.c.products {
			if p.Category == cat {
				list = append(list, p)
			}
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{
		"page": n, "per_page": pageSize, "total": len(list), "products": paged(list, n),
	})
}

func (a *api) getProduct(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 || id > len(a.c.products) {
		apiError(w, http.StatusNotFound, "product not found")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, a.c.products[id-1])
}

func (a *api) search(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		apiError(w, http.StatusBadRequest, "q is required")
		return
	}
	hits := []product{}
	for _, p := range a.c.products {
		if strings.Contains(strings.ToLower(p.Name), q) || strings.Contains(p.Category, q) {
			hits = append(hits, p)
		}
	}
	total := len(hits)
	if len(hits) > 10 {
		hits = hits[:10]
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": q, "total": total, "products": hits})
}

func (a *api) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var email, pass string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var in struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			apiError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		email, pass = in.Email, in.Password
	} else {
		if err := r.ParseForm(); err != nil {
			apiError(w, http.StatusBadRequest, "invalid form body")
			return
		}
		email, pass = r.PostForm.Get("email"), r.PostForm.Get("password")
	}
	u, ok := a.c.byEmail[strings.ToLower(strings.TrimSpace(email))]
	if !ok || pass != password {
		apiError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	session := make([]byte, 16)
	for i := range session {
		session[i] = byte(rand.Uint32())
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: hex.EncodeToString(session), Path: "/", MaxAge: 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{
		"token": a.c.issueToken(u, time.Now()), "token_type": "Bearer", "expires_in": 3600,
		"user": map[string]any{"id": u.ID, "name": u.Name, "email": u.Email},
	})
}

func (a *api) me(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="demo"`)
		apiError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	u, ok := a.c.userFromToken(tok, time.Now())
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="demo", error="invalid_token"`)
		apiError(w, http.StatusUnauthorized, "invalid or expired token")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

type orderRequest struct {
	Email      string `json:"email"`
	CardNumber string `json:"card_number"`
	Items      []struct {
		ProductID int `json:"product_id"`
		Quantity  int `json:"quantity"`
	} `json:"items"`
}

func (a *api) createOrder(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var in orderRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	card := strings.NewReplacer(" ", "", "-", "").Replace(in.CardNumber)
	switch {
	case !strings.Contains(in.Email, "@"):
		apiError(w, http.StatusUnprocessableEntity, "email is required")
		return
	case len(card) < 13 || len(card) > 19 || strings.Trim(card, "0123456789") != "":
		apiError(w, http.StatusUnprocessableEntity, "card_number must have 13 to 19 digits")
		return
	case len(in.Items) == 0 || len(in.Items) > 20:
		apiError(w, http.StatusUnprocessableEntity, "items must have 1 to 20 entries")
		return
	}
	o := &order{
		ID: a.c.newUUID(), Status: "confirmed", Email: in.Email, CardLast4: card[len(card)-4:],
		Currency: "USD", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	for _, it := range in.Items {
		if it.ProductID < 1 || it.ProductID > len(a.c.products) {
			apiError(w, http.StatusUnprocessableEntity, fmt.Sprintf("unknown product_id %d", it.ProductID))
			return
		}
		if it.Quantity < 1 || it.Quantity > 20 {
			apiError(w, http.StatusUnprocessableEntity, "quantity must be 1 to 20")
			return
		}
		p := a.c.products[it.ProductID-1]
		line := p.PriceCents * it.Quantity
		o.Items = append(o.Items, orderItem{p.ID, p.Name, it.Quantity, p.PriceCents, line})
		o.SubtotalCents += line
	}
	o.TaxCents = o.SubtotalCents * 8 / 100
	if o.SubtotalCents < 5000 {
		o.ShippingCents = 599
	}
	o.TotalCents = o.SubtotalCents + o.TaxCents + o.ShippingCents

	a.c.mu.Lock()
	a.c.orders[o.ID] = o
	a.c.orderIDs = append(a.c.orderIDs, o.ID)
	if len(a.c.orderIDs) > maxOrders {
		delete(a.c.orders, a.c.orderIDs[0])
		a.c.orderIDs = a.c.orderIDs[1:]
	}
	a.c.mu.Unlock()

	w.Header().Set("Location", "/api/orders/"+o.ID)
	writeJSON(w, http.StatusCreated, o)
}

func (a *api) getOrder(w http.ResponseWriter, r *http.Request) {
	a.c.mu.Lock()
	o, ok := a.c.orders[r.PathValue("uuid")]
	a.c.mu.Unlock()
	if !ok {
		apiError(w, http.StatusNotFound, "order not found")
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (a *api) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "uptime_s": math.Round(time.Since(a.c.startTime).Seconds()*10) / 10,
	})
}

func (a *api) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.health)
	mux.HandleFunc("GET /api/users", a.listUsers)
	mux.HandleFunc("GET /api/users/{id}", a.getUser)
	mux.HandleFunc("GET /api/products", a.listProducts)
	mux.HandleFunc("GET /api/products/{id}", a.getProduct)
	mux.HandleFunc("GET /api/search", a.search)
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("GET /api/me", a.me)
	mux.HandleFunc("POST /api/orders", a.createOrder)
	mux.HandleFunc("GET /api/orders/{uuid}", a.getOrder)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apiError(w, http.StatusNotFound, "no such endpoint")
	})
	return mux
}

// ---- chaos: latency and errors ----

// chaos delays requests with a long-tailed distribution and fails a fraction
// of them with 503 (mostly) or 500.
type chaos struct {
	median    time.Duration
	errorRate float64

	mu  sync.Mutex
	rng *rand.Rand
}

// scale makes some endpoints slower than others.
func scale(r *http.Request) float64 {
	switch {
	case r.URL.Path == "/health":
		return 0
	case r.URL.Path == "/api/search":
		return 2
	case r.URL.Path == "/api/login":
		return 1.5
	case r.Method == http.MethodPost && r.URL.Path == "/api/orders":
		return 3
	}
	return 1
}

// sample draws a latency (log-normal around the median, 1% slow outliers)
// and whether the request should fail.
func (c *chaos) sample(scale float64) (delay time.Duration, failStatus int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	x := float64(c.median) * scale * math.Exp(0.55*c.rng.NormFloat64())
	if c.rng.Float64() < 0.01 {
		x *= 4 + c.rng.Float64()*6
	}
	delay = time.Duration(min(x, float64(10*time.Second)))
	if scale > 0 && c.rng.Float64() < c.errorRate {
		failStatus = http.StatusServiceUnavailable
		if c.rng.Float64() < 0.2 {
			failStatus = http.StatusInternalServerError
		}
	}
	return delay, failStatus
}

func (c *chaos) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := scale(r)
		if s == 0 {
			next.ServeHTTP(w, r)
			return
		}
		delay, fail := c.sample(s)
		if c.median > 0 {
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-r.Context().Done():
				t.Stop()
				return
			}
		}
		switch fail {
		case http.StatusServiceUnavailable:
			w.Header().Set("Retry-After", "1")
			apiError(w, fail, "service temporarily unavailable")
		case http.StatusInternalServerError:
			apiError(w, fail, "internal error")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// ---- gzip, request ids and logging ----

var gzPool = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

// gzipWriter compresses JSON responses when the client accepts gzip.
type gzipWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	h := g.Header()
	if n, _ := strconv.Atoi(h.Get("Content-Length")); n >= 256 && h.Get("Content-Encoding") == "" &&
		strings.HasPrefix(h.Get("Content-Type"), "application/json") {
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		g.gz = gzPool.Get().(*gzip.Writer)
		g.gz.Reset(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		return g.gz.Write(p)
	}
	return g.ResponseWriter.Write(p)
}

func (g *gzipWriter) close() {
	if g.gz != nil {
		g.gz.Close()
		gzPool.Put(g.gz)
		g.gz = nil
	}
}

func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		g := &gzipWriter{ResponseWriter: w}
		defer g.close()
		next.ServeHTTP(g, r)
	})
}

// statusWriter records the status and size of what goes on the wire.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(p)
	s.bytes += n
	return n, err
}

var reqSeq = struct {
	sync.Mutex
	n uint64
}{}

func withLogging(logger *log.Logger, verbose bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqSeq.Lock()
		reqSeq.n++
		id := fmt.Sprintf("req-%08x", reqSeq.n)
		reqSeq.Unlock()
		w.Header().Set("X-Request-Id", id)
		w.Header().Set("Server", "demo-api/1.0")
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if verbose {
			uri := r.URL.RequestURI()
			logger.Printf("%s %s %d %dB %s", r.Method, uri, sw.status, sw.bytes, time.Since(start).Round(100*time.Microsecond))
		}
	})
}

func main() {
	addr := flag.String("addr", ":7001", "listen address")
	latency := flag.Duration("latency", 25*time.Millisecond, "median response latency (long-tailed); 0 disables")
	errorRate := flag.Float64("error-rate", 0.02, "fraction of requests answered with 503 or 500 (0..1)")
	seed := flag.Uint64("seed", 1, "seed for the generated data and the latency/error randomness")
	verbose := flag.Bool("v", false, "log one line per request")
	flag.Parse()

	if *errorRate < 0 || *errorRate > 1 || *latency < 0 {
		fmt.Fprintln(os.Stderr, "demo-api: -error-rate must be within 0..1 and -latency must not be negative")
		os.Exit(2)
	}
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)

	a := &api{c: newCatalog(*seed)}
	ch := &chaos{median: *latency, errorRate: *errorRate, rng: rand.New(rand.NewPCG(*seed, 0x63686171))}
	handler := withLogging(logger, *verbose, withGzip(ch.wrap(a.routes())))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	logger.Printf("demo-api listening on %s (seed=%d, median latency=%s, error rate=%.1f%%, %d users, %d products)",
		*addr, *seed, *latency, *errorRate*100, numUsers, numProducts)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal(err)
	}
	logger.Print("demo-api stopped")
}
