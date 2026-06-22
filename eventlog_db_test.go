package gr

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/tamnd/gr/vfs"
)

// captureDBEvents builds an event log writing JSON into a buffer plus a reader that
// parses the buffer into one map per entry, so a wiring test can assert what Open and
// Close emitted.
func captureDBEvents() (*EventLog, func() []map[string]any) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
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

// TestDBOpenCloseEvents confirms Open emits an open event with the file's real format
// version and page size and a clean (not recovered) flag on a fresh database, and that
// Close emits a clean close event.
func TestDBOpenCloseEvents(t *testing.T) {
	el, read := captureDBEvents()
	fsys := vfs.NewMem()
	db, err := Open("events.gr", Options{VFS: fsys, SaltSeed: 1, EventLog: el})
	if err != nil {
		t.Fatal(err)
	}

	entries := read()
	if len(entries) != 1 {
		t.Fatalf("after open, entries = %d, want 1", len(entries))
	}
	open := entries[0]
	if open["event"] != EventOpen {
		t.Errorf("event = %v, want %v", open["event"], EventOpen)
	}
	if open["path"] != "events.gr" {
		t.Errorf("path = %v, want events.gr", open["path"])
	}
	if open["page_size"].(float64) != float64(db.PageSize()) {
		t.Errorf("page_size = %v, want %d", open["page_size"], db.PageSize())
	}
	if open["format_version"].(float64) == 0 {
		t.Errorf("format_version = %v, want the file's real version", open["format_version"])
	}
	if open["recovered"] != false {
		t.Errorf("recovered = %v, want false on a fresh open", open["recovered"])
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	entries = read()
	if len(entries) != 2 {
		t.Fatalf("after close, entries = %d, want 2", len(entries))
	}
	closed := entries[1]
	if closed["event"] != EventClose {
		t.Errorf("event = %v, want %v", closed["event"], EventClose)
	}
	if closed["clean"] != true {
		t.Errorf("clean = %v, want true on a normal close", closed["clean"])
	}
}

// TestDBEventsNilDisabled confirms a database opened without an event log neither
// panics nor records, the embedded default.
func TestDBEventsNilDisabled(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("quiet.gr", Options{VFS: fsys, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if db.events != nil {
		t.Errorf("events = %v, want nil when no log configured", db.events)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
