package gr

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// profileText runs a PROFILE statement through Run and joins its "plan" column rows
// into one listing, the same shape the EXPLAIN tests read. It fails the test if the
// result is not the single-column plan listing PROFILE renders.
func profileText(t *testing.T, db *DB, q string) string {
	t.Helper()
	res, err := db.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("PROFILE %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	if cols := res.Columns(); len(cols) != 1 || cols[0] != "plan" {
		t.Fatalf("PROFILE columns = %v, want [plan]", cols)
	}
	var lines []string
	for {
		row, ok, err := res.Row()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
		s, ok := row[0].AsString()
		if !ok {
			t.Fatalf("plan cell type = %v, want string", row[0].Type())
		}
		lines = append(lines, s)
	}
	if len(lines) == 0 {
		t.Fatal("PROFILE produced no rows")
	}
	return strings.Join(lines, "\n")
}

// TestProfileReadShowsActuals confirms PROFILE of a read executes the query and
// annotates the plan with each operator's actual rows alongside the estimate. With
// three Person nodes the scan estimate and the scan's actual rows both read three, so
// the listing shows the estimate and the reality side by side, and the footer reports
// the rows returned and the rows scanned the run measured.
func TestProfileReadShowsActuals(t *testing.T) {
	db := openMem(t, "profileread.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE (:Person), (:Person), (:Person)", nil)

	plan := profileText(t, db, "PROFILE MATCH (p:Person) RETURN p")
	if !strings.Contains(plan, "NodeScan") {
		t.Fatalf("plan does not mention NodeScan:\n%s", plan)
	}
	if !strings.Contains(plan, "(est. rows 3)") {
		t.Fatalf("plan carries no row estimate:\n%s", plan)
	}
	if !strings.Contains(plan, "(actual rows 3") {
		t.Fatalf("plan carries no actual rows:\n%s", plan)
	}
	if !strings.Contains(plan, "rows returned 3") {
		t.Fatalf("footer does not report rows returned:\n%s", plan)
	}
	if !strings.Contains(plan, "rows scanned ") {
		t.Fatalf("footer does not report rows scanned:\n%s", plan)
	}
	if !strings.Contains(plan, "total time ") {
		t.Fatalf("footer does not report total time:\n%s", plan)
	}
}

// TestProfileWriteExecutesThenRollsBack is the discriminator for the write path: a
// PROFILE of a CREATE must run the write operators (so the listing carries actual
// rows) yet leave the database unchanged, since PROFILE of a write rolls back (doc 20
// §9.6). A node count of zero afterward can only hold if the executed write was
// rolled back rather than committed.
func TestProfileWriteExecutesThenRollsBack(t *testing.T) {
	db := openMem(t, "profilewrite.gr")
	defer func() { _ = db.Close() }()

	plan := profileText(t, db, "PROFILE CREATE (:Person {name: 'Ada'})")
	if !strings.Contains(plan, "Create") {
		t.Fatalf("plan does not mention Create:\n%s", plan)
	}
	if !strings.Contains(plan, "(actual rows") {
		t.Fatalf("write plan carries no actuals, so it was not executed:\n%s", plan)
	}
	// A write plan is not cardinality chosen, so PROFILE of a write omits the estimate
	// column, the same as EXPLAIN of a write renders the bare tree.
	if strings.Contains(plan, "(est. rows") {
		t.Fatalf("write plan should carry no estimates:\n%s", plan)
	}
	if n := nodeCount(t, db); n != 0 {
		t.Fatalf("node count = %d, want 0: PROFILE committed the write instead of rolling it back", n)
	}
}

// TestProfileShowsAmplification confirms the footer reports the read amplification,
// the rows scanned over the rows returned. A filter that keeps one of three nodes
// scans three and returns one, so the run did three units of work per row it produced.
func TestProfileShowsAmplification(t *testing.T) {
	db := openMem(t, "profileamp.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE (:Person {age: 1}), (:Person {age: 2}), (:Person {age: 3})", nil)

	plan := profileText(t, db, "PROFILE MATCH (p:Person) WHERE p.age = 1 RETURN p")
	if !strings.Contains(plan, "rows returned 1") {
		t.Fatalf("footer does not report one returned row:\n%s", plan)
	}
	if !strings.Contains(plan, "amplification ") {
		t.Fatalf("footer does not report amplification:\n%s", plan)
	}
}

// TestQueryRejectsProfile confirms the cache-backed read API refuses PROFILE, which
// yields the annotated plan listing rather than the query's rows; PROFILE runs
// through Run.
func TestQueryRejectsProfile(t *testing.T) {
	db := openMem(t, "profilequery.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Query("PROFILE MATCH (p:Person) RETURN p", nil); !errors.Is(err, ErrProfile) {
		t.Fatalf("Query of a PROFILE returned %v, want ErrProfile", err)
	}
}

// TestExecRejectsProfile confirms the summary-only write API refuses PROFILE, which
// produces rows, not a mutation summary.
func TestExecRejectsProfile(t *testing.T) {
	db := openMem(t, "profileexec.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("PROFILE CREATE (:Person)", nil); !errors.Is(err, ErrProfile) {
		t.Fatalf("Exec of a PROFILE returned %v, want ErrProfile", err)
	}
}

// TestProfileRejectsSchemaCommand confirms PROFILE of a schema command is rejected: a
// schema command changes the catalog outside the operator pipeline, so it has no
// execution to instrument.
func TestProfileRejectsSchemaCommand(t *testing.T) {
	db := openMem(t, "profileschema.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "PROFILE CREATE INDEX FOR (p:Person) ON (p.email)", nil); !errors.Is(err, ErrProfileSchema) {
		t.Fatalf("PROFILE of a schema command returned %v, want ErrProfileSchema", err)
	}
}

// TestProfileAndExplainTogether confirms the two prefixes cannot both attach to one
// statement: PROFILE already does everything EXPLAIN does, so the parser rejects the
// pair rather than guess which the caller meant.
func TestProfileAndExplainTogether(t *testing.T) {
	db := openMem(t, "profileexplain.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Run(context.Background(), "EXPLAIN PROFILE MATCH (p:Person) RETURN p", nil); err == nil {
		t.Fatal("EXPLAIN PROFILE was accepted, want a parse error")
	}
}

// TestProfileRejectedInTransaction confirms a managed transaction refuses PROFILE: it
// rolls the statement back to leave nothing behind, which it cannot do inside a
// transaction the caller owns and will commit. PROFILE runs through the
// database-level Run.
func TestProfileRejectedInTransaction(t *testing.T) {
	db := openMem(t, "profiletx.gr")
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(context.Background(), Write)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Run(context.Background(), "PROFILE MATCH (p:Person) RETURN p", nil); !errors.Is(err, ErrProfile) {
		t.Fatalf("tx PROFILE returned %v, want ErrProfile", err)
	}
}
