package ops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/justinburger/mirage/internal/har"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
)

func TestHARRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	src := seed(t, st, "orders", 1100)

	var buf bytes.Buffer
	if err := ExportHAR(ctx, st, src.ID, &buf, nil, "v9.9.9"); err != nil {
		t.Fatalf("ExportHAR: %v", err)
	}
	doc, err := har.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("exported document is not valid HAR: %v", err)
	}
	if len(doc.Log.Entries) != 1100 || doc.Log.Creator.Version != "v9.9.9" {
		t.Fatalf("exported %d entries, creator %+v", len(doc.Log.Entries), doc.Log.Creator)
	}

	dst, err := ImportHAR(ctx, st, bytes.NewReader(buf.Bytes()), "orders (reimported)", nil, pathnorm.Default)
	if err != nil {
		t.Fatalf("ImportHAR: %v", err)
	}
	if dst.Source != SourceHAR || dst.Anonymized || dst.ExchangeCount != 1100 || dst.ErrorCount != src.ErrorCount {
		t.Errorf("imported recording: source=%q anonymized=%v count=%d errors=%d (source errors %d)", dst.Source, dst.Anonymized, dst.ExchangeCount, dst.ErrorCount, src.ErrorCount)
	}
	if !strings.Contains(dst.Description, "1100 exchanges") || !strings.Contains(dst.Description, "Mirage") && !strings.Contains(dst.Description, "written by") {
		t.Errorf("Description = %q", dst.Description)
	}

	a, b := loadAll(t, st, src.ID), loadAll(t, st, dst.ID)
	if len(a) != len(b) {
		t.Fatalf("round trip changed the count: %d -> %d", len(a), len(b))
	}
	for i := range a {
		x, y := a[i], b[i]
		switch {
		case x.Request.Method != y.Request.Method, x.Request.Path != y.Request.Path, x.Request.RawQuery != y.Request.RawQuery:
			t.Fatalf("exchange %d request line: %s %s?%s -> %s %s?%s", i, x.Request.Method, x.Request.Path, x.Request.RawQuery, y.Request.Method, y.Request.Path, y.Request.RawQuery)
		case x.Response.Status != y.Response.Status:
			t.Fatalf("exchange %d status: %d -> %d", i, x.Response.Status, y.Response.Status)
		case !bytes.Equal(x.Request.Body, y.Request.Body), !bytes.Equal(x.Response.Body, y.Response.Body):
			t.Fatalf("exchange %d bodies differ", i)
		case x.Endpoint != y.Endpoint:
			t.Fatalf("exchange %d endpoint: %q -> %q", i, x.Endpoint, y.Endpoint)
		case !x.StartedAt.Equal(y.StartedAt):
			t.Fatalf("exchange %d start: %v -> %v", i, x.StartedAt, y.StartedAt)
		case x.Request.Header.Get("Authorization") != y.Request.Header.Get("Authorization"):
			t.Fatalf("exchange %d lost its Authorization header", i)
		}
		if d := time.Duration(x.Duration - y.Duration).Abs(); d > time.Microsecond {
			t.Fatalf("exchange %d duration: %v -> %v", i, x.Duration, y.Duration)
		}
	}
}

