package gr

import (
	"context"
	"errors"
	"testing"

	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// pragmaDB opens a fresh in-memory database for the pragma tests.
func pragmaDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// runPragma runs a statement through Run and returns the result, failing on error.
func runPragma(t *testing.T, db *DB, cypher string) *Result {
	t.Helper()
	res, err := db.Run(context.Background(), cypher, nil)
	if err != nil {
		t.Fatalf("run %q: %v", cypher, err)
	}
	return res
}

// TestPragmaQueryReadsLiveValue confirms the query form reports the connection's effective
// value, here the open-time lazy_properties default.
func TestPragmaQueryReadsLiveValue(t *testing.T) {
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem(), LazyProperties: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	res := runPragma(t, db, "PRAGMA lazy_properties")
	defer func() { _ = res.Close() }()
	if got := res.Keys(); len(got) != 1 || got[0] != "lazy_properties" {
		t.Fatalf("columns = %v, want [lazy_properties]", got)
	}
	if !res.Next() {
		t.Fatal("no row")
	}
	b, err := res.Record().GetBool("lazy_properties")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !b {
		t.Error("lazy_properties = false, want true (set at open)")
	}
}

// TestPragmaSetSessionKnob confirms a set form changes a session knob live and the query
// form reads the new value back.
func TestPragmaSetSessionKnob(t *testing.T) {
	db := pragmaDB(t)
	if _, err := db.Run(context.Background(), "PRAGMA lazy_properties = true", nil); err != nil {
		t.Fatalf("set: %v", err)
	}
	res := runPragma(t, db, "PRAGMA lazy_properties")
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatal("no row")
	}
	if b, _ := res.Record().GetBool("lazy_properties"); !b {
		t.Error("lazy_properties did not take the set value")
	}
}

// TestPragmaSetTakesEffect confirms a set knob is observed live by the read path: after
// PRAGMA lazy_properties = true the database default the run path resolves is the new value.
func TestPragmaSetTakesEffect(t *testing.T) {
	db := pragmaDB(t)
	if db.lazyDefault() {
		t.Fatal("default lazy_properties should be false")
	}
	if _, err := db.Run(context.Background(), "PRAGMA lazy_properties = on", nil); err != nil {
		t.Fatalf("set: %v", err)
	}
	if !db.lazyDefault() {
		t.Error("lazy_properties not live after the set")
	}
}

