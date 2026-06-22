package gr

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/gr/bolt"
	"github.com/tamnd/gr/value"
)

// captureBoltLog builds a query log that records everything into a buffer plus a reader that
// parses the buffer into one map per entry, so a Bolt adapter test can assert what was
// logged.
func captureBoltLog(level QueryLogLevel) (*QueryLog, func() []map[string]any) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ql := NewQueryLog(logger, level, RedactAll, 0)
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

// TestBoltQueryLogRecordsQuery confirms a successful Bolt query is recorded on cursor close
// with its kind, status, row count, and the cypher, with the parameter value redacted.
func TestBoltQueryLogRecordsQuery(t *testing.T) {
	db := boltDB(t)
	ql, read := captureBoltLog(QueryLogAll)
	h := db.BoltHandler(WithBoltQueryLog(ql))

	params := map[string]value.Value{"name": value.String("secret")}
	runBolt(t, h, "UNWIND [1,2,3] AS n RETURN n", params)

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
	if p := e["params"].(map[string]any); p["name"] != "<string>" {
		t.Errorf("param name = %v, want <string>", p["name"])
	}
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), "secret") {
		t.Errorf("query log leaked a parameter value: %s", raw)
	}
}

// TestBoltQueryLogRecordsUser confirms the connection's principal names the user field.
func TestBoltQueryLogRecordsUser(t *testing.T) {
	db := boltDB(t)
	ql, read := captureBoltLog(QueryLogAll)
	h := db.BoltHandler(WithBoltQueryLog(ql))

	tx, err := h.Begin(map[string]any{}, bolt.Auth{Principal: "alice"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cur, err := tx.Run("RETURN 1 AS n", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for {
		_, ok, err := cur.Next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
	}
	_ = cur.Close()

	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0]["user"] != "alice" {
		t.Errorf("user = %v, want alice", entries[0]["user"])
	}
}

// TestBoltQueryLogRecordsParseFailure confirms a statement that does not parse is recorded
// as an error at the errors level, while an ok query at that level is dropped.
func TestBoltQueryLogRecordsParseFailure(t *testing.T) {
	db := boltDB(t)
	ql, read := captureBoltLog(QueryLogErrors)
	h := db.BoltHandler(WithBoltQueryLog(ql))

	// An ok query at the errors level is dropped.
	runBolt(t, h, "RETURN 1 AS n", nil)

	// A statement that does not parse is logged as a failure before any cursor exists.
	tx, err := h.Begin(map[string]any{}, bolt.Auth{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Run("THIS IS NOT CYPHER", nil); err == nil {
		t.Fatal("expected a parse error")
	}

	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (only the failure)", len(entries))
	}
	if entries[0]["status"] != "error" {
		t.Errorf("status = %v, want error", entries[0]["status"])
	}
}

// TestBoltQueryLogRecordsThrottle confirms a rate-limited query is recorded as an error,
// since a throttle is a failed query the operator wants to see.
func TestBoltQueryLogRecordsThrottle(t *testing.T) {
	db := boltDB(t)
	ql, read := captureBoltLog(QueryLogErrors)
	h := db.BoltHandler(WithBoltQueryLog(ql), WithBoltRateLimiter(NewRateLimiter(0.001, 1)))

	tx, err := h.Begin(map[string]any{}, bolt.Auth{Principal: "alice"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// First query passes (burst of one), draining its cursor, and is an ok query so it is
	// dropped at the errors level.
	cur, err := tx.Run("RETURN 1 AS n", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for {
		_, ok, err := cur.Next()
		if err != nil || !ok {
			break
		}
	}
	_ = cur.Close()
	// Second query is throttled and recorded as an error.
	if _, err := tx.Run("RETURN 2 AS n", nil); err == nil {
		t.Fatal("expected a throttle error")
	}

	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (only the throttle)", len(entries))
	}
	if entries[0]["status"] != "error" {
		t.Errorf("status = %v, want error", entries[0]["status"])
	}
}

// TestBoltQueryLogDisabled confirms an unconfigured log records nothing and does not disturb
// execution.
func TestBoltQueryLogDisabled(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()
	rows, _, _ := runBolt(t, h, "RETURN 1 AS n", nil)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
}

// TestBoltQueryLogTimeout confirms a query cut off by the wall-clock cap is recorded with a
// timeout status, distinguishing it from an ordinary error.
func TestBoltQueryLogTimeout(t *testing.T) {
	db := boltDB(t)
	ql, read := captureBoltLog(QueryLogAll)
	h := db.BoltHandler(WithBoltQueryLog(ql), WithBoltQueryMaxTime(time.Nanosecond))

	tx, err := h.Begin(map[string]any{}, bolt.Auth{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// The deadline has already passed, so the run fails before a cursor is returned.
	if _, err := tx.Run("RETURN 1 AS n", nil); err == nil {
		t.Fatal("expected a timeout error")
	}

	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0]["status"] != "timeout" {
		t.Errorf("status = %v, want timeout", entries[0]["status"])
	}
}
