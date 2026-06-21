package gr

import (
	"errors"
	"testing"

	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/vfs"
)

// TestExecCreateExistenceConstraintSummary confirms a node existence constraint is
// declared and reported in the summary.
func TestExecCreateExistenceConstraintSummary(t *testing.T) {
	db := openMem(t, "exsummary.gr")
	defer func() { _ = db.Close() }()

	sum := mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS NOT NULL", nil)
	if sum.ConstraintsAdded != 1 {
		t.Fatalf("summary = %+v, want ConstraintsAdded 1", sum)
	}
}

// TestExecExistenceConstraintRejectsMissing confirms a node created without the
// required property is rejected at commit and leaves no trace (doc 13 §12).
func TestExecExistenceConstraintRejectsMissing(t *testing.T) {
	db := openMem(t, "exmissing.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS NOT NULL", nil)

	_, err := db.Exec("CREATE (:Person {name: 'A'})", nil)
	if err == nil {
		t.Fatal("node missing the required property was accepted")
	}
	var ce *engine.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a ConstraintError", err)
	}
	if n := nodeCount(t, db); n != 0 {
		t.Fatalf("node count = %d after rejected insert, want 0", n)
	}
}

// TestExecExistenceConstraintAllowsPresent confirms a node carrying the required
// property commits cleanly.
func TestExecExistenceConstraintAllowsPresent(t *testing.T) {
	db := openMem(t, "expresent.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS NOT NULL", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	if n := nodeCount(t, db); n != 1 {
		t.Fatalf("node count = %d, want 1", n)
	}
}

// TestExecExistenceConstraintLabelScoped confirms a node of another label without
// the property is fine, since the constraint is scoped to its label.
func TestExecExistenceConstraintLabelScoped(t *testing.T) {
	db := openMem(t, "exscope.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS NOT NULL", nil)
	mustExec(t, db, "CREATE (:Company {name: 'Acme'})", nil)
	if n := nodeCount(t, db); n != 1 {
		t.Fatalf("node count = %d, want 1", n)
	}
}

// TestExecExistenceConstraintViaRemove confirms removing the required property from
// an existing node violates the constraint and rolls the removal back (doc 13 §12).
func TestExecExistenceConstraintViaRemove(t *testing.T) {
	db := openMem(t, "exremove.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS NOT NULL", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)

	_, err := db.Exec("MATCH (p:Person) REMOVE p.email", nil)
	if err == nil {
		t.Fatal("removing the required property was accepted")
	}
	var ce *engine.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a ConstraintError", err)
	}
	// The removal rolled back: the property is still present.
	rows := collectRows(t, db, "MATCH (p:Person) WHERE p.email = 'a@x' RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 1 {
		t.Fatalf("email present = %d, want 1 (REMOVE should have rolled back)", n)
	}
}

// TestExecExistenceConstraintViaSetNull confirms setting the required property to
// null (a removal, doc 13 §6) violates the constraint and rolls back.
func TestExecExistenceConstraintViaSetNull(t *testing.T) {
	db := openMem(t, "exsetnull.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS NOT NULL", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)

	_, err := db.Exec("MATCH (p:Person) SET p.email = null", nil)
	if err == nil {
		t.Fatal("setting the required property to null was accepted")
	}
	var ce *engine.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a ConstraintError", err)
	}
}

// TestExecCreateExistenceConstraintValidatesExistingData confirms an existence
// constraint cannot be added over a node that already lacks the property, including
// when the property was never interned (doc 08 §6.4).
func TestExecCreateExistenceConstraintValidatesExistingData(t *testing.T) {
	db := openMem(t, "exvalidate.gr")
	defer func() { _ = db.Close() }()

	// A Person node with only a name: the email key is never interned.
	mustExec(t, db, "CREATE (:Person {name: 'A'})", nil)

	_, err := db.Exec("CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS NOT NULL", nil)
	if err == nil {
		t.Fatal("constraint was added over a node missing the property")
	}
	var ce *engine.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a ConstraintError", err)
	}
	// The failed creation left no constraint: a later node missing email is fine.
	mustExec(t, db, "CREATE (:Person {name: 'B'})", nil)
	if n := nodeCount(t, db); n != 2 {
		t.Fatalf("node count = %d, want 2 (no constraint was added)", n)
	}
}

// TestExecDropExistenceConstraint confirms dropping an existence constraint lets a
// node without the property be created again.
func TestExecDropExistenceConstraint(t *testing.T) {
	db := openMem(t, "exdrop.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT person_email FOR (p:Person) REQUIRE p.email IS NOT NULL", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)

	sum := mustExec(t, db, "DROP CONSTRAINT person_email", nil)
	if sum.ConstraintsRemoved != 1 {
		t.Fatalf("summary = %+v, want ConstraintsRemoved 1", sum)
	}
	mustExec(t, db, "CREATE (:Person {name: 'B'})", nil)
	if n := nodeCount(t, db); n != 2 {
		t.Fatalf("node count = %d, want 2 after drop", n)
	}
}

// TestExecExistenceAndUniqueCoexist confirms a uniqueness and an existence
// constraint on the same label and property both hold, each enforcing its own rule.
func TestExecExistenceAndUniqueCoexist(t *testing.T) {
	db := openMem(t, "excoexist.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE", nil)
	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS NOT NULL", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)

	// Missing the property violates existence.
	if _, err := db.Exec("CREATE (:Person {name: 'A'})", nil); err == nil {
		t.Fatal("node missing email accepted despite existence constraint")
	}
	// A duplicate violates uniqueness.
	if _, err := db.Exec("CREATE (:Person {email: 'a@x'})", nil); err == nil {
		t.Fatal("duplicate email accepted despite uniqueness constraint")
	}
	if n := nodeCount(t, db); n != 1 {
		t.Fatalf("node count = %d, want 1", n)
	}
}

// TestExecExistenceConstraintPersistsAcrossReopen confirms an existence constraint
// survives a close and reopen.
func TestExecExistenceConstraintPersistsAcrossReopen(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("expersist.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS NOT NULL", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open("expersist.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	if _, err := db2.Exec("CREATE (:Person {name: 'B'})", nil); err == nil {
		t.Fatal("node missing email accepted after reopen: constraint did not persist")
	}
}