// TestPragmaSetIntKnobs confirms the integer session knobs round-trip through the set and
// query forms.
func TestPragmaSetIntKnobs(t *testing.T) {
	db := pragmaDB(t)
	for _, tc := range []struct {
		name string
		set  string
		want int64
	}{
		{"mem_budget", "PRAGMA mem_budget = 4096", 4096},
		{"max_retries", "PRAGMA max_retries = 7", 7},
	} {
		if _, err := db.Run(context.Background(), tc.set, nil); err != nil {
			t.Fatalf("%s set: %v", tc.name, err)
		}
		res := runPragma(t, db, "PRAGMA "+tc.name)
		if !res.Next() {
			t.Fatalf("%s: no row", tc.name)
		}
		got, _ := res.Record().GetInt(tc.name)
		_ = res.Close()
		if got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestPragmaSetFloatKnob confirms the float session knob round-trips, including an integer
// value widened to float.
func TestPragmaSetFloatKnob(t *testing.T) {
	db := pragmaDB(t)
	if _, err := db.Run(context.Background(), "PRAGMA replan_drift_factor = 3", nil); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := db.drift(); got != 3 {
		t.Errorf("drift = %v, want 3", got)
	}
}

// TestPragmaReadOnlyKnobs confirms the create-time and read-only computed pragmas report
// real engine state through the query form.
func TestPragmaReadOnlyKnobs(t *testing.T) {
	db := pragmaDB(t)
	res := runPragma(t, db, "PRAGMA page_size")
	if !res.Next() {
		t.Fatal("page_size: no row")
	}
	ps, _ := res.Record().GetInt("page_size")
	_ = res.Close()
	if ps != int64(db.PageSize()) {
		t.Errorf("page_size pragma = %d, want %d", ps, db.PageSize())
	}

	res = runPragma(t, db, "PRAGMA page_count")
	if !res.Next() {
		t.Fatal("page_count: no row")
	}
	pc, _ := res.Record().GetInt("page_count")
	_ = res.Close()
	if pc <= 0 {
		t.Errorf("page_count = %d, want > 0", pc)
	}
}

// TestPragmaReadOnlyReflectsOpenMode confirms read_only reports the handle's open mode.
func TestPragmaReadOnlyReflectsOpenMode(t *testing.T) {
	db := pragmaDB(t)
	res := runPragma(t, db, "PRAGMA read_only")
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatal("no row")
	}
	if b, _ := res.Record().GetBool("read_only"); b {
		t.Error("read_only = true on a read-write database")
	}
}

// TestPragmaUnknownName confirms an unknown pragma name is a loud error, not a silent
// no-op (doc 24 §3.8).
func TestPragmaUnknownName(t *testing.T) {
	db := pragmaDB(t)
	_, err := db.Run(context.Background(), "PRAGMA synchronus", nil)
	if !errors.Is(err, ErrUnknownPragma) {
		t.Fatalf("err = %v, want ErrUnknownPragma", err)
	}
	_, err = db.Run(context.Background(), "PRAGMA synchronus = 1", nil)
	if !errors.Is(err, ErrUnknownPragma) {
		t.Fatalf("set err = %v, want ErrUnknownPragma", err)
	}
}

// TestPragmaSetCreateTimeConflicts confirms a set against a create-time knob conflicts
// rather than silently doing nothing (doc 24 §24.3).
func TestPragmaSetCreateTimeConflicts(t *testing.T) {
	db := pragmaDB(t)
	_, err := db.Run(context.Background(), "PRAGMA page_size = 8192", nil)
	if !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("err = %v, want ErrConfigConflict", err)
	}
}

// TestPragmaSetReadOnlyKnob confirms a set against a read-only computed pragma is rejected
// (doc 24 §24.4).
func TestPragmaSetReadOnlyKnob(t *testing.T) {
	db := pragmaDB(t)
	_, err := db.Run(context.Background(), "PRAGMA page_count = 10", nil)
	if !errors.Is(err, ErrNotSettable) {
		t.Fatalf("err = %v, want ErrNotSettable", err)
	}
}

// TestPragmaSetWrongType confirms a value that cannot coerce to the knob's type is a type
// error (doc 24 §24.4).
func TestPragmaSetWrongType(t *testing.T) {
	db := pragmaDB(t)
	_, err := db.Run(context.Background(), "PRAGMA mem_budget = 'big'", nil)
	if !errors.Is(err, ErrConfigType) {
		t.Fatalf("err = %v, want ErrConfigType", err)
	}
}

// TestPragmaSetOutOfRange confirms a value of the right type but outside the valid range is
// a range error (doc 24 §24.4).
func TestPragmaSetOutOfRange(t *testing.T) {
	db := pragmaDB(t)
	_, err := db.Run(context.Background(), "PRAGMA mem_budget = -1", nil)
	if !errors.Is(err, ErrConfigRange) {
		t.Fatalf("err = %v, want ErrConfigRange", err)
	}
}

// TestPragmaList confirms the discovery pragma lists every known knob with its tier and
// type, including itself (doc 24 §23.2).
func TestPragmaList(t *testing.T) {
	db := pragmaDB(t)
	res := runPragma(t, db, "PRAGMA pragma_list")
	defer func() { _ = res.Close() }()
	if got := res.Keys(); len(got) != 3 || got[0] != "name" || got[1] != "tier" || got[2] != "type" {
		t.Fatalf("columns = %v, want [name tier type]", got)
	}
	seen := map[string]string{}
	for res.Next() {
		name, _ := res.Record().GetString("name")
		tier, _ := res.Record().GetString("tier")
		seen[name] = tier
	}
	for _, want := range []string{"lazy_properties", "mem_budget", "page_size", "page_count", "pragma_list"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("pragma_list missing %q", want)
		}
	}
	if seen["lazy_properties"] != "session" {
		t.Errorf("lazy_properties tier = %q, want session", seen["lazy_properties"])
	}
	if seen["page_size"] != "create-time" {
		t.Errorf("page_size tier = %q, want create-time", seen["page_size"])
	}
}

