package httpd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tamnd/gr"
)

// defaultTxTimeout bounds how long a server-side HTTP transaction may sit between
// requests before the store reaps it (doc 18 §9.5, §8.3). A held-open transaction
// pins the writer, so a dead HTTP client must not keep one alive forever; the timeout
// is refreshed on every request to the transaction.
const defaultTxTimeout = 60 * time.Second

// txEntry is one held-open transaction in the store (doc 18 §9.9). The principal that
// opened it scopes who may touch it; busy enforces single-flight, since the held *Tx
// is one engine transaction and is not safe for concurrent statements; expires drives
// reaping.
type txEntry struct {
	tx        *gr.Tx
	principal string
	expires   time.Time
	busy      bool
}

// acquire result codes.
const (
	acqOK = iota
	acqNotFound
	acqBusy
	acqForbidden
)

// txStore holds the server-side transactions keyed by their minted ids (doc 18 §9.9).
// A single mutex guards the map and the per-entry busy flag; the actual statement
// execution happens outside the lock, with the busy flag standing in for the entry so
// a concurrent request to the same id is rejected rather than racing the engine
// transaction.
type txStore struct {
	mu sync.Mutex
	m  map[string]*txEntry
}

func newTxStore() *txStore { return &txStore{m: make(map[string]*txEntry)} }

// mintID returns an unguessable transaction id (doc 18 §9.9): a random 128-bit token
// hex-encoded, not a sequential counter, so a client cannot enumerate or hijack
// another client's transaction.
func mintID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// put stores a freshly begun transaction under its id.
func (st *txStore) put(id string, e *txEntry) {
	st.mu.Lock()
	st.m[id] = e
	st.mu.Unlock()
}

// acquire looks up a transaction, verifies ownership, reaps it if expired, and claims
// it for single-flight use (doc 18 §9.9). A claimed entry must be released or removed.
// An expired entry is rolled back and removed under the lock, so a later request to it
// reports not-found.
func (st *txStore) acquire(id, principal string, now time.Time) (*txEntry, int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	e, ok := st.m[id]
	if !ok {
		return nil, acqNotFound
	}
	if now.After(e.expires) {
		delete(st.m, id)
		_ = e.tx.Rollback()
		return nil, acqNotFound
	}
	if principal != e.principal {
		return nil, acqForbidden
	}
	if e.busy {
		return nil, acqBusy
	}
	e.busy = true
	return e, acqOK
}

// release clears the single-flight claim and extends the expiry (doc 18 §9.9). It is
// called after a run that leaves the transaction open.
func (st *txStore) release(id string, now time.Time, timeout time.Duration) time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	e, ok := st.m[id]
	if !ok {
		return now
	}
	e.busy = false
	e.expires = now.Add(timeout)
	return e.expires
}

// remove deletes a transaction from the store, used after commit or rollback.
func (st *txStore) remove(id string) {
	st.mu.Lock()
	delete(st.m, id)
	st.mu.Unlock()
}

// closeAll rolls back every held transaction, used when the server shuts down so no
// open transaction leaks a snapshot or a write intent.
func (st *txStore) closeAll() {
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, e := range st.m {
		_ = e.tx.Rollback()
		delete(st.m, id)
	}
}

// statement is one Cypher statement in a multi-statement transactional request (doc 18
// §9.5). Parameters arrive as decoded JSON, the same lossy-integer edge query/v2 has.
type statement struct {
	Statement  string         `json:"statement"`
	Parameters map[string]any `json:"parameters"`
}

// txBeginRequest is the body of a begin or run request (doc 18 §9.5): a batch of
// statements plus, on begin, the transaction parameters.
type txBeginRequest struct {
	Statements       []statement `json:"statements"`
	MaxExecutionTime int         `json:"maxExecutionTime"`
	AccessMode       string      `json:"accessMode"`
}

