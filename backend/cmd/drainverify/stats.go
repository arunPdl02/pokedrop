package main

import (
	"math"
	"sort"
)

// stats summarizes a set of durations in milliseconds.
type stats struct {
	N     int     `json:"n"`
	MinMs float64 `json:"minMs"`
	P50Ms float64 `json:"p50Ms"`
	P95Ms float64 `json:"p95Ms"`
	MaxMs float64 `json:"maxMs"`
}

// percentile is the nearest-rank percentile of an ascending slice: the smallest value with at least
// p percent of the values at or below it. Every number in the benchmark uses this one method.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	rank = min(max(rank, 1), len(sorted))
	return sorted[rank-1]
}

func summarize(values []float64) stats {
	if len(values) == 0 {
		return stats{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return stats{
		N:     len(sorted),
		MinMs: sorted[0],
		P50Ms: percentile(sorted, 50),
		P95Ms: percentile(sorted, 95),
		MaxMs: sorted[len(sorted)-1],
	}
}
