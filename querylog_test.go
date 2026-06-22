package gr

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// captureLog builds a query log whose entries land in a buffer as JSON, so a test can read
// back what was emitted. The returned function parses the buffer into one map per entry.
func captureLog(level QueryLogLevel, redact RedactPolicy, slow time.Duration) (*QueryLog, func() []map[string]any) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ql := NewQueryLog(logger, level, redact, slow)
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

// TestQueryLogNilDisabled confirms a nil logger yields a nil log and that Record on a nil
// log is a safe no-op, so an embedder with no logging configured pays nothing.
func TestQueryLogNilDisabled(t *testing.T) {
	if ql := NewQueryLog(nil, QueryLogAll, RedactAll, 0); ql != nil {
		t.Fatalf("nil logger should make a nil log, got %v", ql)
	}
	var ql *QueryLog
	ql.Record(QueryRecord{Cypher: "RETURN 1", Status: "ok"}) // must not panic
}

// TestQueryLogAllRecordsEveryQuery confirms QueryLogAll emits an ok query with the core
// fields populated.
func TestQueryLogAllRecordsEveryQuery(t *testing.T) {
	ql, read := captureLog(QueryLogAll, RedactAll, time.Second)
	ql.Record(QueryRecord{
		StartedAt:    time.Unix(1_700_000_000, 0),
		QueryID:      "q1",
		SessionID:    "s1",
		User:         "alice",
		Client:       "10.0.0.1",
		Cypher:       "MATCH (n) RETURN n",
		Kind:         "read",
		Status:       "ok",
		Duration:     5 * time.Millisecond,
		RowsReturned: 3,
		TxID:         "tx7",
	})
	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	for k, want := range map[string]any{
		"event":         "query",
		"query_id":      "q1",
		"session_id":    "s1",
		"user":          "alice",
		"client":        "10.0.0.1",
		"cypher":        "MATCH (n) RETURN n",
		"kind":          "read",
		"status":        "ok",
		"rows_returned": float64(3),
		"tx_id":         "tx7",
	} {
		if e[k] != want {
			t.Errorf("entry[%q] = %v, want %v", k, e[k], want)
		}
	}
	if e["duration_ms"].(float64) != 5 {
		t.Errorf("duration_ms = %v, want 5", e["duration_ms"])
	}
}

// TestQueryLogOffStillLogsSlow confirms the slow-query rule fires even at QueryLogOff: an
// ordinary query is dropped, a slow one is kept and marked slow at warn level.
func TestQueryLogOffStillLogsSlow(t *testing.T) {
	ql, read := captureLog(QueryLogOff, RedactAll, 10*time.Millisecond)
	ql.Record(QueryRecord{Cypher: "RETURN 1", Status: "ok", Duration: time.Millisecond})
	ql.Record(QueryRecord{Cypher: "MATCH (n) RETURN n", Status: "ok", Duration: 50 * time.Millisecond})
	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (only the slow query)", len(entries))
	}
	e := entries[0]
	if e["cypher"] != "MATCH (n) RETURN n" {
		t.Errorf("logged the wrong query: %v", e["cypher"])
	}
	if e["slow"] != true {
		t.Errorf("slow flag = %v, want true", e["slow"])
	}
	if e["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", e["level"])
	}
}

// TestQueryLogErrorsKeepsFailures confirms QueryLogErrors drops an ok query but keeps a
// failed one, logged at error level with the error string.
func TestQueryLogErrorsKeepsFailures(t *testing.T) {
	ql, read := captureLog(QueryLogErrors, RedactAll, time.Second)
	ql.Record(QueryRecord{Cypher: "RETURN 1", Status: "ok", Duration: time.Millisecond})
	ql.Record(QueryRecord{Cypher: "RETURN 1/0", Status: "error", Err: errors.New("divide by zero"), Duration: time.Millisecond})
	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (only the failure)", len(entries))
	}
	e := entries[0]
	if e["status"] != "error" {
		t.Errorf("status = %v, want error", e["status"])
	}
	if e["error"] != "divide by zero" {
		t.Errorf("error = %v, want 'divide by zero'", e["error"])
	}
	if e["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", e["level"])
	}
}

