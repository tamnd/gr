package httpd

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHealthDetailReports confirms GET /healthz/detail serves the structured health report
// as JSON with a 200 for a healthy engine, carrying the open state and the ready flag (doc 20
// §13.3).
func TestHealthDetailReports(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	req := httptest.NewRequest(http.MethodGet, "/healthz/detail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	doc := decode(t, rec.Body.Bytes())
	if doc["state"] != "open" {
		t.Errorf("state = %v, want open", doc["state"])
	}
	if doc["ready"] != true {
		t.Errorf("ready = %v, want true", doc["ready"])
	}
	if _, ok := doc["warnings"]; !ok {
		t.Errorf("report has no warnings field: %v", doc)
	}
}

// TestHealthDetailUnreadyAfterClose confirms /healthz/detail returns 503 with a stopped state
// once the engine is closed, so a probe watching this endpoint still gets a fail (doc 20
// §13.3, §13.5).
func TestHealthDetailUnreadyAfterClose(t *testing.T) {
	db := newTestDB(t)
	h := Handler(db, Options{})
	_ = db.Close()

	req := httptest.NewRequest(http.MethodGet, "/healthz/detail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	doc := decode(t, rec.Body.Bytes())
	if doc["state"] != "stopped" {
		t.Errorf("state = %v, want stopped", doc["state"])
	}
	if doc["ready"] != false {
		t.Errorf("ready = %v, want false", doc["ready"])
	}
}

// TestHealthDetailUnauthenticated confirms the detail endpoint serves without a credential,
// like the other health probes, so an operator reaches it on a server with auth on.
func TestHealthDetailUnauthenticated(t *testing.T) {
	h := authHandler(t, map[string]string{"alice": "pw"})
	req := httptest.NewRequest(http.MethodGet, "/healthz/detail", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 without a credential", rec.Code)
	}
}