// TestPragmaRejectedByQuery confirms Query refuses a PRAGMA and points at Run (doc 24 §3).
func TestPragmaRejectedByQuery(t *testing.T) {
	db := pragmaDB(t)
	_, err := db.Query("PRAGMA page_size", nil)
	if !errors.Is(err, ErrPragmaCommand) {
		t.Fatalf("Query err = %v, want ErrPragmaCommand", err)
	}
}

// TestPragmaRejectedByExec confirms Exec refuses a PRAGMA (doc 24 §3).
func TestPragmaRejectedByExec(t *testing.T) {
	db := pragmaDB(t)
	_, err := db.Exec("PRAGMA lazy_properties = true", nil)
	if !errors.Is(err, ErrPragmaCommand) {
		t.Fatalf("Exec err = %v, want ErrPragmaCommand", err)
	}
}

// TestPragmaRejectedInTransaction confirms a PRAGMA is not part of a managed transaction
// (doc 24 §3): it changes connection or file configuration, not transactional graph data.
func TestPragmaRejectedInTransaction(t *testing.T) {
	db := pragmaDB(t)
	tx, err := db.Begin(context.Background(), Write)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Run(context.Background(), "PRAGMA lazy_properties = true", nil); !errors.Is(err, ErrPragmaCommand) {
		t.Errorf("tx.Run err = %v, want ErrPragmaCommand", err)
	}
	if _, err := tx.Exec("PRAGMA lazy_properties = true", nil); !errors.Is(err, ErrPragmaCommand) {
		t.Errorf("tx.Exec err = %v, want ErrPragmaCommand", err)
	}
}

// TestPragmaExplainRejected confirms EXPLAIN of a PRAGMA is rejected: a PRAGMA has no
// operator plan to render (doc 24 §3).
func TestPragmaExplainRejected(t *testing.T) {
	db := pragmaDB(t)
	_, err := db.Run(context.Background(), "EXPLAIN PRAGMA page_count", nil)
	if !errors.Is(err, ErrExplainPragma) {
		t.Fatalf("err = %v, want ErrExplainPragma", err)
	}
}

// TestPragmaStatementKind confirms a PRAGMA classifies as PragmaStatement without running
// it (doc 24 §3).
func TestPragmaStatementKind(t *testing.T) {
	db := pragmaDB(t)
	k, err := db.StatementKind("PRAGMA synchronous = NORMAL")
	if err != nil {
		t.Fatalf("kind: %v", err)
	}
	if k != PragmaStatement {
		t.Errorf("kind = %v, want PragmaStatement", k)
	}
	if k.String() != "pragma" {
		t.Errorf("kind string = %q, want pragma", k.String())
	}
}

// TestPragmaUnknownEnumWordRejected confirms a known knob given a value of the wrong type
// (an enum word where a bool is expected) fails as a type error, not silently.
func TestPragmaUnknownEnumWordRejected(t *testing.T) {
	db := pragmaDB(t)
	_, err := db.Run(context.Background(), "PRAGMA lazy_properties = MAYBE", nil)
	if !errors.Is(err, ErrConfigType) {
		t.Fatalf("err = %v, want ErrConfigType", err)
	}
}