func TestExportHAR(t *testing.T) {
	ctx := context.Background()

	t.Run("anonymized export leaves the store alone", func(t *testing.T) {
		st := newStore(t)
		src := seed(t, st, "orders", 60)
		eng, err := LoadEngine(ctx, st, "")
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := ExportHAR(ctx, st, src.ID, &buf, eng, "test"); err != nil {
			t.Fatal(err)
		}
		for _, secret := range rawSecrets {
			if strings.Contains(buf.String(), secret) {
				t.Fatalf("anonymized export still contains %q", secret)
			}
		}
		if _, err := har.Decode(&buf); err != nil {
			t.Fatalf("not valid HAR: %v", err)
		}
		if !strings.Contains(dump(loadAll(t, st, src.ID)[0]), "Pa$$w0rd!x") {
			t.Error("the stored recording was anonymized by the export")
		}
	})

	t.Run("empty recording is a valid document", func(t *testing.T) {
		st := newStore(t)
		src := seed(t, st, "empty", 0)
		var buf bytes.Buffer
		if err := ExportHAR(ctx, st, src.ID, &buf, nil, "test"); err != nil {
			t.Fatal(err)
		}
		doc, err := har.Decode(&buf)
		if err != nil || len(doc.Log.Entries) != 0 {
			t.Fatalf("Decode: %v, %d entries", err, len(doc.Log.Entries))
		}
	})

	t.Run("unknown recording writes nothing", func(t *testing.T) {
		st := newStore(t)
		var buf bytes.Buffer
		err := ExportHAR(ctx, st, "rec_missing", &buf, nil, "test")
		if !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("err = %v, want not found", err)
		}
		if buf.Len() != 0 {
			t.Errorf("%d bytes written for an unknown recording", buf.Len())
		}
	})

	t.Run("failed export is left visibly unfinished", func(t *testing.T) {
		st := newStore(t)
		src := seed(t, st, "orders", 400)
		w := &failWriter{n: exportBufferSize + 1000}
		err := ExportHAR(ctx, st, src.ID, w, nil, "test")
		if !errors.Is(err, errInjected) {
			t.Fatalf("err = %v, want the writer error", err)
		}
		if w.buf.Len() == 0 {
			t.Error("nothing was flushed before the failure")
		}
		if _, err := har.Decode(bytes.NewReader(w.buf.Bytes())); err == nil {
			t.Error("a truncated export decoded as a valid HAR")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		st := newStore(t)
		src := seed(t, st, "orders", 50)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		var buf bytes.Buffer
		if err := ExportHAR(cctx, st, src.ID, &buf, nil, "test"); err == nil {
			t.Fatal("want an error")
		}
	})
}

func TestImportHARFixtures(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		file        string
		wantCount   int64
		wantSkipped int
		wantErrors  int64
		wantCreator string
	}{
		{file: "charles.har", wantCount: 1, wantCreator: "Charles Proxy"},
		{file: "chrome.har", wantCount: 4, wantSkipped: 1, wantErrors: 1, wantCreator: "WebInspector"},
		{file: "firefox.har", wantCount: 2, wantCreator: "Firefox"},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			f, err := os.Open(filepath.Join("..", "har", "testdata", tc.file))
			if err != nil {
				t.Skipf("fixture not available: %v", err)
			}
			defer f.Close()
			st := newStore(t)
			rec, err := ImportHAR(ctx, st, f, "", nil, nil)
			if err != nil {
				t.Fatalf("ImportHAR: %v", err)
			}
			if rec.ExchangeCount != tc.wantCount || rec.ErrorCount != tc.wantErrors {
				t.Errorf("count=%d errors=%d, want %d/%d", rec.ExchangeCount, rec.ErrorCount, tc.wantCount, tc.wantErrors)
			}
			if !strings.HasPrefix(rec.Name, "import 20") {
				t.Errorf("default name = %q", rec.Name)
			}
			if !strings.Contains(rec.Description, tc.wantCreator) {
				t.Errorf("Description %q does not name %q", rec.Description, tc.wantCreator)
			}
			if tc.wantSkipped > 0 && !strings.Contains(rec.Description, "Skipped 1 unsupported entry") {
				t.Errorf("Description %q does not count the skipped entry", rec.Description)
			}
			if tc.wantSkipped == 0 && strings.Contains(rec.Description, "Skipped") {
				t.Errorf("Description %q mentions skipped entries", rec.Description)
			}
			for _, ex := range loadAll(t, st, rec.ID) {
				if ex.Endpoint == "" || ex.Endpoint != pathnorm.Default.Endpoint(ex.Request.Method, ex.Request.Path) {
					t.Errorf("Endpoint = %q for %s %s", ex.Endpoint, ex.Request.Method, ex.Request.Path)
				}
			}
		})
	}
}

