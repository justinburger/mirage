package ops

import (
	"bytes"
	"context"
	"testing"

	"github.com/justinburger/mirage/internal/pathnorm"
)

const benchExchanges = 2000

func BenchmarkCopyAnonymized(b *testing.B) {
	ctx := context.Background()
	st := newStore(b)
	src := seed(b, st, "bench", benchExchanges)
	eng, err := LoadEngine(ctx, st, "")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst, err := CopyAnonymized(ctx, st, src.ID, eng, "copy", nil)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := st.DeleteRecording(ctx, dst.ID); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	b.ReportMetric(float64(benchExchanges)*float64(b.N)/b.Elapsed().Seconds(), "exchanges/s")
}

func BenchmarkExportHAR(b *testing.B) {
	ctx := context.Background()
	st := newStore(b)
	src := seed(b, st, "bench", benchExchanges)
	var buf bytes.Buffer
	if err := ExportHAR(ctx, st, src.ID, &buf, nil, "bench"); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(buf.Len()))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		if err := ExportHAR(ctx, st, src.ID, &buf, nil, "bench"); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(benchExchanges)*float64(b.N)/b.Elapsed().Seconds(), "exchanges/s")
}

func BenchmarkExportHARAnonymized(b *testing.B) {
	ctx := context.Background()
	st := newStore(b)
	src := seed(b, st, "bench", benchExchanges)
	eng, err := LoadEngine(ctx, st, "")
	if err != nil {
		b.Fatal(err)
	}
	var buf bytes.Buffer
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		if err := ExportHAR(ctx, st, src.ID, &buf, eng, "bench"); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(benchExchanges)*float64(b.N)/b.Elapsed().Seconds(), "exchanges/s")
}

func BenchmarkImportHAR(b *testing.B) {
	ctx := context.Background()
	st := newStore(b)
	src := seed(b, st, "bench", benchExchanges)
	var doc bytes.Buffer
	if err := ExportHAR(ctx, st, src.ID, &doc, nil, "bench"); err != nil {
		b.Fatal(err)
	}
	raw := doc.Bytes()
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec, err := ImportHAR(ctx, st, bytes.NewReader(raw), "import", nil, pathnorm.Default)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := st.DeleteRecording(ctx, rec.ID); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	b.ReportMetric(float64(benchExchanges)*float64(b.N)/b.Elapsed().Seconds(), "exchanges/s")
}