// TestQueryLogSlowLevelKeepsFailuresAndSlow confirms the production default keeps failures
// and the slow tail but drops ordinary fast queries.
func TestQueryLogSlowLevelKeepsFailuresAndSlow(t *testing.T) {
	ql, read := captureLog(QueryLogSlow, RedactAll, 10*time.Millisecond)
	ql.Record(QueryRecord{Cypher: "fast ok", Status: "ok", Duration: time.Millisecond})
	ql.Record(QueryRecord{Cypher: "slow ok", Status: "ok", Duration: 50 * time.Millisecond})
	ql.Record(QueryRecord{Cypher: "fast fail", Status: "error", Err: errors.New("boom"), Duration: time.Millisecond})
	entries := read()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (slow + failure, not the fast ok)", len(entries))
	}
}

// TestQueryLogRedactAll confirms the default policy logs parameter keys and types but never
// their values.
func TestQueryLogRedactAll(t *testing.T) {
	ql, read := captureLog(QueryLogAll, RedactAll, time.Second)
	ql.Record(QueryRecord{
		Cypher: "MATCH (n {name:$name}) RETURN n",
		Status: "ok",
		Params: map[string]any{
			"name":  "Alice Secret",
			"age":   int64(42),
			"tags":  []any{"a", "b"},
			"score": 1.5,
		},
	})
	params := read()[0]["params"].(map[string]any)
	if params["name"] != "<string>" {
		t.Errorf("name shape = %v, want <string>", params["name"])
	}
	if params["age"] != "<int>" {
		t.Errorf("age shape = %v, want <int>", params["age"])
	}
	if params["tags"] != "<list[string] len=2>" {
		t.Errorf("tags shape = %v, want <list[string] len=2>", params["tags"])
	}
	if params["score"] != "<float>" {
		t.Errorf("score shape = %v, want <float>", params["score"])
	}
	// The secret value must not appear anywhere in the entry.
	for _, line := range read() {
		raw, _ := json.Marshal(line)
		if strings.Contains(string(raw), "Alice Secret") {
			t.Errorf("redacted log leaked a value: %s", raw)
		}
	}
}

// TestQueryLogRedactHashed confirms the hashed policy replaces values with stable tokens:
// the same value hashes the same, a different value hashes differently, and the raw value
// never appears.
func TestQueryLogRedactHashed(t *testing.T) {
	ql, read := captureLog(QueryLogAll, RedactHashed, time.Second)
	ql.Record(QueryRecord{Cypher: "q", Status: "ok", Params: map[string]any{"a": "secret", "b": "secret", "c": "other"}})
	params := read()[0]["params"].(map[string]any)
	if params["a"] != params["b"] {
		t.Errorf("same value hashed differently: %v vs %v", params["a"], params["b"])
	}
	if params["a"] == params["c"] {
		t.Errorf("different values hashed the same: %v", params["a"])
	}
	if !strings.HasPrefix(params["a"].(string), "sha256:") {
		t.Errorf("hash missing prefix: %v", params["a"])
	}
	raw, _ := json.Marshal(read()[0])
	if strings.Contains(string(raw), "secret") {
		t.Errorf("hashed log leaked a value: %s", raw)
	}
}

// TestQueryLogRedactNone confirms the none policy logs values verbatim, for a debugging
// session that opts in.
func TestQueryLogRedactNone(t *testing.T) {
	ql, read := captureLog(QueryLogAll, RedactNone, time.Second)
	ql.Record(QueryRecord{Cypher: "q", Status: "ok", Params: map[string]any{"name": "Alice"}})
	params := read()[0]["params"].(map[string]any)
	if params["name"] != "Alice" {
		t.Errorf("none policy = %v, want verbatim Alice", params["name"])
	}
}

// TestQueryLogDefaultSlowThreshold confirms a zero slow uses the one-second default.
func TestQueryLogDefaultSlowThreshold(t *testing.T) {
	ql, read := captureLog(QueryLogOff, RedactAll, 0)
	ql.Record(QueryRecord{Cypher: "under a second", Status: "ok", Duration: 500 * time.Millisecond})
	ql.Record(QueryRecord{Cypher: "over a second", Status: "ok", Duration: 1500 * time.Millisecond})
	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (only the over-a-second query)", len(entries))
	}
	if entries[0]["cypher"] != "over a second" {
		t.Errorf("logged %v, want the over-a-second query", entries[0]["cypher"])
	}
}
