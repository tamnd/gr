package gr

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
	"github.com/tamnd/gr/wal"
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

// TestDBCheckpointEvent confirms a wal_checkpoint PRAGMA emits a checkpoint_complete
// event carrying the work the checkpoint did and its duration, the operational
// narrative an operator reads for checkpoint cadence (doc 20 §11.3).
func TestDBCheckpointEvent(t *testing.T) {
	el, read := captureDBEvents()
	fsys := vfs.NewMem()
	db, err := Open("ckpt.gr", Options{VFS: fsys, SaltSeed: 1, EventLog: el})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("CREATE (:Person {name: 'a'})", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Run(context.Background(), "PRAGMA wal_checkpoint", nil); err != nil {
		t.Fatal(err)
	}

	var ckpt map[string]any
	for _, e := range read() {
		if e["event"] == EventCheckpointComplete {
			ckpt = e
		}
	}
	if ckpt == nil {
		t.Fatalf("no checkpoint_complete event after a checkpoint; entries = %v", read())
	}
	if _, ok := ckpt["duration_ms"]; !ok {
		t.Errorf("checkpoint event has no duration_ms: %v", ckpt)
	}
	if pw, ok := ckpt["pages_written"].(float64); !ok || pw < 0 {
		t.Errorf("pages_written = %v, want a nonnegative count", ckpt["pages_written"])
	}
	if _, ok := ckpt["delta_folded"]; !ok {
		t.Errorf("checkpoint event has no delta_folded: %v", ckpt)
	}
}

// TestDBRecoveryEvent confirms reopening a database with a committed-but-uncheckpointed
// WAL prefix emits a recovery_complete event carrying the transactions it replayed, the
// durable commit sequence as the last_lsn, and a duration, the operator's confirmation
// of a clean recovery (doc 20 §11.3). The clean commit protocol folds and resets the WAL
// on every commit, so the only state that recovers is a crash between a commit's fsync
// and its checkpoint; we stage exactly that by appending a committed frame to the WAL
// directly, the same way the pager's recovery test does.
func TestDBRecoveryEvent(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("rec.gr", Options{VFS: fsys, SaltSeed: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"A", "B", "C"} {
		if _, err := db.Exec("CREATE (:Person {name: $n})", map[string]value.Value{"n": value.String(name)}); err != nil {
			t.Fatal(err)
		}
	}
	ps := db.PageSize()
	info, err := db.Info()
	if err != nil {
		t.Fatal(err)
	}
	pageCount := info.PageCount
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Read the header page's real image so the staged redo rewrites it with its own
	// content: the recovery is then a real, valid replay that leaves the database intact.
	page0 := make([]byte, ps)
	dbf, err := fsys.Open("rec.gr", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbf.ReadAt(page0, 0); err != nil {
		t.Fatal(err)
	}
	_ = dbf.Close()

	// Stage the torn commit: append a committed frame for page 0 to the WAL without
	// folding it, the state a crash between fsync and checkpoint leaves behind.
	wf, err := fsys.Open("rec.gr-wal", true)
	if err != nil {
		t.Fatal(err)
	}
	w, err := wal.Open(wf, ps, wal.SyncFull, 99)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append([]wal.Frame{{PageID: 0, Image: page0}}, true, pageCount); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen with an event log: recovery redoes the committed frame and records it.
	el, read := captureDBEvents()
	db2, err := Open("rec.gr", Options{VFS: fsys, SaltSeed: 1, EventLog: el})
	if err != nil {
		t.Fatalf("reopen after a torn commit: %v", err)
	}
	defer func() { _ = db2.Close() }()

	var start, rec map[string]any
	for _, e := range read() {
		switch e["event"] {
		case EventRecoveryStart:
			start = e
		case EventRecoveryComplete:
			rec = e
		}
	}
	if start == nil {
		t.Fatalf("no recovery_start event after a torn-commit reopen; entries = %v", read())
	}
	if sz, ok := start["wal_size"].(float64); !ok || sz <= 0 {
		t.Errorf("recovery_start wal_size = %v, want the WAL backlog found at open", start["wal_size"])
	}
	if _, ok := start["last_checkpoint_lsn"]; !ok {
		t.Errorf("recovery_start has no last_checkpoint_lsn: %v", start)
	}
	if rec == nil {
		t.Fatalf("no recovery_complete event after a torn-commit reopen; entries = %v", read())
	}
	if tx, ok := rec["transactions_replayed"].(float64); !ok || tx < 1 {
		t.Errorf("transactions_replayed = %v, want the staged commit", rec["transactions_replayed"])
	}
	if lsn, ok := rec["last_lsn"].(float64); !ok || lsn == 0 {
		t.Errorf("last_lsn = %v, want the durable commit sequence", rec["last_lsn"])
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Errorf("recovery event has no duration_ms: %v", rec)
	}
	// The database survived the recovery intact: the three writes are still there.
	if n := nodeCount(t, db2); n != 3 {
		t.Errorf("recovered node count = %d, want 3", n)
	}
}

// TestDBConfigChangeEvent confirms setting a value pragma emits a config_change event
// carrying the setting, its old and new values, and the principal, the audit trail for a
// runtime reconfiguration (doc 20 §11.3).
func TestDBConfigChangeEvent(t *testing.T) {
	el, read := captureDBEvents()
	fsys := vfs.NewMem()
	db, err := Open("cfg.gr", Options{VFS: fsys, SaltSeed: 1, EventLog: el})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "PRAGMA max_retries = 7", nil); err != nil {
		t.Fatal(err)
	}

	var cfg map[string]any
	for _, e := range read() {
		if e["event"] == EventConfigChange {
			cfg = e
		}
	}
	if cfg == nil {
		t.Fatalf("no config_change event after a pragma set; entries = %v", read())
	}
	if cfg["setting"] != "max_retries" {
		t.Errorf("setting = %v, want max_retries", cfg["setting"])
	}
	if cfg["new"] != "7" {
		t.Errorf("new = %v, want 7", cfg["new"])
	}
	if _, ok := cfg["old"]; !ok {
		t.Errorf("config_change event has no old value: %v", cfg)
	}
	if cfg["who"] != "embedded" {
		t.Errorf("who = %v, want embedded", cfg["who"])
	}
}

// TestDBNoConfigChangeEventOnRead confirms reading a pragma value emits no config_change
// event, since nothing changed.
func TestDBNoConfigChangeEventOnRead(t *testing.T) {
	el, read := captureDBEvents()
	fsys := vfs.NewMem()
	db, err := Open("cfgr.gr", Options{VFS: fsys, SaltSeed: 1, EventLog: el})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	res, err := db.Run(context.Background(), "PRAGMA max_retries", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Close()
	for _, e := range read() {
		if e["event"] == EventConfigChange {
			t.Errorf("reading a pragma emitted a config_change event: %v", e)
		}
	}
}

// TestDBConstraintViolationEvent confirms a write that violates a uniqueness constraint
// emits a constraint_violation event carrying the constraint kind, name, label, property,
// and the offending value (doc 20 §11.3). The write runs through Run so it crosses the
// measureQuery boundary where the event fires.
func TestDBConstraintViolationEvent(t *testing.T) {
	el, read := captureDBEvents()
	fsys := vfs.NewMem()
	db, err := Open("con.gr", Options{VFS: fsys, SaltSeed: 1, EventLog: el})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE CONSTRAINT person_email FOR (p:Person) REQUIRE p.email IS UNIQUE",
		"CREATE (:Person {email: 'a@x'})",
	} {
		if _, err := db.Run(ctx, stmt, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Run(ctx, "CREATE (:Person {email: 'a@x'})", nil); err == nil {
		t.Fatal("colliding insert was accepted")
	}

	var con map[string]any
	for _, e := range read() {
		if e["event"] == EventConstraintViolation {
			con = e
		}
	}
	if con == nil {
		t.Fatalf("no constraint_violation event after a rejected write; entries = %v", read())
	}
	if con["kind"] != "unique" {
		t.Errorf("kind = %v, want unique", con["kind"])
	}
	if con["constraint"] != "person_email" {
		t.Errorf("constraint = %v, want person_email", con["constraint"])
	}
	if con["label"] != "Person" {
		t.Errorf("label = %v, want Person", con["label"])
	}
	if con["property"] != "email" {
		t.Errorf("property = %v, want email", con["property"])
	}
	if con["value"] != `"a@x"` {
		t.Errorf("value = %q, want \"a@x\"", con["value"])
	}
}

// TestDBNoConstraintEventOnCleanWrite confirms a write that satisfies its constraints
// emits no constraint_violation event.
func TestDBNoConstraintEventOnCleanWrite(t *testing.T) {
	el, read := captureDBEvents()
	fsys := vfs.NewMem()
	db, err := Open("conok.gr", Options{VFS: fsys, SaltSeed: 1, EventLog: el})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE CONSTRAINT person_email FOR (p:Person) REQUIRE p.email IS UNIQUE",
		"CREATE (:Person {email: 'a@x'})",
		"CREATE (:Person {email: 'b@x'})",
	} {
		if _, err := db.Run(ctx, stmt, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range read() {
		if e["event"] == EventConstraintViolation {
			t.Errorf("a clean write emitted a constraint_violation event: %v", e)
		}
	}
}

// TestDBNoRecoveryEventOnCleanOpen confirms a fresh open that recovered nothing emits
// neither a recovery_start nor a recovery_complete event, only the open event.
func TestDBNoRecoveryEventOnCleanOpen(t *testing.T) {
	el, read := captureDBEvents()
	fsys := vfs.NewMem()
	db, err := Open("clean.gr", Options{VFS: fsys, SaltSeed: 1, EventLog: el})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, e := range read() {
		if e["event"] == EventRecoveryStart || e["event"] == EventRecoveryComplete {
			t.Errorf("a clean open emitted a recovery event: %v", e)
		}
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
