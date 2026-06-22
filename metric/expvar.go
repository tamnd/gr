package metric

import (
	"encoding/json"
	"io"
	"math"
	"sort"
	"strings"
)

// WriteExpvar renders a snapshot as the expvar JSON tree (doc 20 §7.6, §33.2): the same
// registry the Prometheus surface renders, encoded as a nested object under a "gr" root for
// the operator who has the Go expvar convention but not a Prometheus scraper. A metric with no
// labels is a scalar leaf; a metric with labels nests one object level per label value, sorted,
// so gr_buffer_pool_pages{state="dirty"} becomes {"buffer_pool_pages":{"dirty":1842}}. A
// histogram leaf is {"count","sum","buckets"}, the finite bucket bounds mapped to their
// cumulative counts. The values match the Prometheus rendering in meaning (doc 20 invariant 4).
func WriteExpvar(w io.Writer, snap Snapshot) error {
	root := map[string]any{}
	for _, m := range snap.Metrics() {
		key := strings.TrimPrefix(m.Name, "gr_")
		place(root, key, m)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"gr": root})
}

// place inserts one metric's value into the tree under key, descending one object level per
// label value (sorted by label name) so a multi-label series lands at a unique leaf. A series
// with no labels sets the leaf directly under key.
func place(root map[string]any, key string, m MetricSnapshot) {
	leaf := leafValue(m)
	names := make([]string, 0, len(m.Labels))
	for k := range m.Labels {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) == 0 {
		root[key] = leaf
		return
	}
	node := root
	// Descend to the parent of the deepest level, creating intermediate objects.
	parent := childObject(node, key)
	for i := 0; i < len(names)-1; i++ {
		parent = childObject(parent, m.Labels[names[i]])
	}
	parent[m.Labels[names[len(names)-1]]] = leaf
}

// childObject returns the object stored at key in node, creating it if absent, so a path of
// label values builds nested objects on demand.
func childObject(node map[string]any, key string) map[string]any {
	if existing, ok := node[key].(map[string]any); ok {
		return existing
	}
	child := map[string]any{}
	node[key] = child
	return child
}

// leafValue is the JSON value for one series: a counter's or gauge's number, or a histogram's
// {count, sum, buckets} object.
func leafValue(m MetricSnapshot) any {
	switch m.Type {
	case TypeCounter:
		return m.Counter
	case TypeGauge:
		return m.Gauge
	case TypeHistogram:
		buckets := map[string]uint64{}
		for i, bound := range m.Histogram.Bounds {
			if math.IsInf(bound, 1) {
				continue // the +Inf catch-all is implied by count, omitted from the bucket map
			}
			buckets[formatFloat(bound)] = m.Histogram.Counts[i]
		}
		return map[string]any{
			"count":   m.Histogram.Count,
			"sum":     m.Histogram.Sum,
			"buckets": buckets,
		}
	default:
		return nil
	}
}
