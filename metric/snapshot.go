package metric

// Snapshot is an immutable, point-in-time copy of the whole registry (doc 20 §7.3): every
// counter value, every gauge value, every histogram's buckets, sum, and count, taken
// atomically enough to be internally consistent (doc 20 §22.6). It is what db.Metrics()
// returns and what the Prometheus and expvar surfaces render, so a test, an embedder, and an
// operator all read the same numbers (doc 20 §19.2).
type Snapshot struct {
	metrics []MetricSnapshot
}

// MetricSnapshot is one series in a snapshot: its name, type, help, unit, and label set, plus
// the value for its type (the Counter and Gauge fields hold zero for the other types, and the
// Histogram field is the zero HistogramValue unless the type is a histogram).
type MetricSnapshot struct {
	Name      string
	Type      Type
	Help      string
	Unit      string
	Labels    Labels
	Counter   uint64         // valid when Type is TypeCounter
	Gauge     int64          // valid when Type is TypeGauge
	Histogram HistogramValue // valid when Type is TypeHistogram
}

// Metrics returns every series in the snapshot, sorted by name then label key (doc 20 §7.3),
// the iteration surface the Prometheus and expvar encoders walk.
func (s Snapshot) Metrics() []MetricSnapshot {
	return s.metrics
}

// Counter returns the value of the counter named name with exactly the given labels (doc 20
// §7.3), or zero if no such series is in the snapshot. The labels must match the registered
// set, since a metric's identity is its name plus its labels.
func (s Snapshot) Counter(name string, labels Labels) uint64 {
	if m := s.find(name, labels); m != nil && m.Type == TypeCounter {
		return m.Counter
	}
	return 0
}

// Gauge returns the value of the gauge named name with the given labels (doc 20 §7.3), or
// zero if absent.
func (s Snapshot) Gauge(name string, labels Labels) int64 {
	if m := s.find(name, labels); m != nil && m.Type == TypeGauge {
		return m.Gauge
	}
	return 0
}

// Histogram returns the histogram value named name with the given labels (doc 20 §7.3), so a
// caller computes quantiles off it: snap.Histogram(name, labels).Quantile(0.99). An absent
// series returns the zero HistogramValue, whose Quantile is zero.
func (s Snapshot) Histogram(name string, labels Labels) HistogramValue {
	if m := s.find(name, labels); m != nil && m.Type == TypeHistogram {
		return m.Histogram
	}
	return HistogramValue{}
}

// HitRate is the convenience derivation doc 20 §7.3 names: for a pair of counters under one
// base name suffixed _hits and _misses (the cache catalogue's shape), the hit fraction
// hits/(hits+misses) with the given labels, so an embedder reads a cache's hit rate without
// re-deriving it. No observations is a zero rate, not a divide by zero.
func (s Snapshot) HitRate(base string, labels Labels) float64 {
	hits := s.Counter(base+"_hits", labels)
	misses := s.Counter(base+"_misses", labels)
	total := hits + misses
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

// Rate computes the per-second rate of a counter against a previous snapshot over the elapsed
// seconds between them (doc 20 §7.3), the same derivation the monitoring backend's rate()
// performs, offered here so a test or an embedder reads the rate the operator sees. A
// non-positive interval or a counter that went backwards (a restart) returns zero.
func (s Snapshot) Rate(name string, labels Labels, prev Snapshot, seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	now := s.Counter(name, labels)
	was := prev.Counter(name, labels)
	if now < was {
		return 0
	}
	return float64(now-was) / seconds
}

// find returns the series matching name and labels exactly, or nil. It is a linear scan over
// the snapshot's metrics, which is fine: a snapshot is read at exposition or in a test, not on
// the hot path, and the catalogue is bounded (doc 20 §2.6).
func (s Snapshot) find(name string, labels Labels) *MetricSnapshot {
	want := labelKey(labels)
	for i := range s.metrics {
		if s.metrics[i].Name == name && labelKey(s.metrics[i].Labels) == want {
			return &s.metrics[i]
		}
	}
	return nil
}
