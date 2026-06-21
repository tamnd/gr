package gr

import (
	"errors"
	"strings"
	"testing"
)

// planText runs an EXPLAIN statement through Run and joins its "plan" column rows
// back into one listing, the form the rendering produced before it was split into
// rows. It fails the test if the result does not look like a plan listing.
func planText(t *testing.T, db *DB, q string) string {
	t.Helper()
	res, err := db.Run(q, nil)
	if err != nil {
		t.Fatalf("EXPLAIN %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	if cols := res.Columns(); len(cols) != 1 || cols[0] != "plan" {
		t.Fatalf("EXPLAIN columns = %v, want [plan]", cols)
	}
	var lines []string
	for {
		row, ok, err := res.Next()
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
		t.Fatal("EXPLAIN produced no plan rows")
	}
	return strings.Join(lines, "\n")
}

// TestExplainReadShowsPlanWithoutExecuting confirms EXPLAIN of a read returns the
// operator tree as rows rather than the query's own rows. The graph has one Person,
// so the unexplained query would return a row; EXPLAIN returns the plan instead, and
// the listing names the scan the planner chose.
func TestExplainReadShowsPlanWithoutExecuting(t *testing.T) {
	db := openMem(t, "explainread.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE (:Person {name: 'Ada'})", nil)

	plan := planText(t, db, "EXPLAIN MATCH (p:Person) RETURN p.name")
	if !strings.Contains(plan, "NodeScan") {
		t.Fatalf("plan does not mention NodeScan:\n%s", plan)
	}
	if !strings.Contains(plan, "Project") {
		t.Fatalf("plan does not mention Project:\n%s", plan)
	}
	// The plan listing must not carry the query's own column. A row whose only
	// column is "plan" cannot be the MATCH result, which would project "p.name".
}

// TestExplainWriteDoesNotMutate is the discriminator for the write path: EXPLAIN of
// a CREATE must render the plan and create nothing. A node count of zero afterward
// can only hold if the statement was planned but never run.
func TestExplainWriteDoesNotMutate(t *testing.T) {
	db := openMem(t, "explainwrite.gr")
	defer func() { _ = db.Close() }()

	plan := planText(t, db, "EXPLAIN CREATE (:Person {name: 'Ada'})")
	if !strings.Contains(plan, "Create") {
		t.Fatalf("plan does not mention Create:\n%s", plan)
	}
	if n := nodeCount(t, db); n != 0 {
		t.Fatalf("node count = %d, want 0: EXPLAIN ran the write instead of planning it", n)
	}
}

// TestExplainShowsIndexSeek confirms EXPLAIN reflects the planner's access-path
// choice: with an index on Person.email, an equality match plans a NodeIndexSeek,
// not a NodeScan. This proves the explain path runs the same seek rewrite the read
// path does, so a later planner change is inspectable through EXPLAIN.
func TestExplainShowsIndexSeek(t *testing.T) {
	db := openMem(t, "explainseek.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE INDEX FOR (p:Person) ON (p.email)", nil)

	plan := planText(t, db, "EXPLAIN MATCH (p:Person) WHERE p.email = 'a@x' RETURN p")
	if !strings.Contains(plan, "NodeIndexSeek") {
		t.Fatalf("plan does not use the index:\n%s", plan)
	}
	if strings.Contains(plan, "NodeScan") {
		t.Fatalf("plan still scans despite the index:\n%s", plan)
	}
}

// TestExplainShowsRowEstimates confirms the read path annotates the plan with the
// cost model's per-operator row estimates: with three Person nodes, the scan line
// carries an estimate drawn from the live label count, so EXPLAIN shows not just the
// plan but the cardinalities it was chosen on.
func TestExplainShowsRowEstimates(t *testing.T) {
	db := openMem(t, "explainrows.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE (:Person), (:Person), (:Person)", nil)

	plan := planText(t, db, "EXPLAIN MATCH (p:Person) RETURN p")
	if !strings.Contains(plan, "(est. rows ") {
		t.Fatalf("plan carries no row estimates:\n%s", plan)
	}
	if !strings.Contains(plan, "NodeScan p:#1  (est. rows 3)") {
		t.Fatalf("scan estimate does not reflect the three Person nodes:\n%s", plan)
	}
}

// TestExplainWriteTxOmitsEstimates confirms EXPLAIN inside a write transaction shows
// the plan without estimates: the transaction holds the engine lock, so it passes no
// statistics for the same reason it passes no index oracle, and the listing must
// still render rather than block.
func TestExplainWriteTxOmitsEstimates(t *testing.T) {
	db := openMem(t, "explainwritetxrows.gr")
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(Write)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Run("EXPLAIN MATCH (p:Person) RETURN p", nil)
	if err != nil {
		t.Fatalf("write-tx EXPLAIN: %v", err)
	}
	defer func() { _ = res.Close() }()
	var sawScan, sawEstimate bool
	for {
		row, ok, err := res.Next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
		s, _ := row[0].AsString()
		if strings.Contains(s, "NodeScan") {
			sawScan = true
		}
		if strings.Contains(s, "(est. rows ") {
			sawEstimate = true
		}
	}
	if !sawScan {
		t.Fatal("write-tx EXPLAIN did not render the plan")
	}
	if sawEstimate {
		t.Fatal("write-tx EXPLAIN carried estimates despite holding the engine lock")
	}
}

// TestExplainRejectsSchemaCommand confirms EXPLAIN of a schema command is an error:
// a schema command changes the catalog outside the operator pipeline, so it has no
// plan to render.
func TestExplainRejectsSchemaCommand(t *testing.T) {
	db := openMem(t, "explainschema.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Run("EXPLAIN CREATE INDEX FOR (p:Person) ON (p.email)", nil); !errors.Is(err, ErrExplainSchema) {
		t.Fatalf("EXPLAIN of a schema command returned %v, want ErrExplainSchema", err)
	}
}

// TestQueryRejectsExplain confirms the cache-backed read API refuses EXPLAIN, since
// it yields a plan listing rather than the query's rows; EXPLAIN runs through Run.
func TestQueryRejectsExplain(t *testing.T) {
	db := openMem(t, "explainquery.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Query("EXPLAIN MATCH (p:Person) RETURN p", nil); !errors.Is(err, ErrExplain) {
		t.Fatalf("Query of an EXPLAIN returned %v, want ErrExplain", err)
	}
}

// TestExecRejectsExplain confirms the summary-only write API refuses EXPLAIN, which
// produces rows, not a mutation summary.
func TestExecRejectsExplain(t *testing.T) {
	db := openMem(t, "explainexec.gr")
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("EXPLAIN CREATE (:Person)", nil); !errors.Is(err, ErrExplain) {
		t.Fatalf("Exec of an EXPLAIN returned %v, want ErrExplain", err)
	}
}

// TestExplainInReadTransaction confirms a read transaction's Run serves EXPLAIN from
// the engine catalog and returns the plan without running the query.
func TestExplainInReadTransaction(t *testing.T) {
	db := openMem(t, "explainreadtx.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE (:Person {name: 'Ada'})", nil)

	tx, err := db.Begin(Read)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Run("EXPLAIN MATCH (p:Person) RETURN p", nil)
	if err != nil {
		t.Fatalf("tx EXPLAIN: %v", err)
	}
	defer func() { _ = res.Close() }()
	if cols := res.Columns(); len(cols) != 1 || cols[0] != "plan" {
		t.Fatalf("columns = %v, want [plan]", cols)
	}
	row, ok, err := res.Next()
	if err != nil || !ok {
		t.Fatalf("first plan row: ok=%v err=%v", ok, err)
	}
	first, _ := row[0].AsString()
	if !strings.Contains(first, "Project") {
		t.Fatalf("first plan row = %q, want a Project root", first)
	}
}

// TestExplainInWriteTransaction confirms a write transaction's Run serves EXPLAIN
// while it holds the engine lock: it must plan against the transaction's own catalog
// view and skip the seek rewrite, so it cannot deadlock, and it must mutate nothing.
func TestExplainInWriteTransaction(t *testing.T) {
	db := openMem(t, "explainwritetx.gr")
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(Write)
	if err != nil {
		t.Fatal(err)
	}

	res, err := tx.Run("EXPLAIN CREATE (:Person {name: 'Ada'})", nil)
	if err != nil {
		t.Fatalf("write-tx EXPLAIN: %v", err)
	}
	var planted bool
	for {
		row, ok, err := res.Next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
		if s, _ := row[0].AsString(); strings.Contains(s, "Create") {
			planted = true
		}
	}
	_ = res.Close()
	if !planted {
		t.Fatal("write-tx EXPLAIN plan did not mention Create")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if n := nodeCount(t, db); n != 0 {
		t.Fatalf("node count = %d, want 0: EXPLAIN ran the write inside the transaction", n)
	}
}

// TestTxExecRejectsExplain confirms a transaction's summary-only Exec refuses
// EXPLAIN, the same as the database-level Exec.
func TestTxExecRejectsExplain(t *testing.T) {
	db := openMem(t, "explaintxexec.gr")
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(Write)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec("EXPLAIN CREATE (:Person)", nil); !errors.Is(err, ErrExplain) {
		t.Fatalf("tx.Exec of an EXPLAIN returned %v, want ErrExplain", err)
	}
}
