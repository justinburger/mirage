package har

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

func benchExchange(bodyBytes int) *model.Exchange {
	body := bytes.Repeat([]byte(`{"k":"v","n":12345},`), bodyBytes/20+1)[:bodyBytes]
	return &model.Exchange{
		StartedAt: time.Date(2024, 3, 5, 17, 20, 31, 0, time.UTC),
		Duration:  model.Duration(12 * time.Millisecond), TTFB: model.Duration(8 * time.Millisecond),
		Request: model.Request{Method: "GET", Scheme: "https", Host: "api.example.com", Path: "/v1/items/1", RawQuery: "a=1&b=2", Proto: "HTTP/1.1",
			Header: http.Header{"Accept": {"application/json"}, "User-Agent": {"loadgen/1.0"}, "Cookie": {"sid=abc"}}},
		Response: model.Response{Status: 200, Proto: "HTTP/1.1",
			Header:   http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}},
			Body:     body,
			BodySize: int64(len(body))},
	}
}

func BenchmarkFromExchange(b *testing.B) {
	for _, size := range []int{0, 1 << 10, 64 << 10} {
		ex := benchExchange(size)
		b.Run(fmt.Sprintf("body=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				_ = FromExchange(ex)
			}
		})
	}
}

func BenchmarkEncoderWrite(b *testing.B) {
	for _, size := range []int{0, 1 << 10, 64 << 10} {
		ex := benchExchange(size)
		b.Run(fmt.Sprintf("body=%d", size), func(b *testing.B) {
			enc, err := NewEncoder(io.Discard, "bench")
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				if err := enc.Write(ex); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkEncoderWriteParallel(b *testing.B) {
	enc, err := NewEncoder(io.Discard, "bench")
	if err != nil {
		b.Fatal(err)
	}
	ex := benchExchange(1 << 10)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := enc.Write(ex); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkToExchange(b *testing.B) {
	for _, size := range []int{0, 1 << 10, 64 << 10} {
		e := FromExchange(benchExchange(size))
		b.Run(fmt.Sprintf("body=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				if _, err := ToExchange(&e); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	var buf bytes.Buffer
	enc, _ := NewEncoder(&buf, "bench")
	ex := benchExchange(1 << 10)
	for range 1000 {
		_ = enc.Write(ex)
	}
	_ = enc.Close()
	doc := buf.Bytes()
	b.Run("Decode/1000 entries", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(doc)))
		for b.Loop() {
			if _, err := Decode(bytes.NewReader(doc)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("DecodeEntries/1000 entries", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(doc)))
		for b.Loop() {
			n := 0
			if _, err := DecodeEntries(bytes.NewReader(doc), func(int, *Entry) error { n++; return nil }); err != nil || n != 1000 {
				b.Fatal(err, n)
			}
		}
	})
}

func BenchmarkTimestamp(b *testing.B) {
	for _, in := range []string{"2024-03-05T17:20:31.123Z", "2024-03-05T18:20:31.123+0100", "2024-03-05 17:20:31"} {
		b.Run(strings.ReplaceAll(in, " ", "_"), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := parseTimestamp(in); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
