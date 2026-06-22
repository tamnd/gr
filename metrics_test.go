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

// TestMetricsExpand confirms the per-type expand metrics record a source position expanded: the
// operation and neighbor counters labelled by type and direction, and the fan-out and time
// histograms labelled by type (doc 20 §6.1). The query anchors at one node and expands its two
// KNOWS edges, so the counts are exact.
func TestMetricsExpand(t *testing.T) {
	db, err := Open("mexp.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.Run(ctx, "CREATE (a:Person {n: 'a'})-[:KNOWS]->(:Person {n: 'b'})", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Run(ctx,
		"MATCH (a:Person {n: 'a'}) CREATE (a)-[:KNOWS]->(:Person {n: 'c'})", nil); err != nil {
		t.Fatal(err)
	}

	r, err := db.Run(ctx, "MATCH (a:Person {n: 'a'})-[:KNOWS]->(x) RETURN x", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, r)

	out := Labels{"type": "KNOWS", "dir": "out"}
	if c := db.Metrics().Counter("gr_expand_total", out); c != 1 {
		t.Errorf("expand operations = %d, want 1", c)
	}
	if c := db.Metrics().Counter("gr_expand_neighbors_total", out); c != 2 {
		t.Errorf("expand neighbors = %d, want 2", c)
	}
	fan := db.Metrics().Histogram("gr_expand_fanout", Labels{"type": "KNOWS"})
	if fan.Count != 1 {
		t.Errorf("fan-out observations = %d, want 1", fan.Count)
	}
	if fan.Sum != 2 {
		t.Errorf("fan-out sum = %v, want 2", fan.Sum)
	}
	if s := db.Metrics().Histogram("gr_expand_seconds", Labels{"type": "KNOWS"}); s.Count != 1 {
		t.Errorf("expand time observations = %d, want 1", s.Count)
	}
}

