package gr

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tamnd/gr/value"
)

// QueryLogLevel sets how much of the query stream the query log records (doc 20 §10.4),
// trading volume against completeness. The slow-query log is always on regardless of the
// level: a query past the slow threshold is logged even at QueryLogOff (doc 20 §10.6).
type QueryLogLevel int

const (
	// QueryLogOff records no query per se, but the slow-query log still fires, so an
	// operator keeps the slow tail without the volume of every query.
	QueryLogOff QueryLogLevel = iota
	// QueryLogErrors records only queries that errored, timed out, or were killed.
	QueryLogErrors
	// QueryLogSlow records the failures plus the slow tail, the production default.
	QueryLogSlow
	// QueryLogAll records every query, the full audit trail.
	QueryLogAll
)

// RedactPolicy governs how query parameters appear in the log (doc 20 §10.3). Parameters
// carry the actual queried data, so logging them verbatim turns the log into a copy of the
// data; the default redacts every value to its key and type.
type RedactPolicy int

const (
	// RedactAll logs each parameter's key and type but not its value (the default), enough
	// to reproduce the query's plan without leaking the data (doc 20 §10.3).
	RedactAll RedactPolicy = iota
	// RedactHashed logs a stable hash of each value, so repeated values correlate across
	// entries without revealing them.
	RedactHashed
	// RedactNone logs full values, for a debugging session on non-sensitive data only.
	RedactNone
)

// defaultSlowQuery is the slow-query threshold when none is configured (doc 20 §10.6, doc
// 24): a query slower than this is logged in full regardless of the query-log level.
const defaultSlowQuery = time.Second

// PlanCapture sets how much of a slow query's plan the slow-query log keeps (doc 20 §10.6,
// §28.2). A slow-query entry carries the plan the query ran, so an operator diagnoses an
// incident from the captured plan without reproducing it, the difference between "a query
// was slow last night" and "here is the plan it ran". Capture costs the plan serialization,
// so the depth is a configured choice.
type PlanCapture int

const (
	// PlanCaptureOff keeps no plan with a slow-query entry, the lightest setting for a
	// deployment that wants the slow tail without the plan-serialization cost.
	PlanCaptureOff PlanCapture = iota
	// PlanCaptureExplain keeps the EXPLAIN-grade plan tree (section 8), the production
	// default: it serializes the already-computed plan, so it is cheap and adds nothing to
	// the fast path of a query that turns out fast.
	PlanCaptureExplain
	// PlanCaptureProfile keeps the PROFILE-grade plan with actual rows and times (section 9).
	// It is opt-in because it instruments every query in case it turns out slow, paying the
	// per-call profiling overhead on all of them, a trade for diagnosing a recurring slow
	// query that cannot be caught live. The profiling shim is not yet wired into the served
	// query path, so this currently captures the EXPLAIN-grade plan and is documented as the
	// ceiling the capture will reach.
	PlanCaptureProfile
)

// QueryRecord is one query execution's facts, assembled by a server surface and handed to
// the query log (doc 20 §10.2). A surface fills what it knows; the engine-internal fields
// the spec also lists (peak memory, plan time, spill) wait on the executor instrumentation
// that produces them and are omitted until then. Rows scanned is wired: the scan counter the
// metrics path already reads feeds it, so the amplification ratio (doc 20 §16.2) is on the
// record.
type QueryRecord struct {
	StartedAt    time.Time      // when the query started, the entry's ts
	QueryID      string         // unique per execution, correlates log, trace, and slow-query entries
	SessionID    string         // the connection that issued the query
	User         string         // the authenticated user, or "embedded" for a library call
	Client       string         // the client address or agent, for source attribution
	Cypher       string         // the query text with parameter placeholders intact
	Params       map[string]any // the parameters, redacted on emit per the policy
	Kind         string         // read / write / schema / admin / pragma
	Status       string         // ok / error / timeout / killed
	Err          error          // on a non-ok status, the failure
	Duration     time.Duration  // end-to-end query duration
	PlanMs       float64        // time the plan phase took in milliseconds; zero when the planner was skipped (parse failure, EXPLAIN)
	RowsReturned int            // result row count
	RowsScanned  int            // rows scans and expands touched, the work; the scanned/returned ratio is the amplification
	TxID         string         // the transaction the query ran in
	// Plan, when set, renders the EXPLAIN-grade plan tree the query ran (doc 20 §10.6). It
	// is a thunk, not the rendered text, so the cost of serializing the plan is paid only
	// when the log decides a query is slow and plan capture is on, never on the fast path of
	// a query that turns out quick. A surface sets it to the result's PlanText; nil leaves a
	// slow-query entry without a captured plan, the same as plan capture being off.
	Plan func() string
}

