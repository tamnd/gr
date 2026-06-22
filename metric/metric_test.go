package metric

import (
	"math"
	"sync"
	"testing"
)

// TestCounterAddsAcrossShards confirms a counter sums every shard's cell, so concurrent
// increments from many goroutines (landing on different shards) all count.
func TestCounterAddsAcrossShards(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("gr_test_total", "test counter", "", Labels{"kind": "read"})

	const goroutines = 8
	const each = 10000
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				c.Inc()
			}
		}()
	}
	wg.Wait()

	if got := c.Value(); got != goroutines*each {
		t.Errorf("counter = %d, want %d", got, goroutines*each)
	}
}

// TestCounterSameSeriesIsShared confirms that registering the same name and label set twice
// returns the same counter, so two subsystems naming one metric increment one series.
func TestCounterSameSeriesIsShared(t *testing.T) {
	r := NewRegistry()
	a := r.Counter("gr_test_total", "test", "", Labels{"kind": "read"})
	b := r.Counter("gr_test_total", "test", "", Labels{"kind": "read"})
	a.Add(5)
	b.Add(7)
	if got := a.Value(); got != 12 {
		t.Errorf("shared counter = %d, want 12", got)
	}
}

// TestCounterDistinctLabelsAreDistinctSeries confirms a different label set under one name is
// a separate series, the per-dimension split labels exist for.
func TestCounterDistinctLabelsAreDistinctSeries(t *testing.T) {
	r := NewRegistry()
	read := r.Counter("gr_queries_total", "q", "", Labels{"kind": "read"})
	write := r.Counter("gr_queries_total", "q", "", Labels{"kind": "write"})
	read.Add(3)
	write.Add(4)
	if read.Value() != 3 || write.Value() != 4 {
		t.Errorf("read=%d write=%d, want 3 and 4", read.Value(), write.Value())
	}
}

// TestCounterTypeConflictPanics confirms reusing a name as a different type is caught at
// registration, a catalogue programming error surfaced at startup.
func TestCounterTypeConflictPanics(t *testing.T) {
	r := NewRegistry()
	r.Counter("gr_dup", "c", "", nil)
	defer func() {
		if recover() == nil {
			t.Error("re-registering a counter name as a gauge should panic")
		}
	}()
	r.Gauge("gr_dup", "g", "", nil)
}

// TestCounterLabelSchemaConflictPanics confirms reusing a name with different label names is
// caught, the schema discipline of doc 20 §2.6.
func TestCounterLabelSchemaConflictPanics(t *testing.T) {
	r := NewRegistry()
	r.Counter("gr_q_total", "c", "", Labels{"kind": "read"})
	defer func() {
		if recover() == nil {
			t.Error("re-registering with a different label schema should panic")
		}
	}()
	r.Counter("gr_q_total", "c", "", Labels{"status": "ok"})
}

// TestGaugeUpAndDown confirms a gauge moves both ways, unlike a counter.
func TestGaugeUpAndDown(t *testing.T) {
	r := NewRegistry()
	g := r.Gauge("gr_inflight", "in flight", "", nil)
	g.Inc()
	g.Inc()
	g.Inc()
	g.Dec()
	g.Add(10)
	g.Set(2)
	if got := g.Value(); got != 2 {
		t.Errorf("gauge = %d, want 2 after Set", got)
	}
}

// TestComputedGauge confirms a computed gauge reads its function and ignores stored writes.
func TestComputedGauge(t *testing.T) {
	r := NewRegistry()
	n := int64(42)
	g := r.ComputedGauge("gr_cache_pages", "pages", "", nil, func() int64 { return n })
	if got := g.Value(); got != 42 {
		t.Errorf("computed gauge = %d, want 42", got)
	}
	g.Set(0) // ignored: a computed gauge has no stored cell
	n = 99
	if got := g.Value(); got != 99 {
		t.Errorf("computed gauge = %d, want 99 after the source changed", got)
	}
}

