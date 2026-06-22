package gr

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/metric"
	"github.com/tamnd/gr/parse"
)

// Labels is a metric's bounded label set (doc 20 §7.2), re-exported from the metric package
// so an embedder names a series without importing the low-level package: db.Metrics().Counter(
// "gr_queries_total", gr.Labels{"status": "ok", "kind": "read"}).
type Labels = metric.Labels

// MetricsSnapshot is an immutable, point-in-time copy of the whole metric registry (doc 20
// §7.3), what db.Metrics returns. Its Counter, Gauge, and Histogram lookups and its HitRate,
// Quantile, and Rate derivations read the same numbers the Prometheus and expvar surfaces
// render, so a test asserts on the value an operator will see.
type MetricsSnapshot = metric.Snapshot

// MetricSnapshot is one series in a snapshot: its name, type, help, unit, labels, and value.
type MetricSnapshot = metric.MetricSnapshot

// HistogramValue is an immutable view of a histogram (doc 20 §7.3), with Quantile and Mean
// derived off its buckets.
type HistogramValue = metric.HistogramValue

// MetricType is a metric's kind: counter, gauge, or histogram (doc 20 §2.2).
type MetricType = metric.Type

// The three metric types (doc 20 §2.2), re-exported so a caller switches on a snapshot's
// MetricSnapshot.Type without importing the metric package.
const (
	MetricCounter   = metric.TypeCounter
	MetricGauge     = metric.TypeGauge
	MetricHistogram = metric.TypeHistogram
)

// Metrics returns a snapshot of the database's metric registry (doc 20 §7.3): every counter,
// gauge, and histogram a subsystem has registered, read at this instant. It is the
// programmatic exposition surface, the one the CLI's .metrics command and a test read; the
// server renders the same registry as Prometheus text and expvar JSON.
func (db *DB) Metrics() MetricsSnapshot {
	return db.metrics.reg.Snapshot()
}

// WritePrometheus renders a metrics snapshot in the Prometheus text exposition format (doc 20
// §7.5), re-exported so the server and an embedder render db.Metrics() without importing the
// low-level metric package.
func WritePrometheus(w io.Writer, snap MetricsSnapshot) error {
	return metric.WritePrometheus(w, snap)
}

// WriteExpvar renders a metrics snapshot as the expvar JSON tree (doc 20 §7.6), the same
// registry the Prometheus surface renders, for an operator on the Go expvar convention.
func WriteExpvar(w io.Writer, snap MetricsSnapshot) error {
	return metric.WriteExpvar(w, snap)
}

// queryLatencyBuckets is the bucket layout for gr_query_duration_seconds and the other query
// latency histograms (doc 20 §2.5): exponential from a hundred microseconds to ten seconds,
// several buckets per decade, so the p99 and p999 in the millisecond-to-second range an
// operator watches are well resolved. The +Inf catch-all is added by the histogram itself.
var queryLatencyBuckets = []float64{
	0.0001, 0.00025, 0.0005,
	0.001, 0.0025, 0.005,
	0.01, 0.025, 0.05,
	0.1, 0.25, 0.5,
	1, 2.5, 5, 10,
}

// rowCountBuckets is the bucket layout for gr_query_rows_returned and gr_query_rows_scanned
// (doc 20 §3.1): powers of ten from one row to ten million, so the output cardinality and the
// scan work an operator reads span the orders of magnitude a graph query ranges over. A query
// that touches no rows lands in the first bucket; the +Inf catch-all is added by the histogram.
var rowCountBuckets = []float64{1, 10, 100, 1000, 10000, 100000, 1000000, 10000000}

// metricQueryKinds is the bounded domain of the kind label on the query metrics (doc 20 §3.1,
// §7.2): the statement classes a query falls into. The handles for every kind are pre-resolved
// at open so recording one is a map read and an atomic add, never a registry lock.
var metricQueryKinds = []string{"read", "write", "schema", "admin", "pragma", "explain"}

// metricQueryStatuses is the bounded domain of the status label on gr_queries_total (doc 20
// §3.1): the outcomes a query ends in.
var metricQueryStatuses = []string{"ok", "error", "timeout", "killed"}

// metricPlanCache is the bounded domain of the cache label on gr_query_plan_duration_seconds
// (doc 20 §3.1): whether the plan came from the plan cache (hit, which skips most of the work)
// or was compiled fresh (miss, which pays full planning). A write is always a miss, since the
// write path does not cache.
var metricPlanCache = []string{"hit", "miss"}

// metricErrorClasses is the bounded domain of the class label on gr_query_errors_total (doc 20
// §3.1): the cause an error falls into. The metric is a sub-view of
// gr_queries_total{status="error"} broken out by cause, plus the syntax errors that fail before
// a query classifies into a kind and so never reach gr_queries_total at all.
var metricErrorClasses = []string{"syntax", "semantic", "constraint", "conflict", "resource", "internal"}