// QueryLog writes structured query-log entries through an slog.Logger (doc 20 §10, §11.2),
// so the log lands wherever the embedder or the server points slog: a JSON aggregator, a
// console handler, a file. It applies the level filter (doc 20 §10.4), the always-on
// slow-query rule (doc 20 §10.6), and parameter redaction (doc 20 §10.3).
//
// A nil *QueryLog is disabled and records nothing, the embedded-friendly default when no
// logger is configured.
type QueryLog struct {
	logger  *slog.Logger
	level   QueryLogLevel
	redact  RedactPolicy
	slow    time.Duration
	capture PlanCapture
	// events, when set, receives a query_slow event for a slow query and a query_error
	// event for a failed one (doc 20 §11.3), the lighter signals an operator alerts on
	// alongside the full query-log record. nil leaves the event stream untouched, so the
	// query log works without an event log. It fires on the event log's own threshold,
	// independent of the query-log level, so a failed query at QueryLogOff still raises a
	// query_error event even though its full record is not written.
	events *EventLog
}

// NewQueryLog builds a query log that writes through logger at the given level and
// redaction policy, treating a query slower than slow as a slow query (doc 20 §10). A nil
// logger returns nil, a disabled log. A slow of zero or less uses defaultSlowQuery.
func NewQueryLog(logger *slog.Logger, level QueryLogLevel, redact RedactPolicy, slow time.Duration) *QueryLog {
	if logger == nil {
		return nil
	}
	if slow <= 0 {
		slow = defaultSlowQuery
	}
	// EXPLAIN-grade capture is the production default (doc 20 §28.2): it serializes the
	// already-computed plan, so a slow-query entry carries the plan it ran at no cost to the
	// fast path. WithPlanCapture changes the depth.
	return &QueryLog{logger: logger, level: level, redact: redact, slow: slow, capture: PlanCaptureExplain}
}

// WithPlanCapture sets how much of a slow query's plan the slow-query log keeps (doc 20
// §10.6, §28.2) and returns the log for chaining. A nil query log is left nil. The default
// from NewQueryLog is PlanCaptureExplain, the cheap, default-on capture; PlanCaptureOff
// drops the plan, PlanCaptureProfile is the opt-in profiled capture.
func (l *QueryLog) WithPlanCapture(c PlanCapture) *QueryLog {
	if l != nil {
		l.capture = c
	}
	return l
}

// WithEvents links the query log to an event log so a slow query raises a query_slow event
// and a failed one a query_error event (doc 20 §11.3), in addition to the full query-log
// record. It returns the log for chaining. A nil query log is left nil. The event log fires
// on its own threshold, independent of the query-log level.
func (l *QueryLog) WithEvents(e *EventLog) *QueryLog {
	if l != nil {
		l.events = e
	}
	return l
}

