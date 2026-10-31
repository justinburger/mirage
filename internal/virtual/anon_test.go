package virtual

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/bodycodec"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
)

const anonTestSecret = "virtual-anon-test-secret"

// newAnonymizer returns a real engine over the default profile with a fixed
// secret, like the one a service with anonymize_capture on uses.
func newAnonymizer(t testing.TB) *anonymize.Engine {
	t.Helper()
	p := model.DefaultAnonymizationProfile()
	p.Secret = anonTestSecret
	a, err := anonymize.New(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// captureAnon stores ex the way the proxy's capture pipeline does: bodies are
// decoded, the exchange is anonymized, and the endpoint is the one computed
// from the raw path and the anonymized path.
func captureAnon(t testing.TB, a *anonymize.Engine, n *pathnorm.Normalizer, ex *model.Exchange) *model.Exchange {
	t.Helper()
	rawPath := ex.Request.Path
	bodycodec.PrepareForAnonymization(ex, 1<<20)
	if err := a.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	ex.Endpoint = n.AnonymizedEndpoint(ex.Request.Method, rawPath, ex.Request.Path)
	return ex
}

// storedRequest is what an anonymized exchange holds of its request, copied
// before the engine takes ownership.
type storedRequest struct {
	method, path, rawQuery string
	body                   []byte
	header                 http.Header
}

func storedOf(ex *model.Exchange) storedRequest {
	return storedRequest{
		method: ex.Request.Method, path: ex.Request.Path, rawQuery: ex.Request.RawQuery,
		body: bytes.Clone(ex.Request.Body), header: ex.Request.Header.Clone(),
	}
}

func (s storedRequest) request() *http.Request {
	target := s.path
	if s.rawQuery != "" {
		target += "?" + s.rawQuery
	}
	r := httptest.NewRequest(s.method, target, nil)
	for k, v := range s.header {
		r.Header[k] = v
	}
	return r
}

// anonCorpus is a small recording of two users, captured with anonymization.
type anonCorpus struct {
	norm   *pathnorm.Normalizer
	anon   *anonymize.Engine
	exs    []*model.Exchange
	stored map[string]storedRequest // by response body
}

func jsonEx(seq int64, method, target, reqBody, resp string, hdr ...string) *model.Exchange {
	ex := mkEx(seq, method, target, 200, resp, withReqHeader("Content-Type", "application/json"))
	if reqBody != "" {
		withReqBody(reqBody)(ex)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		withReqHeader(hdr[i], hdr[i+1])(ex)
	}
	return ex
}

func newAnonCorpus(t testing.TB, extra ...*model.Exchange) *anonCorpus {
	t.Helper()
	c := &anonCorpus{norm: pathnorm.Default, anon: newAnonymizer(t), stored: map[string]storedRequest{}}
	raw := []*model.Exchange{
		jsonEx(1, "GET", "/users/alice@example.com/profile", "", "profile-alice"),
		jsonEx(2, "GET", "/users/bob@example.com/profile", "", "profile-bob"),
		jsonEx(3, "GET", "/search?email=alice%40example.com&card=4111111111111111&page=1", "", "search-alice"),
		jsonEx(4, "GET", "/search?email=bob%40example.com&card=5500005555555559&page=1", "", "search-bob"),
		jsonEx(5, "POST", "/orders", `{"email":"alice@example.com","card":"4111 1111 1111 1111","sku":"A"}`, "order-alice"),
		jsonEx(6, "POST", "/orders", `{"email":"bob@example.com","card":"5500 0055 5555 5559","sku":"A"}`, "order-bob"),
	}
	raw = append(raw, extra...)
	for _, ex := range raw {
		captureAnon(t, c.anon, c.norm, ex)
		c.stored[string(ex.Response.Body)] = storedOf(ex)
		c.exs = append(c.exs, ex)
	}
	return c
}

// engine indexes the corpus. withAnon sets Options.Anonymizer.
func (c *anonCorpus) engine(t testing.TB, o Options, withAnon bool) *Engine {
	t.Helper()
	if withAnon {
		o.Anonymizer = c.anon
	}
	o.Normalizer = c.norm
	e := NewEngine(nil, noLatency(o))
	for _, ex := range c.exs {
		cp := *ex // the engine owns what it is given
		e.Add(&cp)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// rawLookup looks up a raw request carrying body (and a JSON content type).
func rawLookup(e *Engine, method, target, body string, hdr ...string) (level, resp string) {
	r := mkReq(method, target, append([]string{"Content-Type", "application/json"}, hdr...)...)
	m := e.Lookup(r, []byte(body))
	if m == nil {
		return LevelMiss, ""
	}
	return m.Level, string(m.Exchange.Response.Body)
}

// The anonymized recording holds none of the raw values.
func TestAnonCorpusIsAnonymized(t *testing.T) {
	c := newAnonCorpus(t)
	for _, ex := range c.exs {
		all := ex.Request.Path + "?" + ex.Request.RawQuery + string(ex.Request.Body) + ex.Endpoint
		for _, pii := range []string{"alice@example.com", "bob@example.com", "alice%40example.com", "4111", "5500"} {
			if strings.Contains(all, pii) {
				t.Fatalf("%s %s keeps %q: %s", ex.Request.Method, ex.Request.Path, pii, all)
			}
		}
	}
}

// The raw requests of a client carry emails and card numbers in the path, the
// query and the body. With the recording's anonymizer they match the exchange
// of their own user at the exact level; without it they do not.
func TestAnonymizerMatchesRawRequestsExactly(t *testing.T) {
	c := newAnonCorpus(t)
	with := c.engine(t, Options{Match: model.MatchConfig{Body: model.BodyJSON}}, true)
	without := c.engine(t, Options{Match: model.MatchConfig{Body: model.BodyJSON}}, false)

	cases := []struct {
		name, method, target, body, want string
	}{
		{"path alice", "GET", "/users/alice@example.com/profile", "", "profile-alice"},
		{"path bob", "GET", "/users/bob@example.com/profile", "", "profile-bob"},
		{"path escaped", "GET", "/users/alice%40example.com/profile", "", "profile-alice"},
		{"query alice", "GET", "/search?email=alice%40example.com&card=4111111111111111&page=1", "", "search-alice"},
		{"query reordered", "GET", "/search?page=1&card=4111111111111111&email=alice%40example.com", "", "search-alice"},
		{"query bob", "GET", "/search?email=bob%40example.com&card=5500005555555559&page=1", "", "search-bob"},
		{"body alice", "POST", "/orders", `{"email":"alice@example.com","card":"4111 1111 1111 1111","sku":"A"}`, "order-alice"},
		{"body reformatted", "POST", "/orders", `{ "sku": "A", "card": "4111 1111 1111 1111", "email": "alice@example.com" }`, "order-alice"},
		{"body bob", "POST", "/orders", `{"email":"bob@example.com","card":"5500 0055 5555 5559","sku":"A"}`, "order-bob"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			level, got := rawLookup(with, tc.method, tc.target, tc.body)
			if level != LevelExact || got != tc.want {
				t.Errorf("with anonymizer: level=%s resp=%q, want exact %q", level, got, tc.want)
			}
			// Without the anonymizer the raw request never matches its own
			// exchange at the exact level (it misses, or falls back to a
			// path-level match that may belong to another user).
			if level, got := rawLookup(without, tc.method, tc.target, tc.body); level == LevelExact {
				t.Errorf("without anonymizer: exact match %q (the recording is anonymized)", got)
			}
		})
	}
	if s := with.Stats(); s.ByLevel[LevelExact] != int64(len(cases)) || s.Misses != 0 {
		t.Errorf("stats = %+v", s)
	}
}

// The response a PII-bearing request gets is its own user's, never another's:
// a POST whose path has no PII used to match the path level of whichever
// exchange came first.
func TestAnonymizerNeverServesAnotherUsersResponse(t *testing.T) {
	c := newAnonCorpus(t)
	e := c.engine(t, Options{Match: model.MatchConfig{Body: model.BodyJSON, Selection: model.SelectFirst}}, true)
	for range 5 {
		if _, got := rawLookup(e, "POST", "/orders", `{"email":"bob@example.com","card":"5500 0055 5555 5559","sku":"A"}`); got != "order-bob" {
			t.Fatalf("bob's order got %q", got)
		}
		if _, got := rawLookup(e, "POST", "/orders", `{"email":"alice@example.com","card":"4111 1111 1111 1111","sku":"A"}`); got != "order-alice" {
			t.Fatalf("alice's order got %q", got)
		}
	}
	// Not recorded: never an exact match (the path level is the documented
	// fallback for an unrecorded body).
	if level, got := rawLookup(e, "POST", "/orders", `{"email":"carol@example.com","card":"4111 1111 1111 1111","sku":"B"}`); level == LevelExact {
		t.Fatalf("carol matched exactly: %q", got)
	}
}

// A replay sends the anonymized values of the recording. Anonymizing them
// again changes them, so the request as received must be tried as well.
func TestAnonymizerStillMatchesAnonymizedRequests(t *testing.T) {
	c := newAnonCorpus(t)
	e := c.engine(t, Options{Match: model.MatchConfig{Body: model.BodyJSON}}, true)
	for want, s := range c.stored {
		r := s.request()
		m := e.Lookup(r, s.body)
		if m == nil || m.Level != LevelExact || string(m.Exchange.Response.Body) != want {
			t.Errorf("replay of %q: match = %+v", want, m)
		}
	}
	if s := e.Stats(); s.ByLevel[LevelExact] != int64(len(c.stored)) {
		t.Errorf("stats = %+v", s)
	}
}

// A request that was never recorded stays a miss.
func TestAnonymizerMissStaysMiss(t *testing.T) {
	c := newAnonCorpus(t)
	e := c.engine(t, Options{Match: model.MatchConfig{Body: model.BodyJSON}}, true)
	for _, target := range []string{
		"/users/carol@example.com/profile",
		"/accounts/carol@example.com/cards/4111111111111111",
		"/nothing/here",
	} {
		if level, got := rawLookup(e, "GET", target, ""); level != LevelMiss {
			t.Errorf("GET %s: level=%s resp=%q, want a miss", target, level, got)
		}
	}
	// The path level ignores the query, as without an anonymizer: a /search
	// for someone else is never an exact match, though.
	if level, _ := rawLookup(e, "GET", "/search?email=carol%40example.com&card=4111111111111111&page=1", ""); level == LevelExact {
		t.Error("an unrecorded search matched exactly")
	}
	rec := httptest.NewRecorder()
	if e.Serve(rec, mkReq("GET", "/users/carol@example.com/profile"), nil) {
		t.Error("Serve answered a miss")
	}
	if s := e.Stats(); s.Misses != 4 || s.Hits != 1 {
		t.Errorf("stats = %+v, want 4 misses and the path-level hit", s)
	}
}

// The path level and the template level work on the anonymized form too, and
// the template level uses the endpoint the capture stored.
func TestAnonymizerPathAndTemplateLevels(t *testing.T) {
	c := newAnonCorpus(t)
	e := c.engine(t, Options{Match: model.MatchConfig{PathTemplates: true, Body: model.BodyJSON, Selection: model.SelectFirst}}, true)

	// Same user, other query: the anonymized path matches at the path level.
	if level, got := rawLookup(e, "GET", "/users/alice@example.com/profile?extra=1", ""); level != LevelPath || got != "profile-alice" {
		t.Errorf("path: level=%s resp=%q, want path profile-alice", level, got)
	}
	// An unrecorded user: the endpoint /users/{email}/profile of the capture.
	if level, got := rawLookup(e, "GET", "/users/dave@example.com/profile", ""); level != LevelTemplate || !strings.HasPrefix(got, "profile-") {
		t.Errorf("template: level=%s resp=%q, want template profile-*", level, got)
	}
	// Exact still wins over the broad template.
	if level, got := rawLookup(e, "GET", "/users/bob@example.com/profile", ""); level != LevelExact || got != "profile-bob" {
		t.Errorf("exact: level=%s resp=%q", level, got)
	}
	// A replay of an anonymized path is exact (the request as received), not
	// the template match the anonymized form would give.
	s := c.stored["profile-bob"]
	if m := e.Lookup(s.request(), nil); m == nil || m.Level != LevelExact || string(m.Exchange.Response.Body) != "profile-bob" {
		t.Errorf("replay: %+v", m)
	}
}

// A configured endpoint pattern is kept as the template of an anonymized
// exchange, and lookups find it through the same endpoint.
func TestAnonymizerTemplateWithConfiguredPattern(t *testing.T) {
	n, err := pathnorm.New([]string{"/accounts/{acct}/notes"})
	if err != nil {
		t.Fatal(err)
	}
	a := newAnonymizer(t)
	ex := captureAnon(t, a, n, jsonEx(1, "GET", "/accounts/alice@example.com/notes", "", "notes-alice"))
	if ex.Endpoint != "GET /accounts/{acct}/notes" {
		t.Fatalf("endpoint = %q", ex.Endpoint)
	}
	e := NewEngine(nil, noLatency(Options{Normalizer: n, Anonymizer: a, Match: model.MatchConfig{PathTemplates: true}}))
	t.Cleanup(func() { _ = e.Close() })
	e.Add(ex)
	if level, got := rawLookup(e, "GET", "/accounts/alice@example.com/notes", ""); level != LevelExact || got != "notes-alice" {
		t.Errorf("exact: level=%s resp=%q", level, got)
	}
	if level, got := rawLookup(e, "GET", "/accounts/zed@example.com/notes", ""); level != LevelTemplate || got != "notes-alice" {
		t.Errorf("template: level=%s resp=%q", level, got)
	}
}

// Compressed request bodies are decoded the way the capture stored them.
func TestAnonymizerDecodesCompressedRequestBodies(t *testing.T) {
	plain := `{"email":"alice@example.com","card":"4111 1111 1111 1111","sku":"A"}`
	gz := func(s string) []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return b.Bytes()
	}
	zl := func(s string) []byte {
		var b bytes.Buffer
		w := zlib.NewWriter(&b)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return b.Bytes()
	}
	rawDeflate := func(s string) []byte {
		var b bytes.Buffer
		w, _ := flate.NewWriter(&b, flate.DefaultCompression)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return b.Bytes()
	}
	cases := []struct {
		name, encoding string
		body           []byte
	}{
		{"gzip", "gzip", gz(plain)},
		{"deflate", "deflate", zl(plain)},
		{"raw deflate", "deflate", rawDeflate(plain)},
		{"stacked gzip", "gzip, gzip", gz(string(gz(plain)))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The capture of a client that compressed its request.
			rec := jsonEx(1, "POST", "/orders", "", "order-alice-"+tc.name)
			rec.Request.Body, rec.Request.BodySize = tc.body, int64(len(tc.body))
			rec.Request.Header.Set("Content-Encoding", tc.encoding)
			a := newAnonymizer(t)
			captureAnon(t, a, pathnorm.Default, rec)
			if rec.Request.Header.Get("Content-Encoding") != "" || strings.Contains(string(rec.Request.Body), "alice@example.com") {
				t.Fatalf("setup: capture kept the request encoded or raw: %v %q", rec.Request.Header, rec.Request.Body)
			}
			e := NewEngine(nil, noLatency(Options{Anonymizer: a, Match: model.MatchConfig{Body: model.BodyJSON}}))
			t.Cleanup(func() { _ = e.Close() })
			e.Add(rec)

			orig := bytes.Clone(tc.body)
			r := mkReq("POST", "/orders", "Content-Type", "application/json", "Content-Encoding", tc.encoding)
			m := e.Lookup(r, tc.body)
			if m == nil || m.Level != LevelExact {
				t.Fatalf("match = %+v, want exact", m)
			}
			if !bytes.Equal(tc.body, orig) {
				t.Error("Lookup modified the caller's body")
			}
			if r.Header.Get("Content-Encoding") != tc.encoding {
				t.Errorf("Lookup changed the request headers: %v", r.Header)
			}
		})
	}
}

// Lookup never changes the request or the body it is given.
func TestAnonymizerDoesNotModifyTheRequest(t *testing.T) {
	c := newAnonCorpus(t)
	e := c.engine(t, Options{Match: model.MatchConfig{Body: model.BodyJSON, Headers: []string{"Authorization"}}}, true)

	body := []byte(`{"email":"alice@example.com","card":"4111 1111 1111 1111","sku":"A"}`)
	r := httptest.NewRequest("POST", "/users/alice@example.com/profile?email=alice%40example.com&card=4111111111111111", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer secret-token-123456")
	r.Header.Add("Cookie", "session=abcdef0123456789")
	r.Header.Add("X-Email", "alice@example.com")

	wantHeader := r.Header.Clone()
	wantURL := r.URL.String()
	wantBody := bytes.Clone(body)
	wantPath, wantQuery := r.URL.EscapedPath(), r.URL.RawQuery

	for range 3 {
		_ = e.Lookup(r, body)
		_ = e.Lookup(r, nil)
	}
	rec := httptest.NewRecorder()
	e.Serve(rec, r, body)

	if !reflect.DeepEqual(r.Header, wantHeader) {
		t.Errorf("headers changed:\n got %v\nwant %v", r.Header, wantHeader)
	}
	if r.URL.String() != wantURL || r.URL.EscapedPath() != wantPath || r.URL.RawQuery != wantQuery {
		t.Errorf("URL changed: %s", r.URL)
	}
	if !bytes.Equal(body, wantBody) {
		t.Errorf("body changed: %s", body)
	}
}

// A header listed in match.headers is compared in its anonymized form.
func TestAnonymizerMatchesAnonymizedHeaders(t *testing.T) {
	c := newAnonCorpus(t,
		jsonEx(7, "GET", "/me", "", "me-alice", "Authorization", "Bearer token-of-alice-0123456789"),
		jsonEx(8, "GET", "/me", "", "me-bob", "Authorization", "Bearer token-of-bob-9876543210"),
	)
	if c.stored["me-alice"].header.Get("Authorization") == "Bearer token-of-alice-0123456789" {
		t.Fatal("setup: the token was not anonymized")
	}
	e := c.engine(t, Options{Match: model.MatchConfig{Headers: []string{"Authorization"}}}, true)
	for user, token := range map[string]string{"alice": "token-of-alice-0123456789", "bob": "token-of-bob-9876543210"} {
		level, got := rawLookup(e, "GET", "/me", "", "Authorization", "Bearer "+token)
		if level != LevelExact || got != "me-"+user {
			t.Errorf("%s: level=%s resp=%q", user, level, got)
		}
	}
	// An unknown token only matches at the path level, never exactly.
	if level, _ := rawLookup(e, "GET", "/me", "", "Authorization", "Bearer token-of-carol-5555555555"); level == LevelExact {
		t.Errorf("unknown token matched exactly")
	}
}

// With query "subset" the recorded parameters must be among the request's,
// compared in their anonymized form.
func TestAnonymizerSubsetQuery(t *testing.T) {
	c := newAnonCorpus(t)
	e := c.engine(t, Options{Match: model.MatchConfig{Query: model.QuerySubset}}, true)
	level, got := rawLookup(e, "GET", "/search?email=bob%40example.com&card=5500005555555559&page=1&utm=x", "")
	if level != LevelExact || got != "search-bob" {
		t.Errorf("level=%s resp=%q, want exact search-bob", level, got)
	}
}

// HEAD is answered from the anonymized GET recording.
func TestAnonymizerHeadFallsBackToGet(t *testing.T) {
	c := newAnonCorpus(t)
	e := c.engine(t, Options{}, true)
	rec, ok := serve(e, mkReq("HEAD", "/users/alice@example.com/profile"), "")
	if !ok || rec.Code != 200 || rec.Header().Get("Content-Length") != fmt.Sprint(len("profile-alice")) || rec.Body.Len() != 0 {
		t.Fatalf("ok=%v code=%d hdr=%v body=%q", ok, rec.Code, rec.Header(), rec.Body.String())
	}
	if rec.Header().Get(HeaderMatch) != LevelExact {
		t.Errorf("match = %q", rec.Header().Get(HeaderMatch))
	}
}

// A nil *anonymize.Engine inside the interface means "no anonymizer", and an
// anonymizer that fails leaves the request to be looked up as received.
func TestAnonymizerNilAndFailing(t *testing.T) {
	var nilEngine *anonymize.Engine
	e := newTestEngine(t, Options{Anonymizer: nilEngine}, mkEx(1, "GET", "/plain", 200, "plain"))
	if e.anon != nil {
		t.Error("typed nil anonymizer kept")
	}
	if level, got := lookup(e, "GET", "/plain", ""); level != LevelExact || got != "plain" {
		t.Errorf("level=%s resp=%q", level, got)
	}

	e = newTestEngine(t, Options{Anonymizer: failingAnonymizer{}}, mkEx(1, "GET", "/plain", 200, "plain"))
	if level, got := lookup(e, "GET", "/plain", ""); level != LevelExact || got != "plain" {
		t.Errorf("failing anonymizer: level=%s resp=%q", level, got)
	}
}

type failingAnonymizer struct{}

func (failingAnonymizer) Request(*model.Request) error { return errors.New("boom") }

// A recording without anonymized exchanges (a mixed recording) still matches
// raw requests through the request as received.
func TestAnonymizerRawExchangesStillMatch(t *testing.T) {
	a := newAnonymizer(t)
	e := newTestEngine(t, Options{Anonymizer: a}, mkEx(1, "GET", "/users/zed@example.com/profile", 200, "raw-zed"))
	if level, got := lookup(e, "GET", "/users/zed@example.com/profile", ""); level != LevelExact || got != "raw-zed" {
		t.Errorf("level=%s resp=%q", level, got)
	}
}

// A request the anonymizer leaves unchanged is probed once and costs the same
// answer.
func TestAnonymizerUnchangedRequest(t *testing.T) {
	c := newAnonCorpus(t)
	e := c.engine(t, Options{Match: model.MatchConfig{Body: model.BodyJSON}}, true)
	if _, got := rawLookup(e, "GET", "/does/not/exist", ""); got != "" {
		t.Errorf("resp = %q", got)
	}
	if s := e.Stats(); s.Misses != 1 {
		t.Errorf("stats = %+v", s)
	}
}

// Anonymized lookups are safe under concurrency (run with -race) and give
// every goroutine its own user's answer.
func TestAnonymizerConcurrentLookups(t *testing.T) {
	c := newAnonCorpus(t)
	e := c.engine(t, Options{Match: model.MatchConfig{Body: model.BodyJSON}}, true)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user := []string{"alice", "bob"}[g%2]
			card := map[string]string{"alice": "4111 1111 1111 1111", "bob": "5500 0055 5555 5559"}[user]
			for range 200 {
				if _, got := rawLookup(e, "GET", "/users/"+user+"@example.com/profile", ""); got != "profile-"+user {
					t.Errorf("%s: path resp %q", user, got)
					return
				}
				body := fmt.Sprintf(`{"email":"%s@example.com","card":"%s","sku":"A"}`, user, card)
				if _, got := rawLookup(e, "POST", "/orders", body); got != "order-"+user {
					t.Errorf("%s: body resp %q", user, got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// Hybrid mode adds anonymized captures while serving: lookups find them.
func TestAnonymizerHybridAdd(t *testing.T) {
	c := newAnonCorpus(t)
	e := c.engine(t, Options{Match: model.MatchConfig{Body: model.BodyJSON}}, true)
	if level, _ := rawLookup(e, "GET", "/users/erin@example.com/profile", ""); level != LevelMiss {
		t.Fatalf("level = %s before the capture", level)
	}
	e.Add(captureAnon(t, c.anon, c.norm, func() *model.Exchange {
		ex := jsonEx(9, "GET", "/users/erin@example.com/profile", "", "profile-erin")
		ex.ID = 0
		return ex
	}()))
	if level, got := rawLookup(e, "GET", "/users/erin@example.com/profile", ""); level != LevelExact || got != "profile-erin" {
		t.Errorf("level=%s resp=%q after the capture", level, got)
	}
}
