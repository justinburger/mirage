package replay

import (
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func pt(sec int64, requests int64) model.SeriesPoint {
	return model.SeriesPoint{
		T: 1_700_000_000_000 + sec*1000, Requests: requests, Errors: requests / 10,
		P50: float64(sec), P95: float64(sec) * 2, P99: float64(sec) * 3, Max: float64(sec) * 4,
		InFlight: sec, BytesIn: requests * 10, BytesOut: requests * 100,
		CorrectedP95: float64(sec) * 5, CorrectedP99: float64(sec) * 6,
	}
}

func TestMergePoints(t *testing.T) {
	a, b := pt(0, 100), pt(1, 50)
	m := mergePoints(a, b)
	want := model.SeriesPoint{
		T: a.T, Requests: 150, Errors: 15, P50: 1, P95: 2, P99: 3, Max: 4, InFlight: 1,
		BytesIn: 1500, BytesOut: 15000, CorrectedP95: 5, CorrectedP99: 6,
	}
	if m != want {
		t.Errorf("merged = %+v\nwant     %+v", m, want)
	}
	// Maxima do not depend on which side is larger.
	if m2 := mergePoints(b, a); m2.P99 != 3 || m2.Requests != 150 || m2.T != b.T {
		t.Errorf("merge(b, a) = %+v", m2)
	}
}

func TestSeriesBufBelowTheCapKeepsEverything(t *testing.T) {
	s := newSeriesBuf(100)
	for i := int64(0); i < 100; i++ {
		s.add(pt(i, 1))
	}
	got := s.all()
	if len(got) != 100 || got[0].T != pt(0, 1).T || got[99].T != pt(99, 1).T {
		t.Errorf("%d points, first %d last %d", len(got), got[0].T, got[len(got)-1].T)
	}
}

func TestSeriesBufDownsamplesAboveTheCap(t *testing.T) {
	for _, total := range []int64{101, 150, 200, 201, 399, 400, 401, 1000, 5000} {
		s := newSeriesBuf(100)
		var sum int64
		for i := int64(0); i < total; i++ {
			s.add(pt(i, i+1))
			sum += i + 1
		}
		got := s.all()
		if len(got) > 101 {
			t.Fatalf("total %d: %d points kept, cap 100", total, len(got))
		}
		// Nothing is lost: counts sum to the total and the beginning is kept.
		var gotSum int64
		for _, p := range got {
			gotSum += p.Requests
		}
		if gotSum != sum {
			t.Errorf("total %d: requests sum %d, want %d", total, gotSum, sum)
		}
		if got[0].T != pt(0, 0).T {
			t.Errorf("total %d: first point at %d, the beginning was not kept", total, got[0].T)
		}
		// Timestamps strictly increase and spacing is a uniform multiple of
		// the stride, except possibly the trailing partial group.
		for i := 1; i < len(got); i++ {
			if got[i].T <= got[i-1].T {
				t.Fatalf("total %d: timestamps not increasing at %d", total, i)
			}
		}
	}
}

func TestSeriesBufStrideAndResolution(t *testing.T) {
	s := newSeriesBuf(8)
	for i := int64(0); i < 9; i++ { // the 9th point exceeds the cap of 8
		s.add(pt(i, 1))
	}
	// 8 pairs ... 9 raw points: compaction at the 9th merges pairs, leaving
	// 4 two-second points and the 9th as a half-filled group.
	got := s.all()
	if len(got) != 5 {
		t.Fatalf("%d points, want 5: %+v", len(got), got)
	}
	for i := 0; i < 4; i++ {
		if got[i].Requests != 2 || got[i].T != pt(int64(2*i), 0).T {
			t.Errorf("point %d = %+v, want a merged pair starting at second %d", i, got[i], 2*i)
		}
	}
	if got[4].Requests != 1 || got[4].T != pt(8, 0).T {
		t.Errorf("trailing partial group = %+v", got[4])
	}
	// The next raw point completes the group at the new stride of 2.
	s.add(pt(9, 1))
	got = s.all()
	if len(got) != 5 || got[4].Requests != 2 {
		t.Errorf("after completing: %+v", got)
	}
}

func TestSeriesBufSince(t *testing.T) {
	s := newSeriesBuf(100)
	for i := int64(0); i < 10; i++ {
		s.add(pt(i, 1))
	}
	if got := s.since(pt(7, 0).T); len(got) != 3 || got[0].T != pt(7, 0).T {
		t.Errorf("since(7) = %d points", len(got))
	}
	if got := s.since(pt(100, 0).T); got == nil || len(got) != 0 {
		t.Errorf("since(future) = %#v, want empty non-nil", got)
	}
	if got := newSeriesBuf(5).all(); got == nil || len(got) != 0 {
		t.Errorf("empty buffer = %#v, want empty non-nil", got)
	}
}

func TestSeriesBufUsesModelCapInRunner(t *testing.T) {
	r, err := newRunner(openStore(t), baseCfg("rec", "http://t"), RunOptions{Collector: newTestCollector()})
	if err != nil {
		t.Fatal(err)
	}
	if r.series.max != model.MaxReplaySeries {
		t.Errorf("series cap = %d, want %d", r.series.max, model.MaxReplaySeries)
	}
}

func BenchmarkSeriesAdd(b *testing.B) {
	s := newSeriesBuf(model.MaxReplaySeries)
	p := pt(0, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.T += 1000
		s.add(p)
	}
}
