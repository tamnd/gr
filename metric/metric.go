// Package metric is gr's in-process metric registry (spec 2060 doc 20 §2, §7): the typed,
// always-on counters, gauges, and histograms every subsystem registers and the exposition
// surfaces render. It is a low-level package the storage, WAL, planner, and executor layers
// import to register what they own, so the registry is the federation point rather than a
// god object (doc 20 §2.1). Package gr wraps it with db.Metrics() and the Prometheus and
// expvar surfaces.
//
// The design rule is doc 20 §1.2: observation must not perturb measurement. An update is an
// atomic add to a fixed cell, never a lock, a map insert, or an allocation, so metrics stay
// on in production always. A counter or gauge holds a pointer to its cells once resolved at
// the call site, so the per-update cost is the atomic add alone (doc 20 §7.4).
package metric

import (
	"runtime"
	"sort"
	"strings"
	_ "unsafe" // for go:linkname to runtime.procPin
)

// Type is one of the three metric types gr uses (doc 20 §2.2): exactly counter, gauge, and
// histogram, the Prometheus-aligned trio, because three suffice and a small vocabulary keeps
// dashboards uniform. There is deliberately no fourth type.
type Type int

const (
	// TypeCounter is a monotonically non-decreasing cumulative count of events (doc 20 §2.3).
	TypeCounter Type = iota
	// TypeGauge is an instantaneous value that goes up and down (doc 20 §2.4).
	TypeGauge
	// TypeHistogram is a distribution of observations bucketed by value (doc 20 §2.5).
	TypeHistogram
)

// String names the type for the Prometheus TYPE line.
func (t Type) String() string {
	switch t {
	case TypeCounter:
		return "counter"
	case TypeGauge:
		return "gauge"
	case TypeHistogram:
		return "histogram"
	default:
		return "untyped"
	}
}

// Labels is a metric's bounded label set (doc 20 §7.2): name=value pairs drawn only from
// schema-sized dimensions (status, kind, result, a relationship type), never from unbounded
// sets (a property value, a node id, a query text), so a metric's cardinality is the product
// of small domains and cannot explode with the data. A metric's identity is its name plus
// its label set.
type Labels map[string]string

// cacheLine is the padding that keeps each shard's cell on its own cache line, so increments
// on different cores hit different lines and never contend (doc 20 §7.4). 64 bytes is the
// common line size on amd64 and arm64.
const cacheLine = 64

// labelKey renders a label set to a canonical, order-stable string so the same logical set
// resolves to the same series regardless of map iteration order. It sorts the keys, so two
// calls with the same pairs in different literal order share one series.
func labelKey(l Labels) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(l[k])
	}
	return b.String()
}

// labelNames returns the sorted label names of a set, the schema dimension a metric family
// is registered with. A redefinition of the same metric name must present the same names.
func labelNames(l Labels) []string {
	if len(l) == 0 {
		return nil
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sameNames reports whether two sorted name slices are identical, used to reject a metric
// redefined with a different label schema (doc 20 §2.1).
func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

//go:linkname runtimeProcPin runtime.procPin
func runtimeProcPin() int

//go:linkname runtimeProcUnpin runtime.procUnpin
func runtimeProcUnpin()

// shardCount is the number of per-core cells a sharded counter or gauge holds. It is the
// GOMAXPROCS at registry construction, the count of scheduler Ps, so a goroutine running on
// a given P increments that P's cell with no cross-core contention (doc 20 §7.4). At least
// one shard, so the index is always valid.
func shardCount() int {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	return n
}

// shardIndex returns the current P's index, bounded to n shards. procPin returns the P id in
// [0, GOMAXPROCS); if GOMAXPROCS grew past the shard count since construction, the modulo
// folds it back, so a few cores may share a cell but correctness holds. The pin is the
// minimal critical section: it only reads the id, so it unpins immediately.
func shardIndex(n int) int {
	pid := runtimeProcPin()
	runtimeProcUnpin()
	if pid < 0 {
		pid = 0
	}
	return pid % n
}
