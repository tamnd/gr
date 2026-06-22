package gr

import (
	"errors"
	"testing"
	"time"
)

// TestQueryLogEmitsQueryError confirms a failed query raises a query_error event through the
// linked event log, carrying the query id, kind, status, and error text.
func TestQueryLogEmitsQueryError(t *testing.T) {
	ql, _ := captureLog(QueryLogAll, RedactAll, time.Second)
	el, readEvents := captureEventLog(LevelTrace)
	ql.WithEvents(el)

	ql.Record(QueryRecord{
		QueryID:  "q1",
		Kind:     "read",
		Status:   "error",
		Err:      errors.New("boom"),
		Duration: time.Millisecond,
	})

	events := readEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	e := events[0]
	if e["event"] != EventQueryError {
		t.Errorf("event = %v, want %v", e["event"], EventQueryError)
	}
	if e["query_id"] != "q1" || e["kind"] != "read" || e["status"] != "error" {
		t.Errorf("query_error fields = %v", e)
	}
	if e["error"] != "boom" {
		t.Errorf("error = %v, want boom", e["error"])
	}
}

// TestQueryLogEmitsQuerySlow confirms a slow but successful query raises a query_slow event
// with its duration and the threshold it crossed.
func TestQueryLogEmitsQuerySlow(t *testing.T) {
	ql, _ := captureLog(QueryLogAll, RedactAll, 10*time.Millisecond)
	el, readEvents := captureEventLog(LevelTrace)
	ql.WithEvents(el)

	ql.Record(QueryRecord{
		QueryID:  "q2",
		Kind:     "read",
		Status:   "ok",
		Duration: 50 * time.Millisecond,
	})

	events := readEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	e := events[0]
	if e["event"] != EventQuerySlow {
		t.Errorf("event = %v, want %v", e["event"], EventQuerySlow)
	}
	if e["query_id"] != "q2" {
		t.Errorf("query_id = %v, want q2", e["query_id"])
	}
	if e["duration_ms"].(float64) != 50 {
		t.Errorf("duration_ms = %v, want 50", e["duration_ms"])
	}
	if e["threshold_ms"].(float64) != 10 {
		t.Errorf("threshold_ms = %v, want 10", e["threshold_ms"])
	}
}

// TestQueryLogSlowFailureIsOneError confirms a query that is both slow and failed raises a
// single query_error, not also a query_slow, matching the query-log severity rule.
func TestQueryLogSlowFailureIsOneError(t *testing.T) {
	ql, _ := captureLog(QueryLogAll, RedactAll, 10*time.Millisecond)
	el, readEvents := captureEventLog(LevelTrace)
	ql.WithEvents(el)

	ql.Record(QueryRecord{
		QueryID:  "q3",
		Kind:     "write",
		Status:   "timeout",
		Err:      errors.New("deadline"),
		Duration: 100 * time.Millisecond,
	})

	events := readEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1 (the failure dominates)", len(events))
	}
	if events[0]["event"] != EventQueryError {
		t.Errorf("event = %v, want %v", events[0]["event"], EventQueryError)
	}
}

// TestQueryLogEventsIndependentOfLevel confirms a failed query at QueryLogOff still raises a
// query_error event even though its full query-log record is suppressed.
func TestQueryLogEventsIndependentOfLevel(t *testing.T) {
	ql, readLog := captureLog(QueryLogOff, RedactAll, time.Second)
	el, readEvents := captureEventLog(LevelTrace)
	ql.WithEvents(el)

	ql.Record(QueryRecord{
		QueryID:  "q4",
		Kind:     "read",
		Status:   "error",
		Err:      errors.New("nope"),
		Duration: time.Millisecond, // not slow, so the query log at Off suppresses it
	})

	if got := len(readLog()); got != 0 {
		t.Errorf("query-log entries = %d, want 0 (Off suppresses a non-slow failure)", got)
	}
	events := readEvents()
	if len(events) != 1 || events[0]["event"] != EventQueryError {
		t.Errorf("want one query_error event independent of the query-log level, got %v", events)
	}
}

// TestQueryLogNoEventsWithoutLink confirms that without a linked event log no events are
// raised, and that an ok fast query raises none even when linked.
func TestQueryLogNoEventsWithoutLink(t *testing.T) {
	ql, _ := captureLog(QueryLogAll, RedactAll, time.Second)
	// No WithEvents: the query log records, but there is no event stream to disturb.
	ql.Record(QueryRecord{QueryID: "q5", Kind: "read", Status: "ok", Duration: time.Millisecond})

	el, readEvents := captureEventLog(LevelTrace)
	ql.WithEvents(el)
	// An ok, fast query is neither slow nor failed, so it raises no event.
	ql.Record(QueryRecord{QueryID: "q6", Kind: "read", Status: "ok", Duration: time.Millisecond})
	if got := len(readEvents()); got != 0 {
		t.Errorf("events = %d, want 0 for an ok fast query", got)
	}
}
