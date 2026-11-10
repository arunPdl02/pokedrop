package main

import "testing"

func TestPercentileIsNearestRank(t *testing.T) {
	// 1..20: p50 is the 10th value, p95 the 19th (ceil(0.95 * 20) = 19).
	var v []float64
	for i := 1; i <= 20; i++ {
		v = append(v, float64(i))
	}
	for p, want := range map[float64]float64{0: 1, 1: 1, 50: 10, 90: 18, 95: 19, 99: 20, 100: 20} {
		if got := percentile(v, p); got != want {
			t.Errorf("p%v = %v, want %v", p, got, want)
		}
	}
	if percentile([]float64{7}, 95) != 7 || percentile(nil, 50) != 0 {
		t.Error("a single value is every percentile; no values is zero")
	}
}

func TestSummarizeDoesNotReorderItsInput(t *testing.T) {
	in := []float64{30, 10, 20}
	got := summarize(in)
	if got.N != 3 || got.MinMs != 10 || got.P50Ms != 20 || got.P95Ms != 30 || got.MaxMs != 30 {
		t.Fatalf("got %+v", got)
	}
	if in[0] != 30 || in[1] != 10 || in[2] != 20 {
		t.Errorf("input was reordered: %v", in)
	}
	if (summarize(nil) != stats{}) {
		t.Error("no values must summarize to zeros")
	}
}
