package ops

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/har"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
	"github.com/justinburger/mirage/internal/store"
)

const (
	piiEmail = "zed.hacker@acme.io"
	piiPass  = "Pa$$w0rd!x"
	piiSSN   = "123-45-6789"
)

var piiSecrets = []string{piiEmail, piiPass, "acme.io", "zed.hacker"}

// piiJSON is a body that holds personal data the default profile removes.
func piiJSON() []byte {
	return []byte(fmt.Sprintf(`{"email":%q,"password":%q,"note":"hello"}`, piiEmail, piiPass))
}

func zlibOf(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

func rawDeflateOf(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(b)
	_ = fw.Close()
	return buf.Bytes()
}

func gzipOf(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

// cutGzipOf returns a gzip stream of a long JSON document, with the personal
// data at the front, cut off in the middle of the list that follows it the way
// a capture cap cuts a long body: the stream does not verify, yet its start
// decompresses to the personal data.
func cutGzipOf(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = fmt.Fprintf(zw, `{"email":%q,"password":%q,"items":[`, piiEmail, piiPass)
	if err := zw.Flush(); err != nil { // a sync point after the personal data
		t.Fatal(err)
	}
	mark := buf.Len()
	for i := 0; i < 600; i++ {
		_, _ = fmt.Fprintf(zw, `{"id":%d,"sku":"%x"},`, i, uint32(i)*2654435761)
	}
	_ = zw.Close()
	cut := mark + 200
	if cut >= buf.Len() {
		t.Fatalf("cut gzip: stream is only %d bytes", buf.Len())
	}
	return append([]byte(nil), buf.Bytes()[:cut]...)
}

// cutDeflateOf is cutGzipOf for a deflate stream, zlib or raw (no header):
// cut 200 bytes after a sync point that follows the personal data.
func cutDeflateOf(t testing.TB, zlibHeader bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w interface {
		io.Writer
		Flush() error
		Close() error
	}
	if zlibHeader {
		w = zlib.NewWriter(&buf)
	} else {
		fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
		w = fw
	}
	_, _ = fmt.Fprintf(w, `{"email":%q,"password":%q,"items":[`, piiEmail, piiPass)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	mark := buf.Len()
	for i := 0; i < 600; i++ {
		_, _ = fmt.Fprintf(w, `{"id":%d,"sku":"%x"},`, i, uint32(i)*2654435761)
	}
	_ = w.Close()
	cut := mark + 200
	if cut >= buf.Len() {
		t.Fatalf("cut deflate: stream is only %d bytes", buf.Len())
	}
	return append([]byte(nil), buf.Bytes()[:cut]...)
}

// latin1JSON is a long ISO-8859-1 document with the personal data at the
// front: not valid UTF-8, so a cut deflate stream of it does not inflate
// into UTF-8 text.
func latin1JSON() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "{\"name\":\"Jos\xe9\",\"email\":%q,\"password\":%q,\"items\":[", piiEmail, piiPass)
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&b, `{"id":%d,"sku":"%x"},`, i, uint32(i)*2654435761)
	}
	b.WriteString(`{}]}`)
	return b.Bytes()
}

// brLike is a body that claims to be brotli: no decoder in the standard
// library can read it, and it is not text.
func brLike() []byte {
	return append([]byte{0x1b, 0xff, 0xfe, 0x03, 0x80}, piiJSON()...)
}

// encodedCase is a body under a content coding that the anonymizer cannot read
// as it is.
type encodedCase struct {
	name string
	enc  string
	body func(testing.TB) []byte
	cut  bool // the stored body is flagged truncated
}

