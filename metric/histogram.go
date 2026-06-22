package metric

import (
	"math"
	"sort"
	"sync/atomic"
)

// Histogram is a distribution of observations bucketed by value (doc 20 §2.5): query latency,
// fsync latency, expand fan-out, group-commit batch size. The operator question for these is
// the distribution (the p50, the p99, the p999), never the mean alone, because the mean hides
// the tail and the tail is the product (doc 20 §2.5, §00 §11).
//
// The storage is fixed regardless of observation count (doc 20 §2.5, §22.4): a set of buckets,
// each an upper bound and a cumulative-on-read counter, plus a sum and a count. An observation
// finds its bucket, adds to that bucket, adds the value to sum, and increments count, all with
// atomic adds and no allocation. The quantile is not stored; it is computed from the buckets
// at read time, so a histogram costs per-bucket, not per-observation, in memory, which is what
// makes it cheap to keep always-on for millions of observations.
type Histogram struct {
	bounds []float64       // upper bounds, ascending; bucket i counts values in (bounds[i-1], bounds[i]]
	counts []atomic.Uint64 // one counter per bound, the count of observations at or below that bound
	sum    atomic.Uint64   // total of observed values, a float64 bit-cast, updated by CAS (doc 20 §22.4)
	count  atomic.Uint64   // total number of observations
}

// newHistogram builds a histogram over the given upper bounds (doc 20 §2.5). The bounds are
// sorted and de-duplicated so the bucket search is a clean binary search and a caller need
// not pre-sort. An empty bounds slice yields a single catch-all bucket at +Inf, so a
// histogram always has somewhere to put an observation.
func newHistogram(bounds []float64) *Histogram {
	b := append([]float64(nil), bounds...)
	sort.Float64s(b)
	// De-duplicate so two equal bounds do not create a zero-width bucket.
	out := b[:0]
	for i, v := range b {
		if i == 0 || v != b[i-1] {
			out = append(out, v)
		}
	}
	b = out
	if len(b) == 0 || !math.IsInf(b[len(b)-1], 1) {
		b = append(b, math.Inf(1)) // the catch-all top bucket for values past the last finite bound
	}
	return &Histogram{bounds: b, counts: make([]atomic.Uint64, len(b))}
}

// Observe records one value (doc 20 §2.5, §22.4): it finds the lowest bucket whose bound is
// at or above the value and increments that bucket, adds the value to sum through a CAS loop
// (atomic has no native float add), and increments count. The bucket counts are kept
// non-cumulative in storage and made cumulative at read (Snapshot), so an observation touches
// exactly one bucket cell rather than every bucket at or above the value.
func (h *Histogram) Observe(v float64) {
	i := h.bucket(v)
	h.counts[i].Add(1)
	h.count.Add(1)
	h.addSum(v)
}

// bucket returns the index of the lowest bucket whose upper bound is at or above v, a binary
// search over the small sorted bounds array (doc 20 §22.4). A value past every finite bound
// lands in the +Inf catch-all, the last bucket.
func (h *Histogram) bucket(v float64) int {
	return sort.Search(len(h.bounds), func(i int) bool { return v <= h.bounds[i] })
}

// addSum adds v to the float64 sum held bit-cast in a uint64, the one place a histogram
// observation may spin (doc 20 §22.4): atomic has no float add, so it is a load-add-CAS loop.
// The contention is per-histogram and the spin is rare, so the cost is negligible against the
// operation the observation measures.
func (h *Histogram) addSum(v float64) {
	for {
		old := h.sum.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if h.sum.CompareAndSwap(old, next) {
			return
		}
	}
}

// snapshot reads the histogram into an immutable HistogramValue (doc 20 §7.3): the cumulative
// bucket counts, the sum, and the count, read consistently enough within the one metric that
// the value is coherent (doc 20 §22.6). The stored per-bucket counts are folded into the
// cumulative form Prometheus and the quantile math expect here, on the read side.
func (h *Histogram) snapshot() HistogramValue {
	bounds := append([]float64(nil), h.bounds...)
	cumulative := make([]uint64, len(h.counts))
	var running uint64
	for i := range h.counts {
		running += h.counts[i].Load()
		cumulative[i] = running
	}
	return HistogramValue{
		Bounds: bounds,
		Counts: cumulative,
		Sum:    math.Float64frombits(h.sum.Load()),
		Count:  h.count.Load(),
	}
}

// HistogramValue is an immutable point-in-time view of a histogram (doc 20 §7.3): the bucket
// upper bounds, the cumulative count at or below each bound, the sum of observed values, and
// the total count. The quantile and the mean are derived from it, not stored.
type HistogramValue struct {
	Bounds []float64 // bucket upper bounds, ascending, last is +Inf
	Counts []uint64  // cumulative count at or below each bound, last equals Count
	Sum    float64   // sum of all observed values
	Count  uint64    // total number of observations
}

// Quantile computes the q-th quantile (0..1) from the buckets by interpolating within the
// bucket that holds it (doc 20 §2.5, §22.4): the resolution of the result is the width of
// that bucket, which is why the bucket layout puts resolution at the tail where the operator
// looks. An empty histogram returns 0; a quantile in the +Inf top bucket returns the last
// finite bound, since the true value is unknown past it.
func (h HistogramValue) Quantile(q float64) float64 {
	if h.Count == 0 {
		return 0
	}
	if q <= 0 {
		return 0
	}
	if q >= 1 {
		q = 1
	}
	target := q * float64(h.Count)
	var prevCount uint64
	var prevBound float64
	for i, bound := range h.Bounds {
		if float64(h.Counts[i]) >= target {
			if math.IsInf(bound, 1) {
				// The quantile falls in the catch-all: the true value is unknown past the
				// last finite bound, so report that bound as the best lower estimate.
				if i > 0 {
					return h.Bounds[i-1]
				}
				return 0
			}
			// Linear interpolation within bucket i, between its lower and upper bound, by how
			// far the target falls into the bucket's own count.
			inBucket := target - float64(prevCount)
			bucketCount := float64(h.Counts[i]) - float64(prevCount)
			frac := 0.0
			if bucketCount > 0 {
				frac = inBucket / bucketCount
			}
			lower := prevBound
			if i == 0 {
				lower = 0
			}
			return lower + frac*(bound-lower)
		}
		prevCount = h.Counts[i]
		prevBound = bound
	}
	return prevBound
}

// Mean is the sum over the count (doc 20 §2.5), the rare view an operator wants when the
// distribution is not the question. Zero observations is a zero mean.
func (h HistogramValue) Mean() float64 {
	if h.Count == 0 {
		return 0
	}
	return h.Sum / float64(h.Count)
}