// TestPragmaActionCheckpoint runs the wal_checkpoint action in its call form after a write
// and confirms it reports the mode that ran (doc 24 §3.7).
func TestPragmaActionCheckpoint(t *testing.T) {
	db := pragmaDB(t)
	if _, err := db.Run(context.Background(), "CREATE (:Person {name:'Ada'})", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	res := runPragma(t, db, "PRAGMA wal_checkpoint(TRUNCATE)")
	defer func() { _ = res.Close() }()
	if got := res.Keys(); len(got) != 1 || got[0] != "wal_checkpoint" {
		t.Fatalf("columns = %v, want [wal_checkpoint]", got)
	}
	if !res.Next() {
		t.Fatal("no row")
	}
	if s, _ := res.Record().GetString("wal_checkpoint"); s != "truncate" {
		t.Errorf("mode = %q, want truncate", s)
	}
}

// TestPragmaActionCheckpointBareForm confirms the bare query form invokes the action with
// its default mode, so PRAGMA wal_checkpoint runs a checkpoint without parentheses.
func TestPragmaActionCheckpointBareForm(t *testing.T) {
	db := pragmaDB(t)
	res := runPragma(t, db, "PRAGMA wal_checkpoint")
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatal("no row")
	}
	if s, _ := res.Record().GetString("wal_checkpoint"); s != "truncate" {
		t.Errorf("mode = %q, want truncate (the default)", s)
	}
}

// TestPragmaActionCheckpointFullMode confirms FULL runs, since gr's checkpoint satisfies it.
func TestPragmaActionCheckpointFullMode(t *testing.T) {
	db := pragmaDB(t)
	res := runPragma(t, db, "PRAGMA wal_checkpoint(FULL)")
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatal("no row")
	}
	if s, _ := res.Record().GetString("wal_checkpoint"); s != "full" {
		t.Errorf("mode = %q, want full", s)
	}
}

// TestPragmaActionCheckpointUnsupportedMode confirms a mode gr does not implement is a loud
// range error rather than a silent fallback to the truncating checkpoint it is not.
func TestPragmaActionCheckpointUnsupportedMode(t *testing.T) {
	db := pragmaDB(t)
	for _, mode := range []string{"PASSIVE", "RESTART", "BOGUS"} {
		_, err := db.Run(context.Background(), "PRAGMA wal_checkpoint("+mode+")", nil)
		if !errors.Is(err, ErrConfigRange) {
			t.Errorf("mode %s: err = %v, want ErrConfigRange", mode, err)
		}
	}
}

// TestPragmaActionNotSettable confirms the set form is rejected on an action pragma: an
// action is invoked, not assigned.
func TestPragmaActionNotSettable(t *testing.T) {
	db := pragmaDB(t)
	_, err := db.Run(context.Background(), "PRAGMA wal_checkpoint = 1", nil)
	if !errors.Is(err, ErrNotSettable) {
		t.Fatalf("err = %v, want ErrNotSettable", err)
	}
}

// TestPragmaValueNotCallable confirms the call form is rejected on a value pragma: a knob
// that reports or sets a value has no action to invoke.
func TestPragmaValueNotCallable(t *testing.T) {
	db := pragmaDB(t)
	_, err := db.Run(context.Background(), "PRAGMA page_count(1)", nil)
	if !errors.Is(err, ErrNotSettable) {
		t.Fatalf("err = %v, want ErrNotSettable", err)
	}
}

// TestPragmaActionCheckpointReadOnly confirms a checkpoint on a read-only handle is refused:
// it would write the main file, which a read-only open forbids.
func TestPragmaActionCheckpointReadOnly(t *testing.T) {
	db := pragmaDB(t)
	db.readOnly = true
	_, err := db.Run(context.Background(), "PRAGMA wal_checkpoint", nil)
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("err = %v, want ErrReadOnly", err)
	}
}

// TestPragmaListIncludesAction confirms the discovery surface lists an action pragma with
// the action tier, so wal_checkpoint is discoverable like any other knob (doc 24 §23.2).
func TestPragmaListIncludesAction(t *testing.T) {
	db := pragmaDB(t)
	res := runPragma(t, db, "PRAGMA pragma_list")
	defer func() { _ = res.Close() }()
	var tier string
	for res.Next() {
		if name, _ := res.Record().GetString("name"); name == "wal_checkpoint" {
			tier, _ = res.Record().GetString("tier")
		}
	}
	if tier != "action" {
		t.Errorf("wal_checkpoint tier = %q, want action", tier)
	}
}

var _ = value.Null
