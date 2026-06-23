package metric

import (
	"fmt"
	"sort"
	"sync"
)

// Registry is gr's single in-process metric registry (doc 20 §2.1): the federation point
// every subsystem registers what it owns into, and the one place the exposition surfaces read
// from. A subsystem registers a counter, gauge, or histogram by name, help text, unit, and
// label set, gets back a typed handle it holds and updates on its hot path, and the registry
// owns the lifetime (doc 20 §2.6): a metric lives from first registration to process exit, so
// a handle a subsystem holds stays valid for the life of the process.
//
// Registration is the only mutating operation under a lock; the update hot path (Counter.Inc,
// Histogram.Observe) is lock-free against the cells the handle points at (doc 20 §7.4), so the
// lock here guards the rarely-walked family map, not the per-update path.
type Registry struct {
	mu       sync.RWMutex
	families map[string]*family
}

// family is all the series of one metric name (doc 20 §7.2): the type, the help and unit for
// exposition, the schema label names every series must present, the histogram bucket bounds
// when the type is a histogram, and the series keyed by their canonical label string. A
// metric's identity is its name plus its label set, so one family holds many series, one per
// distinct label set.
type family struct {
	name       string
	typ        Type
	help       string
	unit       string
	labelNames []string
	bounds     []float64 // histogram bucket bounds; nil for counters and gauges
	series     map[string]*entry
}

// entry is one series: its label set and the typed metric instance behind it. Exactly one of
// counter, gauge, or histogram is non-nil, matching the family's type.
type entry struct {
	labels    Labels
	counter   *Counter
	gauge     *Gauge
	histogram *Histogram
}

// NewRegistry builds an empty registry (doc 20 §2.1). A gr database holds one, built at open
// and shared by every subsystem, so the catalogue an operator scrapes is the union of what
// every subsystem registered into this one registry.
func NewRegistry() *Registry {
	return &Registry{families: make(map[string]*family)}
}

// Counter registers (or returns the already-registered) counter series for name with the
// given label set (doc 20 §2.3, §7.2). The first call for a name fixes the family's type,
// help, unit, and label-name schema; a later call with the same name and the same label names
// returns the existing series (or makes a new series for a new label set under that name). It
// panics on a contradiction (a name reused with a different type or a different label schema),
// because that is a programming error in the catalogue, caught at startup not in production.
func (r *Registry) Counter(name, help, unit string, labels Labels) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.resolve(name, TypeCounter, help, unit, nil, labels)
	if e.counter == nil {
		e.counter = newCounter()
	}
	return e.counter
}

// Gauge registers (or returns) the settable gauge series for name with the given label set
// (doc 20 §2.4). The schema rules match Counter.
func (r *Registry) Gauge(name, help, unit string, labels Labels) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.resolve(name, TypeGauge, help, unit, nil, labels)
	if e.gauge == nil {
		e.gauge = newGauge()
	}
	return e.gauge
}

// ComputedGauge registers a gauge whose value fn produces at read time (doc 20 §2.4), for a
// quantity a subsystem reports on demand more cheaply than it keeps a stored gauge in step
// with. If the series already exists as a stored gauge, the function replaces its read path.
func (r *Registry) ComputedGauge(name, help, unit string, labels Labels, fn func() int64) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.resolve(name, TypeGauge, help, unit, nil, labels)
	e.gauge = newComputedGauge(fn)
	return e.gauge
}

// Histogram registers (or returns) the histogram series for name with the given bucket bounds
// and label set (doc 20 §2.5). The bounds are fixed at first registration; a later call for
// the same name reuses the family's bounds, so every series under one histogram name shares
// one bucket layout, the layout the catalogue chose for that metric.
func (r *Registry) Histogram(name, help, unit string, bounds []float64, labels Labels) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.resolve(name, TypeHistogram, help, unit, bounds, labels)
	if e.histogram == nil {
		e.histogram = newHistogram(e.familyBounds)
	}
	return e.histogram
}

