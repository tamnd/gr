package metric

import (
	"encoding/json"
	"strings"
	"testing"
)

// buildSnap returns a registry with one of each type for the exposition tests.
func buildSnap() Snapshot {
	r := NewRegistry()
	r.Counter("gr_queries_total", "Cypher queries executed", "queries",
		Labels{"kind": "read", "status": "ok"}).Add(19994)
	r.Gauge("gr_buffer_pool_pages", "Pages resident in the buffer pool", "pages",
		Labels{"state": "dirty"}).Set(1842)
	h := r.Histogram("gr_query_duration_seconds", "End-to-end query latency", "seconds",
		[]float64{0.001, 0.01, 0.1}, Labels{"kind": "read"})
	for i := 0; i < 5; i++ {
		h.Observe(0.0005)
	}
	h.Observe(2) // lands in +Inf
	return r.Snapshot()
}

// TestPrometheusFormat confirms the text exposition has the HELP and TYPE headers once per
// family and renders each type in its Prometheus form.
func TestPrometheusFormat(t *testing.T) {
	var b strings.Builder
	if err := WritePrometheus(&b, buildSnap()); err != nil {
		t.Fatal(err)
	}
	out := b.String()

	for _, want := range []string{
		"# HELP gr_queries_total Cypher queries executed",
		"# TYPE gr_queries_total counter",
		`gr_queries_total{kind="read",status="ok"} 19994`,
		"# TYPE gr_buffer_pool_pages gauge",
		`gr_buffer_pool_pages{state="dirty"} 1842`,
		"# TYPE gr_query_duration_seconds histogram",
		`gr_query_duration_seconds_bucket{kind="read",le="0.001"} 5`,
		`gr_query_duration_seconds_bucket{kind="read",le="+Inf"} 6`,
		`gr_query_duration_seconds_count{kind="read"} 6`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, out)
		}
	}

	// The HELP header appears exactly once per family.
	if n := strings.Count(out, "# HELP gr_queries_total"); n != 1 {
		t.Errorf("HELP for gr_queries_total appears %d times, want 1", n)
	}
}

// TestPrometheusBucketsCumulative confirms the bucket counts are cumulative: each le bound's
// count includes every lower bucket.
func TestPrometheusBucketsCumulative(t *testing.T) {
	var b strings.Builder
	if err := WritePrometheus(&b, buildSnap()); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// All five 0.0005 observations are at or below every finite bound, so 0.001, 0.01, and 0.1
	// each read 5, and +Inf reads 6 (the single value of 2).
	for _, le := range []string{"0.001", "0.01", "0.1"} {
		want := `gr_query_duration_seconds_bucket{kind="read",le="` + le + `"} 5`
		if !strings.Contains(out, want) {
			t.Errorf("missing cumulative bucket %q", want)
		}
	}
}

// TestExpvarTree confirms the expvar JSON nests by label and carries the histogram shape.
func TestExpvarTree(t *testing.T) {
	var b strings.Builder
	if err := WriteExpvar(&b, buildSnap()); err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := json.Unmarshal([]byte(b.String()), &tree); err != nil {
		t.Fatalf("expvar output is not valid JSON: %v\n%s", err, b.String())
	}
	gr, ok := tree["gr"].(map[string]any)
	if !ok {
		t.Fatalf("missing gr root: %v", tree)
	}

	// gr_buffer_pool_pages{state="dirty"} -> buffer_pool_pages.dirty == 1842
	pages := gr["buffer_pool_pages"].(map[string]any)
	if pages["dirty"].(float64) != 1842 {
		t.Errorf("dirty pages = %v, want 1842", pages["dirty"])
	}

	// gr_queries_total{kind="read",status="ok"} nests kind then status.
	q := gr["queries_total"].(map[string]any)["read"].(map[string]any)
	if q["ok"].(float64) != 19994 {
		t.Errorf("queries_total.read.ok = %v, want 19994", q["ok"])
	}

	// The histogram leaf carries count, sum, and a buckets object.
	hist := gr["query_duration_seconds"].(map[string]any)["read"].(map[string]any)
	if hist["count"].(float64) != 6 {
		t.Errorf("histogram count = %v, want 6", hist["count"])
	}
	if _, ok := hist["buckets"].(map[string]any); !ok {
		t.Errorf("histogram missing buckets object: %v", hist)
	}
}