// runStatements runs each statement on the held transaction and buffers each result in
// the response shape (doc 18 §9.5). It stops at the first error and returns it, so the
// caller can roll the whole transaction back, the Neo4j behavior where a statement
// error aborts the transaction.
func runStatements(ctx context.Context, tx *gr.Tx, stmts []statement, intAsString bool) ([]map[string]any, error) {
	results := make([]map[string]any, 0, len(stmts))
	for _, st := range stmts {
		res, err := tx.Run(ctx, st.Statement, gr.Params(st.Parameters))
		if err != nil {
			return nil, err
		}
		one, err := drainResult(res, intAsString)
		_ = res.Close()
		if err != nil {
			return nil, err
		}
		results = append(results, one)
	}
	return results, nil
}

// drainResult buffers one result into the per-statement response object (doc 18 §9.3).
func drainResult(res *gr.Result, intAsString bool) (map[string]any, error) {
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
		return nil, err
	}
	sum := res.Summary()
	return map[string]any{
		"data":      map[string]any{"fields": fields, "values": values},
		"queryType": queryType(sum, len(fields) > 0),
		"counters":  counters(sum),
	}, nil
}

// handleTxBegin serves POST /db/{name}/tx (doc 18 §9.5): it begins a transaction in the
// requested access mode, runs any initial statements, stores the transaction under a
// freshly minted id, and replies 201 with the id in the body and the Location header.
// An error in an initial statement rolls the new transaction back and is reported
// without ever storing it.
func (s *server) handleTxBegin(w http.ResponseWriter, r *http.Request, name string) {
	var req txBeginRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, apiError{
			Code:    "Neo.ClientError.Request.InvalidFormat",
			Message: "invalid JSON request body: " + err.Error(),
		})
		return
	}
	if !s.authorizeStatements(w, r, req.Statements) {
		return
	}
	ctx, cancel := s.withTimeout(r.Context(), req.MaxExecutionTime)
	defer cancel()

	mode := gr.Write
	if strings.EqualFold(req.AccessMode, "READ") {
		mode = gr.Read
	}
	tx, err := s.db.Begin(ctx, mode)
	if err != nil {
		status, ae := mapError(err)
		s.writeError(w, status, ae)
		return
	}
	results, err := runStatements(ctx, tx, req.Statements, wantStringInts(r))
	if err != nil {
		_ = tx.Rollback()
		status, ae := mapError(err)
		s.writeError(w, status, ae)
		return
	}
	id, err := mintID()
	if err != nil {
		_ = tx.Rollback()
		s.writeError(w, http.StatusInternalServerError, apiError{
			Code:    "Neo.DatabaseError.General.UnknownError",
			Message: "could not mint a transaction id",
		})
		return
	}
	expires := s.now().Add(s.txTimeout)
	s.txns.put(id, &txEntry{tx: tx, principal: principal(r), expires: expires})

	w.Header().Set("Location", "/db/"+name+"/tx/"+id)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"transaction": map[string]any{"id": id, "expires": formatExpiry(expires)},
		"results":     results,
		"errors":      []apiError{},
	})
}

// handleTxRun serves POST /db/{name}/tx/{id} (doc 18 §9.5): it runs more statements in
// the open transaction, which stays open, and returns their results with a refreshed
// expiry. A statement error rolls the transaction back and removes it.
func (s *server) handleTxRun(w http.ResponseWriter, r *http.Request, id string) {
	var req txBeginRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, apiError{
			Code:    "Neo.ClientError.Request.InvalidFormat",
			Message: "invalid JSON request body: " + err.Error(),
		})
		return
	}
	if !s.authorizeStatements(w, r, req.Statements) {
		return
	}
	e, code := s.txns.acquire(id, principal(r), s.now())
	if !s.checkAcquire(w, code) {
		return
	}
	ctx, cancel := s.withTimeout(r.Context(), req.MaxExecutionTime)
	defer cancel()

	results, err := runStatements(ctx, e.tx, req.Statements, wantStringInts(r))
	if err != nil {
		s.txns.remove(id)
		_ = e.tx.Rollback()
		status, ae := mapError(err)
		s.writeError(w, status, ae)
		return
	}
	expires := s.txns.release(id, s.now(), s.txTimeout)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"transaction": map[string]any{"expires": formatExpiry(expires)},
		"results":     results,
		"errors":      []apiError{},
	})
}

