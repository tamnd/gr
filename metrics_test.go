package gr

import (
	"context"
	"errors"
	"testing"
	"time"

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

// TestMetricsPlanCacheSplit confirms gr_query_plan_duration_seconds splits a cold compile from a
// warm cache hit: the first run of a read misses the plan cache, the second run of the same text
// hits it, and a write is always a miss (doc 20 §3.1).
func TestMetricsPlanCacheSplit(t *testing.T) {
	db, err := Open("mp.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// A write compiles fresh: one plan-cache miss.
	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'a'})", nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if h := db.Metrics().Histogram("gr_query_plan_duration_seconds", Labels{"cache": "miss"}); h.Count == 0 {
		t.Error("write should record a plan-cache miss")
	}

	// First run of a read text: a miss that fills the cache.
	r1, err := db.Run(context.Background(), "MATCH (p:Person) RETURN p", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, r1)
	missAfterFirst := db.Metrics().Histogram("gr_query_plan_duration_seconds", Labels{"cache": "miss"}).Count

	// Second run of the same text: a cache hit, recorded under cache=hit, not miss.
	r2, err := db.Run(context.Background(), "MATCH (p:Person) RETURN p", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, r2)

	hit := db.Metrics().Histogram("gr_query_plan_duration_seconds", Labels{"cache": "hit"})
	if hit.Count != 1 {
		t.Errorf("plan hits = %d, want 1 (the second run of the same read)", hit.Count)
	}
	if missNow := db.Metrics().Histogram("gr_query_plan_duration_seconds", Labels{"cache": "miss"}).Count; missNow != missAfterFirst {
		t.Errorf("misses grew on a cache hit: %d then %d", missAfterFirst, missNow)
	}
}

// TestMetricsExecuteDuration confirms gr_query_execute_duration_seconds records the executor span
// for both a streaming read (at Close) and an eager write (doc 20 §3.1).
func TestMetricsExecuteDuration(t *testing.T) {
	db, err := Open("mx.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'a'})", nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if h := db.Metrics().Histogram("gr_query_execute_duration_seconds", Labels{"kind": "write"}); h.Count != 1 {
		t.Errorf("write execute count = %d, want 1", h.Count)
	}

	// A streaming read records its executor span at Close, not before.
	res, err := db.Run(context.Background(), "MATCH (p:Person) RETURN p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := db.Metrics().Histogram("gr_query_execute_duration_seconds", Labels{"kind": "read"}); h.Count != 0 {
		t.Errorf("read execute count before close = %d, want 0", h.Count)
	}
	drainResult(t, res)
	if h := db.Metrics().Histogram("gr_query_execute_duration_seconds", Labels{"kind": "read"}); h.Count != 1 {
		t.Errorf("read execute count after close = %d, want 1", h.Count)
	}
}

// TestMetricsPlanCacheLookups confirms gr_plan_cache_lookups_total splits a cold compile from a
// warm hit and that gr_plan_cache_entries tracks the resident plan count (doc 20 §3.2).
func TestMetricsPlanCacheLookups(t *testing.T) {
	db, err := Open("mcl.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const q = "MATCH (p:Person) RETURN p"
	r1, err := db.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, r1)
	r2, err := db.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, r2)

	snap := db.Metrics()
	if hit := snap.Counter("gr_plan_cache_lookups_total", Labels{"result": "hit"}); hit != 1 {
		t.Errorf("cache hits = %d, want 1", hit)
	}
	if miss := snap.Counter("gr_plan_cache_lookups_total", Labels{"result": "miss"}); miss != 1 {
		t.Errorf("cache misses = %d, want 1", miss)
	}
	if e := snap.Gauge("gr_plan_cache_entries", nil); e != 1 {
		t.Errorf("resident plans = %d, want 1", e)
	}
}

