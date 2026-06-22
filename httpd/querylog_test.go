package httpd

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/gr"
)

// captureQueryLog builds a query log that records every query into a buffer, plus a reader
// that parses the buffer into one map per entry, so an HTTP test can assert what the server
// logged.
func captureQueryLog() (*gr.QueryLog, func() []map[string]any) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ql := gr.NewQueryLog(logger, gr.QueryLogAll, gr.RedactAll, 0)
	read := func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				panic(err)
			}
			out = append(out, m)
		}
		return out
	}
	return ql, read
}

// TestQueryLogRecordsQuery confirms a successful auto-commit query is logged with its kind,
// status, row count, and the cypher, and that parameter values are redacted to type shapes.
func TestQueryLogRecordsQuery(t *testing.T) {
	ql, read := captureQueryLog()
	h := Handler(newTestDB(t), Options{QueryLog: ql})

	if rec := post(t, h, `{"statement":"UNWIND [1,2,3] AS n RETURN n","parameters":{"x":"secret"}}`); rec.Code != http.StatusOK {
		t.Fatalf("query status = %d, body = %s", rec.Code, rec.Body.String())
	}

	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e["status"] != "ok" {
		t.Errorf("status = %v, want ok", e["status"])
	}
	if e["kind"] != "read" {
		t.Errorf("kind = %v, want read", e["kind"])
	}
	if e["cypher"] != "UNWIND [1,2,3] AS n RETURN n" {
		t.Errorf("cypher = %v", e["cypher"])
	}
	if e["rows_returned"].(float64) != 3 {
		t.Errorf("rows_returned = %v, want 3", e["rows_returned"])
	}
	if e["user"] != "anonymous" {
		t.Errorf("user = %v, want anonymous", e["user"])
	}
	params := e["params"].(map[string]any)
	if params["x"] != "<string>" {
		t.Errorf("param x = %v, want <string>", params["x"])
	}
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), "secret") {
		t.Errorf("query log leaked a parameter value: %s", raw)
	}
}

// TestQueryLogRecordsFailure confirms a query that fails to parse is logged with an error
// status and the error string, at the errors level (an ok query at this level is dropped).
func TestQueryLogRecordsFailure(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ql := gr.NewQueryLog(logger, gr.QueryLogErrors, gr.RedactAll, 0)
	h := Handler(newTestDB(t), Options{QueryLog: ql})

	// A valid ok query is dropped at the errors level.
	post(t, h, `{"statement":"RETURN 1 AS n"}`)
	// A statement that does not parse is logged as a failure.
	post(t, h, `{"statement":"RETURN ("}`)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("want exactly one logged entry (the failure), got: %q", buf.String())
	}
	var e map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e["status"] != "error" {
		t.Errorf("status = %v, want error", e["status"])
	}
	if e["error"] == nil || e["error"] == "" {
		t.Errorf("error field missing on a failed query")
	}
}

// TestQueryLogDisabled confirms an unconfigured query log records nothing and does not
// disturb the response.
func TestQueryLogDisabled(t *testing.T) {
	h := Handler(newTestDB(t), Options{})
	if rec := post(t, h, `{"statement":"RETURN 1 AS n"}`); rec.Code != http.StatusOK {
		t.Fatalf("query status = %d", rec.Code)
	}
}

// TestQueryLogRecordsUser confirms the authenticated principal names the user field.
func TestQueryLogRecordsUser(t *testing.T) {
	ql, read := captureQueryLog()
	p := NewStaticProvider()
	if err := p.AddUser("alice", "secret", "admin"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	h := Handler(newTestDB(t), Options{Auth: p, QueryLog: ql})

	req := httptest.NewRequest(http.MethodPost, "/db/neo4j/query/v2", strings.NewReader(`{"statement":"RETURN 1 AS n"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", basic("alice", "secret"))
	rc := httptest.NewRecorder()
	h.ServeHTTP(rc, req)
	if rc.Code != http.StatusOK {
		t.Fatalf("query status = %d, body = %s", rc.Code, rc.Body.String())
	}

	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0]["user"] != "alice" {
		t.Errorf("user = %v, want alice", entries[0]["user"])
	}
}
