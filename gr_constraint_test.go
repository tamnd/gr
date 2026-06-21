package gr

import (
	"errors"
	"testing"

	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// mustExec runs a statement and fails the test on error, returning the summary.
func mustExec(t *testing.T, db *DB, q string, params map[string]value.Value) Summary {
	t.Helper()
	sum, err := db.Exec(q, params)
	if err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
	return sum
}

func nodeCount(t *testing.T, db *DB) int64 {
	t.Helper()
	rows := collectRows(t, db, "MATCH (n) RETURN count(*) AS n", nil)
	n, _ := rows[0]["n"].AsInt()
	return n
}

func TestExecCreateConstraintSummary(t *testing.T) {
	db := openMem(t, "ccsummary.gr")
	defer func() { _ = db.Close() }()

	sum := mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	if sum.ConstraintsAdded != 1 {
		t.Fatalf("summary = %+v, want ConstraintsAdded 1", sum)
	}
}

func TestExecUniqueConstraintRejectsDuplicate(t *testing.T) {
	db := openMem(t, "cdup.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)

	_, err := db.Exec("CREATE (:Person {email: 'a@x'})", nil)
	if err == nil {
		t.Fatal("duplicate insert was accepted")
	}
	var ce *engine.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a ConstraintError", err)
	}
	// The rejected transaction left no trace.
	if n := nodeCount(t, db); n != 1 {
		t.Fatalf("node count = %d after rejected insert, want 1", n)
	}
}

func TestExecUniqueConstraintAllowsDistinct(t *testing.T) {
	db := openMem(t, "cdistinct.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	mustExec(t, db, "CREATE (:Person {email: 'b@x'})", nil)
	if n := nodeCount(t, db); n != 2 {
		t.Fatalf("node count = %d, want 2", n)
	}
}

// TestExecUniqueConstraintNullsExempt confirms nodes missing the property do not
// collide with each other (doc 08 §4.1: uniqueness exempts nulls).
func TestExecUniqueConstraintNullsExempt(t *testing.T) {
	db := openMem(t, "cnull.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	mustExec(t, db, "CREATE (:Person {name: 'A'})", nil)
	mustExec(t, db, "CREATE (:Person {name: 'B'})", nil)
	if n := nodeCount(t, db); n != 2 {
		t.Fatalf("node count = %d, want 2", n)
	}
}

// TestExecUniqueConstraintLabelScoped confirms the same value under a different
// label is fine, since the constraint is scoped to its label.
func TestExecUniqueConstraintLabelScoped(t *testing.T) {
	db := openMem(t, "cscope.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	mustExec(t, db, "CREATE (:Company {email: 'a@x'})", nil)
	if n := nodeCount(t, db); n != 2 {
		t.Fatalf("node count = %d, want 2", n)
	}
}

// TestExecMergeIdempotentUnderConstraint confirms MERGE on a constrained property
// matches the existing node the second time rather than creating a duplicate.
func TestExecMergeIdempotentUnderConstraint(t *testing.T) {
	db := openMem(t, "cmerge.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	mustExec(t, db, "MERGE (p:Person {email: 'a@x'}) ON CREATE SET p.name = 'Alice'", nil)
	sum := mustExec(t, db, "MERGE (p:Person {email: 'a@x'}) ON CREATE SET p.name = 'Bob'", nil)
	if sum.NodesCreated != 0 {
		t.Fatalf("second MERGE created %d nodes, want 0", sum.NodesCreated)
	}
	rows := collectRows(t, db, "MATCH (p:Person) RETURN p.name AS name", nil)
	if len(rows) != 1 {
		t.Fatalf("Person count = %d, want 1", len(rows))
	}
	if name, _ := rows[0]["name"].AsString(); name != "Alice" {
		t.Fatalf("name = %q, want Alice (ON MATCH did not fire ON CREATE)", name)
	}
}

// TestExecUniqueConstraintViaSet drives a duplicate through SET and confirms the
// violation aborts and rolls the mutation back.
func TestExecUniqueConstraintViaSet(t *testing.T) {
	db := openMem(t, "cset.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	mustExec(t, db, "CREATE (:Person {email: 'b@x'})", nil)

	_, err := db.Exec("MATCH (p:Person {email: 'b@x'}) SET p.email = 'a@x'", nil)
	if err == nil {
		t.Fatal("colliding SET was accepted")
	}
	var ce *engine.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a ConstraintError", err)
	}
	// The SET rolled back: 'b@x' is still present.
	rows := collectRows(t, db, "MATCH (p:Person {email: 'b@x'}) RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 1 {
		t.Fatalf("b@x present = %d, want 1 (SET should have rolled back)", n)
	}
}

// TestExecCreateConstraintValidatesExistingData confirms a constraint cannot be
// added over data that already violates it (doc 08 §6.4).
func TestExecCreateConstraintValidatesExistingData(t *testing.T) {
	db := openMem(t, "cvalidate.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)

	_, err := db.Exec("CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	if err == nil {
		t.Fatal("constraint was added over duplicate data")
	}
	var ce *engine.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a ConstraintError", err)
	}
	// The failed creation left no constraint behind: a later distinct insert is
	// still unconstrained, and the two duplicates remain.
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	if n := nodeCount(t, db); n != 3 {
		t.Fatalf("node count = %d, want 3 (no constraint was added)", n)
	}
}

func TestExecConstraintPersistsAcrossReopen(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("cpersist.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open("cpersist.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	if _, err := db2.Exec("CREATE (:Person {email: 'a@x'})", nil); err == nil {
		t.Fatal("duplicate accepted after reopen: constraint did not persist")
	}
}

func TestExecDropConstraint(t *testing.T) {
	db := openMem(t, "cdrop.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT person_email FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)

	sum := mustExec(t, db, "DROP CONSTRAINT person_email", nil)
	if sum.ConstraintsRemoved != 1 {
		t.Fatalf("summary = %+v, want ConstraintsRemoved 1", sum)
	}
	// The duplicate is now allowed.
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	if n := nodeCount(t, db); n != 2 {
		t.Fatalf("node count = %d, want 2 after drop", n)
	}
}

func TestExecCreateConstraintIfNotExists(t *testing.T) {
	db := openMem(t, "cifnot.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT c FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)

	// Plain re-create errors.
	if _, err := db.Exec("CREATE CONSTRAINT c FOR (p:Person) REQUIRE p.email IS UNIQUE", nil); err == nil {
		t.Fatal("re-create of existing constraint did not error")
	}
	// IF NOT EXISTS is a no-op.
	sum := mustExec(t, db, "CREATE CONSTRAINT c IF NOT EXISTS FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	if sum.ConstraintsAdded != 0 {
		t.Fatalf("IF NOT EXISTS re-create added %d, want 0", sum.ConstraintsAdded)
	}
}

func TestExecDropConstraintIfExists(t *testing.T) {
	db := openMem(t, "cdropif.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("DROP CONSTRAINT missing", nil); err == nil {
		t.Fatal("plain drop of missing constraint did not error")
	}
	sum := mustExec(t, db, "DROP CONSTRAINT missing IF EXISTS", nil)
	if sum.ConstraintsRemoved != 0 {
		t.Fatalf("IF EXISTS drop removed %d, want 0", sum.ConstraintsRemoved)
	}
}

func TestQueryRejectsSchemaCommand(t *testing.T) {
	db := openMem(t, "cqueryreject.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Query("CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil); !errors.Is(err, ErrSchemaCommand) {
		t.Fatalf("Query of schema command returned %v, want ErrSchemaCommand", err)
	}
}
