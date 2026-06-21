package gr

import (
	"errors"
	"testing"

	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/vfs"
)

// TestExecCreateTypeConstraintSummary confirms a node property-type constraint is
// declared and reported in the summary.
func TestExecCreateTypeConstraintSummary(t *testing.T) {
	db := openMem(t, "tcsummary.gr")
	defer func() { _ = db.Close() }()

	sum := mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS :: INTEGER", nil)
	if sum.ConstraintsAdded != 1 {
		t.Fatalf("summary = %+v, want ConstraintsAdded 1", sum)
	}
}

// TestExecTypeConstraintRejectsWrongType confirms a node whose property holds the
// wrong type is rejected at commit and leaves no trace (doc 13 §12).
func TestExecTypeConstraintRejectsWrongType(t *testing.T) {
	db := openMem(t, "tcwrong.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS :: INTEGER", nil)

	_, err := db.Exec("CREATE (:Person {age: 'old'})", nil)
	if err == nil {
		t.Fatal("node with a wrong-typed property was accepted")
	}
	var ce *engine.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a ConstraintError", err)
	}
	if n := nodeCount(t, db); n != 0 {
		t.Fatalf("node count = %d after rejected insert, want 0", n)
	}
}

// TestExecTypeConstraintAllowsRightType confirms a node carrying the declared type
// commits cleanly.
func TestExecTypeConstraintAllowsRightType(t *testing.T) {
	db := openMem(t, "tcright.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS :: INTEGER", nil)
	mustExec(t, db, "CREATE (:Person {age: 30})", nil)
	if n := nodeCount(t, db); n != 1 {
		t.Fatalf("node count = %d, want 1", n)
	}
}

// TestExecTypeConstraintExemptsMissing confirms a node without the property is fine:
// a type constraint restricts only values that are present, unlike existence.
func TestExecTypeConstraintExemptsMissing(t *testing.T) {
	db := openMem(t, "tcmissing.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS :: INTEGER", nil)
	mustExec(t, db, "CREATE (:Person {name: 'A'})", nil)
	if n := nodeCount(t, db); n != 1 {
		t.Fatalf("node count = %d, want 1", n)
	}
}

// TestExecTypeConstraintViaSet confirms changing a property to the wrong type on an
// existing node violates the constraint and rolls the change back (doc 13 §12).
func TestExecTypeConstraintViaSet(t *testing.T) {
	db := openMem(t, "tcset.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS :: INTEGER", nil)
	mustExec(t, db, "CREATE (:Person {age: 30})", nil)

	_, err := db.Exec("MATCH (p:Person) SET p.age = 'old'", nil)
	if err == nil {
		t.Fatal("setting the property to the wrong type was accepted")
	}
	var ce *engine.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a ConstraintError", err)
	}
	// The change rolled back: the integer value is still present.
	rows := collectRows(t, db, "MATCH (p:Person) WHERE p.age = 30 RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 1 {
		t.Fatalf("age 30 present = %d, want 1 (SET should have rolled back)", n)
	}
}

// TestExecTypeConstraintCrossTypeNumericRejected confirms the type is exact: a float
// value does not satisfy an INTEGER type constraint even though 1.0 equals 1 under
// Cypher numeric equality.
func TestExecTypeConstraintCrossTypeNumericRejected(t *testing.T) {
	db := openMem(t, "tcnumeric.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS :: INTEGER", nil)
	if _, err := db.Exec("CREATE (:Person {age: 1.0})", nil); err == nil {
		t.Fatal("a float value was accepted under an INTEGER type constraint")
	}
	if n := nodeCount(t, db); n != 0 {
		t.Fatalf("node count = %d, want 0", n)
	}
}

// TestExecCreateTypeConstraintValidatesExistingData confirms a type constraint
// cannot be added over a node that already holds a wrong-typed value (doc 08 §6.4).
func TestExecCreateTypeConstraintValidatesExistingData(t *testing.T) {
	db := openMem(t, "tcvalidate.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE (:Person {age: 'old'})", nil)

	_, err := db.Exec("CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS :: INTEGER", nil)
	if err == nil {
		t.Fatal("constraint was added over a wrong-typed value")
	}
	var ce *engine.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a ConstraintError", err)
	}
	// The failed creation left no constraint: a later wrong-typed node is fine.
	mustExec(t, db, "CREATE (:Person {age: 'older'})", nil)
	if n := nodeCount(t, db); n != 2 {
		t.Fatalf("node count = %d, want 2 (no constraint was added)", n)
	}
}

// TestExecDropTypeConstraint confirms dropping a type constraint lets a wrong-typed
// node be created again.
func TestExecDropTypeConstraint(t *testing.T) {
	db := openMem(t, "tcdrop.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT person_age FOR (p:Person) REQUIRE p.age IS :: INTEGER", nil)
	mustExec(t, db, "CREATE (:Person {age: 30})", nil)

	sum := mustExec(t, db, "DROP CONSTRAINT person_age", nil)
	if sum.ConstraintsRemoved != 1 {
		t.Fatalf("summary = %+v, want ConstraintsRemoved 1", sum)
	}
	mustExec(t, db, "CREATE (:Person {age: 'old'})", nil)
	if n := nodeCount(t, db); n != 2 {
		t.Fatalf("node count = %d, want 2 after drop", n)
	}
}

// TestExecTypeConstraintIfNotExists confirms the guarded form is a no-op on a second
// create while the plain form is an error.
func TestExecTypeConstraintIfNotExists(t *testing.T) {
	db := openMem(t, "tcifnotexists.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT c FOR (p:Person) REQUIRE p.age IS :: INTEGER", nil)
	if _, err := db.Exec("CREATE CONSTRAINT c FOR (p:Person) REQUIRE p.age IS :: INTEGER", nil); err == nil {
		t.Fatal("plain re-create was accepted")
	}
	sum := mustExec(t, db, "CREATE CONSTRAINT c IF NOT EXISTS FOR (p:Person) REQUIRE p.age IS :: INTEGER", nil)
	if sum.ConstraintsAdded != 0 {
		t.Fatalf("guarded re-create added a constraint: %+v", sum)
	}
}

// TestExecTypeConstraintPersistsAcrossReopen confirms a type constraint survives a
// close and reopen and still enforces its rule.
func TestExecTypeConstraintPersistsAcrossReopen(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("tcpersist.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.age IS :: INTEGER", nil)
	mustExec(t, db, "CREATE (:Person {age: 30})", nil)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open("tcpersist.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	if _, err := db2.Exec("CREATE (:Person {age: 'old'})", nil); err == nil {
		t.Fatal("wrong-typed node accepted after reopen: constraint did not persist")
	}
}
