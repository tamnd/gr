package gr

import (
	"context"
	"errors"
	"testing"

	"github.com/tamnd/gr/vfs"
)

// drain consumes and closes a result, so a streaming read records its query metrics (the
// recording fires at Close, doc 20 §3.1).
func drainResult(t *testing.T, r *Result) {
	t.Helper()
	for r.Next() {
	}
	if err := r.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestMetricsQueryThroughput confirms a read through Run records gr_queries_total and
// gr_query_duration_seconds under the read kind, and only at Close (the latency ends when the
// stream is drained).
func TestMetricsQueryThroughput(t *testing.T) {
	db, err := Open("m.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	res, err := db.Run(context.Background(), "RETURN 1 AS n", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Before Close the read is still in flight: counted in the gauge, not yet in the total.
	if g := db.Metrics().Gauge("gr_query_inflight", Labels{"kind": "read"}); g != 1 {
		t.Errorf("in-flight before close = %d, want 1", g)
	}
	if c := db.Metrics().Counter("gr_queries_total", Labels{"kind": "read", "status": "ok"}); c != 0 {
		t.Errorf("total before close = %d, want 0 (latency ends at close)", c)
	}

	drainResult(t, res)

	snap := db.Metrics()
	if c := snap.Counter("gr_queries_total", Labels{"kind": "read", "status": "ok"}); c != 1 {
		t.Errorf("total after close = %d, want 1", c)
	}
	if g := snap.Gauge("gr_query_inflight", Labels{"kind": "read"}); g != 0 {
		t.Errorf("in-flight after close = %d, want 0", g)
	}
	h := snap.Histogram("gr_query_duration_seconds", Labels{"kind": "read"})
	if h.Count != 1 {
		t.Errorf("duration count = %d, want 1", h.Count)
	}
}

// TestMetricsWriteKind confirms an eager write records immediately (no streaming wait) under
// the write kind.
func TestMetricsWriteKind(t *testing.T) {
	db, err := Open("mw.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	res, err := db.Run(context.Background(), "CREATE (:Person {name: 'a'})", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A write executes eagerly, so the total is already recorded before Close.
	if c := db.Metrics().Counter("gr_queries_total", Labels{"kind": "write", "status": "ok"}); c != 1 {
		t.Errorf("write total = %d, want 1 (eager)", c)
	}
	if g := db.Metrics().Gauge("gr_query_inflight", Labels{"kind": "write"}); g != 0 {
		t.Errorf("write in-flight = %d, want 0 (eager, already finished)", g)
	}
	_ = res.Close()
}

// TestMetricsErrorStatus confirms a failing query records the error status, and that a parse
// error (which never reaches a kind) records nothing, the catalogue's bounded-kind discipline.
func TestMetricsErrorStatus(t *testing.T) {
	db, err := Open("me.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// A read that fails at runtime: divide is fine, so force a semantic error instead by
	// referencing an undefined variable, which fails after the kind is known.
	if _, err := db.Run(context.Background(), "RETURN missing.prop", nil); err == nil {
		t.Fatal("expected an error for an undefined variable")
	}
	if c := db.Metrics().Counter("gr_queries_total", Labels{"kind": "read", "status": "error"}); c != 1 {
		t.Errorf("read error total = %d, want 1", c)
	}

	// A parse error never classifies into a kind, so it is not counted in gr_queries_total.
	if _, err := db.Run(context.Background(), "this is not cypher", nil); err == nil {
		t.Fatal("expected a parse error")
	}
	total := uint64(0)
	for _, k := range metricQueryKinds {
		for _, s := range metricQueryStatuses {
			total += db.Metrics().Counter("gr_queries_total", Labels{"kind": k, "status": s})
		}
	}
	if total != 1 {
		t.Errorf("total across all series = %d, want 1 (the parse error is not counted)", total)
	}
}

// TestMetricsErrorClasses confirms gr_query_errors_total splits errors by cause: a parse
// failure is syntax (and is counted here even though it never reaches gr_queries_total), and a
// constraint violation is constraint.
func TestMetricsErrorClasses(t *testing.T) {
	db, err := Open("mc.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// A parse error: counted in gr_query_errors_total{class="syntax"} though absent from
	// gr_queries_total.
	if _, err := db.Run(context.Background(), "this is not cypher", nil); err == nil {
		t.Fatal("expected a parse error")
	}
	if c := db.Metrics().Counter("gr_query_errors_total", Labels{"class": "syntax"}); c != 1 {
		t.Errorf("syntax errors = %d, want 1", c)
	}

	// A constraint violation: insert a duplicate against a unique constraint.
	if _, err := db.Run(context.Background(),
		"CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil); err != nil {
		t.Fatalf("create constraint: %v", err)
	}
	if _, err := db.Run(context.Background(), "CREATE (:Person {email: 'a@b.c'})", nil); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := db.Run(context.Background(), "CREATE (:Person {email: 'a@b.c'})", nil); err == nil {
		t.Fatal("expected a constraint violation on the duplicate")
	}
	if c := db.Metrics().Counter("gr_query_errors_total", Labels{"class": "constraint"}); c != 1 {
		t.Errorf("constraint errors = %d, want 1", c)
	}

	// The error total across all classes is the two failures (syntax + constraint), since the
	// successful statements did not count.
	total := uint64(0)
	for _, class := range metricErrorClasses {
		total += db.Metrics().Counter("gr_query_errors_total", Labels{"class": class})
	}
	if total != 2 {
		t.Errorf("errors across all classes = %d, want 2", total)
	}
}

// TestMetricsErrorClassMapping checks the classifier maps the library's typed errors and
// sentinels to their class labels, the same taxonomy the CLI and server share.
func TestMetricsErrorClassMapping(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrConflict, "conflict"},
		{ErrOverloaded, "resource"},
		{ErrRateLimited, "resource"},
		{context.DeadlineExceeded, "resource"},
		{context.Canceled, "resource"},
		{errors.New("something unexpected"), "internal"},
	}
	for _, c := range cases {
		if got := metricErrorClass(c.err); got != c.want {
			t.Errorf("metricErrorClass(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestMetricsRowAmplification confirms the rows-returned and rows-scanned histograms record a
// read's output cardinality and its scan work, the amplification pair (doc 20 §3.1). A scan
// that touches several nodes but a filter that keeps one row shows scanned > returned.
func TestMetricsRowAmplification(t *testing.T) {
	db, err := Open("mr.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Seed five Person nodes; only one matches the filter below.
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		if _, err := db.Run(context.Background(),
			"CREATE (:Person {name: $n})", Params{"n": name}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// A label scan touches all five nodes; the property filter returns one row.
	res, err := db.Run(context.Background(),
		"MATCH (p:Person) WHERE p.name = 'c' RETURN p", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, res)

	snap := db.Metrics()
	ret := snap.Histogram("gr_query_rows_returned", nil)
	scan := snap.Histogram("gr_query_rows_scanned", nil)
	// The read returned one row.
	if ret.Count == 0 || ret.Sum != 1 {
		t.Errorf("rows_returned: count=%d sum=%v, want count>=1 sum=1", ret.Count, ret.Sum)
	}
	// The scan touched all five nodes, so the scanned sum includes the read's five (the five
	// seeding writes scanned nothing).
	if scan.Sum < 5 {
		t.Errorf("rows_scanned sum = %v, want at least 5 (the label scan)", scan.Sum)
	}
}

// TestMetricsAlwaysOn confirms the registry exists on a plain Open with no logging configured,
// so the metrics plane is always on (doc 20 §1.2).
func TestMetricsAlwaysOn(t *testing.T) {
	db, err := Open("ma.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.metrics == nil {
		t.Fatal("metrics registry should be built at Open")
	}
	// db.Metrics() returns a usable snapshot even before any query runs.
	if got := db.Metrics().Counter("gr_queries_total", Labels{"kind": "read", "status": "ok"}); got != 0 {
		t.Errorf("fresh counter = %d, want 0", got)
	}
}