// TestMetricsExpandWildcard confirms an expand with no named type is attributed to the all-types
// bucket: the type label is "*" rather than a relationship-type name (doc 20 §6.1).
func TestMetricsExpandWildcard(t *testing.T) {
	db, err := Open("mexpw.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.Run(ctx, "CREATE (a:Person {n: 'a'})-[:KNOWS]->(:Person {n: 'b'})", nil); err != nil {
		t.Fatal(err)
	}

	r, err := db.Run(ctx, "MATCH (a:Person {n: 'a'})-->(x) RETURN x", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, r)

	// A typeless expand is attributed to the all-types bucket. The planner is free to pick the
	// expand direction, so sum the wildcard series across directions rather than assume one.
	var wildcard uint64
	for _, s := range db.Metrics().Metrics() {
		if s.Name == "gr_expand_total" && s.Labels["type"] == "*" {
			wildcard += s.Counter
		}
	}
	if wildcard == 0 {
		t.Error("wildcard expand operations = 0, want at least one all-types expand")
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
	if h := db.Metrics().Histogram("gr_wcoj_intersect_seconds", nil); h.Count == 0 {
		t.Error("wcoj intersect time has no observations, want at least one")
	}
}

// TestMetricsJoinBuild confirms the hash-join build-side timing records once per binary join. A
// disconnected pattern is a binary join whose build side is read once (doc 20 §6.3).
func TestMetricsJoinBuild(t *testing.T) {
	db, err := Open("mjb.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.Run(ctx, "CREATE (:Tag {n: 't'}), (:Doc {n: 'd'})", nil); err != nil {
		t.Fatal(err)
	}
	r, err := db.Run(ctx, "MATCH (a:Tag), (b:Doc) RETURN a, b", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, r)
	if h := db.Metrics().Histogram("gr_join_build_seconds", nil); h.Count != 1 {
		t.Errorf("join build observations = %d, want 1", h.Count)
	}
}

// TestMetricsFactorized confirms a factorized count records the engaged counter and the
// compression ratio. A count over an expand factorizes into an ExpandCount that emits one tally
// row standing in for the edges it counted (doc 20 §6.3).
func TestMetricsFactorized(t *testing.T) {
	db, err := Open("mfz.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.Run(ctx, "CREATE (a:Person {n: 'a'})-[:KNOWS]->(:Person {n: 'b'})", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Run(ctx, "MATCH (a:Person {n: 'a'}) CREATE (a)-[:KNOWS]->(:Person {n: 'c'})", nil); err != nil {
		t.Fatal(err)
	}
	// Extra non-Person nodes make the labeled scan the selective end, so the optimizer anchors at
	// a:Person and expands toward an unlabeled b, the shape the count factorizes (no target label
	// on the expand).
	for i := 0; i < 20; i++ {
		if _, err := db.Run(ctx, "CREATE (:Widget {i: $i})", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}

	r, err := db.Run(ctx, "MATCH (a:Person)-[:KNOWS]->(b) RETURN count(*) AS c", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, r)
	if c := db.Metrics().Counter("gr_factorized_total", nil); c != 1 {
		t.Errorf("factorized operators = %d, want 1", c)
	}
	// Two KNOWS edges were counted into one tally row, so the flat-over-factorized ratio is two.
	h := db.Metrics().Histogram("gr_factorization_ratio", nil)
	if h.Count != 1 {
		t.Errorf("factorization-ratio observations = %d, want 1", h.Count)
	}
	if h.Sum != 2 {
		t.Errorf("factorization ratio = %v, want 2", h.Sum)
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

// TestMetricsBoltSession confirms the Bolt observer feeds the session and protocol metrics
// (doc 20 §3.3): a session opens and closes, messages and an auth outcome are counted, and the
// open gauge returns to zero when the session ends.
func TestMetricsBoltSession(t *testing.T) {
	db, err := Open("mbolt.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	obs := db.BoltObserver()
	obs.SessionOpen()
	obs.Auth(true)
	obs.Message("HELLO")
	obs.Message("RUN")
	obs.Message("RUN")

	snap := db.Metrics()
	if g := snap.Gauge("gr_sessions_open", Labels{"protocol": "bolt"}); g != 1 {
		t.Errorf("sessions_open = %d, want 1 while the session is live", g)
	}
	if c := snap.Counter("gr_sessions_total", Labels{"protocol": "bolt"}); c != 1 {
		t.Errorf("sessions_total = %d, want 1", c)
	}
	if c := snap.Counter("gr_bolt_messages_total", Labels{"type": "RUN"}); c != 2 {
		t.Errorf("RUN messages = %d, want 2", c)
	}
	if c := snap.Counter("gr_bolt_messages_total", Labels{"type": "HELLO"}); c != 1 {
		t.Errorf("HELLO messages = %d, want 1", c)
	}
	if c := snap.Counter("gr_auth_attempts_total", Labels{"result": "ok"}); c != 1 {
		t.Errorf("auth ok = %d, want 1", c)
	}

	obs.SessionClose(2 * time.Second)
	snap = db.Metrics()
	if g := snap.Gauge("gr_sessions_open", Labels{"protocol": "bolt"}); g != 0 {
		t.Errorf("sessions_open = %d, want 0 after the session closed", g)
	}
	if h := snap.Histogram("gr_session_duration_seconds", Labels{"protocol": "bolt"}); h.Count != 1 {
		t.Errorf("session duration count = %d, want 1", h.Count)
	}
}

// TestMetricsBoltError confirms a reported protocol error and a failed auth land on their own
// counters (doc 20 §3.3).
func TestMetricsBoltError(t *testing.T) {
	db, err := Open("mbolterr.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	obs := db.BoltObserver()
	obs.Error("handshake")
	obs.Auth(false)

	snap := db.Metrics()
	if c := snap.Counter("gr_bolt_errors_total", Labels{"code": "handshake"}); c != 1 {
		t.Errorf("handshake errors = %d, want 1", c)
	}
	if c := snap.Counter("gr_auth_attempts_total", Labels{"result": "denied"}); c != 1 {
		t.Errorf("auth denied = %d, want 1", c)
	}
}

// TestMetricsIndexSeek confirms an index-served equality match records the index-lookup metric
// (doc 20 §6.4): the lookup counts under the index name with kind point, and the descent latency
// histogram takes an observation.
func TestMetricsIndexSeek(t *testing.T) {
	db := openMem(t, "mixseek.gr")
	defer db.Close()

	mustExec(t, db, "CREATE INDEX person_email FOR (p:Person) ON (p.email)", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	mustExec(t, db, "CREATE (:Person {email: 'b@x'})", nil)

	res, err := db.Query("MATCH (p:Person {email: 'a@x'}) RETURN p", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	drainResult(t, res)

	snap := db.Metrics()
	if c := snap.Counter("gr_index_lookups_total", Labels{"index": "person_email", "kind": "point"}); c != 1 {
		t.Errorf("index lookups = %d, want 1", c)
	}
	if h := snap.Histogram("gr_index_lookup_seconds", Labels{"kind": "point"}); h.Count != 1 {
		t.Errorf("index lookup seconds count = %d, want 1", h.Count)
	}
}

func TestMetricsIndexEntries(t *testing.T) {
	db := openMem(t, "mxentries.gr")
	defer db.Close()

	mustExec(t, db, "CREATE INDEX person_email FOR (p:Person) ON (p.email)", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	mustExec(t, db, "CREATE (:Person {email: 'b@x'})", nil)
	// A Person with no email value is not an index entry, so it does not count.
	mustExec(t, db, "CREATE (:Person {name: 'c'})", nil)

	if g := db.Metrics().Gauge("gr_index_entries", Labels{"index": "person_email"}); g != 2 {
		t.Errorf("index entries = %d, want 2", g)
	}

	// The gauge reads the live count at snapshot, so a delete lowers it.
	mustExec(t, db, "MATCH (p:Person {email: 'a@x'}) DELETE p", nil)
	if g := db.Metrics().Gauge("gr_index_entries", Labels{"index": "person_email"}); g != 1 {
		t.Errorf("index entries after delete = %d, want 1", g)
	}
}

func TestMetricsIndexMemoryBytes(t *testing.T) {
	db := openMem(t, "mxbytes.gr")
	defer db.Close()

	mustExec(t, db, "CREATE INDEX person_email FOR (p:Person) ON (p.email)", nil)
	// An empty index has no entries, so its footprint estimate is zero.
	if g := db.Metrics().Gauge("gr_index_memory_bytes", Labels{"index": "person_email"}); g != 0 {
		t.Errorf("empty index bytes = %d, want 0", g)
	}

	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	one := db.Metrics().Gauge("gr_index_memory_bytes", Labels{"index": "person_email"})
	if one <= 0 {
		t.Fatalf("one-entry index bytes = %d, want > 0", one)
	}

	// A second distinct value adds a bucket, so the footprint grows.
	mustExec(t, db, "CREATE (:Person {email: 'b@x'})", nil)
	two := db.Metrics().Gauge("gr_index_memory_bytes", Labels{"index": "person_email"})
	if two <= one {
		t.Errorf("two-entry index bytes = %d, want > %d", two, one)
	}
}

func TestMetricsColcacheAccesses(t *testing.T) {
	db := openMem(t, "mxcolcache.gr")
	defer db.Close()

	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	mustExec(t, db, "CREATE (:Person {email: 'b@x'})", nil)
	mustExec(t, db, "CREATE (:Person {email: 'c@x'})", nil)
	// Fold the properties into the segmented base so reads go through the column cache.
	runPragma(t, db, "PRAGMA wal_checkpoint")

	// No property read has touched the segmented base yet, so the cache is untouched.
	if c := db.Metrics().Counter("gr_colcache_accesses_total", Labels{"result": "miss"}); c != 0 {
		t.Fatalf("colcache misses before any read = %d, want 0", c)
	}

	read := func() {
		res, err := db.Run(context.Background(), "MATCH (p:Person) RETURN p.email", nil)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		drainResult(t, res)
	}

	// The first scan decodes its segments on a miss and caches them, so misses climb and
	// the resident-population gauges report the cached blocks.
	read()
	snap := db.Metrics()
	miss := snap.Counter("gr_colcache_accesses_total", Labels{"result": "miss"})
	if miss == 0 {
		t.Fatalf("colcache misses after first scan = %d, want > 0", miss)
	}
	if b := snap.Gauge("gr_colcache_blocks_resident", nil); b == 0 {
		t.Fatalf("colcache blocks resident = %d, want > 0", b)
	}
	if b := snap.Gauge("gr_colcache_memory_bytes", nil); b <= 0 {
		t.Fatalf("colcache memory bytes = %d, want > 0", b)
	}

	// A second scan over the same segments is served from the cache, so hits climb while
	// misses hold steady at the first scan's count.
	read()
	snap = db.Metrics()
	if hit := snap.Counter("gr_colcache_accesses_total", Labels{"result": "hit"}); hit == 0 {
		t.Fatalf("colcache hits after second scan = %d, want > 0", hit)
	}
	if c := snap.Counter("gr_colcache_accesses_total", Labels{"result": "miss"}); c != miss {
		t.Fatalf("colcache misses after second scan = %d, want %d (no new misses on a warm cache)", c, miss)
	}
}

func TestMetricsFileSizeBytes(t *testing.T) {
	db := openMem(t, "mxfilesize.gr")
	defer db.Close()

	// A freshly opened database holds a header and its initial store pages, so the file
	// has a size, and that size is a whole number of pages.
	base := db.Metrics().Gauge("gr_file_size_bytes", nil)
	if base <= 0 {
		t.Fatalf("file size after open = %d, want > 0", base)
	}
	if ps := int64(db.PageSize()); base%ps != 0 {
		t.Fatalf("file size = %d, want a multiple of the page size %d", base, ps)
	}

	// Writing enough nodes allocates new pages, so the file grows after the commits.
	for i := 0; i < 200; i++ {
		mustExec(t, db, "CREATE (:Person {note: 'a longer value to push the file past its initial pages'})", nil)
	}
	grown := db.Metrics().Gauge("gr_file_size_bytes", nil)
	if grown <= base {
		t.Fatalf("file size after writes = %d, want > %d", grown, base)
	}
}

func TestMetricsFreelistPages(t *testing.T) {
	db := openMem(t, "mxfreelist.gr")
	defer db.Close()

	// A fresh database has nothing freed yet.
	if g := db.Metrics().Gauge("gr_freelist_pages", nil); g != 0 {
		t.Fatalf("freelist pages on fresh db = %d, want 0", g)
	}

	// Build a segmented base, then rewrite the column and checkpoint again: the second
	// fold replaces the base and returns the old base's pages to the free list.
	for i := 0; i < 300; i++ {
		mustExec(t, db, "CREATE (:Person {note: 'value'})", nil)
	}
	runPragma(t, db, "PRAGMA wal_checkpoint")
	mustExec(t, db, "MATCH (p:Person) SET p.note = 'changed'", nil)
	runPragma(t, db, "PRAGMA wal_checkpoint")

	if g := db.Metrics().Gauge("gr_freelist_pages", nil); g <= 0 {
		t.Fatalf("freelist pages = %d, want > 0 after a reclaimed base", g)
	}
}

func TestMetricsBufferPoolAccesses(t *testing.T) {
	db := openMem(t, "mxbufpool.gr")
	defer db.Close()

	mustExec(t, db, "CREATE (:Person {name: 'a'})", nil)
	mustExec(t, db, "CREATE (:Person {name: 'b'})", nil)
	res, err := db.Run(context.Background(), "MATCH (p:Person) RETURN p.name", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	drainResult(t, res)

	snap := db.Metrics()
	// Any write or read touches pages through the pool, so accesses are nonzero and the
	// resident gauges report a live pool.
	hit := snap.Counter("gr_bufferpool_accesses_total", Labels{"result": "hit"})
	miss := snap.Counter("gr_bufferpool_accesses_total", Labels{"result": "miss"})
	if hit+miss == 0 {
		t.Fatalf("buffer-pool accesses = %d, want > 0 after queries", hit+miss)
	}
	if r := snap.Gauge("gr_bufferpool_resident_frames", nil); r == 0 {
		t.Fatalf("resident frames = %d, want > 0", r)
	}
	if b := snap.Gauge("gr_bufferpool_memory_bytes", nil); b <= 0 {
		t.Fatalf("memory bytes = %d, want > 0", b)
	}
}

func TestMetricsUnifiedCacheMemory(t *testing.T) {
	db := openMem(t, "mxcachemem.gr")
	defer db.Close()

	mustExec(t, db, "CREATE (:Person {name: 'a'})", nil)
	res, err := db.Run(context.Background(), "MATCH (p:Person) RETURN p.name", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	drainResult(t, res)

	snap := db.Metrics()
	// The buffer pool holds the pages every read and write touched, so its unified series is
	// live and matches the standalone buffer-pool gauge, the same number by construction.
	bp := snap.Gauge("gr_cache_memory_bytes", Labels{"cache": "bufferpool"})
	if bp <= 0 {
		t.Fatalf("unified bufferpool memory = %d, want > 0", bp)
	}
	if standalone := snap.Gauge("gr_bufferpool_memory_bytes", nil); bp != standalone {
		t.Fatalf("unified bufferpool memory = %d, want %d to match the standalone gauge", bp, standalone)
	}
	// The column series matches its standalone gauge too, whatever its current value.
	col := snap.Gauge("gr_cache_memory_bytes", Labels{"cache": "column"})
	if standalone := snap.Gauge("gr_colcache_memory_bytes", nil); col != standalone {
		t.Fatalf("unified column memory = %d, want %d to match the standalone gauge", col, standalone)
	}
	// The budget-used sum is exactly the caches that account bytes today.
	if used := snap.Gauge("gr_cache_budget_used_bytes", nil); used != bp+col {
		t.Fatalf("budget used = %d, want %d (bufferpool + column)", used, bp+col)
	}
}

func TestMetricsWalWrites(t *testing.T) {
	db := openMem(t, "mxwal.gr")
	defer db.Close()

	// Opening the database commits its initial catalog, so the WAL counters start non-zero; measure
	// against that baseline rather than zero.
	base := db.Metrics()
	baseBytes := base.Counter("gr_wal_bytes_written_total", nil)
	baseCommit := base.Counter("gr_wal_frames_written_total", Labels{"kind": "commit"})
	basePage := base.Counter("gr_wal_frames_written_total", Labels{"kind": "page"})

	for i := 0; i < 50; i++ {
		mustExec(t, db, "CREATE (:Person {note: 'value'})", nil)
	}

	snap := db.Metrics()
	if c := snap.Counter("gr_wal_bytes_written_total", nil); c <= baseBytes {
		t.Fatalf("wal bytes after fifty commits = %d, want > %d", c, baseBytes)
	}
	// Each committed transaction writes a commit frame, and the data pages it dirties are page frames.
	if c := snap.Counter("gr_wal_frames_written_total", Labels{"kind": "commit"}); c <= baseCommit {
		t.Fatalf("commit frames after fifty commits = %d, want > %d", c, baseCommit)
	}
	if c := snap.Counter("gr_wal_frames_written_total", Labels{"kind": "page"}); c <= basePage {
		t.Fatalf("page frames after fifty commits = %d, want > %d", c, basePage)
	}
	// The WAL holds its header on disk between transactions, so the size gauge is non-zero. In the
	// inline-checkpoint design each commit resets the WAL, so it oscillates back to the header size.
	if g := snap.Gauge("gr_wal_size_bytes", nil); g <= 0 {
		t.Fatalf("wal size = %d, want > 0 (at least the header)", g)
	}

	// More commits advance the cumulative byte counter further.
	bytes := snap.Counter("gr_wal_bytes_written_total", nil)
	for i := 0; i < 50; i++ {
		mustExec(t, db, "CREATE (:Person {note: 'more'})", nil)
	}
	if more := db.Metrics().Counter("gr_wal_bytes_written_total", nil); more <= bytes {
		t.Fatalf("wal bytes after more commits = %d, want > %d", more, bytes)
	}
}

func TestMetricsWalFsync(t *testing.T) {
	db := openMem(t, "mxwalfsync.gr")
	defer db.Close()

	// Opening commits the catalog, which fsyncs the WAL, so the count starts non-zero; measure deltas.
	base := db.Metrics()
	baseFsync := base.Counter("gr_wal_fsync_total", nil)
	baseHist := base.Histogram("gr_wal_fsync_seconds", nil).Count

	for i := 0; i < 50; i++ {
		mustExec(t, db, "CREATE (:Person {note: 'value'})", nil)
	}

	snap := db.Metrics()
	// Each committed transaction fsyncs the WAL at its commit point, so the count climbs past the
	// baseline and the latency histogram observed each barrier.
	fsync := snap.Counter("gr_wal_fsync_total", nil)
	if fsync <= baseFsync {
		t.Fatalf("wal fsyncs after fifty commits = %d, want > %d", fsync, baseFsync)
	}
	if h := snap.Histogram("gr_wal_fsync_seconds", nil); h.Count <= baseHist {
		t.Fatalf("fsync-seconds samples after fifty commits = %d, want > %d", h.Count, baseHist)
	}
	// No fsync failed, so the durability-alarm counter stays at zero.
	if e := snap.Counter("gr_wal_fsync_errors_total", nil); e != 0 {
		t.Fatalf("wal fsync errors = %d, want 0 on a healthy run", e)
	}

	// A checkpoint resets the WAL, which fsyncs again, so the count keeps climbing.
	runPragma(t, db, "PRAGMA wal_checkpoint")
	if more := db.Metrics().Counter("gr_wal_fsync_total", nil); more <= fsync {
		t.Fatalf("wal fsyncs after a checkpoint = %d, want > %d", more, fsync)
	}
}

func TestMetricsCheckpoint(t *testing.T) {
	db := openMem(t, "mxcheckpoint.gr")
	defer db.Close()

	// No checkpoint has run yet, so the manual trigger sits at zero and the timestamp is unset.
	base := db.Metrics()
	if c := base.Counter("gr_checkpoint_total", Labels{"trigger": "manual"}); c != 0 {
		t.Fatalf("manual checkpoints before any run = %d, want 0", c)
	}
	if g := base.Gauge("gr_checkpoint_last_timestamp_seconds", nil); g != 0 {
		t.Fatalf("last-checkpoint timestamp before any run = %d, want 0", g)
	}

	for i := 0; i < 50; i++ {
		mustExec(t, db, "CREATE (:Person {note: 'value'})", nil)
	}
	runPragma(t, db, "PRAGMA wal_checkpoint")

	snap := db.Metrics()
	if c := snap.Counter("gr_checkpoint_total", Labels{"trigger": "manual"}); c != 1 {
		t.Fatalf("manual checkpoints after one run = %d, want 1", c)
	}
	// The last-checkpoint timestamp moved off zero, the staleness clock an operator watches.
	if g := snap.Gauge("gr_checkpoint_last_timestamp_seconds", nil); g <= 0 {
		t.Fatalf("last-checkpoint timestamp = %d, want > 0 after a checkpoint", g)
	}
	// The duration histogram observed exactly the one checkpoint.
	if h := snap.Histogram("gr_checkpoint_duration_seconds", nil); h.Count != 1 {
		t.Fatalf("checkpoint duration observations = %d, want 1", h.Count)
	}
	// The triggers that have no scheduler yet are present and still zero.
	if c := snap.Counter("gr_checkpoint_total", Labels{"trigger": "timer"}); c != 0 {
		t.Fatalf("timer checkpoints = %d, want 0", c)
	}
}

func TestMetricsCheckpointSegments(t *testing.T) {
	db := openMem(t, "mxckptseg.gr")
	defer db.Close()

	// Before any checkpoint the fold has rewritten nothing.
	if c := db.Metrics().Counter("gr_checkpoint_segments_rewritten_total", Labels{"store": "node"}); c != 0 {
		t.Fatalf("node segments before any checkpoint = %d, want 0", c)
	}

	// Node and rel properties give both folds real columns to rewrite into segments.
	for i := 0; i < 50; i++ {
		mustExec(t, db, "CREATE (:Person {note: 'value'})", nil)
	}
	mustExec(t, db, "MATCH (a:Person), (b:Person) WITH a, b LIMIT 20 CREATE (a)-[:KNOWS {since: 2020}]->(b)", nil)
	runPragma(t, db, "PRAGMA wal_checkpoint")

	snap := db.Metrics()
	node := snap.Counter("gr_checkpoint_segments_rewritten_total", Labels{"store": "node"})
	if node == 0 {
		t.Fatalf("node segments after a checkpoint = %d, want > 0", node)
	}
	rel := snap.Counter("gr_checkpoint_segments_rewritten_total", Labels{"store": "rel"})
	if rel == 0 {
		t.Fatalf("rel segments after a checkpoint = %d, want > 0", rel)
	}

	// A second checkpoint folds again, so the cumulative counter advances past the first.
	mustExec(t, db, "MATCH (p:Person) SET p.note = 'changed'", nil)
	runPragma(t, db, "PRAGMA wal_checkpoint")
	if again := db.Metrics().Counter("gr_checkpoint_segments_rewritten_total", Labels{"store": "node"}); again <= node {
		t.Fatalf("node segments after a second checkpoint = %d, want > %d", again, node)
	}
}

func TestMetricsVersionsResident(t *testing.T) {
	db := openMem(t, "mxversions.gr")
	defer db.Close()

	// A fresh database carries no version history.
	if g := db.Metrics().Gauge("gr_mvcc_versions_resident", nil); g != 0 {
		t.Fatalf("versions resident on fresh db = %d, want 0", g)
	}

	mustExec(t, db, "CREATE (:Counter {n: 0})", nil)
	// Each overwrite of the committed value records a pre-image, the version history GC reclaims.
	for i := 0; i < 20; i++ {
		mustExec(t, db, "MATCH (c:Counter) SET c.n = c.n + 1", nil)
	}
	if g := db.Metrics().Gauge("gr_mvcc_versions_resident", nil); g <= 0 {
		t.Fatalf("versions resident after updates = %d, want > 0", g)
	}

	// With no open reader the watermark reaches the latest commit, so a checkpoint's GC reclaims
	// every retained pre-image and the version store drains.
	runPragma(t, db, "PRAGMA wal_checkpoint")
	if g := db.Metrics().Gauge("gr_mvcc_versions_resident", nil); g != 0 {
		t.Fatalf("versions resident after GC = %d, want 0", g)
	}
}

func TestMetricsCheckpointDeltaFolded(t *testing.T) {
	db := openMem(t, "mxckptfold.gr")
	defer db.Close()

	// Before any checkpoint no delta has been folded.
	if c := db.Metrics().Counter("gr_checkpoint_delta_folded_total", nil); c != 0 {
		t.Fatalf("delta folded before any checkpoint = %d, want 0", c)
	}

	// Create relationships so the adjacency delta has staged edges for the fold to absorb.
	for i := 0; i < 50; i++ {
		mustExec(t, db, "CREATE (:Person {note: 'value'})", nil)
	}
	mustExec(t, db, "MATCH (a:Person), (b:Person) WITH a, b LIMIT 20 CREATE (a)-[:KNOWS {since: 2020}]->(b)", nil)
	runPragma(t, db, "PRAGMA wal_checkpoint")

	folded := db.Metrics().Counter("gr_checkpoint_delta_folded_total", nil)
	if folded == 0 {
		t.Fatalf("delta folded after a checkpoint with edges = %d, want > 0", folded)
	}

	// A checkpoint with no new edges folds nothing, so the cumulative counter holds steady.
	runPragma(t, db, "PRAGMA wal_checkpoint")
	if again := db.Metrics().Counter("gr_checkpoint_delta_folded_total", nil); again != folded {
		t.Fatalf("delta folded after an empty checkpoint = %d, want %d (unchanged)", again, folded)
	}

	// New edges stage a fresh delta, so the next checkpoint advances the counter again.
	mustExec(t, db, "MATCH (a:Person), (b:Person) WITH a, b LIMIT 10 CREATE (a)-[:LIKES]->(b)", nil)
	runPragma(t, db, "PRAGMA wal_checkpoint")
	if more := db.Metrics().Counter("gr_checkpoint_delta_folded_total", nil); more <= folded {
		t.Fatalf("delta folded after more edges = %d, want > %d", more, folded)
	}
}

func TestMetricsCheckpointPagesWritten(t *testing.T) {
	db := openMem(t, "mxckptpages.gr")
	defer db.Close()

	// Before any checkpoint nothing has been written back by a fold.
	if c := db.Metrics().Counter("gr_checkpoint_pages_written_total", nil); c != 0 {
		t.Fatalf("pages written before any checkpoint = %d, want 0", c)
	}

	for i := 0; i < 50; i++ {
		mustExec(t, db, "CREATE (:Person {note: 'value'})", nil)
	}
	runPragma(t, db, "PRAGMA wal_checkpoint")

	pages := db.Metrics().Counter("gr_checkpoint_pages_written_total", nil)
	if pages == 0 {
		t.Fatalf("pages written after a checkpoint = %d, want > 0", pages)
	}

	// A second checkpoint folds and commits again, so it writes back more pages and the cumulative
	// counter advances past the first.
	for i := 0; i < 50; i++ {
		mustExec(t, db, "CREATE (:Person {note: 'more'})", nil)
	}
	runPragma(t, db, "PRAGMA wal_checkpoint")
	if again := db.Metrics().Counter("gr_checkpoint_pages_written_total", nil); again <= pages {
		t.Fatalf("pages written after a second checkpoint = %d, want > %d", again, pages)
	}
}

func TestMetricsMvccGC(t *testing.T) {
	db := openMem(t, "mxgc.gr")
	defer db.Close()

	// No GC has run yet, so the run counter and both reclaim series sit at zero.
	base := db.Metrics()
	if c := base.Counter("gr_mvcc_gc_runs_total", nil); c != 0 {
		t.Fatalf("gc runs before any checkpoint = %d, want 0", c)
	}
	if c := base.Counter("gr_mvcc_gc_reclaimed_total", Labels{"element": "node"}); c != 0 {
		t.Fatalf("node versions reclaimed before any GC = %d, want 0", c)
	}

	mustExec(t, db, "CREATE (:Counter {n: 0})", nil)
	for i := 0; i < 15; i++ {
		mustExec(t, db, "MATCH (c:Counter) SET c.n = c.n + 1", nil)
	}
	runPragma(t, db, "PRAGMA wal_checkpoint")

	snap := db.Metrics()
	if c := snap.Counter("gr_mvcc_gc_runs_total", nil); c != 1 {
		t.Fatalf("gc runs after one checkpoint = %d, want 1", c)
	}
	// The overwrites left node pre-images, which GC reclaimed once the watermark advanced.
	if c := snap.Counter("gr_mvcc_gc_reclaimed_total", Labels{"element": "node"}); c == 0 {
		t.Fatalf("node versions reclaimed = %d, want > 0", c)
	}
	// No relationship versions were created, so that series is present and zero.
	if c := snap.Counter("gr_mvcc_gc_reclaimed_total", Labels{"element": "rel"}); c != 0 {
		t.Fatalf("rel versions reclaimed = %d, want 0", c)
	}

	// A second checkpoint runs GC again, so the cumulative run counter advances.
	runPragma(t, db, "PRAGMA wal_checkpoint")
	if c := db.Metrics().Counter("gr_mvcc_gc_runs_total", nil); c != 2 {
		t.Fatalf("gc runs after a second checkpoint = %d, want 2", c)
	}
}

func TestMetricsGCDuration(t *testing.T) {
	db := openMem(t, "mxgcdur.gr")
	defer db.Close()

	// No GC pass has run, so the duration histogram has observed nothing.
	if h := db.Metrics().Histogram("gr_mvcc_gc_duration_seconds", nil); h.Count != 0 {
		t.Fatalf("gc duration observations before any checkpoint = %d, want 0", h.Count)
	}

	mustExec(t, db, "CREATE (:Counter {n: 0})", nil)
	for i := 0; i < 15; i++ {
		mustExec(t, db, "MATCH (c:Counter) SET c.n = c.n + 1", nil)
	}
	runPragma(t, db, "PRAGMA wal_checkpoint")

	// The checkpoint ran one GC pass, so the next scrape drains exactly one buffered duration.
	if h := db.Metrics().Histogram("gr_mvcc_gc_duration_seconds", nil); h.Count != 1 {
		t.Fatalf("gc duration observations after one checkpoint = %d, want 1", h.Count)
	}
	// A second scrape with no further GC must not re-observe the drained duration.
	if h := db.Metrics().Histogram("gr_mvcc_gc_duration_seconds", nil); h.Count != 1 {
		t.Fatalf("gc duration observations on a second scrape = %d, want 1 (no re-observe)", h.Count)
	}

	// A second checkpoint runs GC again, so the histogram observes a second pass.
	runPragma(t, db, "PRAGMA wal_checkpoint")
	if h := db.Metrics().Histogram("gr_mvcc_gc_duration_seconds", nil); h.Count != 2 {
		t.Fatalf("gc duration observations after a second checkpoint = %d, want 2", h.Count)
	}
}

func TestMetricsWatermarkLag(t *testing.T) {
	db := openMem(t, "mxlag.gr")
	defer db.Close()

	mustExec(t, db, "CREATE (:Person {n: 0})", nil)

	// With no reader pinning a snapshot the watermark is caught up to the latest commit.
	if g := db.Metrics().Gauge("gr_mvcc_watermark_lag_versions", nil); g != 0 {
		t.Fatalf("watermark lag with no live reader = %d, want 0", g)
	}

	// Hold a read snapshot open, then commit writes: the watermark stays pinned at the reader's
	// read sequence while the commit sequence advances, so the reclaimable backlog grows.
	rtx, err := db.Begin(context.Background(), Read)
	if err != nil {
		t.Fatalf("begin read: %v", err)
	}
	for i := 0; i < 5; i++ {
		mustExec(t, db, "MATCH (p:Person) SET p.n = p.n + 1", nil)
	}
	if g := db.Metrics().Gauge("gr_mvcc_watermark_lag_versions", nil); g <= 0 {
		t.Fatalf("watermark lag with a pinned reader = %d, want > 0", g)
	}

	// Releasing the reader lets the watermark catch up, so the lag falls back to zero.
	if err := rtx.Rollback(); err != nil {
		t.Fatalf("rollback read: %v", err)
	}
	if g := db.Metrics().Gauge("gr_mvcc_watermark_lag_versions", nil); g != 0 {
		t.Fatalf("watermark lag after the reader released = %d, want 0", g)
	}
}

func TestMetricsOldestSnapshotAge(t *testing.T) {
	db := openMem(t, "oldsnap.gr")
	defer db.Close()

	mustExec(t, db, "CREATE (:Person {n: 0})", nil)

	// With no live reader there is no snapshot to age, so the gauge reads zero.
	if g := db.Metrics().Gauge("gr_mvcc_oldest_snapshot_age_seconds", nil); g != 0 {
		t.Fatalf("oldest snapshot age with no live reader = %d, want 0", g)
	}

	// Hold a read snapshot open across a second so its age crosses the whole-second boundary the
	// gauge truncates to, then assert the gauge sees the live reader's age.
	rtx, err := db.Begin(context.Background(), Read)
	if err != nil {
		t.Fatalf("begin read: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if g := db.Metrics().Gauge("gr_mvcc_oldest_snapshot_age_seconds", nil); g < 1 {
		t.Fatalf("oldest snapshot age with a reader held over a second = %d, want >= 1", g)
	}

	// Releasing the reader leaves no live snapshot, so the gauge falls back to zero.
	if err := rtx.Rollback(); err != nil {
		t.Fatalf("rollback read: %v", err)
	}
	if g := db.Metrics().Gauge("gr_mvcc_oldest_snapshot_age_seconds", nil); g != 0 {
		t.Fatalf("oldest snapshot age after the reader released = %d, want 0", g)
	}
}

func TestMetricsConstraintChecks(t *testing.T) {
	db := openMem(t, "mxcons.gr")
	defer db.Close()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	// A clean insert runs the uniqueness check and it passes.
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	// A duplicate runs the check and it fails, aborting the write.
	if _, err := db.Exec("CREATE (:Person {email: 'a@x'})", nil); err == nil {
		t.Fatal("duplicate insert was accepted")
	}

	snap := db.Metrics()
	if c := snap.Counter("gr_constraint_checks_total", Labels{"constraint": "unique", "result": "pass"}); c != 1 {
		t.Errorf("unique pass checks = %d, want 1", c)
	}
	if c := snap.Counter("gr_constraint_checks_total", Labels{"constraint": "unique", "result": "violation"}); c != 1 {
		t.Errorf("unique violation checks = %d, want 1", c)
	}
	// The other kinds are registered at zero, present before any check of that kind runs.
	if c := snap.Counter("gr_constraint_checks_total", Labels{"constraint": "exists", "result": "violation"}); c != 0 {
		t.Errorf("exists violation checks = %d, want 0", c)
	}
}
