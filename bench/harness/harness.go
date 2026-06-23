// Package harness provides the shared benchmark runner for gr micro-benchmarks
// (doc 22 §8.3). It implements percentile computation, custom metric reporting,
// and warm-up helpers on top of Go's testing.B.
package harness

import (
	"math"
	"sort"
	"testing"
)

// Percentiles computes p50, p95, p99, and p999 from a slice of sample values
// (in nanoseconds or any comparable unit).  The input slice is sorted in-place.
func Percentiles(samples []float64) (p50, p95, p99, p999 float64) {
	if len(samples) == 0 {
		return 0, 0, 0, 0
	}
	sort.Float64s(samples)
	p50 = samples[percentileIdx(len(samples), 0.50)]
	p95 = samples[percentileIdx(len(samples), 0.95)]
	p99 = samples[percentileIdx(len(samples), 0.99)]
	p999 = samples[percentileIdx(len(samples), 0.999)]
	return
}

func percentileIdx(n int, p float64) int {
	idx := int(math.Ceil(p*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return idx
}

// Mean returns the arithmetic mean of samples.
func Mean(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, v := range samples {
		sum += v
	}
	return sum / float64(len(samples))
}

// ReportLatencyPercentiles calls b.ReportMetric with the standard
// gr percentile metrics (p50-ns, p95-ns, p99-ns, p999-ns).
// samples must be in nanoseconds.
func ReportLatencyPercentiles(b *testing.B, samples []float64) {
	b.Helper()
	p50, p95, p99, p999 := Percentiles(samples)
	b.ReportMetric(p50, "p50-ns")
	b.ReportMetric(p95, "p95-ns")
	b.ReportMetric(p99, "p99-ns")
	b.ReportMetric(p999, "p999-ns")
}

// ReportThroughput reports a throughput metric (unit: ops/s).
func ReportThroughput(b *testing.B, opsPerSec float64, label string) {
	b.Helper()
	b.ReportMetric(opsPerSec, label+"-ops/s")
}

// Warmup runs fn for at least warmupN iterations before starting the actual
// measurement, using b.N from the outer loop.  The result of fn (if any cost
// avoidance is needed) is discarded.
func Warmup(b *testing.B, warmupN int, fn func()) {
	b.Helper()
	b.StopTimer()
	for range warmupN {
		fn()
	}
	b.StartTimer()
}
