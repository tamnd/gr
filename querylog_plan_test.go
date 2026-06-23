package gr

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// newJSONLogger builds an slog logger that writes JSON entries to w, the shape the
// query-log tests parse back.
func newJSONLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// readJSONLines parses newline-delimited JSON log entries into one map per line.
func readJSONLines(s string) []map[string]any {
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

// TestSlowQueryCapturesPlan confirms a slow query's entry carries the captured plan (doc 20
// §10.6): the EXPLAIN-grade tree the query ran, rendered from the record's plan thunk, so an
// operator reads not just that a query was slow but the plan it was slow on.
func TestSlowQueryCapturesPlan(t *testing.T) {
	ql, read := captureLog(QueryLogSlow, RedactAll, 10*time.Millisecond)
	ql.Record(QueryRecord{
		StartedAt: time.Unix(1_700_000_000, 0),
		QueryID:   "q1",
		Cypher:    "MATCH (n) RETURN n",
		Kind:      "read",
		Status:    "ok",
		Duration:  50 * time.Millisecond,
		Plan:      func() string { return "Project\n  AllNodesScan" },
	})

	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	plan, ok := entries[0]["plan"].(string)
	if !ok {
		t.Fatalf("slow entry has no plan field: %v", entries[0])
	}
	if !strings.Contains(plan, "AllNodesScan") {
		t.Errorf("captured plan = %q, want it to carry the operator tree", plan)
	}
}

// TestFastQueryOmitsPlanAndSkipsThunk confirms a query under the threshold carries no plan
// and never runs the plan thunk, so EXPLAIN-grade capture stays off the fast path (doc 20
// §10.6).
func TestFastQueryOmitsPlanAndSkipsThunk(t *testing.T) {
	ql, read := captureLog(QueryLogAll, RedactAll, time.Second)
	called := false
	ql.Record(QueryRecord{
		StartedAt: time.Unix(1_700_000_000, 0),
		QueryID:   "q1",
		Cypher:    "RETURN 1",
		Kind:      "read",
		Status:    "ok",
		Duration:  time.Millisecond,
		Plan:      func() string { called = true; return "Project" },
	})

	if called {
		t.Error("plan thunk ran for a fast query, the cost should land only on the slow path")
	}
	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if _, ok := entries[0]["plan"]; ok {
		t.Errorf("fast entry carries a plan field, want none: %v", entries[0])
	}
}

// TestPlanCaptureOffDropsPlan confirms PlanCaptureOff leaves even a slow query without a
// plan and does not run the thunk, the lightest setting (doc 20 §10.6, §28.2).
func TestPlanCaptureOffDropsPlan(t *testing.T) {
	ql, read := captureLog(QueryLogSlow, RedactAll, 10*time.Millisecond)
	ql.WithPlanCapture(PlanCaptureOff)
	called := false
	ql.Record(QueryRecord{
		StartedAt: time.Unix(1_700_000_000, 0),
		QueryID:   "q1",
		Cypher:    "MATCH (n) RETURN n",
		Kind:      "read",
		Status:    "ok",
		Duration:  50 * time.Millisecond,
		Plan:      func() string { called = true; return "Project" },
	})

	if called {
		t.Error("plan thunk ran with capture off")
	}
	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if _, ok := entries[0]["plan"]; ok {
		t.Errorf("capture-off entry carries a plan field, want none: %v", entries[0])
	}
}

// TestPlanTextRendersReadPlan confirms a read result renders its EXPLAIN-grade plan through
// PlanText, the listing the slow-query log captures, with the per-operator row estimates a
// cost-chosen plan carries (doc 20 §10.6).
func TestPlanTextRendersReadPlan(t *testing.T) {
	db := openMem(t, "planread.gr")
	mustExec(t, db, "CREATE (:Person {name: 'a'}), (:Person {name: 'b'})", nil)

	res, err := db.Run(context.Background(), "MATCH (n:Person) RETURN n", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer res.Close()
	text := res.PlanText()
	if text == "" {
		t.Fatal("read result has no captured plan")
	}
	if !strings.Contains(text, "est. rows") {
		t.Errorf("read plan = %q, want the cost-model row estimates", text)
	}
}

// TestPlanTextRendersWritePlan confirms a write result renders its plan through PlanText
// without row estimates, since a write plan is not cost-chosen (doc 20 §10.6).
func TestPlanTextRendersWritePlan(t *testing.T) {
	db := openMem(t, "planwrite.gr")

	res, err := db.Run(context.Background(), "CREATE (n:Person {name: 'a'}) RETURN n", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer res.Close()
	text := res.PlanText()
	if text == "" {
		t.Fatal("write result has no captured plan")
	}
	if strings.Contains(text, "est. rows") {
		t.Errorf("write plan = %q, want no row estimates", text)
	}
}

// TestBoltSlowQueryCapturesPlan drives a query end to end through the Bolt surface with the
// slow threshold set so every query counts as slow, and confirms the recorded entry carries
// the plan the query ran (doc 20 §10.6). It is the wiring test: the surface supplies the
// result's PlanText, the log runs it because the query is slow, and the plan lands in the
// entry.
func TestBoltSlowQueryCapturesPlan(t *testing.T) {
	db := boltDB(t)
	mustExec(t, db, "CREATE (:Person {name: 'a'})", nil)
	var buf strings.Builder
	logger := newJSONLogger(&buf)
	// A one-nanosecond threshold makes every query slow, so the plan is always captured.
	ql := NewQueryLog(logger, QueryLogSlow, RedactAll, time.Nanosecond)
	h := db.BoltHandler(WithBoltQueryLog(ql))

	runBolt(t, h, "MATCH (n:Person) RETURN n", nil)

	entries := readJSONLines(buf.String())
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	plan, ok := entries[0]["plan"].(string)
	if !ok {
		t.Fatalf("slow Bolt entry has no plan field: %v", entries[0])
	}
	if !strings.Contains(plan, "Scan") {
		t.Errorf("captured plan = %q, want the operator tree", plan)
	}
}

// TestPlanTextEmptyForSchemaResult confirms a result with no executed plan (a schema
// command) renders the empty string, so the slow-query log captures no plan for it.
func TestPlanTextEmptyForSchemaResult(t *testing.T) {
	db := openMem(t, "planschema.gr")
	res, err := db.Run(context.Background(), "CREATE INDEX FOR (n:Person) ON (n.name)", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer res.Close()
	if text := res.PlanText(); text != "" {
		t.Errorf("schema result plan = %q, want empty", text)
	}
}
