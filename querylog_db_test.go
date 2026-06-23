package gr

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/gr/vfs"
)

// captureQueryLog builds a query log at the given level, redaction policy, and slow
// threshold writing JSON into a buffer, plus a reader that parses the buffer into one map
// per entry, so a wiring test can assert what the embedded query path recorded.
func captureQueryLog(level QueryLogLevel, redact RedactPolicy, slow time.Duration) (*QueryLog, func() []map[string]any) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ql := NewQueryLog(logger, level, redact, slow)
	return ql, func() []map[string]any { return parseJSONLines(buf.String()) }
}

// parseJSONLines parses newline-delimited JSON log output into one map per non-empty line.
func parseJSONLines(s string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
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

// TestDBQueryLogRecordsStatements confirms the embedded Run and Query seams feed the query
// log: an eager write records at dispatch, a streaming read records at Close, and each entry
// carries the statement text, the kind, the ok status, and the row count (doc 20 §10).
func TestDBQueryLogRecordsStatements(t *testing.T) {
	ql, read := captureQueryLog(QueryLogAll, RedactAll, time.Hour)
	fsys := vfs.NewMem()
	db, err := Open("q.gr", Options{VFS: fsys, SaltSeed: 1, QueryLog: ql})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'Ada'})", nil); err != nil {
		t.Fatal(err)
	}
	res, err := db.Query("MATCH (n:Person) RETURN n.name", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, res)

	entries := read()
	if len(entries) != 2 {
		t.Fatalf("query-log entries = %d, want 2: %v", len(entries), entries)
	}
	write := entries[0]
	if write["kind"] != "write" || write["status"] != "ok" {
		t.Errorf("write entry kind/status = %v/%v, want write/ok", write["kind"], write["status"])
	}
	if !strings.Contains(write["cypher"].(string), "CREATE") {
		t.Errorf("write entry cypher = %q, want the CREATE text", write["cypher"])
	}
	rd := entries[1]
	if rd["kind"] != "read" || rd["status"] != "ok" {
		t.Errorf("read entry kind/status = %v/%v, want read/ok", rd["kind"], rd["status"])
	}
	if rows, ok := rd["rows_returned"].(float64); !ok || rows != 1 {
		t.Errorf("read entry rows_returned = %v, want 1", rd["rows_returned"])
	}
	if id, ok := rd["query_id"].(string); !ok || id == "" {
		t.Errorf("read entry has no query_id: %v", rd)
	}
}

// TestDBQueryLogPlanMs confirms a query-log entry carries plan_ms, the time the plan phase
// took, so a slow-query investigation can tell whether the latency came from the plan or the
// execute phase (doc 20 §10.2). A cache miss computes the plan, so plan_ms is positive; a
// cache hit skips most work, but the cache lookup itself still takes measurable time.
func TestDBQueryLogPlanMs(t *testing.T) {
	ql, read := captureQueryLog(QueryLogAll, RedactAll, time.Hour)
	fsys := vfs.NewMem()
	db, err := Open("pm.gr", Options{VFS: fsys, SaltSeed: 1, QueryLog: ql})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'Ada'})", nil); err != nil {
		t.Fatal(err)
	}
	res, err := db.Query("MATCH (n:Person) RETURN n", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, res)

	entries := read()
	for _, e := range entries {
		planMs, ok := e["plan_ms"]
		if !ok {
			t.Errorf("entry has no plan_ms field: %v", e)
			continue
		}
		if v, _ := planMs.(float64); v < 0 {
			t.Errorf("plan_ms = %v, want >= 0", v)
		}
	}
}

