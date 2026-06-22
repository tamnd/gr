package gr

import (
	"context"
	"errors"
	"io"
	"sync"
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

// expandFanoutBuckets is the bucket layout for gr_expand_fanout (doc 20 §6.1), the per-source
// neighbor count. It is finer than rowCountBuckets at the low end, where most expands sit, and
// reaches the millions so a supernode's fan-out lands in a distinct high bucket rather than
// saturating the top one: the gap between a p50 of a few and a p999 in the millions is the
// supernode signal the histogram exists to show (§16.1).
var expandFanoutBuckets = []float64{
	1, 2, 4, 8, 16, 32, 64, 128, 256, 512,
	1024, 4096, 16384, 65536, 262144, 1048576, 4194304,
}

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

// metricCacheResults is the bounded domain of the result label on gr_plan_cache_lookups_total
// (doc 20 §3.2): whether a plan-cache lookup found a usable plan (hit) or had to compile one
// (miss). The hit rate, hit / (hit + miss), is the headline plan-cache efficiency.
var metricCacheResults = []string{"hit", "miss"}

// metricCacheEvictReasons is the bounded domain of the reason label on
// gr_plan_cache_evictions_total (doc 20 §3.2): why a plan left the cache. capacity is the LRU
// eviction under size pressure, the one the cache itself drives; the schema_change, stats_change,
// and manual reasons wait on the hooks that produce them (a catalog-version flush, an explicit
// clear), so they are registered here but not yet incremented.
var metricCacheEvictReasons = []string{"capacity", "schema_change", "stats_change", "manual"}

// metricCacheInvalidCauses is the bounded domain of the cause label on
// gr_plan_cache_invalidations_total (doc 20 §3.2): why a cached plan was rebuilt for coherence.
// stats_refresh is the drift re-plan, where a cache hit's basis has moved far enough that the
// plan is recompiled; the ddl and constraint causes wait on their hooks.
var metricCacheInvalidCauses = []string{"ddl", "stats_refresh", "constraint"}

// metricTxModes is the bounded domain of the mode label on the transaction metrics (doc 20
// §3.3): a transaction is a read or a write. The write gauge stuck above the single-writer
// baseline is the long-held-writer leak signal (§16.4).
var metricTxModes = []string{"read", "write"}

// metricTxOutcomes is the bounded domain of the outcome label on gr_transactions_total (doc 20
// §3.3): a transaction ends in a commit, an abort (an explicit rollback or a commit that failed
// for a non-conflict reason and rolled back), or a conflict (a write that lost the optimistic
// race at commit). The conflict rate is the contention signal (§5, §16.4).
var metricTxOutcomes = []string{"commit", "abort", "conflict"}

// txDurationBuckets is the bucket layout for gr_transaction_duration_seconds (doc 20 §3.3): like
// the query latency buckets but stretched to a minute, since a transaction lifetime ranges longer
// than a single query and the long-tailed write distribution is the symptom worth resolving.
var txDurationBuckets = []float64{
	0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60,
}

// metricErrorClasses is the bounded domain of the class label on gr_query_errors_total (doc 20
// §3.1): the cause an error falls into. The metric is a sub-view of
// gr_queries_total{status="error"} broken out by cause, plus the syntax errors that fail before
// a query classifies into a kind and so never reach gr_queries_total at all.
var metricErrorClasses = []string{"syntax", "semantic", "constraint", "conflict", "resource", "internal"}

// sessionDurationBuckets covers a connection's lifetime, gr_session_duration_seconds (doc 20
// §3.3). A session lives far longer than a query, from a sub-second health probe that connects and
// disconnects to a pooled connection held open for hours, so the buckets run from a millisecond out
// to an hour rather than reusing the query latency scale.
var sessionDurationBuckets = []float64{
	0.001, 0.01, 0.1, 1, 10, 60, 300, 900, 1800, 3600,
}

// metricAuthResults is the bounded domain of the result label on gr_auth_attempts_total (doc 20
// §3.3): whether an authentication attempt succeeded or failed. Both are pre-registered so a server
// that has only ever seen successes still exposes the failure series at zero, the shape an alert on
// a sudden failure rate needs present from the start.
var metricAuthResults = []string{"success", "failure"}

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

	cacheLookups       map[string]*metric.Counter // gr_plan_cache_lookups_total by [result]
	cacheEvictions     map[string]*metric.Counter // gr_plan_cache_evictions_total by [reason]
	cacheInvalidations map[string]*metric.Counter // gr_plan_cache_invalidations_total by [cause]

	queued    *metric.Counter   // gr_query_queued_total (queries that waited for admission)
	queueWait *metric.Histogram // gr_query_queue_wait_seconds (admission queue wait time)

	txOpen     map[string]*metric.Gauge              // gr_transactions_open by [mode]
	txTotal    map[string]map[string]*metric.Counter // gr_transactions_total by [mode][outcome]
	txDuration map[string]*metric.Histogram          // gr_transaction_duration_seconds by [mode]

	shortestPath *metric.Counter // gr_shortest_path_total
	wcoj         *metric.Counter // gr_wcoj_total
	binaryJoin   *metric.Counter // gr_binary_join_total

	wcojIntersect      *metric.Histogram // gr_wcoj_intersect_seconds
	joinBuild          *metric.Histogram // gr_join_build_seconds
	factorized         *metric.Counter   // gr_factorized_total
	factorizationRatio *metric.Histogram // gr_factorization_ratio

	authAttempts map[string]*metric.Counter // gr_auth_attempts_total by [result]

	// sessionHandles caches the per-protocol session metric handles, keyed by protocol string
	// (bolt, http). A protocol's handles are registered on its first session and loaded lock-free
	// after, the same pattern the expand handles use. The key space is the set of wire protocols the
	// server speaks, two at most, so the cache stays tiny.
	sessionHandles sync.Map // string protocol -> *sessionHandles
	// boltMessages caches the per-type message counter, keyed by uppercase message name, and
	// boltErrors the per-code error counter. Both label domains are the protocol's own vocabulary, a
	// dozen message types and a handful of status codes, so a sync.Map of resolved handles bounds the
	// registry work to one resolution per distinct label seen.
	boltMessages sync.Map // string type -> *metric.Counter
	boltErrors   sync.Map // string code -> *metric.Counter

	// relTypeName resolves a relationship-type token to its name for the expand metrics' type
	// label. It is wired at Open once the engine exists; until then it is nil and an expand is
	// attributed to the all-types bucket. The expand metrics are labelled by type, so a token
	// that does not resolve (the all-types wildcard, or an unknown token) falls back to "*".
	relTypeName func(engine.Token) (string, bool)
	// expandHandles caches the per-(type,dir) metric handles for the expand operator, keyed by
	// expandKey. Resolving a handle takes a registry lock and a label-map build, too much for the
	// per-source expand hot path, so the first expand of a (type,dir) resolves and stores the four
	// handles and every later one is a lock-free load. The key space is bounded by the schema's
	// relationship types times three directions, so the cache stays small (§7.2).
	expandHandles sync.Map // expandKey -> *expandHandles
}

