package gr

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// TestIntersectCountMatchesIntersect is the end-to-end proof that the fused
// triangle count returns the same number the row-returning Intersect plan would.
// It builds a deterministic graph with several directed triangles, a dangling edge
// that closes nothing, and a parallel edge that forms a second triangle through the
// same three nodes (so the count must pair the multigraph edges, not dedupe by
// node), then checks that count(*) over the triangle equals the row count of the
// same triangle pattern returned without aggregation.
func TestIntersectCountMatchesIntersect(t *testing.T) {
	db := openMem(t, "intersectcount.gr")
	defer func() { _ = db.Close() }()

	// A directed graph over all ordered pairs i != j with a deterministic sparse
	// subset of edges. Directed edges in both orientations give directed cycles, so
	// the triangle pattern has matches (a forward-only DAG would have none).
	const n = 40
	for i := range n {
		mustExec(t, db, fmt.Sprintf("CREATE (:N {i:%d})", i), nil)
	}
	for i := range n {
		for j := range n {
			if i == j || (i*7+j*13)%5 != 0 { // deterministic ~1/5-dense directed subset
				continue
			}
			mustExec(t, db, fmt.Sprintf("MATCH (a:N {i:%d}), (b:N {i:%d}) CREATE (a)-[:R]->(b)", i, j), nil)
		}
	}
	// A parallel edge: a second 0 -> chosen edge, so a triangle through it is a
	// distinct match that shares two of its three nodes with another.
	mustExec(t, db, "MATCH (a:N {i:0}), (b:N {i:3}) CREATE (a)-[:R]->(b)", nil)
	// A dangling edge that closes nothing.
	mustExec(t, db, "CREATE (:N {i:1000})-[:R]->(:N {i:1001})", nil)

	const tri = "MATCH (a:N)-[:R]->(b:N)-[:R]->(c:N)-[:R]->(a)"

	// The count query must plan to the fused IntersectCount.
	if plan := planText(t, db, "EXPLAIN "+tri+" RETURN count(*) AS n"); !strings.Contains(plan, "IntersectCount") {
		t.Fatalf("count query did not plan to IntersectCount:\n%s", plan)
	}

	rows := collectRows(t, db, tri+" RETURN count(*) AS n", nil)
	gotCount, _ := rows[0]["n"].AsInt()

	// The independent oracle: the same pattern returned row by row, counted in Go.
	// This runs through the row-returning Intersect operator, not the fused count.
	wantRows := collectRows(t, db, tri+" RETURN a.i AS ai, b.i AS bi, c.i AS ci", nil)
	want := int64(len(wantRows))

	if gotCount != want {
		t.Fatalf("IntersectCount returned %d, row-returning Intersect returned %d", gotCount, want)
	}
	if gotCount == 0 {
		t.Fatal("test graph produced no triangles; it is not exercising the count")
	}
	t.Logf("triangles counted both ways: %d", gotCount)
}

// TestIntersectCountParallelMatchesOracle drives the fused count over enough anchor
// nodes to cross the morsel-parallel threshold, so the count runs across goroutines
// that each sum the triangles their morsels close. It builds many disjoint directed
// 3-cycles (cheap to create, one statement each) and checks the fused parallel count
// equals the row-returning Intersect's count, the same oracle the serial test uses.
// The sum reduction is associative and commutative, so the parallel total must match
// the serial one node for node; a concurrency bug would show up as a wrong total here.
func TestIntersectCountParallelMatchesOracle(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("parallel path needs more than one core")
	}
	db := openMem(t, "intersectcount_parallel.gr")
	defer func() { _ = db.Close() }()

	// 800 disjoint directed triangles is 2400 nodes, past the 2*1024 morsel threshold,
	// so the count fans across workers instead of draining on one goroutine.
	const triangles = 800
	for range triangles {
		mustExec(t, db, "CREATE (a:N)-[:R]->(b:N)-[:R]->(c:N)-[:R]->(a)", nil)
	}

	const tri = "MATCH (a:N)-[:R]->(b:N)-[:R]->(c:N)-[:R]->(a)"
	if plan := planText(t, db, "EXPLAIN "+tri+" RETURN count(*) AS n"); !strings.Contains(plan, "IntersectCount") {
		t.Fatalf("count query did not plan to IntersectCount:\n%s", plan)
	}

	rows := collectRows(t, db, tri+" RETURN count(*) AS n", nil)
	gotCount, _ := rows[0]["n"].AsInt()

	wantRows := collectRows(t, db, tri+" RETURN a.i AS ai", nil)
	want := int64(len(wantRows))

	if gotCount != want {
		t.Fatalf("parallel IntersectCount returned %d, row-returning Intersect returned %d", gotCount, want)
	}
	// Each directed 3-cycle matches the closed triangle in three rotations.
	if gotCount != int64(triangles*3) {
		t.Fatalf("expected %d triangle matches, got %d", triangles*3, gotCount)
	}
	t.Logf("triangles counted in parallel: %d", gotCount)
}
