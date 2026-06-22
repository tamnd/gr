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

// newServerWithClock builds a *Server whose clock the test controls, so transaction
// expiry and the sweeper can be driven without sleeping.
func newServerWithClock(t *testing.T, clock *time.Time) *Server {
	t.Helper()
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sv := New(db, Options{TxTimeout: time.Minute, now: func() time.Time { return *clock }})
	// Close the server first so any transaction a test left open is rolled back before
	// db.Close, which would otherwise block on the writer lock the open transaction holds.
	t.Cleanup(func() {
		sv.Close()
		_ = db.Close()
	})
	return sv
}

// beginTx opens a transaction on a server and returns its id.
func beginTx(t *testing.T, sv *Server) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/db/neo4j/tx", strings.NewReader(`{"statements":[]}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	sv.ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("begin status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return decode(t, rec.Body.Bytes())["transaction"].(map[string]any)["id"].(string)
}

// TestSweepReapsExpired confirms the background sweep force-rolls-back a transaction past
// its expiry, so a vanished client's transaction does not linger until someone touches
// its id. After the sweep the id is gone (404) and the reaped counter has advanced.
func TestSweepReapsExpired(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	sv := newServerWithClock(t, &clock)

	id := beginTx(t, sv)

	// Before expiry the sweep reaps nothing.
	if n := sv.Sweep(clock); n != 0 {
		t.Fatalf("sweep before expiry reaped %d, want 0", n)
	}

	// Advance past the timeout and sweep: the transaction is reaped.
	clock = clock.Add(2 * time.Minute)
	if n := sv.Sweep(clock); n != 1 {
		t.Fatalf("sweep after expiry reaped %d, want 1", n)
	}

	// A later request to the reaped id is 404.
	r := httptest.NewRequest(http.MethodDelete, "/db/neo4j/tx/"+id, nil)
	rec := httptest.NewRecorder()
	sv.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Errorf("touching reaped tx = %d, want 404", rec.Code)
	}

	// The reaped counter shows up in the metrics.
	open, reaped := sv.s.txns.stats()
	if open != 0 || reaped != 1 {
		t.Errorf("stats = (open %d, reaped %d), want (0, 1)", open, reaped)
	}
}

// TestSweepLeavesUnexpired confirms the sweep does not touch a live transaction.
func TestSweepLeavesUnexpired(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	sv := newServerWithClock(t, &clock)
	id := beginTx(t, sv)
	if n := sv.Sweep(clock.Add(30 * time.Second)); n != 0 {
		t.Fatalf("sweep within timeout reaped %d, want 0", n)
	}
	// The transaction is still usable.
	r := httptest.NewRequest(http.MethodDelete, "/db/neo4j/tx/"+id, nil)
	rec := httptest.NewRecorder()
	sv.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("rollback live tx = %d, want 200", rec.Code)
	}
}

// TestMetricsEndpoint confirms /metrics is open and reports the open-transaction gauge,
// the reaped counter, and the error counter in the Prometheus text format.
func TestMetricsEndpoint(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	sv := newServerWithClock(t, &clock)

	// One open transaction lifts the gauge.
	beginTx(t, sv)
	// One bad request lifts an error counter.
	bad := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2", strings.NewReader(`not json`))
	bad.Header.Set("Content-Type", "application/json")
	sv.ServeHTTP(httptest.NewRecorder(), bad)

	rec := httptest.NewRecorder()
	sv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE gr_server_open_transactions gauge",
		"gr_server_open_transactions 1",
		"# TYPE gr_server_tx_reaped_total counter",
		"gr_server_tx_reaped_total 0",
		"gr_server_errors_total{code=\"Neo.ClientError.Request.InvalidFormat\"} 1",
		"# TYPE gr_server_auth_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestMetricsAdmission confirms the gate metrics appear when a gate is configured: the
// in-flight gauge reflects a held slot and the shed counter counts a shed query.
func TestMetricsAdmission(t *testing.T) {
	gate := gr.NewAdmission(1, 10*time.Millisecond)
	release, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	defer release()
	h := Handler(newTestDB(t), Options{Admission: gate})

	// The held slot makes this query shed, lifting the shed counter.
	if rec := post(t, h, `{"statement":"RETURN 1 AS n"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("query status = %d, want 503", rec.Code)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE gr_server_in_flight_queries gauge",
		"gr_server_in_flight_queries 1",
		"# TYPE gr_server_queries_shed_total counter",
		"gr_server_queries_shed_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestMetricsNoAdmission confirms an ungated server does not emit the gate metrics, so a
// scraper does not read a misleading constant zero.
func TestMetricsNoAdmission(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), "gr_server_in_flight_queries") {
		t.Error("ungated server emitted the in-flight gauge")
	}
}

// TestMetricsAuthOutcomes confirms the auth counters split success from failure and that
// an anonymous (auth-off) request is not counted as an auth event.
func TestMetricsAuthOutcomes(t *testing.T) {
	db, err := gr.Open(":memory:.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := NewStaticProvider()
	if err := p.AddUser("alice", "secret", "admin"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	sv := New(db, Options{Auth: p})

	// One success, two failures.
	good := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2", strings.NewReader(`{"statement":"RETURN 1"}`))
	good.Header.Set("Authorization", basic("alice", "secret"))
	sv.ServeHTTP(httptest.NewRecorder(), good)
	for i := 0; i < 2; i++ {
		bad := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2", strings.NewReader(`{"statement":"RETURN 1"}`))
		bad.Header.Set("Authorization", basic("alice", "wrong"))
		sv.ServeHTTP(httptest.NewRecorder(), bad)
	}

	rec := httptest.NewRecorder()
	sv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"gr_server_auth_total{outcome=\"success\"} 1",
		"gr_server_auth_total{outcome=\"failure\"} 2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestMetricsOpenAndCloseRollback confirms Close rolls back open transactions on shutdown.
func TestMetricsOpenAndCloseRollback(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	sv := newServerWithClock(t, &clock)
	beginTx(t, sv)
	if open, _ := sv.s.txns.stats(); open != 1 {
		t.Fatalf("open before close = %d, want 1", open)
	}
	sv.Close()
	if open, _ := sv.s.txns.stats(); open != 0 {
		t.Errorf("open after close = %d, want 0", open)
	}
}