// expandKey identifies the metric handles for one expand shape: the operator's type token (zero
// for an expand of every type) and its direction. Both are small integers, so the key is
// comparable and cheap as a sync.Map key.
type expandKey struct {
	tok engine.Token
	dir engine.Direction
}

// expandHandles holds the four expand metric series for one (type,dir). total and neighbors carry
// both labels; fanout and seconds are labelled by type only, so the registry hands back the same
// pointer for the in and out directions of one type and the per-direction entries share them.
type expandHandles struct {
	total     *metric.Counter
	neighbors *metric.Counter
	fanout    *metric.Histogram
	seconds   *metric.Histogram
}

// sessionHandles holds the three session metric series for one protocol: the open-connection gauge,
// the lifetime counter, and the duration histogram. They are resolved together on a protocol's
// first session since a connection that opens always later closes, so all three are touched over
// one session's life.
type sessionHandles struct {
	open     *metric.Gauge
	total    *metric.Counter
	duration *metric.Histogram
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

		cacheLookups:       make(map[string]*metric.Counter, len(metricCacheResults)),
		cacheEvictions:     make(map[string]*metric.Counter, len(metricCacheEvictReasons)),
		cacheInvalidations: make(map[string]*metric.Counter, len(metricCacheInvalidCauses)),

		txOpen:     make(map[string]*metric.Gauge, len(metricTxModes)),
		txTotal:    make(map[string]map[string]*metric.Counter, len(metricTxModes)),
		txDuration: make(map[string]*metric.Histogram, len(metricTxModes)),

		authAttempts: make(map[string]*metric.Counter, len(metricAuthResults)),
	}
	for _, r := range metricAuthResults {
		m.authAttempts[r] = reg.Counter("gr_auth_attempts_total",
			"Authentication attempts, by result", "attempts", metric.Labels{"result": r})
	}
	for _, mode := range metricTxModes {
		m.txOpen[mode] = reg.Gauge("gr_transactions_open",
			"Currently open transactions, by mode", "txns", metric.Labels{"mode": mode})
		byOutcome := make(map[string]*metric.Counter, len(metricTxOutcomes))
		for _, oc := range metricTxOutcomes {
			byOutcome[oc] = reg.Counter("gr_transactions_total",
				"Transactions begun, by mode and outcome", "txns",
				metric.Labels{"mode": mode, "outcome": oc})
		}
		m.txTotal[mode] = byOutcome
		m.txDuration[mode] = reg.Histogram("gr_transaction_duration_seconds",
			"Transaction lifetime, begin to commit or rollback", "seconds",
			txDurationBuckets, metric.Labels{"mode": mode})
	}
	for _, r := range metricCacheResults {
		m.cacheLookups[r] = reg.Counter("gr_plan_cache_lookups_total",
			"Plan-cache lookups by result", "lookups", metric.Labels{"result": r})
	}
	for _, r := range metricCacheEvictReasons {
		m.cacheEvictions[r] = reg.Counter("gr_plan_cache_evictions_total",
			"Plans evicted from the cache, by reason", "evictions", metric.Labels{"reason": r})
	}
	for _, c := range metricCacheInvalidCauses {
		m.cacheInvalidations[c] = reg.Counter("gr_plan_cache_invalidations_total",
			"Cached plans invalidated for coherence, by cause", "invalidations", metric.Labels{"cause": c})
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
	m.queued = reg.Counter("gr_query_queued_total",
		"Queries that waited in the admission queue before executing", "queries", nil)
	m.queueWait = reg.Histogram("gr_query_queue_wait_seconds",
		"Time a query waited in the admission queue before starting", "seconds", queryLatencyBuckets, nil)
	m.shortestPath = reg.Counter("gr_shortest_path_total",
		"Shortest-path searches executed", "searches", nil)
	m.wcoj = reg.Counter("gr_wcoj_total",
		"Worst-case-optimal joins executed (cyclic-pattern evaluation)", "joins", nil)
	m.binaryJoin = reg.Counter("gr_binary_join_total",
		"Binary hash joins executed (tree-pattern evaluation)", "joins", nil)
	m.wcojIntersect = reg.Histogram("gr_wcoj_intersect_seconds",
		"Time in the worst-case-optimal multi-way intersection", "seconds", queryLatencyBuckets, nil)
	m.joinBuild = reg.Histogram("gr_join_build_seconds",
		"Time building binary hash-join build sides", "seconds", queryLatencyBuckets, nil)
	m.factorized = reg.Counter("gr_factorized_total",
		"Operators that produced or consumed factorized intermediates", "operators", nil)
	m.factorizationRatio = reg.Histogram("gr_factorization_ratio",
		"Flat size over factorized size of intermediates, the compression factorization achieved",
		"ratio", rowCountBuckets, nil)
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

// recordCacheLookup counts one plan-cache lookup by its result (doc 20 §3.2): hit when the cache
// served a usable plan, miss when the plan had to be compiled. The hit rate read off this counter
// is the plan-cache efficiency an operator watches; a hit rate that falls while capacity
// evictions rise is the cache-too-small signature (§16.3).
func (m *queryMetrics) recordCacheLookup(result string) {
	if c := m.cacheLookups[result]; c != nil {
		c.Inc()
	}
}

// recordCacheEviction counts one plan leaving the cache by reason (doc 20 §3.2). The cache drives
// the capacity reason directly through its eviction hook.
func (m *queryMetrics) recordCacheEviction(reason string) {
	if c := m.cacheEvictions[reason]; c != nil {
		c.Inc()
	}
}

// recordCacheInvalidation counts one cached plan rebuilt for coherence by cause (doc 20 §3.2).
// The drift re-plan is the stats_refresh cause: a cache hit whose cardinality basis has moved far
// enough that the plan is recompiled on the live statistics.
func (m *queryMetrics) recordCacheInvalidation(cause string) {
	if c := m.cacheInvalidations[cause]; c != nil {
		c.Inc()
	}
}

// recordQueued records that a query waited in the admission queue for wait before it was admitted
// or shed (doc 20 §3.1, §18.3): it counts the query in gr_query_queued_total and observes the
// wait in gr_query_queue_wait_seconds. A query that found a free slot at once does not call this,
// so a nonzero queued rate is the saturation signal, the server queueing or shedding under load,
// distinct from a query that is merely slow to execute.
func (m *queryMetrics) recordQueued(wait time.Duration) {
	if m.queued != nil {
		m.queued.Inc()
	}
	if m.queueWait != nil {
		m.queueWait.Observe(wait.Seconds())
	}
}

// txBegin records that a transaction of the given mode opened, raising the open-transaction
// gauge (doc 20 §3.3). Every txBegin is paired with one txFinish, which lowers the gauge and
// records the outcome and lifetime, so the gauge is the count of transactions open right now.
func (m *queryMetrics) txBegin(mode string) {
	if g := m.txOpen[mode]; g != nil {
		g.Inc()
	}
}

// txFinish records that a transaction of the given mode ended with the given outcome after d (doc
// 20 §3.3): it lowers the open gauge, counts the outcome, and observes the lifetime. The conflict
// outcome is the contention signal an operator reads off the rate.
func (m *queryMetrics) txFinish(mode, outcome string, d time.Duration) {
	if g := m.txOpen[mode]; g != nil {
		g.Dec()
	}
	if byOutcome := m.txTotal[mode]; byOutcome != nil {
		if c := byOutcome[outcome]; c != nil {
			c.Inc()
		}
	}
	if h := m.txDuration[mode]; h != nil {
		h.Observe(d.Seconds())
	}
}

// metricTxMode maps a write flag to the transaction mode label (doc 20 §3.3).
func metricTxMode(write bool) string {
	if write {
		return "write"
	}
	return "read"
}

// metricTxOutcome maps a transaction's finishing error to its outcome label (doc 20 §3.3): no
// error is a commit (or a clean rollback, which the caller passes as abort), a conflict error is
// conflict, and any other failure rolled the transaction back, so it is an abort.
func metricTxOutcome(err error) string {
	switch {
	case err == nil:
		return "commit"
	case errors.Is(err, ErrConflict):
		return "conflict"
	default:
		return "abort"
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

// graphObserver implements exec.GraphObserver against the query metrics (doc 20 §6): it counts a
// shortest-path search, a worst-case-optimal join, and a binary hash join as the operators that
// run them open. One observer value serves every execution against a database, since it holds only
// the shared metric handles and each method is a single atomic add.
type graphObserver struct{ m *queryMetrics }

func (g graphObserver) ShortestPath() {
	if g.m.shortestPath != nil {
		g.m.shortestPath.Inc()
	}
}

func (g graphObserver) WCOJ() {
	if g.m.wcoj != nil {
		g.m.wcoj.Inc()
	}
}

func (g graphObserver) BinaryJoin() {
	if g.m.binaryJoin != nil {
		g.m.binaryJoin.Inc()
	}
}

func (g graphObserver) Expand(relType engine.Token, dir engine.Direction, fanout int, dur time.Duration) {
	g.m.recordExpand(relType, dir, fanout, dur)
}

func (g graphObserver) WCOJIntersect(dur time.Duration) {
	if g.m.wcojIntersect != nil {
		g.m.wcojIntersect.Observe(dur.Seconds())
	}
}

func (g graphObserver) JoinBuild(dur time.Duration) {
	if g.m.joinBuild != nil {
		g.m.joinBuild.Observe(dur.Seconds())
	}
}

func (g graphObserver) Factorized() {
	if g.m.factorized != nil {
		g.m.factorized.Inc()
	}
}

func (g graphObserver) FactorizationRatio(ratio float64) {
	if g.m.factorizationRatio != nil {
		g.m.factorizationRatio.Observe(ratio)
	}
}

// recordExpand records one source position expanded (doc 20 §6.1): it bumps the per-(type,dir)
// expand and neighbor counters and observes the fan-out and the time the engine took. The handles
// are resolved once per (type,dir) and cached, so the steady-state cost is a sync.Map load and
// four atomic updates.
func (m *queryMetrics) recordExpand(relType engine.Token, dir engine.Direction, fanout int, dur time.Duration) {
	h := m.expandHandlesFor(relType, dir)
	h.total.Inc()
	if fanout > 0 {
		h.neighbors.Add(uint64(fanout))
	}
	h.fanout.Observe(float64(fanout))
	h.seconds.Observe(dur.Seconds())
}

// expandHandlesFor returns the cached expand handles for a (type,dir), resolving and registering
// them on the first call for that key. The type label is the relationship-type name, or "*" when
// the operator expands every type or the token does not resolve; the dir label is out, in, or
// both.
func (m *queryMetrics) expandHandlesFor(relType engine.Token, dir engine.Direction) *expandHandles {
	key := expandKey{tok: relType, dir: dir}
	if v, ok := m.expandHandles.Load(key); ok {
		return v.(*expandHandles)
	}
	typeLabel := "*"
	if relType != 0 && m.relTypeName != nil {
		if name, ok := m.relTypeName(relType); ok {
			typeLabel = name
		}
	}
	dirLabel := metricExpandDir(dir)
	h := &expandHandles{
		total: m.reg.Counter("gr_expand_total",
			"Expand operations, one per source position expanded, by type and direction", "expands",
			metric.Labels{"type": typeLabel, "dir": dirLabel}),
		neighbors: m.reg.Counter("gr_expand_neighbors_total",
			"Neighbors produced by expands, by type and direction", "neighbors",
			metric.Labels{"type": typeLabel, "dir": dirLabel}),
		fanout: m.reg.Histogram("gr_expand_fanout",
			"Per-expand fan-out, the neighbors one source produced; its tail is the supernode signal",
			"neighbors", expandFanoutBuckets, metric.Labels{"type": typeLabel}),
		seconds: m.reg.Histogram("gr_expand_seconds",
			"Time per expand, by type", "seconds", queryLatencyBuckets, metric.Labels{"type": typeLabel}),
	}
	actual, _ := m.expandHandles.LoadOrStore(key, h)
	return actual.(*expandHandles)
}

// metricExpandDir maps an engine direction to the dir label on the expand metrics (doc 20 §6.1).
func metricExpandDir(dir engine.Direction) string {
	switch dir {
	case engine.Outgoing:
		return "out"
	case engine.Incoming:
		return "in"
	default:
		return "both"
	}
}

// sessionOpen records a connection that finished its handshake (doc 20 §3.3): it bumps the
// per-protocol open gauge and the lifetime counter. The pair lets a reader see both how many
// connections are live now and how many have ever been served.
func (m *queryMetrics) sessionOpen(protocol string) {
	h := m.sessionHandlesFor(protocol)
	h.open.Inc()
	h.total.Inc()
}

// sessionClose records a connection ending after living dur (doc 20 §3.3): it drops the open gauge
// back and observes the lifetime. open is symmetric with sessionOpen so the gauge tracks the true
// live count even across protocols.
func (m *queryMetrics) sessionClose(protocol string, dur time.Duration) {
	h := m.sessionHandlesFor(protocol)
	h.open.Dec()
	h.duration.Observe(dur.Seconds())
}

// sessionHandlesFor returns the cached session handles for a protocol, registering them on the
// first session of that protocol. The protocol label is the wire name, bolt or http.
func (m *queryMetrics) sessionHandlesFor(protocol string) *sessionHandles {
	if v, ok := m.sessionHandles.Load(protocol); ok {
		return v.(*sessionHandles)
	}
	h := &sessionHandles{
		open: m.reg.Gauge("gr_sessions_open",
			"Currently open client sessions, by protocol", "sessions",
			metric.Labels{"protocol": protocol}),
		total: m.reg.Counter("gr_sessions_total",
			"Client sessions opened, by protocol", "sessions",
			metric.Labels{"protocol": protocol}),
		duration: m.reg.Histogram("gr_session_duration_seconds",
			"Session lifetime, handshake to disconnect, by protocol", "seconds",
			sessionDurationBuckets, metric.Labels{"protocol": protocol}),
	}
	actual, _ := m.sessionHandles.LoadOrStore(protocol, h)
	return actual.(*sessionHandles)
}

// recordBoltMessage counts one dispatched Bolt message by its uppercase type (doc 20 §3.3). The
// per-type counter is resolved once and cached, so a steady stream of one message type costs a
// sync.Map load and an atomic increment.
func (m *queryMetrics) recordBoltMessage(msgType string) {
	if v, ok := m.boltMessages.Load(msgType); ok {
		v.(*metric.Counter).Inc()
		return
	}
	c := m.reg.Counter("gr_bolt_messages_total",
		"Bolt protocol messages dispatched, by message type", "messages",
		metric.Labels{"type": msgType})
	actual, _ := m.boltMessages.LoadOrStore(msgType, c)
	actual.(*metric.Counter).Inc()
}

// recordBoltError counts one Bolt protocol error by its status code (doc 20 §3.3): a handshake
// failure, a framing fault, a protocol misuse, or an auth rejection. These are connection-level
// faults an operator separates from query errors, so they live on their own counter rather than
// gr_query_errors_total.
func (m *queryMetrics) recordBoltError(code string) {
	if v, ok := m.boltErrors.Load(code); ok {
		v.(*metric.Counter).Inc()
		return
	}
	c := m.reg.Counter("gr_bolt_errors_total",
		"Bolt protocol errors, by status code", "errors",
		metric.Labels{"code": code})
	actual, _ := m.boltErrors.LoadOrStore(code, c)
	actual.(*metric.Counter).Inc()
}

// recordAuth counts one authentication attempt by result (doc 20 §3.3). Both result series are
// pre-registered, so this is a map read and an atomic increment.
func (m *queryMetrics) recordAuth(ok bool) {
	result := "failure"
	if ok {
		result = "success"
	}
	if c := m.authAttempts[result]; c != nil {
		c.Inc()
	}
}

// scanLoad reads a scan counter, treating a nil counter (a result with no execution, such as a
// schema or EXPLAIN result) as zero scanned rows.
func scanLoad(c *atomic.Int64) int64 {
	if c == nil {
		return 0
	}
	return c.Load()
}