func encodedCases() []encodedCase {
	return []encodedCase{
		{name: "gzip", enc: "gzip", body: func(t testing.TB) []byte { return gzipOf(t, piiJSON()) }},
		{name: "zlib deflate", enc: "deflate", body: func(t testing.TB) []byte { return zlibOf(t, piiJSON()) }},
		{name: "raw deflate", enc: "deflate", body: func(t testing.TB) []byte { return rawDeflateOf(t, piiJSON()) }},
		{name: "stacked gzip", enc: "gzip, gzip", body: func(t testing.TB) []byte { return gzipOf(t, gzipOf(t, piiJSON())) }},
		{name: "deflate then gzip", enc: "deflate, gzip", body: func(t testing.TB) []byte { return gzipOf(t, zlibOf(t, piiJSON())) }},
		{name: "br", enc: "br", body: func(testing.TB) []byte { return brLike() }},
		{name: "cut gzip", enc: "gzip", body: func(t testing.TB) []byte { return cutGzipOf(t) }, cut: true},
		{name: "cut zlib deflate", enc: "deflate", body: func(t testing.TB) []byte { return cutDeflateOf(t, true) }, cut: true},
		{name: "cut raw deflate", enc: "deflate", body: func(t testing.TB) []byte { return cutDeflateOf(t, false) }, cut: true},
		{name: "cut raw deflate of ISO-8859-1", enc: "deflate", body: func(t testing.TB) []byte { return rawDeflateOf(t, latin1JSON())[:2000] }, cut: true},
		{name: "cut gzip, deflate stack", enc: "gzip, deflate", body: func(t testing.TB) []byte { return rawDeflateOf(t, gzipOf(t, latin1JSON()))[:2000] }, cut: true},
		// Larger than the 1 MiB a capture keeps, so a proxy that decodes
		// only up to its cap stores it still compressed.
		{name: "large gzip", enc: "gzip", body: func(t testing.TB) []byte { return gzipOf(t, largeJSON()) }},
	}
}

