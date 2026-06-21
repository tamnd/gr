package gr

import (
	"sort"
	"strings"
	"testing"
)

// TestWcojTriangleResults is the end-to-end proof that the worst-case-optimal join
// returns exactly the binary plan's rows. It builds a directed triangle plus a
// dangling edge that closes nothing, then asks for every directed triangle. The
// answer is the three rotations of the one closed cycle and nothing from the open
// path, which is what the binary expand-into plan would return too, so swapping in
// the Intersect operator is meaning-preserving.
func TestWcojTriangleResults(t *testing.T) {
	db := openMem(t, "wcojtriangle.gr")
	defer func() { _ = db.Close() }()

	// a -> b -> c -> a is the only closed cycle; c -> d dangles and never closes.
	mustExec(t, db, "CREATE (a:N {name: 'a'})-[:R]->(b:N {name: 'b'})-[:R]->(c:N {name: 'c'})-[:R]->(a)", nil)
	mustExec(t, db, "MATCH (c:N {name: 'c'}) CREATE (c)-[:R]->(:N {name: 'd'})", nil)

	const q = "MATCH (x)-[:R]->(y)-[:R]->(z)-[:R]->(x) RETURN x.name AS xn, y.name AS yn, z.name AS zn"

	// The query plans to the Intersect operator on the live statistics: a triangle's
	// closing edge is where WCOJ pays, so the planner takes it.
	if plan := planText(t, db, "EXPLAIN "+q); !strings.Contains(plan, "Intersect") {
		t.Fatalf("triangle query did not plan to a WCOJ Intersect:\n%s", plan)
	}

	rows := collectRows(t, db, q, nil)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		xn, _ := r["xn"].AsString()
		yn, _ := r["yn"].AsString()
		zn, _ := r["zn"].AsString()
		got = append(got, xn+yn+zn)
	}
	sort.Strings(got)

	// The three rotations of the single directed triangle, each a distinct starting node
	// with its own three distinct edges.
	want := []string{"abc", "bca", "cab"}
	if len(got) != len(want) {
		t.Fatalf("triangle query returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("triangle query returned %v, want %v", got, want)
		}
	}
}
