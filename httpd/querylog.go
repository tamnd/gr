package httpd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/tamnd/gr"
)

// recordQuery writes one query-log entry for an executed query (doc 20 §10). It assembles
// the facts the HTTP surface knows (the principal, the source address, the statement with
// its placeholders, the timing, the outcome) and hands them to the shared query log, which
// decides whether to emit by its level and slow threshold and redacts the parameters. A
// nil log makes this a no-op, so the call site never guards it.
func (s *server) recordQuery(r *http.Request, req queryRequest, started time.Time, status string, qerr error, rows int, txID string, plan func() string) {
	if s.qlog == nil {
		return
	}
	// The kind is best-effort: a statement that does not parse has no kind, and the
	// failure itself is already carried in the status and error.
	kind := ""
	if k, err := s.db.StatementKind(req.Statement); err == nil {
		kind = k.String()
	}
	id, _ := mintID()
	s.qlog.Record(gr.QueryRecord{
		StartedAt:    started,
		QueryID:      id,
		SessionID:    txID,
		User:         queryUser(r),
		Client:       clientAddr(r),
		Cypher:       req.Statement,
		Params:       req.Parameters,
		Kind:         kind,
		Status:       status,
		Err:          qerr,
		Duration:     s.now().Sub(started),
		RowsReturned: rows,
		TxID:         txID,
		// The plan thunk renders the EXPLAIN-grade plan for the slow-query log's captured plan
		// (doc 20 §10.6); the query log runs it only if it decides the query was slow, so a fast
		// query pays nothing. A query that failed before it ran has no plan, so the caller passes
		// nil on the error paths.
		Plan: plan,
	})
}

// queryStatus maps an execution error to a query-log status (doc 20 §10.2): no error is
// "ok", a deadline is "timeout" so the slow/cancelled tail is distinguishable, and any
// other failure is "error".
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

// queryUser names the user for a query-log entry: the authenticated principal, or
// "anonymous" when auth is off and the request carries no principal.
func queryUser(r *http.Request) string {
	if p := principalFrom(r.Context()); p != nil && p.Name != "" {
		return p.Name
	}
	return "anonymous"
}

// clientAddr is the source host for a query-log entry, the request's remote address with
// the ephemeral port stripped so entries group by client rather than by connection.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
