package httpd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// txServer builds a handler whose clock is controllable, so a test can drive transaction
// expiry without sleeping. It returns the handler and a pointer to the current time the
// test advances.
func txServer(t *testing.T, clock *time.Time) http.Handler {
	t.Helper()
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return Handler(db, Options{TxTimeout: time.Minute, now: func() time.Time { return *clock }})
}

// do sends a request to a handler and returns the recorder.
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestTxBeginRunCommit(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	h := txServer(t, &clock)

	rec := do(t, h, http.MethodPost, "/db/neo4j/tx",
		`{"statements":[{"statement":"CREATE (:Person {name:'Ada'})"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("begin status = %d, body = %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/db/neo4j/tx/") {
		t.Fatalf("Location = %q", loc)
	}
	out := decode(t, rec.Body.Bytes())
	txn := out["transaction"].(map[string]any)
	id := txn["id"].(string)
	if id == "" || txn["expires"] == "" {
		t.Fatalf("transaction = %v", txn)
	}

	// The created node is visible inside the open transaction but not yet committed.
	rec = do(t, h, http.MethodPost, "/db/neo4j/tx/"+id,
		`{"statements":[{"statement":"MATCH (p:Person) RETURN count(p) AS c"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("run status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out = decode(t, rec.Body.Bytes())
	results := out["results"].([]any)
	row := results[0].(map[string]any)["data"].(map[string]any)["values"].([]any)[0].([]any)
	if row[0] != float64(1) {
		t.Errorf("count inside tx = %v, want 1", row[0])
	}

	rec = do(t, h, http.MethodPost, "/db/neo4j/tx/"+id+"/commit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("commit status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// After commit the id is gone.
	rec = do(t, h, http.MethodPost, "/db/neo4j/tx/"+id, `{"statements":[]}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("run after commit = %d, want 404", rec.Code)
	}

	// The write is durable: a fresh auto-commit query sees it.
	rec = do(t, h, http.MethodPost, "/db/neo4j/query/v2",
		`{"statement":"MATCH (p:Person) RETURN count(p) AS c"}`)
	out = decode(t, rec.Body.Bytes())
	row = out["data"].(map[string]any)["values"].([]any)[0].([]any)
	if row[0] != float64(1) {
		t.Errorf("count after commit = %v, want 1", row[0])
	}
}

func TestTxRollback(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	h := txServer(t, &clock)

	rec := do(t, h, http.MethodPost, "/db/neo4j/tx",
		`{"statements":[{"statement":"CREATE (:Ghost)"}]}`)
	id := decode(t, rec.Body.Bytes())["transaction"].(map[string]any)["id"].(string)

	rec = do(t, h, http.MethodDelete, "/db/neo4j/tx/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// The rolled-back write left nothing behind.
	rec = do(t, h, http.MethodPost, "/db/neo4j/query/v2",
		`{"statement":"MATCH (g:Ghost) RETURN count(g) AS c"}`)
	row := decode(t, rec.Body.Bytes())["data"].(map[string]any)["values"].([]any)[0].([]any)
	if row[0] != float64(0) {
		t.Errorf("count after rollback = %v, want 0", row[0])
	}

	// The id is gone after rollback.
	rec = do(t, h, http.MethodDelete, "/db/neo4j/tx/"+id, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("second rollback = %d, want 404", rec.Code)
	}
}

func TestTxExpiry(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	h := txServer(t, &clock)

	rec := do(t, h, http.MethodPost, "/db/neo4j/tx", `{"statements":[]}`)
	id := decode(t, rec.Body.Bytes())["transaction"].(map[string]any)["id"].(string)

	// Advance past the timeout; the next request reaps the transaction.
	clock = clock.Add(2 * time.Minute)
	rec = do(t, h, http.MethodPost, "/db/neo4j/tx/"+id, `{"statements":[]}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("run after expiry = %d, want 404", rec.Code)
	}
}

func TestTxReadModeRejectsWrite(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	h := txServer(t, &clock)
	rec := do(t, h, http.MethodPost, "/db/neo4j/tx",
		`{"accessMode":"READ","statements":[{"statement":"CREATE (:X)"}]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("begin read+write = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
}

func TestTxStatementErrorRollsBack(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	h := txServer(t, &clock)

	rec := do(t, h, http.MethodPost, "/db/neo4j/tx",
		`{"statements":[{"statement":"CREATE (:Keep)"}]}`)
	id := decode(t, rec.Body.Bytes())["transaction"].(map[string]any)["id"].(string)

	// A syntax error in a follow-up statement aborts the whole transaction.
	rec = do(t, h, http.MethodPost, "/db/neo4j/tx/"+id,
		`{"statements":[{"statement":"RETURE bad"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad statement = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}

	// The transaction is gone and its earlier write was rolled back.
	rec = do(t, h, http.MethodPost, "/db/neo4j/tx/"+id, `{"statements":[]}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("run after aborted tx = %d, want 404", rec.Code)
	}
	rec = do(t, h, http.MethodPost, "/db/neo4j/query/v2",
		`{"statement":"MATCH (k:Keep) RETURN count(k) AS c"}`)
	row := decode(t, rec.Body.Bytes())["data"].(map[string]any)["values"].([]any)[0].([]any)
	if row[0] != float64(0) {
		t.Errorf("count after aborted tx = %v, want 0", row[0])
	}
}

func TestTxUnknownId(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	h := txServer(t, &clock)
	rec := do(t, h, http.MethodPost, "/db/neo4j/tx/deadbeef", `{"statements":[]}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", rec.Code)
	}
}

func TestTxBeginWrongMethod(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	h := txServer(t, &clock)
	rec := do(t, h, http.MethodGet, "/db/neo4j/tx", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /tx = %d, want 405", rec.Code)
	}
}

func TestTxSingleFlight(t *testing.T) {
	// A second request to an id whose entry is marked busy gets 409. The busy flag is
	// set directly here, since driving a real concurrent in-flight request is racy.
	clock := time.Unix(1_700_000_000, 0)
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := &server{
		db:        db,
		name:      "neo4j",
		txns:      newTxStore(),
		now:       func() time.Time { return clock },
		txTimeout: time.Minute,
		metrics:   newMetrics(),
	}
	tx, err := db.Begin(context.Background(), gr.Write)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	s.txns.put("abc", &txEntry{tx: tx, expires: clock.Add(time.Minute), busy: true})

	rec := do(t, http.HandlerFunc(s.route), http.MethodPost, "/db/neo4j/tx/abc", `{"statements":[]}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("busy id = %d, want 409", rec.Code)
	}
}
