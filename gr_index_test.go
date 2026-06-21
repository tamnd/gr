package gr

import (
	"errors"
	"testing"

	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

func TestExecCreateIndexSummary(t *testing.T) {
	db := openMem(t, "ixsummary.gr")
	defer func() { _ = db.Close() }()

	sum := mustExec(t, db, "CREATE INDEX FOR (p:Person) ON (p.email)", nil)
	if sum.IndexesAdded != 1 {
		t.Fatalf("summary = %+v, want IndexesAdded 1", sum)
	}
}

func TestExecDropIndexSummary(t *testing.T) {
	db := openMem(t, "ixdrop.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE INDEX person_email FOR (p:Person) ON (p.email)", nil)
	sum := mustExec(t, db, "DROP INDEX person_email", nil)
	if sum.IndexesRemoved != 1 {
		t.Fatalf("summary = %+v, want IndexesRemoved 1", sum)
	}
}

// TestExecCreateIndexIfNotExists confirms the guarded form is a no-op on a second
// create while the plain form is an error.
func TestExecCreateIndexIfNotExists(t *testing.T) {
	db := openMem(t, "ixifnotexists.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE INDEX i FOR (p:Person) ON (p.email)", nil)
	if _, err := db.Exec("CREATE INDEX i FOR (p:Person) ON (p.email)", nil); err == nil {
		t.Fatal("plain re-create was accepted")
	}
	sum := mustExec(t, db, "CREATE INDEX i IF NOT EXISTS FOR (p:Person) ON (p.email)", nil)
	if sum.IndexesAdded != 0 {
		t.Fatalf("guarded re-create added an index: %+v", sum)
	}
}

// TestExecDropIndexIfExists confirms the guarded drop is a no-op on a missing
// index while the plain form is an error.
func TestExecDropIndexIfExists(t *testing.T) {
	db := openMem(t, "ixdropifexists.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("DROP INDEX nope", nil); err == nil {
		t.Fatal("plain drop of a missing index was accepted")
	}
	sum := mustExec(t, db, "DROP INDEX nope IF EXISTS", nil)
	if sum.IndexesRemoved != 0 {
		t.Fatalf("guarded drop of a missing index removed one: %+v", sum)
	}
}

// TestExecIndexDoesNotChangeResults confirms an index is a transparent access
// path: queries return the same rows whether or not one is declared, and the
// index stays consistent as data changes.
func TestExecIndexDoesNotChangeResults(t *testing.T) {
	db := openMem(t, "ixtransparent.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE INDEX FOR (p:Person) ON (p.email)", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	mustExec(t, db, "CREATE (:Person {email: 'b@x'})", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)

	rows := collectRows(t, db, "MATCH (p:Person) WHERE p.email = 'a@x' RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 2 {
		t.Fatalf("count for a@x = %d, want 2", n)
	}

	// A change to the data is reflected through the index-backed query.
	mustExec(t, db, "MATCH (p:Person {email: 'b@x'}) SET p.email = 'a@x'", nil)
	rows = collectRows(t, db, "MATCH (p:Person) WHERE p.email = 'a@x' RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 3 {
		t.Fatalf("count for a@x after update = %d, want 3", n)
	}
}

// TestExecIndexPersistsAcrossReopen confirms a declared index is present again
// after a close and reopen and still serves queries.
func TestExecIndexPersistsAcrossReopen(t *testing.T) {
	fsys := vfs.NewMem()
	db, err := Open("ixpersist.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE INDEX person_email FOR (p:Person) ON (p.email)", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open("ixpersist.gr", Options{VFS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	// A duplicate create proves the index survived: it would succeed if the index
	// had vanished.
	if _, err := db2.Exec("CREATE INDEX person_email FOR (p:Person) ON (p.email)", nil); err == nil {
		t.Fatal("index did not persist across reopen")
	}
	rows := collectRows(t, db2, "MATCH (p:Person) WHERE p.email = 'a@x' RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 1 {
		t.Fatalf("count after reopen = %d, want 1", n)
	}
}

// TestQueryRejectsCreateIndex confirms schema commands run through Exec, not the
// read-only Query path.
func TestQueryRejectsCreateIndex(t *testing.T) {
	db := openMem(t, "ixreject.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Query("CREATE INDEX FOR (p:Person) ON (p.email)", nil); !errors.Is(err, ErrSchemaCommand) {
		t.Fatalf("Query(CREATE INDEX) = %v, want ErrSchemaCommand", err)
	}
	if _, err := db.Query("DROP INDEX i", nil); !errors.Is(err, ErrSchemaCommand) {
		t.Fatalf("Query(DROP INDEX) = %v, want ErrSchemaCommand", err)
	}
}

// TestExecIndexCrossTypeNumeric confirms the index access path honors Cypher's
// cross-type numeric equality: a seek for the integer 1 still finds a node whose
// indexed property was stored as the float 1.0, and the other way round.
func TestExecIndexCrossTypeNumeric(t *testing.T) {
	db := openMem(t, "ixnumeric.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE INDEX FOR (p:Person) ON (p.score)", nil)
	mustExec(t, db, "CREATE (:Person {score: 1})", nil)
	mustExec(t, db, "CREATE (:Person {score: 1.0})", nil)

	// An integer probe matches both the integer and the float node.
	rows := collectRows(t, db, "MATCH (p:Person) WHERE p.score = 1 RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 2 {
		t.Fatalf("count for score = 1 is %d, want 2", n)
	}
	// A float probe matches both as well.
	rows = collectRows(t, db, "MATCH (p:Person) WHERE p.score = 1.0 RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 2 {
		t.Fatalf("count for score = 1.0 is %d, want 2", n)
	}
	// A different value matches neither.
	rows = collectRows(t, db, "MATCH (p:Person) WHERE p.score = 2 RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 0 {
		t.Fatalf("count for score = 2 is %d, want 0", n)
	}
}

// TestExecIndexResidualLabel confirms a seek on one indexed label still residual-
// checks the node's other required labels.
func TestExecIndexResidualLabel(t *testing.T) {
	db := openMem(t, "ixresidual.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE INDEX FOR (p:Person) ON (p.email)", nil)
	mustExec(t, db, "CREATE (:Person:Admin {email: 'a@x'})", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)

	rows := collectRows(t, db, "MATCH (p:Person:Admin) WHERE p.email = 'a@x' RETURN count(*) AS n", nil)
	if n, _ := rows[0]["n"].AsInt(); n != 1 {
		t.Fatalf("count for Person:Admin with email a@x is %d, want 1", n)
	}
}

// TestExecIndexParamValue confirms an index-eligible query still works when the
// sought value comes from a parameter.
func TestExecIndexParamValue(t *testing.T) {
	db := openMem(t, "ixparam.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE INDEX FOR (p:Person) ON (p.email)", nil)
	mustExec(t, db, "CREATE (:Person {email: 'a@x'})", nil)
	rows := collectRows(t, db, "MATCH (p:Person) WHERE p.email = $e RETURN count(*) AS n",
		map[string]value.Value{"e": value.String("a@x")})
	if n, _ := rows[0]["n"].AsInt(); n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
}
