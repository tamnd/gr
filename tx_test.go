package gr

import (
	"errors"
	"testing"

	"github.com/tamnd/gr/value"
)

// personCount reads the number of Person nodes through a one-shot read query.
func personCount(t *testing.T, db *DB) int64 {
	t.Helper()
	res, err := db.Query("MATCH (p:Person) RETURN count(p) AS c", nil)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	defer func() { _ = res.Close() }()
	row, ok, err := res.Next()
	if err != nil || !ok {
		t.Fatalf("count next: ok=%v err=%v", ok, err)
	}
	n, ok := row[0].AsInt()
	if !ok {
		t.Fatalf("count = %v, not an int", row[0])
	}
	return n
}

// TestUpdateCommitsMultipleStatements runs two writes in one managed write
// transaction and confirms both land atomically: the second statement shares the
// snapshot of the first, and the commit makes both visible.
func TestUpdateCommitsMultipleStatements(t *testing.T) {
	db := openMem(t, "u1.gr")

	err := db.Update(func(tx *Tx) error {
		if _, err := tx.Exec("CREATE (:Person {name:$n})", map[string]value.Value{"n": value.String("Ada")}); err != nil {
			return err
		}
		_, err := tx.Exec("CREATE (:Person {name:$n})", map[string]value.Value{"n": value.String("Lin")})
		return err
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := personCount(t, db); got != 2 {
		t.Fatalf("after update, Person count = %d, want 2", got)
	}
}

// TestUpdateRollsBackOnError confirms a closure error discards every write the
// transaction made, leaving the database unchanged.
func TestUpdateRollsBackOnError(t *testing.T) {
	db := openMem(t, "u2.gr")

	sentinel := errors.New("boom")
	err := db.Update(func(tx *Tx) error {
		if _, err := tx.Exec("CREATE (:Person {name:'Ada'})", nil); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("update error = %v, want sentinel", err)
	}
	if got := personCount(t, db); got != 0 {
		t.Fatalf("after rolled-back update, Person count = %d, want 0", got)
	}
}

// TestUpdateReadYourWrites confirms a read inside a write transaction sees the
// transaction's own uncommitted writes, including a label and property key the
// transaction interned moments earlier.
func TestUpdateReadYourWrites(t *testing.T) {
	db := openMem(t, "u3.gr")

	err := db.Update(func(tx *Tx) error {
		if _, err := tx.Exec("CREATE (:Person {name:'Ada'})", nil); err != nil {
			return err
		}
		res, err := tx.Run("MATCH (p:Person) RETURN p.name AS name", nil)
		if err != nil {
			return err
		}
		defer func() { _ = res.Close() }()
		row, ok, err := res.NextRow()
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("read-your-writes saw no row")
		}
		if name, _ := row["name"].AsString(); name != "Ada" {
			t.Fatalf("read-your-writes name = %q, want Ada", name)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
}

// TestViewReadsSnapshot runs a read through View and confirms it streams a row.
func TestViewReadsSnapshot(t *testing.T) {
	db := openMem(t, "v1.gr")
	if _, err := db.Exec("CREATE (:Person {name:'Ada'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var name string
	err := db.View(func(tx *Tx) error {
		res, err := tx.Run("MATCH (p:Person) RETURN p.name AS name", nil)
		if err != nil {
			return err
		}
		defer func() { _ = res.Close() }()
		row, ok, err := res.NextRow()
		if err != nil || !ok {
			t.Fatalf("view next: ok=%v err=%v", ok, err)
		}
		name, _ = row["name"].AsString()
		return nil
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if name != "Ada" {
		t.Fatalf("view read name = %q, want Ada", name)
	}
}

// TestViewRejectsWrite confirms a write inside a read transaction is rejected with
// ErrReadOnly and changes nothing.
func TestViewRejectsWrite(t *testing.T) {
	db := openMem(t, "v2.gr")

	err := db.View(func(tx *Tx) error {
		_, err := tx.Exec("CREATE (:Person {name:'Ada'})", nil)
		return err
	})
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("view write error = %v, want ErrReadOnly", err)
	}
	if got := personCount(t, db); got != 0 {
		t.Fatalf("Person count = %d, want 0", got)
	}
}

// TestRunWriteOnReadTxRejected confirms a write run through Run inside a read
// transaction is rejected with ErrReadOnly.
func TestRunWriteOnReadTxRejected(t *testing.T) {
	db := openMem(t, "v3.gr")

	err := db.View(func(tx *Tx) error {
		_, err := tx.Run("CREATE (:Person {name:'Ada'})", nil)
		return err
	})
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Run write on read tx = %v, want ErrReadOnly", err)
	}
}

// TestRunWriteReturnsRows confirms Run is the single read/write entry point: a
// write with a RETURN streams its rows and reports its mutations through the
// result's Summary, and the writes commit with the transaction.
func TestRunWriteReturnsRows(t *testing.T) {
	db := openMem(t, "v4.gr")

	err := db.Update(func(tx *Tx) error {
		res, err := tx.Run("CREATE (p:Person {name:$n}) RETURN p.name AS name",
			map[string]value.Value{"n": value.String("Ada")})
		if err != nil {
			return err
		}
		defer func() { _ = res.Close() }()
		row, ok, err := res.NextRow()
		if err != nil || !ok {
			t.Fatalf("write Run next: ok=%v err=%v", ok, err)
		}
		if name, _ := row["name"].AsString(); name != "Ada" {
			t.Fatalf("returned name = %q, want Ada", name)
		}
		if s := res.Summary(); s.NodesCreated != 1 || s.PropertiesSet != 1 {
			t.Fatalf("summary = %+v, want 1 node / 1 prop", s)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := personCount(t, db); got != 1 {
		t.Fatalf("after write Run, Person count = %d, want 1", got)
	}
}

// TestRunAutoCommitsWrite confirms the database-level Run infers a write, executes
// it in an implicit transaction, commits before returning, and reports the mutation
// through the result's Summary, with the write visible to a later read.
func TestRunAutoCommitsWrite(t *testing.T) {
	db := openMem(t, "r1.gr")

	res, err := db.Run("CREATE (p:Person {name:$n}) RETURN p.name AS name",
		map[string]value.Value{"n": value.String("Ada")})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	row, ok, err := res.NextRow()
	if err != nil || !ok {
		t.Fatalf("run next: ok=%v err=%v", ok, err)
	}
	if name, _ := row["name"].AsString(); name != "Ada" {
		t.Fatalf("returned name = %q, want Ada", name)
	}
	if s := res.Summary(); s.NodesCreated != 1 || s.PropertiesSet != 1 {
		t.Fatalf("summary = %+v, want 1 node / 1 prop", s)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := personCount(t, db); got != 1 {
		t.Fatalf("after auto-commit Run, Person count = %d, want 1", got)
	}
}

// TestRunAutoReadsSnapshot confirms the database-level Run infers a read and streams
// the row, committing nothing of its own.
func TestRunAutoReadsSnapshot(t *testing.T) {
	db := openMem(t, "r2.gr")
	if _, err := db.Exec("CREATE (:Person {name:'Ada'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := db.Run("MATCH (p:Person) RETURN p.name AS name", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer func() { _ = res.Close() }()
	row, ok, err := res.NextRow()
	if err != nil || !ok {
		t.Fatalf("run next: ok=%v err=%v", ok, err)
	}
	if name, _ := row["name"].AsString(); name != "Ada" {
		t.Fatalf("read name = %q, want Ada", name)
	}
	if s := res.Summary(); s.NodesCreated != 0 {
		t.Fatalf("read summary = %+v, want zero", s)
	}
}

// TestRunSchemaCommand confirms a schema command run through the database-level Run
// applies and reports its change through the result's Summary.
func TestRunSchemaCommand(t *testing.T) {
	db := openMem(t, "r3.gr")

	res, err := db.Run("CREATE CONSTRAINT person_name FOR (p:Person) REQUIRE p.name IS UNIQUE", nil)
	if err != nil {
		t.Fatalf("run schema: %v", err)
	}
	if s := res.Summary(); s.ConstraintsAdded != 1 {
		t.Fatalf("schema summary = %+v, want 1 constraint added", s)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestExplicitCommit drives a transaction by hand: Begin, Exec, Commit, and a
// deferred Rollback that is a no-op once the commit has finished.
func TestExplicitCommit(t *testing.T) {
	db := openMem(t, "e1.gr")

	tx, err := db.Begin(Write)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("CREATE (:Person {name:'Ada'})", nil); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := personCount(t, db); got != 1 {
		t.Fatalf("after commit, Person count = %d, want 1", got)
	}
}

// TestExplicitRollback confirms an explicit Rollback discards the transaction's
// writes.
func TestExplicitRollback(t *testing.T) {
	db := openMem(t, "e2.gr")

	tx, err := db.Begin(Write)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("CREATE (:Person {name:'Ada'})", nil); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := personCount(t, db); got != 0 {
		t.Fatalf("after rollback, Person count = %d, want 0", got)
	}
}

// TestTxnDoneAfterFinish confirms a transaction method called after the
// transaction has finished returns ErrTxnDone.
func TestTxnDoneAfterFinish(t *testing.T) {
	db := openMem(t, "e3.gr")

	tx, err := db.Begin(Write)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := tx.Exec("CREATE (:Person {name:'Ada'})", nil); !errors.Is(err, ErrTxnDone) {
		t.Fatalf("Exec after commit = %v, want ErrTxnDone", err)
	}
	if _, err := tx.Run("MATCH (p) RETURN p", nil); !errors.Is(err, ErrTxnDone) {
		t.Fatalf("Run after commit = %v, want ErrTxnDone", err)
	}
	if err := tx.Commit(); !errors.Is(err, ErrTxnDone) {
		t.Fatalf("double commit = %v, want ErrTxnDone", err)
	}
}