// queryMetrics holds the pre-resolved handles for the query throughput, latency, and in-flight
// metrics (doc 20 §3.1). Resolving every (kind, status) handle once at open keeps the record
// path off the registry lock: recording a finished query is a couple of map reads and an
// atomic add on a handle, the allocation-free, lock-free hot path (doc 20 §7.4).
type queryMetrics struct {
	reg      *metric.Registry
	total    map[string]map[string]*metric.Counter // gr_queries_total by [kind][status]
	duration map[string]*metric.Histogram          // gr_query_duration_seconds by [kind]
	inflight map[string]*metric.Gauge              // gr_query_inflight by [kind]
	errors   map[string]*metric.Counter            // gr_query_errors_total by [class]
	returned *metric.Histogram                     // gr_query_rows_returned (output cardinality)
	scanned  *metric.Histogram                     // gr_query_rows_scanned (scan and expand work)
	plan     map[string]*metric.Histogram          // gr_query_plan_duration_seconds by [cache]
	execute  map[string]*metric.Histogram          // gr_query_execute_duration_seconds by [kind]
}

// newQueryMetrics builds the registry and pre-resolves every query-metric handle (doc 20
// §3.1). It is called once at Open, so the database always has a live registry and db.Metrics
// never returns nil.
func newQueryMetrics() *queryMetrics {
	reg := metric.NewRegistry()
	m := &queryMetrics{
		reg:      reg,
		total:    make(map[string]map[string]*metric.Counter, len(metricQueryKinds)),
		duration: make(map[string]*metric.Histogram, len(metricQueryKinds)),
		inflight: make(map[string]*metric.Gauge, len(metricQueryKinds)),
		errors:   make(map[string]*metric.Counter, len(metricErrorClasses)),
		plan:     make(map[string]*metric.Histogram, len(metricPlanCache)),
		execute:  make(map[string]*metric.Histogram, len(metricQueryKinds)),
	}
	for _, c := range metricErrorClasses {
		m.errors[c] = reg.Counter("gr_query_errors_total",
			"Query errors by cause", "errors", metric.Labels{"class": c})
	}
	for _, c := range metricPlanCache {
		m.plan[c] = reg.Histogram("gr_query_plan_duration_seconds",
			"Time to obtain the query plan, by plan-cache outcome", "seconds",
			queryLatencyBuckets, metric.Labels{"cache": c})
	}
	for _, k := range metricQueryKinds {
		byStatus := make(map[string]*metric.Counter, len(metricQueryStatuses))
		for _, s := range metricQueryStatuses {
			byStatus[s] = reg.Counter("gr_queries_total",
				"Cypher queries executed, by kind and status", "queries",
				metric.Labels{"kind": k, "status": s})
		}
		m.total[k] = byStatus
		m.duration[k] = reg.Histogram("gr_query_duration_seconds",
			"End-to-end query latency, parse through last result row", "seconds",
			queryLatencyBuckets, metric.Labels{"kind": k})
		m.inflight[k] = reg.Gauge("gr_query_inflight",
			"Currently executing queries, by kind", "queries",
			metric.Labels{"kind": k})
		m.execute[k] = reg.Histogram("gr_query_execute_duration_seconds",
			"Time spent in the executor only, by kind, excluding parse and plan", "seconds",
			queryLatencyBuckets, metric.Labels{"kind": k})
	}
	m.returned = reg.Histogram("gr_query_rows_returned",
		"Rows in the result set, the output cardinality", "rows", rowCountBuckets, nil)
	m.scanned = reg.Histogram("gr_query_rows_scanned",
		"Rows touched by scans and expands, the work the query did", "rows", rowCountBuckets, nil)
	return m
}

// recordRows observes one finished query's output cardinality and scan work (doc 20 §3.1): the
// rows it returned and the rows its scans and expands touched. The ratio scanned/returned is
// the amplification an inefficient query reveals, so the two are always observed together at the
// same completion point a query's latency is recorded.
func (m *queryMetrics) recordRows(returned, scanned int64) {
	if m.returned != nil {
		m.returned.Observe(float64(returned))
	}
	if m.scanned != nil {
		m.scanned.Observe(float64(scanned))
	}
}

// recordPlan observes the time to obtain one query's plan (doc 20 §3.1), labelled by whether the
// plan cache served it (cache=hit, which pays only the lookup) or it was compiled fresh
// (cache=miss, which pays parse, bind, and planning). A write is always a miss, since the write
// path does not cache. Splitting plan time out of the end-to-end latency tells a plan-bound slow
// query (a cold cache, a query that keeps missing) from an execute-bound one.
func (m *queryMetrics) recordPlan(cache string, d time.Duration) {
	if h := m.plan[cache]; h != nil {
		h.Observe(d.Seconds())
	}
}

// recordExecute observes the time one query of the given kind spent in the executor alone (doc 20
// §3.1), the span from the plan being ready to the last result row, excluding parse and plan. It
// is the other half of the split recordPlan starts: the end-to-end latency is roughly parse plus
// plan plus execute, so an operator reads which phase a slow query is bound by.
func (m *queryMetrics) recordExecute(kind string, d time.Duration) {
	if h := m.execute[kind]; h != nil {
		h.Observe(d.Seconds())
	}
}