// Record logs one query execution if the level and slow-query rules call for it (doc 20
// §10.4, §10.6). A nil log records nothing, so a caller always calls Record without first
// checking whether a log is configured.
func (l *QueryLog) Record(r QueryRecord) {
	if l == nil {
		return
	}
	slow := l.slow > 0 && r.Duration >= l.slow
	failed := r.Status != "" && r.Status != "ok"

	// The event log carries the lighter query_slow/query_error signals (doc 20 §11.3),
	// fired here so they are independent of the query-log level: a failed query at
	// QueryLogOff still raises query_error even though its full record below is suppressed.
	// A failure dominates a slow query, matching the query-log severity, so a slow failure
	// is one query_error, not also a query_slow.
	if l.events != nil {
		switch {
		case failed:
			errText := ""
			if r.Err != nil {
				errText = r.Err.Error()
			}
			l.events.QueryError(r.QueryID, r.Kind, r.Status, errText)
		case slow:
			l.events.QuerySlow(r.QueryID, r.Kind, r.Duration, l.slow)
		}
	}

	if !l.shouldLog(slow, failed) {
		return
	}

	// The severity follows the outcome so a log pipeline alerts on the right level (doc 20
	// §11.3): a failure is an error, a slow query a warning, an ordinary query info.
	level := slog.LevelInfo
	switch {
	case failed:
		level = slog.LevelError
	case slow:
		level = slog.LevelWarn
	}

	attrs := []slog.Attr{
		slog.String("ts", r.StartedAt.UTC().Format(time.RFC3339Nano)),
		slog.String("event", "query"),
		slog.String("query_id", r.QueryID),
		slog.String("session_id", r.SessionID),
		slog.String("user", r.User),
		slog.String("client", r.Client),
		slog.String("cypher", r.Cypher),
		slog.Any("params", l.redactParams(r.Params)),
		slog.String("kind", r.Kind),
		slog.String("status", r.Status),
		slog.Float64("duration_ms", float64(r.Duration)/float64(time.Millisecond)),
		slog.Float64("plan_ms", r.PlanMs),
		slog.Int("rows_returned", r.RowsReturned),
		slog.Int("rows_scanned", r.RowsScanned),
	}
	if r.TxID != "" {
		attrs = append(attrs, slog.String("tx_id", r.TxID))
	}
	if slow {
		attrs = append(attrs, slog.Bool("slow", true), slog.Float64("threshold_ms", float64(l.slow)/float64(time.Millisecond)))
		// A slow-query entry carries the captured plan (doc 20 §10.6): the plan the query ran,
		// so an operator diagnoses the incident from the entry without reproducing it. The plan
		// thunk runs only here, on the slow path, so the serialization cost lands only on the
		// queries worth keeping the plan for. Capture off, or a surface that supplied no plan,
		// leaves the entry without one.
		if l.capture != PlanCaptureOff && r.Plan != nil {
			if text := r.Plan(); text != "" {
				attrs = append(attrs, slog.String("plan", text))
			}
		}
	}
	if failed && r.Err != nil {
		attrs = append(attrs, slog.String("error", r.Err.Error()))
	}
	l.logger.LogAttrs(context.Background(), level, "query", attrs...)
}

// shouldLog applies the level filter and the always-on slow-query rule (doc 20 §10.4,
// §10.6): a slow query is logged at any level, a failure from QueryLogErrors up, and
// everything at QueryLogAll.
func (l *QueryLog) shouldLog(slow, failed bool) bool {
	if slow {
		return true
	}
	switch l.level {
	case QueryLogAll:
		return true
	case QueryLogSlow, QueryLogErrors:
		return failed
	default: // QueryLogOff: only the slow-query rule above fires.
		return false
	}
}

// redactParams applies the redaction policy to a parameter map (doc 20 §10.3). The query
// text already carries placeholders rather than values, so the params map is the only
// place a value could leak, and this is where the policy governs it.
func (l *QueryLog) redactParams(p map[string]any) map[string]any {
	if len(p) == 0 {
		return nil
	}
	if l.redact == RedactNone {
		return p
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		if l.redact == RedactHashed {
			out[k] = hashParam(v)
		} else {
			out[k] = paramShape(v)
		}
	}
	return out
}

// paramShape describes a value by its type and, for collections, its length, without its
// content (doc 20 §10.3): a string is "<string>", a list of ints "<list[int] len=42>".
func paramShape(v any) string {
	switch x := v.(type) {
	case nil:
		return "<null>"
	case bool:
		return "<bool>"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "<int>"
	case float32, float64:
		return "<float>"
	case string:
		return "<string>"
	case []byte:
		return fmt.Sprintf("<bytes len=%d>", len(x))
	case []any:
		return fmt.Sprintf("<list[%s] len=%d>", elementType(x), len(x))
	case map[string]any:
		return fmt.Sprintf("<map len=%d>", len(x))
	default:
		return "<value>"
	}
}