// ComputedHistogram registers a histogram whose distribution fn produces at read time (doc 20
// §5.1): fn returns the current population of values and the snapshot buckets them against the
// family's bounds, for a point-in-time distribution over a live population (the version-chain
// lengths over current elements) rather than a stream of past events. Re-reading every scrape
// reflects the current state instead of accumulating, which an Observe-backed histogram cannot do.
func (r *Registry) ComputedHistogram(name, help, unit string, bounds []float64, labels Labels, fn func() []float64) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.resolve(name, TypeHistogram, help, unit, bounds, labels)
	e.histogram = newComputedHistogram(e.familyBounds, fn)
	return e.histogram
}

// resolve finds or creates the family for name and the series for labels, checking the type
// and label-schema consistency the catalogue depends on (doc 20 §2.6). It runs under the
// registry lock. It returns the series entry, carrying the family's histogram bounds so the
// caller builds the histogram with the family's layout, not a per-call one.
func (r *Registry) resolve(name string, typ Type, help, unit string, bounds []float64, labels Labels) *resolvedEntry {
	names := labelNames(labels)
	f := r.families[name]
	if f == nil {
		f = &family{
			name:       name,
			typ:        typ,
			help:       help,
			unit:       unit,
			labelNames: names,
			bounds:     append([]float64(nil), bounds...),
			series:     make(map[string]*entry),
		}
		r.families[name] = f
	} else {
		if f.typ != typ {
			panic(fmt.Sprintf("metric %q already registered as %s, re-registered as %s", name, f.typ, typ))
		}
		if !sameNames(f.labelNames, names) {
			panic(fmt.Sprintf("metric %q label schema %v does not match registered %v", name, names, f.labelNames))
		}
	}
	key := labelKey(labels)
	e := f.series[key]
	if e == nil {
		e = &entry{labels: cloneLabels(labels)}
		f.series[key] = e
	}
	return &resolvedEntry{entry: e, familyBounds: f.bounds}
}

// resolvedEntry pairs a series entry with its family's histogram bounds so a histogram
// registration builds with the family layout. It is an internal handoff from resolve, not a
// stored type.
type resolvedEntry struct {
	*entry
	familyBounds []float64
}

// cloneLabels copies a label set so the registry holds its own immutable copy and a caller
// reusing its labels map cannot mutate a registered series's identity.
func cloneLabels(l Labels) Labels {
	if len(l) == 0 {
		return nil
	}
	out := make(Labels, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out
}

// Snapshot reads the whole registry into an immutable point-in-time copy (doc 20 §7.3):
// every counter's summed value, every gauge's current value, every histogram's buckets, sum,
// and count. Each metric is read consistently within itself; the window across metrics is the
// iteration time (microseconds), a bounded smear far below the scrape interval (doc 20 §22.6).
// The result is sorted by name then label key, so two snapshots of the same registry render
// in the same order, which keeps the Prometheus and expvar output stable.
func (r *Registry) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var metrics []MetricSnapshot
	for _, f := range r.families {
		for _, e := range f.series {
			m := MetricSnapshot{
				Name:   f.name,
				Type:   f.typ,
				Help:   f.help,
				Unit:   f.unit,
				Labels: cloneLabels(e.labels),
			}
			switch f.typ {
			case TypeCounter:
				m.Counter = e.counter.Value()
			case TypeGauge:
				m.Gauge = e.gauge.Value()
			case TypeHistogram:
				m.Histogram = e.histogram.snapshot()
			}
			metrics = append(metrics, m)
		}
	}
	sort.Slice(metrics, func(i, j int) bool {
		if metrics[i].Name != metrics[j].Name {
			return metrics[i].Name < metrics[j].Name
		}
		return labelKey(metrics[i].Labels) < labelKey(metrics[j].Labels)
	})
	return Snapshot{metrics: metrics}
}