// begin records that a query of the given kind started, raising the in-flight gauge (doc 20
// §3.1). Every begin is paired with one finish, which lowers the gauge again, so the gauge is
// the count of queries executing right now.
func (m *queryMetrics) begin(kind string) {
	if g := m.inflight[kind]; g != nil {
		g.Inc()
	}
}

// finish records that a query of the given kind ended with the given status after d (doc 20
// §3.1): it lowers the in-flight gauge, increments the outcome counter, and observes the
// latency. It is the single completion point both the eager paths and the streaming read path
// call, so the throughput, the outcome split, and the latency distribution stay consistent.
func (m *queryMetrics) finish(kind, status string, d time.Duration) {
	if g := m.inflight[kind]; g != nil {
		g.Dec()
	}
	if byStatus := m.total[kind]; byStatus != nil {
		if c := byStatus[status]; c != nil {
			c.Inc()
		}
	}
	if h := m.duration[kind]; h != nil {
		h.Observe(d.Seconds())
	}
}

// recordError increments gr_query_errors_total for the error's class (doc 20 §3.1). It is the
// one error-counting point every terminal error path calls, including a parse failure that
// never classifies into a kind, so the error metric is the complete error-by-cause view: it is
// broader than the status split on gr_queries_total, which omits a pre-kind parse error.
func (m *queryMetrics) recordError(err error) {
	if err == nil {
		return
	}
	if c := m.errors[metricErrorClass(err)]; c != nil {
		c.Inc()
	}
}

// metricQueryKind classifies a parsed statement into its query-metric kind label (doc 20
// §3.1): EXPLAIN is its own kind (it never executes the underlying statement), then admin,
// pragma, and schema by their clause, then a statement with write clauses is a write and the
// rest are reads. PROFILE is not yet a distinct statement form, so it is not split out here.
func metricQueryKind(q *ast.Query) string {
	switch {
	case q.Explain:
		return "explain"
	case q.Admin != nil:
		return "admin"
	case q.Pragma != nil:
		return "pragma"
	case q.Schema != nil:
		return "schema"
	case queryHasWrites(q):
		return "write"
	default:
		return "read"
	}
}

// metricStatusOf maps a query's outcome error to its status label (doc 20 §3.1): no error is
// ok, a cancelled or timed-out context is timeout (so the deadline tail is distinguishable
// from a genuine failure), and any other error is a plain error. The killed status waits on
// the query-kill path that produces it.
func metricStatusOf(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	default:
		return "error"
	}
}

// metricErrorClass maps an error to its gr_query_errors_total class label (doc 20 §3.1). It
// keys off the typed errors and sentinels the library exposes, the same taxonomy the CLI exit
// codes and the server's Neo4j status mapping use: a parse error is syntax, a bind error is
// semantic, a constraint violation is constraint, a transaction conflict is conflict, a
// deadline or admission refusal is resource, and anything that names no known cause is
// internal. The class is coarser than the full error space on purpose, since it is a label
// dimension an operator alerts on, not a message.
func metricErrorClass(err error) string {
	var perr *parse.Error
	var berr *bind.Error
	var cerr *engine.ConstraintError
	switch {
	case errors.As(err, &perr):
		return "syntax"
	case errors.As(err, &berr):
		return "semantic"
	case errors.As(err, &cerr):
		return "constraint"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled),
		errors.Is(err, ErrOverloaded), errors.Is(err, ErrRateLimited):
		return "resource"
	default:
		return "internal"
	}
}

// measureQuery records the query metrics for one statement at a dispatch boundary (doc 20
// §3.1). begin must already have raised the in-flight gauge for kind. An error or an eagerly
// executed result (a write, a schema or admin command, a pragma, an EXPLAIN) is complete now,
// so it records immediately; a streaming read is not finished until the caller drains and
// closes it, so it carries the recording on the result and fires it once at Close, which is
// where the parse-through-last-row latency actually ends.
func (db *DB) measureQuery(kind string, start time.Time, res *Result, err error) (*Result, error) {
	if err != nil {
		db.metrics.recordError(err)
		db.metrics.finish(kind, metricStatusOf(err), time.Since(start))
		return res, err
	}
	if res != nil && res.cursor != nil {
		res.mdb = db
		res.mkind = kind
		res.mstart = start
		return res, nil
	}
	// An eager result is complete now: its returned count is the buffered row count and its
	// scan work is final on the counter, so record the amplification pair alongside the
	// latency (doc 20 §3.1). A write with no RETURN has no output columns and buffers a single
	// effect row the caller never reads, so its output cardinality is zero, not that row.
	if res != nil {
		returned := int64(len(res.buf))
		if len(res.cols) == 0 {
			returned = 0
		}
		db.metrics.recordRows(returned, scanLoad(res.mscan))
	}
	db.metrics.finish(kind, "ok", time.Since(start))
	return res, nil
}

// scanLoad reads a scan counter, treating a nil counter (a result with no execution, such as a
// schema or EXPLAIN result) as zero scanned rows.
func scanLoad(c *atomic.Int64) int64 {
	if c == nil {
		return 0
	}
	return c.Load()
}