// elementType names a list's element type for the shape string, or "any" for an empty or
// mixed list, so "<list[int] len=42>" tells an operator what the list holds.
func elementType(xs []any) string {
	if len(xs) == 0 {
		return "any"
	}
	first := paramShape(xs[0])
	for _, x := range xs[1:] {
		if paramShape(x) != first {
			return "any"
		}
	}
	// Strip the angle brackets from the element's shape: "<int>" becomes "int".
	return first[1 : len(first)-1]
}

// hashParam returns a short stable hash of a value for RedactHashed (doc 20 §10.3), so the
// same value produces the same token across entries without revealing the value.
func hashParam(v any) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%v", v)))
	return "sha256:" + hex.EncodeToString(sum[:6])
}

// mintQueryID returns the correlation id one execution carries through its query-log entry,
// its query_slow or query_error event, and its trace span (doc 20 §10.2, §11.3, §12.3), a
// random 128-bit token hex-encoded so entries from different opens never collide in a shared
// log sink.
func mintQueryID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// queryID mints the correlation id one statement carries through its trace span and its
// query-log entry (doc 20 §10.2, §12.3) when either a tracer or a query log is configured, so
// the two join on the same id. It returns the empty string when neither is, leaving the random
// draw off the fast path of a database that neither traces nor logs.
func (db *DB) queryID() string {
	if db.tracer == nil && db.querylog == nil {
		return ""
	}
	id, _ := mintQueryID()
	return id
}

// logQuery records one completed statement to the query log when one is configured (doc 20
// §10), the embedded counterpart of the server's recordQuery. It converts the internal
// parameter values to the plain Go values the redaction policy inspects, stamps the duration
// from start, and hands the record to the log, which decides by its level and slow threshold
// whether to write it and raises the query_slow or query_error event. A nil query log makes
// this a no-op, so the call sites never guard it, and the parameter conversion runs only when a
// log is present. id is the correlation id the caller already minted so the entry, its events,
// and the trace span share it (doc 20 §12.3). plan renders the captured plan for a slow entry
// and is nil when the statement failed before a plan existed. rows and scanned are the output
// cardinality and the work the query touched, whose ratio is the amplification the slow-query
// log surfaces (doc 20 §16.2); a statement that failed before it ran reports zero for both. The
// user is "embedded", the library-call principal the spec names (doc 20 §10.2).
func (db *DB) logQuery(id, kind, cypher string, params map[string]value.Value, start time.Time, status string, qerr error, rows, scanned int, planDur time.Duration, plan func() string) {
	if db.querylog == nil {
		return
	}
	db.querylog.Record(QueryRecord{
		StartedAt:    start,
		QueryID:      id,
		User:         "embedded",
		Cypher:       cypher,
		Params:       paramsToAny(params),
		Kind:         kind,
		Status:       status,
		Err:          qerr,
		Duration:     time.Since(start),
		PlanMs:       float64(planDur) / float64(time.Millisecond),
		RowsReturned: rows,
		RowsScanned:  scanned,
		Plan:         plan,
	})
}

// paramsToAny converts a statement's internal parameter values to the plain Go values the
// query log's redaction policy inspects (doc 20 §10.3): RedactAll reads each value's type,
// RedactNone its content, and both want the Go shape, not the internal value wrapper. An
// empty map returns nil so the entry omits the params field.
func paramsToAny(params map[string]value.Value) map[string]any {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		out[k] = fromValue(v)
	}
	return out
}

// queryStatus maps an execution error to a query-log status (doc 20 §10.2): no error is
// "ok", a context deadline is "timeout" so the cancelled or slow tail is distinguishable,
// and any other failure is "error".
func queryStatus(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "error"
	}
}