// TestMetricsPlanCacheEviction confirms a plan cache too small for the query variety records a
// capacity eviction (doc 20 §3.2): with room for one plan, a second distinct shape evicts the
// first.
func TestMetricsPlanCacheEviction(t *testing.T) {
	db, err := Open("mce.gr", Options{VFS: vfs.NewMem(), PlanCacheSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, q := range []string{"MATCH (a:A) RETURN a", "MATCH (b:B) RETURN b"} {
		res, err := db.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("run %q: %v", q, err)
		}
		drainResult(t, res)
	}

	snap := db.Metrics()
	if ev := snap.Counter("gr_plan_cache_evictions_total", Labels{"reason": "capacity"}); ev != 1 {
		t.Errorf("capacity evictions = %d, want 1", ev)
	}
	if e := snap.Gauge("gr_plan_cache_entries", nil); e != 1 {
		t.Errorf("resident plans = %d, want 1 (cache holds one)", e)
	}
}

// TestMetricsAdmissionQueued confirms the admission gate's queue wait lands in the database
// metrics once wired: an immediate acquire queues nothing, a second acquire against a full gate
// queues and is counted with its wait observed (doc 20 §3.1).
func TestMetricsAdmissionQueued(t *testing.T) {
	db, err := Open("madm.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	a := NewAdmission(1, 20*time.Millisecond)
	db.InstrumentAdmission(a)

	// The first acquire takes the only slot at once, so it does not queue.
	rel, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if q := db.Metrics().Counter("gr_query_queued_total", nil); q != 0 {
		t.Errorf("queued after an immediate acquire = %d, want 0", q)
	}

	// The gate is full, so the second acquire queues for the wait and then sheds.
	if _, err := a.Acquire(context.Background()); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("second acquire = %v, want ErrOverloaded", err)
	}
	rel()

	snap := db.Metrics()
	if q := snap.Counter("gr_query_queued_total", nil); q != 1 {
		t.Errorf("queued = %d, want 1 (the second acquire waited)", q)
	}
	if h := snap.Histogram("gr_query_queue_wait_seconds", nil); h.Count != 1 {
		t.Errorf("queue wait count = %d, want 1", h.Count)
	}
}

// TestMetricsTransactions confirms the transaction lifecycle metrics track an open transaction,
// a committed write, and a rolled-back read (doc 20 §3.3).
func TestMetricsTransactions(t *testing.T) {
	db, err := Open("mtx.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tx, err := db.Begin(context.Background(), Write)
	if err != nil {
		t.Fatal(err)
	}
	if g := db.Metrics().Gauge("gr_transactions_open", Labels{"mode": "write"}); g != 1 {
		t.Errorf("open write txns = %d, want 1", g)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	snap := db.Metrics()
	if g := snap.Gauge("gr_transactions_open", Labels{"mode": "write"}); g != 0 {
		t.Errorf("open write txns after commit = %d, want 0", g)
	}
	if c := snap.Counter("gr_transactions_total", Labels{"mode": "write", "outcome": "commit"}); c != 1 {
		t.Errorf("write commits = %d, want 1", c)
	}
	if h := snap.Histogram("gr_transaction_duration_seconds", Labels{"mode": "write"}); h.Count != 1 {
		t.Errorf("write tx duration count = %d, want 1", h.Count)
	}

	// A rolled-back read counts an abort under the read mode.
	rtx, err := db.Begin(context.Background(), Read)
	if err != nil {
		t.Fatal(err)
	}
	if err := rtx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if c := db.Metrics().Counter("gr_transactions_total", Labels{"mode": "read", "outcome": "abort"}); c != 1 {
		t.Errorf("read aborts = %d, want 1", c)
	}
}

// TestMetricsTxOutcomeMapping checks the outcome classifier maps a clean finish, a conflict, and
// any other failure to their labels (doc 20 §3.3).
func TestMetricsTxOutcomeMapping(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "commit"},
		{ErrConflict, "conflict"},
		{errors.New("disk gone"), "abort"},
	}
	for _, c := range cases {
		if got := metricTxOutcome(c.err); got != c.want {
			t.Errorf("metricTxOutcome(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestMetricsGraphOperators confirms the graph-operator counters increment when their operators
// run: a disconnected pattern is a binary join, and a shortestPath query is a shortest-path
// search (doc 20 §6).
func TestMetricsGraphOperators(t *testing.T) {
	db, err := Open("mg.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Seed a small graph: a KNOWS path a -> b -> c and a node of another label.
	for _, q := range []string{
		"CREATE (a:Person {name: 'a'})-[:KNOWS]->(b:Person {name: 'b'})-[:KNOWS]->(c:Person {name: 'c'})",
		"CREATE (:Movie {title: 'm'})",
	} {
		if _, err := db.Run(context.Background(), q, nil); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	// A disconnected pattern joins the two legs with a binary hash join.
	rj, err := db.Run(context.Background(), "MATCH (p:Person), (m:Movie) RETURN p, m", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, rj)
	if c := db.Metrics().Counter("gr_binary_join_total", nil); c != 1 {
		t.Errorf("binary joins = %d, want 1", c)
	}

	// A shortestPath query runs the dedicated shortest-path operator.
	rs, err := db.Run(context.Background(),
		"MATCH p = shortestPath((a:Person {name: 'a'})-[:KNOWS*]->(c:Person {name: 'c'})) RETURN length(p) AS len", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, rs)
	if c := db.Metrics().Counter("gr_shortest_path_total", nil); c != 1 {
		t.Errorf("shortest-path searches = %d, want 1", c)
	}
}

// TestMetricsWcoj confirms the worst-case-optimal-join counter increments when the cost model
// rewrites a triangle's closing expand into an Intersect. The rewrite only engages with a
// non-trivial average degree, so the graph is seeded dense enough to make the binary plan's
// intermediate dwarf the closing matches (doc 20 §6).
func TestMetricsWcoj(t *testing.T) {
	db, err := Open("mwcoj.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		if _, err := db.Run(ctx, "CREATE (:Person {id: $i})", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 12; i++ {
		for k := 1; k <= 5; k++ {
			if _, err := db.Run(ctx,
				"MATCH (x:Person {id: $a}), (y:Person {id: $b}) CREATE (x)-[:KNOWS]->(y)",
				map[string]any{"a": i, "b": (i + k) % 12}); err != nil {
				t.Fatal(err)
			}
		}
	}

	r, err := db.Run(ctx, "MATCH (a:Person)-[:KNOWS]->(b)-[:KNOWS]->(c)-[:KNOWS]->(a) RETURN a", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, r)
	if c := db.Metrics().Counter("gr_wcoj_total", nil); c != 1 {
		t.Errorf("worst-case-optimal joins = %d, want 1", c)
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