func TestImportHARAnonymizesAfterComputingEndpoints(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	eng, err := LoadEngine(ctx, st, "")
	if err != nil {
		t.Fatal(err)
	}
	e := entry("https://api.example.com/users/jane.doe@acme.io/orders/12345?token=abc123", 200)
	e.Request.Headers = []har.NameValue{{Name: "Authorization", Value: "Bearer secret-token-1"}}
	e.Response.Content = har.Content{MimeType: "application/json", Text: `{"email":"jane.doe@acme.io"}`, Size: 28}

	rec, err := ImportHAR(ctx, st, bytes.NewReader(harDoc(t, e)), "anon import", eng, pathnorm.Default)
	if err != nil {
		t.Fatalf("ImportHAR: %v", err)
	}
	if !rec.Anonymized || !strings.Contains(rec.Description, `profile "Default"`) {
		t.Errorf("Anonymized=%v Description=%q", rec.Anonymized, rec.Description)
	}
	exs := loadAll(t, st, rec.ID)
	if len(exs) != 1 {
		t.Fatalf("%d exchanges", len(exs))
	}
	ex := exs[0]
	if want := "GET /users/{email}/orders/{id}"; ex.Endpoint != want {
		t.Errorf("Endpoint = %q, want %q (computed before anonymizing)", ex.Endpoint, want)
	}
	if !ex.Anonymized {
		t.Error("exchange not flagged anonymized")
	}
	if strings.Contains(dump(ex), "acme.io") || strings.Contains(dump(ex), "secret-token-1") || strings.Contains(dump(ex), "abc123") {
		t.Errorf("raw values survived:\n%s", dump(ex))
	}
}

func TestImportHARSkipsUnsupportedEntries(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	doc := harDoc(t,
		entry("https://a.example.com/one", 200),
		entry("data:image/gif;base64,R0lGODlhAQABAAAAACw=", 200),
		entry("https://a.example.com/two", 201),
		entry("chrome-extension://abcdefgh/popup.js", 200),
		entry("https://a.example.com/three", 500),
	)
	rec, err := ImportHAR(ctx, st, bytes.NewReader(doc), "mixed", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ExchangeCount != 3 || rec.ErrorCount != 1 {
		t.Errorf("count=%d errors=%d, want 3/1", rec.ExchangeCount, rec.ErrorCount)
	}
	if !strings.Contains(rec.Description, "Imported 3 exchanges") || !strings.Contains(rec.Description, "Skipped 2 unsupported entries") {
		t.Errorf("Description = %q", rec.Description)
	}
}

func TestImportHARUsesNormalizer(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	n, err := pathnorm.New([]string{"/files/{path...}"})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := ImportHAR(ctx, st, bytes.NewReader(harDoc(t, entry("https://x.example.com/files/a/b/c.txt", 200))), "n", nil, n)
	if err != nil {
		t.Fatal(err)
	}
	if got := loadAll(t, st, rec.ID)[0].Endpoint; got != "GET /files/{path...}" {
		t.Errorf("Endpoint = %q", got)
	}
}

// Unreadable input is reported as an *InvalidHARError whose problems are plain
// sentences: no package prefixes, no "validation failed" wrappers.
func TestImportHARInvalidInputHasCleanProblems(t *testing.T) {
	ctx := context.Background()
	good := entry("https://a.example.com/ok", 200)
	bad := entry("not a url", 200)
	tests := []struct {
		name  string
		input io.Reader
		want  string
	}{
		{"empty input", strings.NewReader(""), "input is empty"},
		{"white space only", strings.NewReader(" \n\t "), "input is empty"},
		{"not JSON", strings.NewReader("hello"), "malformed JSON at byte"},
		{"HTML", strings.NewReader("<html>nope</html>"), "malformed JSON at byte"},
		{"JSON without log", strings.NewReader(`{"a":1}`), `no "log" object`},
		{"truncated document", bytes.NewReader(harDoc(t, good, good)[:150]), "end of input"},
		{"corrupt entry", bytes.NewReader(harDoc(t, good, bad)), "entry 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			_, err := ImportHAR(ctx, st, tc.input, "doomed", nil, nil)
			var ih *InvalidHARError
			if !errors.As(err, &ih) {
				t.Fatalf("err = %v (%T), want an *InvalidHARError", err, err)
			}
			if !errors.Is(err, model.ErrValidation) {
				t.Errorf("err = %v does not wrap model.ErrValidation", err)
			}
			if len(ih.Problems) != 1 || !strings.Contains(ih.Problems[0], tc.want) {
				t.Fatalf("problems = %q, want one mentioning %q", ih.Problems, tc.want)
			}
			for _, internal := range []string{"har:", "ops:", "validation failed", "invalid HAR", "unexpected EOF"} {
				if strings.Contains(ih.Problems[0], internal) {
					t.Errorf("problem %q leaks %q", ih.Problems[0], internal)
				}
			}
			if !strings.HasPrefix(err.Error(), "ops: import HAR: invalid HAR: ") {
				t.Errorf("Error() = %q", err)
			}
		})
	}
}

