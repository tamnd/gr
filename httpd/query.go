package httpd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tamnd/gr"
)

// queryRequest is the auto-commit query body (doc 18 §9.2). Only statement is
// required; the rest default. parameters arrive as decoded JSON, so an integer
// parameter arrives as a JSON number (float64) unless sent in a string form.
type queryRequest struct {
	Statement        string         `json:"statement"`
	Parameters       map[string]any `json:"parameters"`
	IncludeCounters  bool           `json:"includeCounters"`
	MaxExecutionTime int            `json:"maxExecutionTime"`
	AccessMode       string         `json:"accessMode"`
	ImpersonatedUser string         `json:"impersonatedUser"`
}

// handleQuery serves POST /db/{name}/query/v2 (doc 18 §9.2): it runs the statement as
// one auto-commit transaction and returns the whole result, or streams it as NDJSON
// when the client asks. The database name in the path is accepted and ignored beyond
// validation, since a gr server holds one database (doc 18 §7.5).
func (s *server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, apiError{
			Code:    "Neo.ClientError.Request.Invalid",
			Message: "query endpoint accepts POST",
		})
		return
	}
	var req queryRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, apiError{
			Code:    "Neo.ClientError.Request.InvalidFormat",
			Message: "invalid JSON request body: " + err.Error(),
		})
		return
	}
	if strings.TrimSpace(req.Statement) == "" {
		s.writeError(w, http.StatusBadRequest, apiError{
			Code:    "Neo.ClientError.Request.Invalid",
			Message: "statement is required",
		})
		return
	}

	r, ok := s.impersonate(w, r, req.ImpersonatedUser)
	if !ok {
		return
	}

	if !s.authorize(w, r, req.Statement) {
		return
	}
	if !s.rateLimit(w, r) {
		return
	}

	// Apply the effective deadline: the request's maxExecutionTime capped by the
	// server-wide query time limit (doc 18 §8.6). withTimeout takes the smaller of the
	// two, so a server cap bounds even a request that asks for longer or for none.
	ctx, cancel := s.withTimeout(r.Context(), req.MaxExecutionTime)
	defer cancel()

	started := s.now()
	res, release, err := s.run(ctx, req)
	if err != nil {
		status, ae := mapError(err)
		s.writeError(w, status, ae)
		s.recordQuery(r, req, started, queryStatus(err), err, 0, "", nil)
		return
	}
	defer release()

	intAsString := wantStringInts(r)
	var rows int
	var rerr error
	if wantStream(r) {
		rows, rerr = s.streamNDJSON(w, res, intAsString)
	} else {
		rows, rerr = s.bufferedResponse(w, res, req.IncludeCounters, intAsString)
	}
	// The result is fully drained but not yet released here, so its captured plan is still
	// readable: pass res.PlanText so a slow query carries its plan in the log (doc 20 §10.6).
	s.recordQuery(r, req, started, queryStatus(rerr), rerr, rows, "", res.PlanText)
}

// run executes the request's statement under the requested access mode (doc 18 §9.2).
// A READ request runs in a read transaction so a write statement is rejected with
// ErrReadOnly; the default and WRITE run as an auto-commit statement that picks the
// read or write path itself. The returned release closes the result and, for a read
// transaction, rolls it back.
func (s *server) run(ctx context.Context, req queryRequest) (*gr.Result, func(), error) {
	// Pass the in-flight gate before executing, so a query that finds it full sheds as
	// a retryable transient (doc 18 §8.8). The slot is held until the returned release
	// runs, which the handler defers until the result is fully sent, so the bound covers
	// the query while it streams, not only at submission. A nil gate admits at once.
	slot, err := s.admission.Acquire(ctx)
	if err != nil {
		// A shed query is an overload event an operator watches to decide whether to raise
		// the in-flight bound (doc 20 §11.3). The queue depth is not tracked, so the event
		// reports the in-flight count and the action; a nil event log makes this a no-op.
		if errors.Is(err, gr.ErrOverloaded) {
			s.elog.Overload(s.admission.InFlight(), 0, "shed")
		}
		return nil, nil, err
	}
	if strings.EqualFold(req.AccessMode, "READ") {
		tx, err := s.db.Begin(ctx, gr.Read)
		if err != nil {
			slot()
			return nil, nil, err
		}
		res, err := tx.Run(ctx, req.Statement, gr.Params(req.Parameters))
		if err != nil {
			_ = tx.Rollback()
			slot()
			return nil, nil, err
		}
		return res, func() { _ = res.Close(); _ = tx.Rollback(); slot() }, nil
	}
	res, err := s.db.Run(ctx, req.Statement, gr.Params(req.Parameters))
	if err != nil {
		slot()
		return nil, nil, err
	}
	return res, func() { _ = res.Close(); slot() }, nil
}

