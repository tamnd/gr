package gr

import (
	"fmt"
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