// handleTxCommit serves POST /db/{name}/tx/{id}/commit (doc 18 §9.5): it runs an
// optional final batch of statements, commits, and removes the transaction. A
// statement error or a commit failure rolls back and removes it.
func (s *server) handleTxCommit(w http.ResponseWriter, r *http.Request, id string) {
	var req txBeginRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, apiError{
			Code:    "Neo.ClientError.Request.InvalidFormat",
			Message: "invalid JSON request body: " + err.Error(),
		})
		return
	}
	if !s.authorizeStatements(w, r, req.Statements) {
		return
	}
	e, code := s.txns.acquire(id, principal(r), s.now())
	if !s.checkAcquire(w, code) {
		return
	}
	ctx, cancel := s.withTimeout(r.Context(), req.MaxExecutionTime)
	defer cancel()

	results, err := runStatements(ctx, e.tx, req.Statements, wantStringInts(r))
	if err != nil {
		s.txns.remove(id)
		_ = e.tx.Rollback()
		status, ae := mapError(err)
		s.writeError(w, status, ae)
		return
	}
	if err := e.tx.Commit(); err != nil {
		s.txns.remove(id)
		status, ae := mapError(err)
		s.writeError(w, status, ae)
		return
	}
	s.txns.remove(id)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"results":   results,
		"bookmarks": []string{},
		"errors":    []apiError{},
	})
}

// handleTxRollback serves DELETE /db/{name}/tx/{id} (doc 18 §9.5): it aborts the open
// transaction and removes it.
func (s *server) handleTxRollback(w http.ResponseWriter, r *http.Request, id string) {
	e, code := s.txns.acquire(id, principal(r), s.now())
	if !s.checkAcquire(w, code) {
		return
	}
	s.txns.remove(id)
	_ = e.tx.Rollback()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []apiError{}})
}

// checkAcquire writes the right error for a failed acquire and reports whether the
// caller may proceed (doc 18 §9.9). A not-found or wrong-principal id is 404 (an
// owner-scoped id leak still cannot touch another's transaction); a busy id is 409,
// the single-flight rejection.
func (s *server) checkAcquire(w http.ResponseWriter, code int) bool {
	switch code {
	case acqOK:
		return true
	case acqBusy:
		s.writeError(w, http.StatusConflict, apiError{
			Code:    "Neo.ClientError.Request.Invalid",
			Message: "a request is already in flight on this transaction",
		})
	default:
		s.writeError(w, http.StatusNotFound, apiError{
			Code:    "Neo.ClientError.Transaction.TransactionNotFound",
			Message: "no such transaction",
		})
	}
	return false
}

// withTimeout wraps a context in the request's maxExecutionTime when one is set.
func (s *server) withTimeout(parent context.Context, ms int) (context.Context, context.CancelFunc) {
	if ms > 0 {
		return context.WithTimeout(parent, time.Duration(ms)*time.Millisecond)
	}
	return context.WithCancel(parent)
}

// decodeBody decodes a request body into v, treating an empty body as an empty value
// so a run or commit with no statements is valid.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

// principal returns the authenticated principal's name for a request (doc 18 §9.9),
// which scopes transaction ownership. With authentication off every request is the
// anonymous principal (the empty name), so the ownership check is a no-op; with auth on
// it is the authenticated user, so one user cannot touch another's transaction.
func principal(r *http.Request) string { return principalFrom(r.Context()).Name }

// formatExpiry formats a transaction expiry for the response (doc 18 §9.5).
func formatExpiry(t time.Time) string { return t.UTC().Format(time.RFC3339) }
