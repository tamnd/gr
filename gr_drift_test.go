package gr

import (
	"context"
	"strings"
	"testing"

	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/vfs"
)

// TestCompileReplansOnDataDrift is the end-to-end proof that a cached plan is re-costed
// when the data drifts under a fixed schema. The schema is interned by the first write,
// so the cache key (which carries the catalog version) is stable across the rest of the
// test: every later compile hits the same key, and only drift can change what comes back.
// The query is a linear chain, which the cost model anchors on the rarer end. With A rare
// and B abundant the plan scans A first; flooding the graph with A flips their relative
// sizes, and the next compile must re-plan to a fresh entry that scans B first.
func TestCompileReplansOnDataDrift(t *testing.T) {
	db := openMem(t, "driftreplan.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE (a:A)-[:R]->(b:B)", nil)
	for range 5 {
		mustExec(t, db, "CREATE (:B)", nil)
	}

	const q = "MATCH (a:A)-[:R]->(b:B) RETURN a"
	first, _, err := db.compile(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	p1 := plan.String(first.Op)
	if !strings.Contains(p1, "NodeScan a") {
		t.Fatalf("plan did not anchor on the rare A end:\n%s", p1)
	}

	// Flood A so it is now the abundant end and B the rare one.
	for range 100 {
		mustExec(t, db, "CREATE (:A)", nil)
	}

	second, _, err := db.compile(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("compile reused the stale plan after the data drifted")
	}
	p2 := plan.String(second.Op)
	if !strings.Contains(p2, "NodeScan b") {
		t.Fatalf("re-planned query did not anchor on the now-rare B end:\n%s", p2)
	}
}

// TestCompileReusesPlanWithinDrift confirms a modest data change that does not move the
// relative sizes past the factor leaves the cached entry in place: the second compile
// returns the very same *Entry, so no re-parse, re-bind, or re-plan happened.
func TestCompileReusesPlanWithinDrift(t *testing.T) {
	db := openMem(t, "driftreuse.gr")
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE (a:A)-[:R]->(b:B)", nil)
	for range 5 {
		mustExec(t, db, "CREATE (:B)", nil)
	}

	const q = "MATCH (a:A)-[:R]->(b:B) RETURN a"
	first, _, err := db.compile(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}

	// A few more B nodes keep A by far the rarer end, well within the drift factor.
	for range 3 {
		mustExec(t, db, "CREATE (:B)", nil)
	}

	second, _, err := db.compile(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("compile re-planned despite no significant drift")
	}
}

// TestCompileDriftDisabled confirms a non-positive ReplanDriftFactor turns re-planning
// off: even a drift that would otherwise flip the plan keeps the cached entry, so the
// option is the escape hatch for a workload that prefers a stable plan to an adaptive one.
func TestCompileDriftDisabled(t *testing.T) {
	db, err := Open("driftdisabled.gr", Options{VFS: vfs.NewMem(), ReplanDriftFactor: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mustExec(t, db, "CREATE (a:A)-[:R]->(b:B)", nil)
	for range 5 {
		mustExec(t, db, "CREATE (:B)", nil)
	}

	const q = "MATCH (a:A)-[:R]->(b:B) RETURN a"
	first, _, err := db.compile(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}

	for range 100 {
		mustExec(t, db, "CREATE (:A)", nil)
	}

	second, _, err := db.compile(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("re-planning fired with the drift factor disabled")
	}
}
