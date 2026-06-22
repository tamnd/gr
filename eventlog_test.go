package gr

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// captureEventLog builds an event log writing JSON into a buffer at the given threshold, plus
// a reader that parses the buffer into one map per entry.
func captureEventLog(threshold slog.Level) (*EventLog, func() []map[string]any) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: threshold}))
	el := NewEventLog(logger)
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
	return el, read
}

// TestEventLogNilDisabled confirms a nil logger yields a nil log and that the record methods
// are safe no-ops on a nil log.
func TestEventLogNilDisabled(t *testing.T) {
	if el := NewEventLog(nil); el != nil {
		t.Fatalf("nil logger should make a nil log, got %v", el)
	}
	var el *EventLog
	el.Open("db.gr", 1, 4096, false) // must not panic
	el.Close("db.gr", true)
	el.Event(slog.LevelInfo, EventGCRun, "gc")
}

// TestEventLogOpenClose confirms the open and close events carry their documented fields and
// the common ts/event/level/msg fields.
func TestEventLogOpenClose(t *testing.T) {
	el, read := captureEventLog(slog.LevelInfo)
	el.Open("data.gr", 2, 8192, true)
	el.Close("data.gr", true)

	entries := read()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	open := entries[0]
	if open["event"] != EventOpen {
		t.Errorf("event = %v, want %v", open["event"], EventOpen)
	}
	if open["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", open["level"])
	}
	if open["ts"] == nil || open["ts"] == "" {
		t.Errorf("ts missing")
	}
	if open["path"] != "data.gr" {
		t.Errorf("path = %v, want data.gr", open["path"])
	}
	if open["format_version"].(float64) != 2 {
		t.Errorf("format_version = %v, want 2", open["format_version"])
	}
	if open["page_size"].(float64) != 8192 {
		t.Errorf("page_size = %v, want 8192", open["page_size"])
	}
	if open["recovered"] != true {
		t.Errorf("recovered = %v, want true", open["recovered"])
	}
	if entries[1]["event"] != EventClose || entries[1]["clean"] != true {
		t.Errorf("close entry = %v", entries[1])
	}
}

// TestEventLogLevelThreshold confirms the slog handler's threshold drops an entry below it:
// an info open is dropped when the threshold is warn, a warn auth failure is kept.
func TestEventLogLevelThreshold(t *testing.T) {
	el, read := captureEventLog(slog.LevelWarn)
	el.Open("db.gr", 1, 4096, false)                    // info, dropped
	el.AuthFailure("alice", "10.0.0.1", "bad password") // warn, kept

	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (only the warn)", len(entries))
	}
	e := entries[0]
	if e["event"] != EventAuthFailure {
		t.Errorf("event = %v, want %v", e["event"], EventAuthFailure)
	}
	if e["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", e["level"])
	}
	if e["user"] != "alice" || e["client"] != "10.0.0.1" || e["reason"] != "bad password" {
		t.Errorf("auth failure fields = %v", e)
	}
}

// TestEventLogTypedHelpers confirms the remaining typed helpers emit their event names and
// fields.
func TestEventLogTypedHelpers(t *testing.T) {
	el, read := captureEventLog(LevelTrace)
	el.RecoveryComplete(7, 1234, 5*time.Millisecond)
	el.CheckpointComplete(42, 3, 10*time.Millisecond)
	el.ConfigChange("query.max_time", "0s", "30s", "admin")
	el.Overload(16, 4, "shed")

	entries := read()
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4", len(entries))
	}
	if entries[0]["event"] != EventRecoveryComplete || entries[0]["transactions_replayed"].(float64) != 7 {
		t.Errorf("recovery entry = %v", entries[0])
	}
	if entries[1]["event"] != EventCheckpointComplete || entries[1]["pages_written"].(float64) != 42 {
		t.Errorf("checkpoint entry = %v", entries[1])
	}
	if entries[2]["event"] != EventConfigChange || entries[2]["new"] != "30s" || entries[2]["who"] != "admin" {
		t.Errorf("config entry = %v", entries[2])
	}
	if entries[3]["event"] != EventOverload || entries[3]["action"] != "shed" {
		t.Errorf("overload entry = %v", entries[3])
	}
}

// TestEventLogGenericEvent confirms the generic Event path stamps the common fields for an
// event with no typed helper.
func TestEventLogGenericEvent(t *testing.T) {
	el, read := captureEventLog(LevelTrace)
	el.Event(slog.LevelError, EventFsyncError, "fsync failed",
		slog.Int("errno", 5),
		slog.String("action", "stop"),
	)
	e := read()[0]
	if e["event"] != EventFsyncError {
		t.Errorf("event = %v, want %v", e["event"], EventFsyncError)
	}
	if e["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", e["level"])
	}
	if e["errno"].(float64) != 5 || e["action"] != "stop" {
		t.Errorf("fsync fields = %v", e)
	}
}
