package gr

import (
	"fmt"
	"testing"

	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// rowKeyOf renders one result row as a stable string for multiset comparison.
func rowKeyOf(cols []string, row map[string]value.Value) string {
	s := ""
	for _, c := range cols {
		s += c + "=" + row[c].String() + ";"
	}
	return s
}

// multiset turns a result row slice into a count-by-canonical-row map.
func multiset(cols []string, rows []map[string]value.Value) map[string]int {
	m := map[string]int{}
	for _, r := range rows {
		m[rowKeyOf(cols, r)]++
	}
	return m
}

// TestSpillThroughDB drives a cartesian join through the public API twice over the
// same graph, once with no memory budget (the in-memory path) and once with a tiny
// budget that forces the join's build side to spill to temp files beside the
// database. The two runs must return the same rows, the budget must actually have
// spilled (the temp-file counter moved), and every spill file must be gone once the
// results are closed.
func TestSpillThroughDB(t *testing.T) {
	const cols = "an" // the single returned column name
	const n = 300

	build := func(memBudget int64) ([]string, []map[string]value.Value, *DB, *vfs.Mem) {
		fsys := vfs.NewMem()
		db, err := Open("spill.gr", Options{VFS: fsys, MemBudget: memBudget})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if _, err := db.Exec(fmt.Sprintf("CREATE (:Person {name: 'p%d'})", i), nil); err != nil {
				t.Fatal(err)
			}
		}
		// A disconnected two-pattern match plans as a cartesian Join over two Person
		// scans; the WHERE filters it back down so the answer is small but the join's
		// build side is the whole Person set.
		q := "MATCH (a:Person), (b:Person) WHERE a.name = b.name RETURN a.name AS an"
		rows := collectRows(t, db, q, nil)
		return []string{"an"}, rows, db, fsys
	}

	colsRef, want, db0, _ := build(0)
	defer func() { _ = db0.Close() }()

	_, got, db1, fsys1 := build(256)
	defer func() { _ = db1.Close() }()

	wantSet := multiset(colsRef, want)
	gotSet := multiset(colsRef, got)
	if len(wantSet) != n {
		t.Fatalf("expected %d distinct rows from the in-memory run, got %d", n, len(wantSet))
	}
	if len(wantSet) != len(gotSet) {
		t.Fatalf("distinct rows: in-memory %d, spilled %d", len(wantSet), len(gotSet))
	}
	for k, c := range wantSet {
		if gotSet[k] != c {
			t.Fatalf("row %q count: in-memory %d, spilled %d", k, c, gotSet[k])
		}
	}

	// The budgeted run must have opened at least one spill file.
	if seq := db1.tmpSeq.Load(); seq == 0 {
		t.Fatal("expected the budgeted run to spill, but no temp file was opened")
	} else {
		// Every spill file the run opened must have been discarded on close of the result.
		for i := uint64(1); i <= seq; i++ {
			name := fmt.Sprintf("spill.gr-spill-%d.tmp", i)
			if fsys1.Exists(name) {
				t.Fatalf("spill file %s was left behind", name)
			}
		}
	}
}
