package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/har"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/ops"
	"github.com/justinburger/mirage/internal/store"
)

const (
	cliEmail = "zed.hacker@acme.io"
	cliPass  = "Pa$$w0rd!x"
	cliSSN   = "123-45-6789"
)

var cliSecrets = []string{cliEmail, cliPass, "zed.hacker", "acme.io"}

func cliPII() []byte {
	return []byte(fmt.Sprintf(`{"email":%q,"password":%q,"note":"hello"}`, cliEmail, cliPass))
}

func cliCompress(t testing.TB, kind string, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch kind {
	case "gzip":
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(b)
		_ = zw.Close()
	case "zlib":
		zw := zlib.NewWriter(&buf)
		_, _ = zw.Write(b)
		_ = zw.Close()
	case "flate":
		fw, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		_, _ = fw.Write(b)
		_ = fw.Close()
	default:
		t.Fatalf("unknown compression %q", kind)
	}
	return buf.Bytes()
}

// cliCutGzip is a gzip stream of a long JSON document cut in the middle of a
// list, with the personal data in the part that survives.
func cliCutGzip(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = fmt.Fprintf(zw, `{"email":%q,"password":%q,"items":[`, cliEmail, cliPass)
	_ = zw.Flush()
	mark := buf.Len()
	for i := 0; i < 600; i++ {
		_, _ = fmt.Fprintf(zw, `{"id":%d,"sku":"%x"},`, i, uint32(i)*2654435761)
	}
	_ = zw.Close()
	return append([]byte(nil), buf.Bytes()[:mark+200]...)
}

type cliCase struct {
	name, enc string
	body      []byte
	cut       bool
}

func cliCases(t testing.TB) []cliCase {
	return []cliCase{
		{name: "zlib deflate", enc: "deflate", body: cliCompress(t, "zlib", cliPII())},
		{name: "raw deflate", enc: "deflate", body: cliCompress(t, "flate", cliPII())},
		{name: "stacked gzip", enc: "gzip, gzip", body: cliCompress(t, "gzip", cliCompress(t, "gzip", cliPII()))},
		{name: "br", enc: "br", body: append([]byte{0x1b, 0xff, 0xfe, 0x03, 0x80}, cliPII()...)},
		{name: "cut gzip", enc: "gzip", body: cliCutGzip(t), cut: true},
	}
}