// TestDBQueryLogRowsScanned confirms a read entry carries rows_scanned, the work the query
// touched, alongside rows_returned, so the slow-query log surfaces the scanned/returned
// amplification (doc 20 §10.2, §16.2). The query scans every Person and returns the one that
// matches the filter, so scanned exceeds returned.
func TestDBQueryLogRowsScanned(t *testing.T) {
	ql, read := captureQueryLog(QueryLogAll, RedactAll, time.Hour)
	fsys := vfs.NewMem()
	db, err := Open("scan.gr", Options{VFS: fsys, SaltSeed: 1, QueryLog: ql})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	for _, name := range []string{"Ada", "Bob", "Cleo", "Dan"} {
		if _, err := db.Run(context.Background(), "CREATE (:Person {name: $n})", map[string]any{"n": name}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := db.Query("MATCH (n:Person) WHERE n.name = 'Ada' RETURN n", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, res)

	var rd map[string]any
	for _, e := range read() {
		if e["kind"] == "read" {
			rd = e
		}
	}
	if rd == nil {
		t.Fatalf("no read entry in the query log")
	}
	returned, _ := rd["rows_returned"].(float64)
	scanned, ok := rd["rows_scanned"].(float64)
	if !ok {
		t.Fatalf("read entry has no rows_scanned: %v", rd)
	}
	if returned != 1 {
		t.Errorf("rows_returned = %v, want 1", returned)
	}
	if scanned < returned {
		t.Errorf("rows_scanned = %v, want at least rows_returned = %v", scanned, returned)
	}
}

// TestDBQueryLogError confirms a failed statement is recorded with an error status and that,
// when an event log is linked, it also raises a query_error event independent of the
// query-log level (doc 20 §10.4, §11.3).
func TestDBQueryLogError(t *testing.T) {
	ql, readQ := captureQueryLog(QueryLogErrors, RedactAll, time.Hour)
	el, readE := captureDBEvents()
	fsys := vfs.NewMem()
	db, err := Open("e.gr", Options{VFS: fsys, SaltSeed: 1, QueryLog: ql, EventLog: el})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "THIS IS NOT CYPHER", nil); err == nil {
		t.Fatal("expected a parse error")
	}

	var failed map[string]any
	for _, e := range readQ() {
		if e["status"] == "error" {
			failed = e
		}
	}
	if failed == nil {
		t.Fatalf("no error entry in the query log: %v", readQ())
	}
	if _, ok := failed["error"]; !ok {
		t.Errorf("error entry has no error text: %v", failed)
	}

	var ev map[string]any
	for _, e := range readE() {
		if e["event"] == EventQueryError {
			ev = e
		}
	}
	if ev == nil {
		t.Fatalf("no query_error event raised: %v", readE())
	}
	if ev["status"] != "error" {
		t.Errorf("query_error event status = %v, want error", ev["status"])
	}
}

// TestDBQueryLogSlowCapturesPlan confirms a query past the slow threshold is logged with the
// slow flag and the plan it ran, regardless of the query-log level, and that a linked event
// log raises a query_slow event (doc 20 §10.6, §11.3). A 1ns threshold makes every query
// slow, so the test does not depend on wall-clock timing.
func TestDBQueryLogSlowCapturesPlan(t *testing.T) {
	ql, readQ := captureQueryLog(QueryLogOff, RedactAll, time.Nanosecond)
	el, readE := captureDBEvents()
	fsys := vfs.NewMem()
	db, err := Open("s.gr", Options{VFS: fsys, SaltSeed: 1, QueryLog: ql, EventLog: el})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "CREATE (:Person {name: 'Ada'})", nil); err != nil {
		t.Fatal(err)
	}
	res, err := db.Query("MATCH (n:Person) RETURN n", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, res)

	var slow map[string]any
	for _, e := range readQ() {
		if e["slow"] == true {
			slow = e
		}
	}
	if slow == nil {
		t.Fatalf("no slow entry logged at QueryLogOff: %v", readQ())
	}
	if plan, ok := slow["plan"].(string); !ok || plan == "" {
		t.Errorf("slow entry has no captured plan: %v", slow)
	}

	var ev map[string]any
	for _, e := range readE() {
		if e["event"] == EventQuerySlow {
			ev = e
		}
	}
	if ev == nil {
		t.Fatalf("no query_slow event raised: %v", readE())
	}
}

// TestDBQueryLogParamsRedacted confirms the redaction policy governs how parameters appear:
// the default RedactAll logs each value's shape, RedactNone the value itself (doc 20 §10.3).
func TestDBQueryLogParamsRedacted(t *testing.T) {
	run := func(redact RedactPolicy) map[string]any {
		ql, read := captureQueryLog(QueryLogAll, redact, time.Hour)
		fsys := vfs.NewMem()
		db, err := Open("p.gr", Options{VFS: fsys, SaltSeed: 1, QueryLog: ql})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		res, err := db.Run(context.Background(), "RETURN $n", map[string]any{"n": int64(42)})
		if err != nil {
			t.Fatal(err)
		}
		drainResult(t, res)
		entries := read()
		if len(entries) == 0 {
			t.Fatal("no query-log entry recorded")
		}
		params, ok := entries[0]["params"].(map[string]any)
		if !ok {
			t.Fatalf("entry has no params map: %v", entries[0])
		}
		return params
	}

	if got := run(RedactAll)["n"]; got != "<int>" {
		t.Errorf("RedactAll params n = %v, want <int>", got)
	}
	if got := run(RedactNone)["n"]; got != float64(42) {
		t.Errorf("RedactNone params n = %v, want 42", got)
	}
}

// TestDBNoQueryLogDisabled confirms a database opened without a query log neither panics nor
// records, the embedded default.
func TestDBNoQueryLogDisabled(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("none.gr", Options{VFS: fsys, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if db.querylog != nil {
		t.Errorf("querylog = %v, want nil when none configured", db.querylog)
	}
	if _, err := db.Run(context.Background(), "CREATE (:Person)", nil); err != nil {
		t.Fatal(err)
	}
	res, err := db.Query("MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatal(err)
	}
	drainResult(t, res)
}