// largeJSON is a 1.5 MiB document with the personal data at the front.
func largeJSON() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"email":%q,"password":%q,"items":[`, piiEmail, piiPass)
	for i := 0; b.Len() < 1536<<10; i++ {
		fmt.Fprintf(&b, `{"id":%d,"sku":"%x"},`, i, uint32(i)*2654435761)
	}
	b.WriteString(`{"id":-1}]}`)
	return b.Bytes()
}

// decompressions returns b and everything that decompressing it (gzip, zlib,
// raw deflate, up to three layers, keeping the output of streams that fail
// after some output) can recover.
func decompressions(b []byte, depth int) [][]byte {
	out := [][]byte{b}
	if depth == 0 || len(b) == 0 {
		return out
	}
	try := func(r io.Reader) {
		got, _ := io.ReadAll(io.LimitReader(r, 64<<20))
		if len(got) > 0 {
			out = append(out, decompressions(got, depth-1)...)
		}
	}
	if zr, err := gzip.NewReader(bytes.NewReader(b)); err == nil {
		try(zr)
	}
	if zr, err := zlib.NewReader(bytes.NewReader(b)); err == nil {
		try(zr)
	}
	try(flate.NewReader(bytes.NewReader(b)))
	return out
}

// leaked lists the secrets that can be recovered from any of the bodies.
func leaked(secrets []string, bodies ...[]byte) []string {
	var found []string
	for _, b := range bodies {
		for _, d := range decompressions(b, 3) {
			for _, s := range secrets {
				if bytes.Contains(d, []byte(s)) {
					found = append(found, s)
				}
			}
		}
	}
	return found
}

// storedEncodedExchange is a captured exchange whose request and response
// bodies are body, still under the coding enc, as the proxy stores one it
// could not decode.
func storedEncodedExchange(i int, c encodedCase, body []byte) *model.Exchange {
	path := fmt.Sprintf("/people/%s/profile", piiSSN)
	h := func() http.Header {
		return http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {c.enc}}
	}
	return &model.Exchange{
		ServiceID: "svc_test",
		StartedAt: baseTime.Add(time.Duration(i) * time.Second),
		Endpoint:  pathnorm.Default.Endpoint("POST", path),
		Request:   model.Request{Method: "POST", Scheme: "http", Host: "localhost:9001", Path: path, Proto: "HTTP/1.1", Header: h(), Body: body, BodySize: int64(len(body)), BodyTruncated: c.cut},
		Response:  model.Response{Status: 200, Proto: "HTTP/1.1", Header: h(), Body: body, BodySize: int64(len(body)), BodyTruncated: c.cut},
	}
}

func seedEncoded(t testing.TB, st store.Store) model.Recording {
	t.Helper()
	ctx := context.Background()
	rec, err := st.CreateRecording(ctx, model.Recording{Name: "encoded", Source: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	var batch []*model.Exchange
	for i, c := range encodedCases() {
		body := c.body(t)
		if got := leaked(piiSecrets, body); len(got) == 0 && c.name != "br" {
			t.Fatalf("test body %q does not hold the personal data in any decompressed form", c.name)
		}
		batch = append(batch, storedEncodedExchange(i, c, body))
	}
	if err := st.AppendExchanges(ctx, rec.ID, batch); err != nil {
		t.Fatal(err)
	}
	return rec
}

func defaultEngine(t testing.TB, st store.Store) *anonymize.Engine {
	t.Helper()
	eng, err := LoadEngine(context.Background(), st, "")
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// checkNoPII fails when anything of the exchange, bodies decompressed in every
// way, still holds a secret, or the endpoint holds a raw path segment.
func checkNoPII(t *testing.T, label string, ex *model.Exchange) {
	t.Helper()
	if got := leaked(piiSecrets, ex.Request.Body, ex.Response.Body); len(got) > 0 {
		t.Errorf("%s: bodies still hold %v (request %q, response %q, headers %v / %v)", label, got, ex.Request.Body, ex.Response.Body, ex.Request.Header, ex.Response.Header)
	}
	if d := dump(ex); strings.Contains(d, piiEmail) || strings.Contains(d, piiPass) {
		t.Errorf("%s: exchange dump holds personal data:\n%s", label, d)
	}
	if strings.Contains(ex.Endpoint, piiSSN) || strings.Contains(ex.Request.Path, piiSSN) {
		t.Errorf("%s: path %q / endpoint %q still hold %s", label, ex.Request.Path, ex.Endpoint, piiSSN)
	}
	if !ex.Anonymized {
		t.Errorf("%s: not flagged anonymized", label)
	}
}

func TestCopyAnonymizedLeavesNoCompressedPII(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	src := seedEncoded(t, st)
	eng := defaultEngine(t, st)

	dst, err := CopyAnonymized(ctx, st, src.ID, eng, "", nil)
	if err != nil {
		t.Fatalf("CopyAnonymized: %v", err)
	}
	got := loadAll(t, st, dst.ID)
	cases := encodedCases()
	if len(got) != len(cases) {
		t.Fatalf("copy has %d exchanges, want %d", len(got), len(cases))
	}
	for i, ex := range got {
		checkNoPII(t, cases[i].name, ex)
	}
}

func TestExportHARLeavesNoCompressedPII(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	src := seedEncoded(t, st)
	eng := defaultEngine(t, st)

	var out bytes.Buffer
	if err := ExportHAR(ctx, st, src.ID, &out, eng, "test"); err != nil {
		t.Fatalf("ExportHAR: %v", err)
	}
	doc, err := har.Decode(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("exported HAR does not parse: %v", err)
	}
	cases := encodedCases()
	if len(doc.Log.Entries) != len(cases) {
		t.Fatalf("export has %d entries, want %d", len(doc.Log.Entries), len(cases))
	}
	// The whole document, as bytes: nothing may be recoverable from anywhere
	// in it, including base64 bodies.
	if got := leaked(piiSecrets, out.Bytes()); len(got) > 0 {
		t.Errorf("the exported document still holds %v", got)
	}
	for i := range doc.Log.Entries {
		ex, err := har.ToExchange(&doc.Log.Entries[i])
		if err != nil {
			t.Fatal(err)
		}
		ex.Anonymized = true // the export is anonymized; the HAR cannot say so
		checkNoPII(t, cases[i].name, ex)
	}
}

func TestImportHARLeavesNoCompressedPII(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	eng := defaultEngine(t, st)

	var entries []har.Entry
	for i, c := range encodedCases() {
		body := c.body(t)
		e := entry(fmt.Sprintf("http://localhost:9001/people/%s/profile", piiSSN), 200)
		e.StartedDateTime = har.Timestamp{Time: baseTime.Add(time.Duration(i) * time.Second)}
		e.Request.Method = "POST"
		e.Request.Headers = []har.NameValue{{Name: "Content-Encoding", Value: c.enc}, {Name: "Content-Type", Value: "application/json"}}
		e.Request.PostData = &har.PostData{MimeType: "application/json", Text: base64.StdEncoding.EncodeToString(body), Encoding: "base64"}
		e.Response.Headers = []har.NameValue{{Name: "Content-Encoding", Value: c.enc}, {Name: "Content-Type", Value: "application/json"}}
		e.Response.Content = har.Content{MimeType: "application/json", Text: base64.StdEncoding.EncodeToString(body), Encoding: "base64", Size: har.Size(len(body))}
		entries = append(entries, e)
	}
	rec, err := ImportHAR(ctx, st, bytes.NewReader(harDoc(t, entries...)), "encoded", eng, nil)
	if err != nil {
		t.Fatalf("ImportHAR: %v", err)
	}
	got := loadAll(t, st, rec.ID)
	if len(got) != len(entries) {
		t.Fatalf("import has %d exchanges, want %d (%s)", len(got), len(entries), rec.Description)
	}
	for i, ex := range got {
		checkNoPII(t, encodedCases()[i].name, ex)
	}
}

// failingEngine fails on the exchanges selected by fail and otherwise
// delegates to the engine it wraps.
type failingEngine struct {
	Anonymizer
	fail func(*model.Exchange) bool
}

func (f failingEngine) Exchange(ex *model.Exchange) error {
	if f.fail(ex) {
		return errInjected
	}
	return f.Anonymizer.Exchange(ex)
}

func (f failingEngine) ExchangeWithReport(ex *model.Exchange) ([]model.AnonChange, error) {
	if f.fail(ex) {
		return nil, errInjected
	}
	return f.Anonymizer.ExchangeWithReport(ex)
}

func boomEngine(real Anonymizer) failingEngine {
	return failingEngine{Anonymizer: real, fail: func(ex *model.Exchange) bool { return strings.Contains(ex.Request.Path, "/boom") }}
}

func TestAnonymizeExchangeEndpointAndErrors(t *testing.T) {
	eng := defaultEngine(t, newStore(t))
	ex := mkExchange(1)
	ex.Request.Path = "/people/" + piiSSN + "/profile"
	ex.Endpoint = pathnorm.Default.Endpoint("POST", ex.Request.Path)
	if !strings.Contains(ex.Endpoint, piiSSN) {
		t.Skipf("the default normalizer already templates the segment: %q", ex.Endpoint)
	}
	if _, err := AnonymizeExchange(eng, ex, nil, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ex.Endpoint, piiSSN) || !strings.HasPrefix(ex.Endpoint, "POST /people/") {
		t.Errorf("endpoint = %q", ex.Endpoint)
	}

	if _, err := AnonymizeExchange(nil, mkExchange(1), nil, false); !errors.Is(err, model.ErrValidation) {
		t.Errorf("nil engine: %v", err)
	}
	bad := boomEngine(eng)
	boom := mkExchange(2)
	boom.Request.Path = "/boom"
	if _, err := AnonymizeExchange(bad, boom, nil, true); !errors.Is(err, errInjected) {
		t.Errorf("failing engine: %v", err)
	}
	changes, err := AnonymizeExchange(eng, mkExchange(3), nil, true)
	if err != nil || len(changes) == 0 {
		t.Errorf("withReport: %d changes, %v", len(changes), err)
	}
}

// seedWithBoom creates a recording of n exchanges in which exchange idx has a
// path the failing engine rejects.
func seedWithBoom(t *testing.T, st store.Store, n int, idx ...int) model.Recording {
	t.Helper()
	ctx := context.Background()
	rec, err := st.CreateRecording(ctx, model.Recording{Name: "boom", Source: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	var batch []*model.Exchange
	for i := 0; i < n; i++ {
		ex := mkExchange(i)
		for _, k := range idx {
			if k == i {
				ex.Request.Path = "/boom/" + piiEmail
				ex.Endpoint = "POST /boom/x"
			}
		}
		batch = append(batch, ex)
	}
	if err := st.AppendExchanges(ctx, rec.ID, batch); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestCopyAnonymizedSkipsAnExchangeThatFails(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	src := seedWithBoom(t, st, 6, 2)
	eng := boomEngine(defaultEngine(t, st))

	var reported int
	dst, stats, err := copyAnonymized(ctx, st, src.ID, eng, "safe", nil, func([]model.AnonChange) { reported++ })
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if stats.Written != 5 || stats.Failed != 1 || !errors.Is(stats.FirstError, errInjected) {
		t.Errorf("stats = %+v", stats)
	}
	if reported != 5 {
		t.Errorf("report called %d times, want 5", reported)
	}
	if dst.ExchangeCount != 5 {
		t.Errorf("ExchangeCount = %d, want 5", dst.ExchangeCount)
	}
	if !strings.Contains(dst.Description, "Left out 1 exchange that could not be anonymized") {
		t.Errorf("Description = %q", dst.Description)
	}
	for _, ex := range loadAll(t, st, dst.ID) {
		if strings.Contains(dump(ex), "/boom") || strings.Contains(dump(ex), piiEmail) {
			t.Fatalf("the exchange that failed was written:\n%s", dump(ex))
		}
	}

	// Nothing but failures: an error, and no recording left behind.
	all := seedWithBoom(t, st, 3, 0, 1, 2)
	before := recordingCount(t, st)
	if _, stats, err := copyAnonymized(ctx, st, all.ID, eng, "none", nil, nil); err == nil || stats.Failed != 3 || stats.Written != 0 {
		t.Fatalf("all failed: err %v stats %+v", err, stats)
	}
	if n := recordingCount(t, st); n != before {
		t.Errorf("a failed copy left %d recordings behind", n-before)
	}
	// An empty recording is not a failure.
	empty, _ := st.CreateRecording(ctx, model.Recording{Name: "empty"})
	if _, _, err := copyAnonymized(ctx, st, empty.ID, eng, "", nil, nil); err != nil {
		t.Errorf("copy of an empty recording: %v", err)
	}
}

func TestExportHARSkipsAnExchangeThatFails(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	src := seedWithBoom(t, st, 6, 4)
	eng := boomEngine(defaultEngine(t, st))

	var out bytes.Buffer
	stats, err := exportHAR(ctx, st, src.ID, &out, eng, "test")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if stats.Written != 5 || stats.Failed != 1 {
		t.Errorf("stats = %+v", stats)
	}
	doc, err := har.Decode(bytes.NewReader(out.Bytes()))
	if err != nil || len(doc.Log.Entries) != 5 {
		t.Fatalf("document: %d entries, %v", len(doc.Log.Entries), err)
	}
	if strings.Contains(out.String(), "/boom") || strings.Contains(out.String(), piiEmail) {
		t.Error("the exchange that failed was written")
	}

	// Nothing but failures: an error and a document that is visibly unfinished.
	all := seedWithBoom(t, st, 2, 0, 1)
	out.Reset()
	if _, err := exportHAR(ctx, st, all.ID, &out, eng, "test"); err == nil {
		t.Fatal("export of nothing but failures succeeded")
	}
	if _, err := har.Decode(bytes.NewReader(out.Bytes())); err == nil {
		t.Error("the unfinished export parses as a valid HAR")
	}
}

func TestImportHARSkipsAnEntryThatFails(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	eng := boomEngine(defaultEngine(t, st))

	mk := func(i int, path string) har.Entry {
		e := entry("http://localhost:9001"+path, 200)
		e.StartedDateTime = har.Timestamp{Time: baseTime.Add(time.Duration(i) * time.Second)}
		return e
	}
	doc := harDoc(t, mk(0, "/a"), mk(1, "/boom/"+piiEmail), mk(2, "/b"), mk(3, "/c"))
	rec, stats, err := importHAR(ctx, st, bytes.NewReader(doc), "partial", eng, nil)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if stats.Written != 3 || stats.Failed != 1 || !errors.Is(stats.FirstError, errInjected) {
		t.Errorf("stats = %+v", stats)
	}
	if rec.ExchangeCount != 3 || !strings.Contains(rec.Description, "Left out 1 entry that could not be anonymized") {
		t.Errorf("recording = %d exchanges, %q", rec.ExchangeCount, rec.Description)
	}
	for _, ex := range loadAll(t, st, rec.ID) {
		if strings.Contains(dump(ex), "/boom") {
			t.Fatal("the entry that failed was stored")
		}
	}

	before := recordingCount(t, st)
	all := harDoc(t, mk(0, "/boom/1"), mk(1, "/boom/2"))
	if _, stats, err := importHAR(ctx, st, bytes.NewReader(all), "none", eng, nil); err == nil || stats.Failed != 2 {
		t.Fatalf("all failed: err %v stats %+v", err, stats)
	}
	if n := recordingCount(t, st); n != before {
		t.Errorf("a failed import left %d recordings behind", n-before)
	}
}

func TestCopyKeepsAServicePatternEndpoint(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	rec, _ := st.CreateRecording(ctx, model.Recording{Name: "patterns", Source: "proxy"})
	mk := func(i int, path, endpoint string) *model.Exchange {
		ex := mkExchange(i)
		ex.Request.Path, ex.Endpoint = path, endpoint
		return ex
	}
	err := st.AppendExchanges(ctx, rec.ID, []*model.Exchange{
		mk(0, "/orders/ABC-1/lines", "POST /orders/{ref}/lines"),  // a service pattern: kept
		mk(1, "/ssn/"+piiSSN, "POST /ssn/"+piiSSN),                // a raw segment in the endpoint: rebuilt
		mk(2, "/orders/XYZ-9/lines", "POST /secret/"+piiSSN+"/x"), // not the path's own segments: rebuilt
	})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := CopyAnonymized(ctx, st, rec.ID, defaultEngine(t, st), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := loadAll(t, st, dst.ID)
	if got[0].Endpoint != "POST /orders/{ref}/lines" {
		t.Errorf("pattern endpoint = %q", got[0].Endpoint)
	}
	for _, ex := range got {
		if strings.Contains(ex.Endpoint, piiSSN) {
			t.Errorf("endpoint %q holds the raw value", ex.Endpoint)
		}
	}
}