// TestHistogramQuantile confirms quantiles read off the buckets land in the right bucket and
// interpolate within it.
func TestHistogramQuantile(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("gr_lat_seconds", "latency", "seconds",
		[]float64{1, 2, 4, 8, 16}, Labels{"kind": "read"})
	// 100 observations spread one per integer value 1..100 would be ideal, but keep it within
	// the bucket range: put 10 at each of 1, 2, 4, 8, 16 and 50 above 16 (the +Inf bucket).
	for i := 0; i < 10; i++ {
		h.Observe(1)
		h.Observe(2)
		h.Observe(4)
		h.Observe(8)
		h.Observe(16)
	}
	for i := 0; i < 50; i++ {
		h.Observe(100) // lands in +Inf
	}
	v := r.Snapshot().Histogram("gr_lat_seconds", Labels{"kind": "read"})
	if v.Count != 100 {
		t.Fatalf("count = %d, want 100", v.Count)
	}
	if v.Sum == 0 {
		t.Error("sum should be non-zero")
	}
	// The median (p50) sits at observation 50, which is in the +Inf top bucket (the first 50
	// observations fill buckets 1..16 exactly), so the quantile reports the last finite bound.
	if q := v.Quantile(0.50); q < 16 {
		t.Errorf("p50 = %v, want at least the 16 bound", q)
	}
	// The mean is sum/count.
	if got := v.Mean(); math.Abs(got-v.Sum/100) > 1e-9 {
		t.Errorf("mean = %v, want %v", got, v.Sum/100)
	}
}

// TestHistogramObserveConcurrent confirms concurrent observations all count and the sum holds
// under the CAS loop.
func TestHistogramObserveConcurrent(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("gr_obs", "obs", "", []float64{1, 10, 100}, nil)
	const goroutines = 8
	const each = 5000
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				h.Observe(5)
			}
		}()
	}
	wg.Wait()
	v := r.Snapshot().Histogram("gr_obs", nil)
	if v.Count != goroutines*each {
		t.Errorf("count = %d, want %d", v.Count, goroutines*each)
	}
	if want := float64(goroutines*each) * 5; math.Abs(v.Sum-want) > 1e-6 {
		t.Errorf("sum = %v, want %v", v.Sum, want)
	}
}

// TestSnapshotDerivations confirms HitRate and Rate compute the standard ratios off a
// snapshot, the derivations doc 20 §7.3 promises.
func TestSnapshotDerivations(t *testing.T) {
	r := NewRegistry()
	hits := r.Counter("gr_cache_hits", "hits", "", Labels{"cache": "page"})
	misses := r.Counter("gr_cache_misses", "misses", "", Labels{"cache": "page"})
	hits.Add(90)
	misses.Add(10)
	snap := r.Snapshot()
	if got := snap.HitRate("gr_cache", Labels{"cache": "page"}); math.Abs(got-0.9) > 1e-9 {
		t.Errorf("hit rate = %v, want 0.9", got)
	}

	q := r.Counter("gr_queries_total", "q", "", Labels{"status": "ok"})
	q.Add(100)
	prev := r.Snapshot()
	q.Add(50)
	now := r.Snapshot()
	if got := now.Rate("gr_queries_total", Labels{"status": "ok"}, prev, 10); math.Abs(got-5) > 1e-9 {
		t.Errorf("rate = %v, want 5 per second", got)
	}
}

// TestSnapshotStableOrder confirms the snapshot orders series by name then label key, so the
// exposition output is stable across scrapes.
func TestSnapshotStableOrder(t *testing.T) {
	r := NewRegistry()
	r.Counter("gr_b", "b", "", nil)
	r.Counter("gr_a", "a", "", Labels{"kind": "write"})
	r.Counter("gr_a", "a", "", Labels{"kind": "read"})
	got := r.Snapshot().Metrics()
	if len(got) != 3 {
		t.Fatalf("metrics = %d, want 3", len(got))
	}
	if got[0].Name != "gr_a" || got[0].Labels["kind"] != "read" {
		t.Errorf("first = %v, want gr_a kind=read", got[0])
	}
	if got[1].Name != "gr_a" || got[1].Labels["kind"] != "write" {
		t.Errorf("second = %v, want gr_a kind=write", got[1])
	}
	if got[2].Name != "gr_b" {
		t.Errorf("third = %v, want gr_b", got[2])
	}
}