// bufferedResponse renders the whole result as one JSON document (doc 18 §9.3). It
// drains the result into a values matrix, then writes the response; a streaming error
// after the first byte cannot change the status code, so the buffered form is used
// unless the client explicitly asks to stream.
func (s *server) bufferedResponse(w http.ResponseWriter, res *gr.Result, includeCounters, intAsString bool) (int, error) {
	fields := res.Keys()
	values := [][]any{}
	for res.Next() {
		rec := res.Record().Values()
		row := make([]any, len(rec))
		for i, v := range rec {
			row[i] = toJSON(v, intAsString)
		}
		values = append(values, row)
	}
	if err := res.Err(); err != nil {
		status, ae := mapError(err)
		s.writeError(w, status, ae)
		return len(values), err
	}
	sum := res.Summary()
	out := map[string]any{
		"data":              map[string]any{"fields": fields, "values": values},
		"bookmarks":         []string{},
		"notifications":     []any{},
		"profiledQueryPlan": nil,
		"queryType":         queryType(sum, len(fields) > 0),
	}
	if includeCounters {
		out["counters"] = counters(sum)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
	return len(values), nil
}

// streamNDJSON streams the result as newline-delimited JSON (doc 18 §9.6): a header
// object, one row object per record, then a summary object, each flushed so a large
// result surfaces incrementally without buffering. Because the 200 status and the
// header line are already written, a mid-stream error is reported as a final error
// object rather than an HTTP status.
func (s *server) streamNDJSON(w http.ResponseWriter, res *gr.Result, intAsString bool) (int, error) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	fields := res.Keys()
	_ = enc.Encode(map[string]any{"header": map[string]any{"fields": fields}})
	rows := 0
	for res.Next() {
		rec := res.Record().Values()
		row := make([]any, len(rec))
		for i, v := range rec {
			row[i] = toJSON(v, intAsString)
		}
		_ = enc.Encode(map[string]any{"row": row})
		rows++
		if flusher != nil {
			flusher.Flush()
		}
	}
	if err := res.Err(); err != nil {
		_, ae := mapError(err)
		_ = enc.Encode(map[string]any{"error": ae})
		return rows, err
	}
	sum := res.Summary()
	_ = enc.Encode(map[string]any{"summary": map[string]any{
		"queryType": queryType(sum, len(fields) > 0),
		"bookmarks": []string{},
		"counters":  counters(sum),
	}})
	if flusher != nil {
		flusher.Flush()
	}
	return rows, nil
}

// wantStream reports whether the client asked for NDJSON streaming (doc 18 §9.6),
// through the Accept header or the ?stream=true query option.
func wantStream(r *http.Request) bool {
	if r.URL.Query().Get("stream") == "true" {
		return true
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/x-ndjson")
}

// wantStringInts reports whether integers should always be string-encoded (doc 18
// §9.4), through the ?integerEncoding=string query option.
func wantStringInts(r *http.Request) bool {
	return r.URL.Query().Get("integerEncoding") == "string"
}