func TestImportHARErrors(t *testing.T) {
	ctx := context.Background()
	good := entry("https://a.example.com/ok", 200)
	bad := entry("not a url", 200)

	tests := []struct {
		name    string
		input   io.Reader
		wantIs  error
		wantMsg string
		notIs   error
	}{
		{name: "empty input", input: strings.NewReader(""), wantIs: model.ErrValidation},
		{name: "not JSON", input: strings.NewReader("<html>nope</html>"), wantIs: model.ErrValidation},
		{name: "JSON without log", input: strings.NewReader(`{"hello":"world"}`), wantIs: model.ErrValidation, wantMsg: "log"},
		{name: "log is not an object", input: strings.NewReader(`{"log":[1,2]}`), wantIs: model.ErrValidation},
		{name: "entries is not an array", input: strings.NewReader(`{"log":{"entries":{"a":1}}}`), wantIs: model.ErrValidation},
		{name: "truncated document", input: bytes.NewReader(harDoc(t, good, good)[:150]), wantIs: model.ErrValidation},
		{name: "corrupt entry names its index", input: bytes.NewReader(harDoc(t, good, bad)), wantIs: model.ErrValidation, wantMsg: "entry 1"},
		{name: "reader failure is not a validation error", input: io.MultiReader(bytes.NewReader(harDoc(t, good)[:100]), iotest.ErrReader(errInjected)), wantIs: errInjected, notIs: model.ErrValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			rec, err := ImportHAR(ctx, st, tc.input, "doomed", nil, nil)
			if err == nil {
				t.Fatalf("want an error, got recording %+v", rec)
			}
			if !errors.Is(err, tc.wantIs) {
				t.Errorf("err = %v, want it to wrap %v", err, tc.wantIs)
			}
			if tc.notIs != nil && errors.Is(err, tc.notIs) {
				t.Errorf("err = %v must not wrap %v", err, tc.notIs)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %q, want it to mention %q", err, tc.wantMsg)
			}
			if n := recordingCount(t, st); n != 0 {
				t.Errorf("%d recordings left behind after a failed import", n)
			}
		})
	}

	t.Run("store failure mid-import deletes the partial recording", func(t *testing.T) {
		real := newStore(t)
		entries := make([]har.Entry, 700)
		for i := range entries {
			entries[i] = entry("https://a.example.com/items/"+strings.Repeat("x", i%5), 200)
		}
		st := &flakyStore{Store: real, failAppendOn: 2}
		_, err := ImportHAR(ctx, st, bytes.NewReader(harDoc(t, entries...)), "doomed", nil, nil)
		if !errors.Is(err, errInjected) {
			t.Fatalf("err = %v, want the store error", err)
		}
		if errors.Is(err, model.ErrValidation) {
			t.Errorf("a store failure must not look like bad input: %v", err)
		}
		if n := recordingCount(t, real); n != 0 {
			t.Errorf("%d recordings left behind", n)
		}
	})

	t.Run("description update failure deletes the recording", func(t *testing.T) {
		real := newStore(t)
		st := &flakyStore{Store: real, failUpdate: true}
		_, err := ImportHAR(ctx, st, bytes.NewReader(harDoc(t, entry("https://a.example.com/x", 200))), "doomed", nil, nil)
		if !errors.Is(err, errInjected) {
			t.Fatalf("err = %v", err)
		}
		if n := recordingCount(t, real); n != 0 {
			t.Errorf("%d recordings left behind", n)
		}
	})

	t.Run("cancellation mid-import", func(t *testing.T) {
		st := newStore(t)
		entries := make([]har.Entry, 2000)
		for i := range entries {
			entries[i] = entry("https://a.example.com/n/1", 200)
		}
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		// Cancel as soon as the decoder asks for the second chunk of input.
		doc := harDoc(t, entries...)
		r := &cancelAfter{r: bytes.NewReader(doc), after: len(doc) / 2, cancel: cancel}
		_, err := ImportHAR(cctx, st, r, "doomed", nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if n := recordingCount(t, st); n != 0 {
			t.Errorf("%d recordings left behind", n)
		}
	})
}

// cancelAfter cancels a context once more than `after` bytes were read.
type cancelAfter struct {
	r      io.Reader
	after  int
	read   int
	cancel context.CancelFunc
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	if c.read > c.after {
		c.cancel()
	}
	return n, err
}
