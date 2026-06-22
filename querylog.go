package gr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"
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

// QueryRecord is one query execution's facts, assembled by a server surface and handed to
// the query log (doc 20 §10.2). A surface fills what it knows; the engine-internal fields
// the spec also lists (peak memory, plan time, rows scanned, spill) wait on the executor
// instrumentation that produces them and are omitted until then.
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
	RowsReturned int            // result row count
	TxID         string         // the transaction the query ran in
}

// QueryLog writes structured query-log entries through an slog.Logger (doc 20 §10, §11.2),
// so the log lands wherever the embedder or the server points slog: a JSON aggregator, a
// console handler, a file. It applies the level filter (doc 20 §10.4), the always-on
// slow-query rule (doc 20 §10.6), and parameter redaction (doc 20 §10.3).
//
// A nil *QueryLog is disabled and records nothing, the embedded-friendly default when no
// logger is configured.
type QueryLog struct {
	logger *slog.Logger
	level  QueryLogLevel
	redact RedactPolicy
	slow   time.Duration
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
	return &QueryLog{logger: logger, level: level, redact: redact, slow: slow}
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
		slog.Int("rows_returned", r.RowsReturned),
	}
	if r.TxID != "" {
		attrs = append(attrs, slog.String("tx_id", r.TxID))
	}
	if slow {
		attrs = append(attrs, slog.Bool("slow", true), slog.Float64("threshold_ms", float64(l.slow)/float64(time.Millisecond)))
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