// recoverable returns b and everything decompressing it can recover.
func recoverable(b []byte, depth int) [][]byte {
	out := [][]byte{b}
	if depth == 0 || len(b) == 0 {
		return out
	}
	try := func(r io.Reader) {
		got, _ := io.ReadAll(io.LimitReader(r, 64<<20))
		if len(got) > 0 {
			out = append(out, recoverable(got, depth-1)...)
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

func cliLeaks(bodies ...[]byte) []string {
	var found []string
	for _, b := range bodies {
		for _, d := range recoverable(b, 3) {
			for _, s := range cliSecrets {
				if bytes.Contains(d, []byte(s)) {
					found = append(found, s)
				}
			}
		}
	}
	return found
}

func writeCLIHAR(t *testing.T, dir string, entries []har.Entry) string {
	t.Helper()
	b, err := json.Marshal(har.HAR{Log: har.Log{Version: "1.2", Creator: har.Creator{Name: "cli-test", Version: "1"}, Entries: entries}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "in.har")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func cliEntry(url string, at time.Time) har.Entry {
	return har.Entry{
		StartedDateTime: har.Timestamp{Time: at},
		Time:            12,
		Request:         har.Request{Method: "GET", URL: url, HTTPVersion: "HTTP/1.1", HeadersSize: -1, BodySize: -1},
		Response:        har.Response{Status: 200, StatusText: "OK", HTTPVersion: "HTTP/1.1", HeadersSize: -1, BodySize: -1, Content: har.Content{MimeType: "text/plain", Text: "ok", Size: 2}},
		Timings:         har.Timings{Send: 0, Wait: 10, Receive: 2},
	}
}

func encodedHAREntries(t *testing.T) []har.Entry {
	var entries []har.Entry
	for i, c := range cliCases(t) {
		b64 := base64.StdEncoding.EncodeToString(c.body)
		e := cliEntry("http://localhost:9001/people/"+cliSSN+"/profile", time.Date(2026, 10, 1, 10, 0, i, 0, time.UTC))
		e.Request.Method = "POST"
		e.Request.Headers = []har.NameValue{{Name: "Content-Encoding", Value: c.enc}, {Name: "Content-Type", Value: "application/json"}}
		e.Request.PostData = &har.PostData{MimeType: "application/json", Text: b64, Encoding: "base64"}
		e.Response.Headers = []har.NameValue{{Name: "Content-Encoding", Value: c.enc}, {Name: "Content-Type", Value: "application/json"}}
		e.Response.Content = har.Content{MimeType: "application/json", Text: b64, Encoding: "base64", Size: har.Size(len(c.body))}
		entries = append(entries, e)
	}
	return entries
}

// checkHARHasNoPII fails when the HAR in b still lets any personal data be
// recovered from its entries.
func checkHARHasNoPII(t *testing.T, label string, b []byte) {
	t.Helper()
	doc, err := har.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%s: not a HAR: %v", label, err)
	}
	cases := cliCases(t)
	if len(doc.Log.Entries) != len(cases) {
		t.Fatalf("%s: %d entries, want %d", label, len(doc.Log.Entries), len(cases))
	}
	if got := cliLeaks(b); len(got) > 0 {
		t.Errorf("%s: the document holds %v", label, got)
	}
	for i := range doc.Log.Entries {
		ex, err := har.ToExchange(&doc.Log.Entries[i])
		if err != nil {
			t.Fatal(err)
		}
		if got := cliLeaks(ex.Request.Body, ex.Response.Body); len(got) > 0 {
			t.Errorf("%s %s: bodies still hold %v (%q / %q)", label, cases[i].name, got, ex.Request.Body, ex.Response.Body)
		}
		if strings.Contains(ex.Request.Path, cliSSN) {
			t.Errorf("%s %s: path %q", label, cases[i].name, ex.Request.Path)
		}
	}
}

func TestAnonymizeHARFileLeavesNoCompressedPII(t *testing.T) {
	dir := t.TempDir()
	in := writeCLIHAR(t, dir, encodedHAREntries(t))
	out := filepath.Join(dir, "anon.har")
	code, stdout, stderr := runCLI(t, "anonymize", "--in", in, "--out", out)
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	checkHARHasNoPII(t, "anonymize", b)
	if !strings.Contains(stdout, "Anonymized 5 exchanges") {
		t.Errorf("summary:\n%s", stdout)
	}
}

// seedCLIDatabase creates a database file holding the compressed exchanges the
// way the proxy stores bodies it could not decode: bodies still compressed,
// with their Content-Encoding header.
func seedCLIDatabase(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "m.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	rec, err := st.CreateRecording(ctx, model.Recording{Name: "encoded", Source: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	var batch []*model.Exchange
	for i, c := range cliCases(t) {
		h := func() http.Header {
			return http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {c.enc}}
		}
		batch = append(batch, &model.Exchange{
			ServiceID: "svc", StartedAt: time.Date(2026, 10, 1, 10, 0, i, 0, time.UTC),
			Endpoint: "POST /people/" + cliSSN + "/profile",
			Request:  model.Request{Method: "POST", Scheme: "http", Host: "localhost:9001", Path: "/people/" + cliSSN + "/profile", Header: h(), Body: c.body, BodySize: int64(len(c.body)), BodyTruncated: c.cut},
			Response: model.Response{Status: 200, Header: h(), Body: c.body, BodySize: int64(len(c.body)), BodyTruncated: c.cut},
		})
	}
	if err := st.AppendExchanges(ctx, rec.ID, batch); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAnonymizeStoredRecordingLeavesNoCompressedPII(t *testing.T) {
	dir := t.TempDir()
	db := seedCLIDatabase(t, dir)
	code, stdout, stderr := runCLI(t, "anonymize", "--data", db, "--recording", "encoded", "--name", "safe")
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	recs, _ := st.ListRecordings(context.Background())
	var copyID string
	for _, r := range recs {
		if r.Name == "safe" {
			copyID = r.ID
		}
	}
	if copyID == "" {
		t.Fatalf("no copy among %d recordings", len(recs))
	}
	n := 0
	err = st.IterateExchanges(context.Background(), copyID, model.IterOptions{RequestBodies: true, ResponseBodies: true}, func(ex *model.Exchange) error {
		n++
		if got := cliLeaks(ex.Request.Body, ex.Response.Body); len(got) > 0 {
			t.Errorf("exchange %d: bodies hold %v", n, got)
		}
		if strings.Contains(ex.Endpoint, cliSSN) || strings.Contains(ex.Request.Path, cliSSN) {
			t.Errorf("exchange %d: path %q endpoint %q", n, ex.Request.Path, ex.Endpoint)
		}
		return nil
	})
	if err != nil || n != 5 {
		t.Fatalf("copy has %d exchanges, %v", n, err)
	}
}

func TestExportAndImportWithProfileLeaveNoCompressedPII(t *testing.T) {
	dir := t.TempDir()
	db := seedCLIDatabase(t, dir)
	out := filepath.Join(dir, "export.har")
	if code, stdout, stderr := runCLI(t, "export", "--data", db, "--recording", "encoded", "--out", out, "--profile", "default"); code != exitOK {
		t.Fatalf("export: exit %d\n%s\n%s", code, stdout, stderr)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	checkHARHasNoPII(t, "export", b)

	// An anonymizing import of the raw encoded HAR.
	in := writeCLIHAR(t, dir, encodedHAREntries(t))
	imp := filepath.Join(dir, "imp.db")
	if code, stdout, stderr := runCLI(t, "import", "--data", imp, "--har", in, "--name", "imp", "--profile", "default"); code != exitOK {
		t.Fatalf("import: exit %d\n%s\n%s", code, stdout, stderr)
	}
	st, err := store.Open(imp)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	recs, _ := st.ListRecordings(context.Background())
	if len(recs) != 1 {
		t.Fatalf("%d recordings", len(recs))
	}
	n := 0
	err = st.IterateExchanges(context.Background(), recs[0].ID, model.IterOptions{RequestBodies: true, ResponseBodies: true}, func(ex *model.Exchange) error {
		n++
		if got := cliLeaks(ex.Request.Body, ex.Response.Body); len(got) > 0 {
			t.Errorf("imported exchange %d: bodies hold %v", n, got)
		}
		if strings.Contains(ex.Endpoint, cliSSN) || strings.Contains(ex.Request.Path, cliSSN) {
			t.Errorf("imported exchange %d: path %q endpoint %q", n, ex.Request.Path, ex.Endpoint)
		}
		return nil
	})
	if err != nil || n != 5 {
		t.Fatalf("import has %d exchanges, %v", n, err)
	}
}

// failOnPath fails the anonymization of exchanges whose path holds /boom.
type failOnPath struct{ ops.Anonymizer }

func (f failOnPath) ExchangeWithReport(ex *model.Exchange) ([]model.AnonChange, error) {
	if strings.Contains(ex.Request.Path, "/boom") {
		return nil, errors.New("injected failure")
	}
	return f.Anonymizer.ExchangeWithReport(ex)
}

func realEngine(t *testing.T) *anonymize.Engine {
	t.Helper()
	p := model.DefaultAnonymizationProfile()
	p.Secret = model.NewSecret()
	eng, err := anonymize.New(p)
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func TestAnonymizeHARFileSkipsAnEntryThatFails(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	in := writeCLIHAR(t, dir, []har.Entry{
		cliEntry("http://x.test/a", at),
		cliEntry("http://x.test/boom/"+cliEmail, at.Add(time.Second)),
		cliEntry("http://x.test/b", at.Add(2*time.Second)),
	})
	out := filepath.Join(dir, "out.har")
	rep := newAnonReport()
	var errb bytes.Buffer
	_, err := anonymizeHARFile(context.Background(), streams{in: strings.NewReader(""), out: io.Discard, err: &errb}, in, out, failOnPath{realEngine(t)}, rep)
	if err != nil {
		t.Fatalf("anonymizeHARFile: %v", err)
	}
	if rep.exchanges != 2 || rep.failed != 1 || rep.firstErr == nil || !strings.Contains(rep.firstErr.Error(), "entry 1") {
		t.Errorf("report = %+v", rep)
	}
	b, _ := os.ReadFile(out)
	doc, err := har.Decode(bytes.NewReader(b))
	if err != nil || len(doc.Log.Entries) != 2 || bytes.Contains(b, []byte("/boom")) {
		t.Errorf("output: %d entries, %v, boom written: %v", len(doc.Log.Entries), err, bytes.Contains(b, []byte("/boom")))
	}
	var summary bytes.Buffer
	rep.print(&summary, model.DefaultAnonymizationProfile())
	if !strings.Contains(summary.String(), "left out") || !strings.Contains(summary.String(), "first failure: entry 1") {
		t.Errorf("summary does not report the failure:\n%s", summary.String())
	}

	// Every entry failing is an error, and nothing is written.
	in2 := writeCLIHAR(t, dir, []har.Entry{cliEntry("http://x.test/boom/1", at), cliEntry("http://x.test/boom/2", at)})
	out2 := filepath.Join(dir, "out2.har")
	rep2 := newAnonReport()
	if _, err := anonymizeHARFile(context.Background(), streams{in: strings.NewReader(""), out: io.Discard, err: &errb}, in2, out2, failOnPath{realEngine(t)}, rep2); err == nil || !strings.Contains(err.Error(), "none of the 2 entries") {
		t.Fatalf("all failed: %v", err)
	}
	if _, err := os.Stat(out2); err == nil {
		t.Error("an output file was written although nothing could be anonymized")
	}
}

func TestReplayHARWithOutOfRangeTimestampsFinishes(t *testing.T) {
	srv, hits := target(t, false)
	dir := t.TempDir()
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	in := writeCLIHAR(t, dir, []har.Entry{
		cliEntry("http://x.test/a", time.Time{}),                                   // .NET's 0001-01-01
		cliEntry("http://x.test/b", at),                                            //
		cliEntry("http://x.test/c", time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)), //
		cliEntry("http://x.test/d", at.Add(300*time.Millisecond)),
		cliEntry("http://x.test/e", time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC)),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	start := time.Now()
	code := run(ctx, []string{"replay", "--har", in, "--target", srv.URL, "--quiet"}, streams{in: strings.NewReader(""), out: &out, err: &errb})
	if code != exitOK || hits.Load() != 5 {
		t.Fatalf("exit %d, %d requests\n%s\n%s", code, hits.Load(), out.String(), errb.String())
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the replay took %v: a time that did not fit stalled the timeline", d)
	}
}

func TestImportReportsRepairedTimestamps(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	in := writeCLIHAR(t, dir, []har.Entry{cliEntry("http://x.test/a", at), cliEntry("http://x.test/b", time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC))})
	code, stdout, stderr := runCLI(t, "import", "--data", filepath.Join(dir, "m.db"), "--har", in, "--name", "t")
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Imported 2 exchanges") || !strings.Contains(stdout, "1 entry had a missing start time") {
		t.Errorf("stdout:\n%s", stdout)
	}
}

func TestReplayGateAndNumberValidation(t *testing.T) {
	srv, hits := target(t, false)
	base := []string{"replay", "--har", tinyHAR, "--target", srv.URL, "--quiet"}
	cases := []struct {
		name string
		args []string
		want string // in stderr
	}{
		{"max-p99 zero", []string{"--max-p99", "0"}, "--max-p99 must be a positive duration"},
		{"max-p99 zero seconds", []string{"--max-p99", "0s"}, "--max-p99 must be a positive duration"},
		{"max-p99 negative", []string{"--max-p99", "-5ms"}, "--max-p99 must be a positive duration"},
		{"max-error-rate negative", []string{"--max-error-rate", "-1"}, "between 0 and 1"},
		{"max-error-rate NaN", []string{"--max-error-rate", "NaN"}, "between 0 and 1"},
		{"max-error-rate Inf", []string{"--max-error-rate", "Inf"}, "between 0 and 1"},
		{"rate NaN", []string{"--rate", "NaN", "--duration", "1s"}, "must be numbers"},
		{"rate Inf", []string{"--rate", "Inf", "--duration", "1s"}, "must be numbers"},
		{"speed NaN", []string{"--speed", "NaN"}, "must be numbers"},
		{"speed Inf", []string{"--speed", "+Inf"}, "must be numbers"},
		{"error-amplification NaN", []string{"--error-amplification", "NaN"}, "must be numbers"},
		{"stage rate NaN", []string{"--stage", "10s:NaN"}, "must be numbers"},
		{"header with CRLF", []string{"--header", "X-A: a\r\nX-Evil: 1"}, "control characters"},
		{"header with LF", []string{"--header", "X-A: a\nX-Evil: 1"}, "control characters"},
		{"host-header with CRLF", []string{"--host-header", "a.test\r\nX-Evil: 1"}, "not a valid host"},
		{"host-header with a space", []string{"--host-header", "a b"}, "not a valid host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, append(append([]string{}, base...), tc.args...)...)
			if code != exitUsage {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, exitUsage, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr lacks %q:\n%s", tc.want, stderr)
			}
		})
	}
	if hits.Load() != 0 {
		t.Errorf("a rejected run sent %d requests", hits.Load())
	}

	// Option errors win over a source that cannot be opened.
	code, _, stderr := runCLI(t, "replay", "--data", filepath.Join(t.TempDir(), "missing.db"), "--recording", "nope",
		"--target", srv.URL, "--quiet", "--rate", "NaN", "--duration", "1s")
	if code != exitUsage || !strings.Contains(stderr, "must be numbers") {
		t.Errorf("bad option with a missing database: exit %d\n%s", code, stderr)
	}

	// The strict limits that are valid still work.
	if code, _, stderr := runCLI(t, append(append([]string{}, base...), "--speed", "1000", "--max-p99", "1h")...); code != exitOK {
		t.Errorf("a valid run: exit %d\n%s", code, stderr)
	}
}
